package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juls0730/fluxd/models"
)

type ContainerProxy struct {
	mu          sync.RWMutex
	urlMap      map[string]*containerRoute
	db          *sql.DB
	cm          *ContainerManager
	activeConns int64
}

type containerRoute struct {
	containerID string
	port        int
	url         string
	proxy       *httputil.ReverseProxy
	isActive    bool
}

func (cp *ContainerProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cp.mu.RLock()
	defer cp.mu.RUnlock()

	// Extract app name from host
	appUrl := r.Host
	var container *containerRoute
	container, exists := cp.urlMap[appUrl]
	if !exists || !container.isActive {
		container = &containerRoute{
			url: appUrl,
		}
		var deploymentID int64
		cp.db.QueryRow("SELECT id FROM deployments WHERE url = ?", appUrl).Scan(&deploymentID)
		if deploymentID == 0 {
			fmt.Printf("No deployment found for url: %s\n", appUrl)
			http.Error(w, "Container not found", http.StatusNotFound)
			return
		}

		cp.db.QueryRow("SELECT container_id FROM containers WHERE deployment_id = ?", deploymentID).Scan(&container.containerID)
		if container.containerID == "" {
			fmt.Printf("No container found for deployment: %d\n", deploymentID)
			http.Error(w, "Container not found", http.StatusNotFound)
			return
		}

		var projectConfigStr string
		if err := cp.db.QueryRow("SELECT project_config FROM apps WHERE deployment_id = ?", deploymentID).Scan(&projectConfigStr); err != nil || projectConfigStr == "" {
			http.Error(w, "Container not found", http.StatusNotFound)
			return
		}
		var projectConfig models.ProjectConfig
		if err := json.Unmarshal([]byte(projectConfigStr), &projectConfig); err != nil {
			http.Error(w, "Failed to parse json", http.StatusNotFound)
			return
		}
		container.port = projectConfig.Port

		cp.urlMap[appUrl] = container
	}

	if container.proxy == nil {
		containerJSON, err := cp.cm.dockerClient.ContainerInspect(r.Context(), container.containerID)
		if err != nil {
			log.Printf("Failed to inspect container: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		url, err := url.Parse(fmt.Sprintf("http://%s:%d", containerJSON.NetworkSettings.IPAddress, container.port))
		if err != nil {
			log.Printf("Failed to parse URL: %v\n", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		container.proxy = httputil.NewSingleHostReverseProxy(url)
	}

	container.proxy.ServeHTTP(w, r)
}

func (cp *ContainerProxy) AddContainer(projectConfig models.ProjectConfig, containerID string) error {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	containerJSON, err := cp.cm.dockerClient.ContainerInspect(context.Background(), containerID)
	if err != nil {
		log.Printf("Failed to inspect container: %v\n", err)
		return err
	}
	containerUrl, err := url.Parse(fmt.Sprintf("http://%s:%d", containerJSON.NetworkSettings.IPAddress, projectConfig.Port))
	if err != nil {
		return err
	}

	proxy := httputil.NewSingleHostReverseProxy(containerUrl)

	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		atomic.AddInt64(&cp.activeConns, 1)
		originalDirector(req)
	}

	proxy.ModifyResponse = func(resp *http.Response) error {
		atomic.AddInt64(&cp.activeConns, -1)
		return nil
	}

	// Handle errors
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("Proxy error: %v", err)
		http.Error(w, "Service unavailable", http.StatusServiceUnavailable)
	}

	newRoute := &containerRoute{
		url:      projectConfig.Url,
		proxy:    proxy,
		port:     projectConfig.Port,
		isActive: true,
	}

	cp.urlMap[projectConfig.Url] = newRoute
	return nil
}

func (cp *ContainerProxy) RemoveContainer(containerID string) error {
	cp.mu.Lock()
	defer cp.mu.Unlock()

	var deploymentID int64
	if err := cp.db.QueryRow("SELECT deployment_id FROM containers WHERE id = ?", containerID).Scan(&deploymentID); err != nil {
		return err
	}

	var url string
	if err := cp.db.QueryRow("SELECT url FROM deployments WHERE id = ?", deploymentID).Scan(&url); err != nil {
		return err
	}

	container, exists := cp.urlMap[url]
	if !exists {
		return fmt.Errorf("container not found")
	}

	container.isActive = false

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			delete(cp.urlMap, url)
			return nil
		default:
			if atomic.LoadInt64(&cp.activeConns) == 0 {
				delete(cp.urlMap, url)
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func (cp *ContainerProxy) Start() {
	server := &http.Server{
		Addr:    ":7465",
		Handler: cp,
	}

	go func() {
		log.Printf("Proxy server starting on http://127.0.0.1:7465\n")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Proxy server error: %v", err)
		}
	}()
}
