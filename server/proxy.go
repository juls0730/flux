package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var ReverseProxy *Proxy

type Proxy struct {
	deployments sync.Map
}

func (p *Proxy) AddDeployment(deployment *Deployment) {
	log.Printf("Adding deployment %s\n", deployment.URL)
	p.deployments.Store(deployment.URL, deployment)
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host

	deployment, ok := p.deployments.Load(host)
	if !ok {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	atomic.AddInt64(&deployment.(*Deployment).Proxy.activeRequests, 1)

	container := deployment.(*Deployment).Proxy.currentHead
	if container == nil {
		http.Error(w, "No active container found", http.StatusNotFound)
		return
	}

	containerJSON, err := dockerClient.ContainerInspect(context.Background(), string(container.ContainerID[:]))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	containerUrl, err := url.Parse(fmt.Sprintf("http://%s:%d", containerJSON.NetworkSettings.IPAddress, container.Deployment.Port))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL = containerUrl
			req.Host = containerUrl.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			atomic.AddInt64(&deployment.(*Deployment).Proxy.activeRequests, -1)
			return nil
		},
	}

	proxy.ServeHTTP(w, r)
}

type DeploymentProxy struct {
	deployment     *Deployment
	currentHead    *Container
	gracePeriod    time.Duration
	activeRequests int64
}

func (dp *DeploymentProxy) GracefulShutdown(oldContainers []*Container) {
	ctx, cancel := context.WithTimeout(context.Background(), dp.gracePeriod)
	defer cancel()

	// Create a channel to signal when wait group is done
	for {
		select {
		case <-ctx.Done():
			break
		default:
			if atomic.LoadInt64(&dp.activeRequests) == 0 {
				break
			}

			time.Sleep(time.Second)
		}

		if atomic.LoadInt64(&dp.activeRequests) == 0 || ctx.Err() != nil {
			break
		}
	}

	for _, container := range oldContainers {
		err := RemoveDockerContainer(context.Background(), string(container.ContainerID[:]))
		if err != nil {
			log.Printf("Failed to remove container: %v\n", err)
		}
	}
}

func InitProxy(apps *AppManager) {
	ReverseProxy = &Proxy{}

	apps.Range(func(key, value interface{}) bool {
		app := value.(*App)
		ReverseProxy.AddDeployment(&app.Deployment)
		return true
	})
}

func InitReverseProxy() {
	InitProxy(Apps)
	port := os.Getenv("FLUXD_PROXY_PORT")
	if port == "" {
		port = "7465"
	}

	go func() {
		log.Printf("Proxy server starting on http://127.0.0.1:%s\n", port)
		if err := http.ListenAndServe(fmt.Sprintf(":%s", port), ReverseProxy); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Proxy server error: %v", err)
		}
	}()
}
