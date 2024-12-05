package server

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juls0730/fluxd/models"
)

type ContainerProxy struct {
	routes      *RouteCache
	db          *sql.DB
	cm          *ContainerManager
	activeConns int64
}

type RouteCache struct {
	m sync.Map
}

type containerRoute struct {
	containerID string
	port        int
	url         string
	proxy       *httputil.ReverseProxy
	isActive    bool
}

func (rc *RouteCache) GetRoute(appUrl string) *containerRoute {

	container, exists := rc.m.Load(appUrl)
	if !exists {
		return nil
	}

	return container.(*containerRoute)
}

func (rc *RouteCache) SetRoute(appUrl string, container *containerRoute) {
	rc.m.Store(appUrl, container)
}

func (rc *RouteCache) DeleteRoute(appUrl string) {
	rc.m.Delete(appUrl)
}

func (cp *ContainerProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Extract app name from host
	appUrl := r.Host

	container := cp.routes.GetRoute(appUrl)
	if container == nil {
		http.Error(w, "Container not found", http.StatusNotFound)
		return
	}

	container.proxy.ServeHTTP(w, r)
}

func (cp *ContainerProxy) AddContainer(projectConfig models.ProjectConfig, containerID string) error {
	containerJSON, err := cp.cm.dockerClient.ContainerInspect(context.Background(), containerID)
	if err != nil {
		log.Printf("Failed to inspect container: %v\n", err)
		return err
	}

	containerUrl, err := url.Parse(fmt.Sprintf("http://%s:%d", containerJSON.NetworkSettings.IPAddress, projectConfig.Port))
	if err != nil {
		log.Printf("Failed to parse URL: %v\n", err)
		return err
	}
	proxy := cp.createProxy(containerUrl)

	newRoute := &containerRoute{
		url:      projectConfig.Url,
		proxy:    proxy,
		port:     projectConfig.Port,
		isActive: true,
	}

	cp.routes.SetRoute(projectConfig.Url, newRoute)
	return nil
}

func (cp *ContainerProxy) createProxy(url *url.URL) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(url)

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		atomic.AddInt64(&cp.activeConns, 1)

		// Validate URL before directing
		if url == nil {
			log.Printf("URL is nil")
			return
		}

		originalDirector(req)
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		atomic.AddInt64(&cp.activeConns, -1)
		return nil
	}

	// Handle errors
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		atomic.AddInt64(&cp.activeConns, -1)

		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)

		// Ensure request body is closed
		if r.Body != nil {
			r.Body.Close()
		}
	}

	return proxy
}

func (cp *ContainerProxy) RemoveContainer(containerID string) error {
	var deploymentID int64
	if err := cp.db.QueryRow("SELECT deployment_id FROM containers WHERE id = ?", containerID).Scan(&deploymentID); err != nil {
		return err
	}

	var url string
	if err := cp.db.QueryRow("SELECT url FROM deployments WHERE id = ?", deploymentID).Scan(&url); err != nil {
		return err
	}

	container := cp.routes.GetRoute(url)
	if container == nil {
		return fmt.Errorf("container not found")
	}

	container.isActive = false

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			cp.routes.DeleteRoute(url)
			return nil
		default:
			if atomic.LoadInt64(&cp.activeConns) == 0 {
				cp.routes.DeleteRoute(url)
				return nil
			}
		}
	}
}

func (cp *ContainerProxy) ScanRoutes() {
	rows, err := cp.db.Query("SELECT url, id FROM deployments")
	if err != nil {
		log.Printf("Failed to query deployments: %v\n", err)
		return
	}
	defer rows.Close()

	var containers []models.Containers
	for rows.Next() {
		var url string
		var deploymentID int64
		if err := rows.Scan(&url, &deploymentID); err != nil {
			log.Printf("Failed to scan deployment: %v\n", err)
			return
		}

		rows, err := cp.db.Query("SELECT * FROM containers WHERE deployment_id = ?", deploymentID)
		if err != nil {
			log.Printf("Failed to query containers: %v\n", err)
			return
		}
		defer rows.Close()

		for rows.Next() {
			var container models.Containers
			if err := rows.Scan(&container.ID, &container.ContainerID, &container.Head, &container.DeploymentID, &container.CreatedAt); err != nil {
				log.Printf("Failed to scan container: %v\n", err)
				return
			}

			fmt.Printf("Found container: %s\n", container.ContainerID)

			containers = append(containers, container)
		}
	}
}

func (cp *ContainerProxy) Start() {
	cp.ScanRoutes()
	port := os.Getenv("FLUXD_PROXY_PORT")
	if port == "" {
		port = "7465"
	}

	server := &http.Server{
		Addr:    fmt.Sprintf(":%s", port),
		Handler: cp,
	}

	go func() {
		log.Printf("Proxy server starting on http://127.0.0.1:%s\n", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Proxy server error: %v", err)
		}
	}()
}
