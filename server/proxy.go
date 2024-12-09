package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

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

	deployment.(*Deployment).Proxy.proxy.ServeHTTP(w, r)
}

type DeploymentProxy struct {
	deployment     *Deployment
	currentHead    *Container
	proxy          *httputil.ReverseProxy
	gracePeriod    time.Duration
	activeRequests int64
}

func NewDeploymentProxy(deployment *Deployment, head *Container) (*DeploymentProxy, error) {
	if deployment == nil {
		return nil, fmt.Errorf("Deployment is nil")
	}

	containerJSON, err := Flux.dockerClient.ContainerInspect(context.Background(), string(head.ContainerID[:]))
	if err != nil {
		return nil, err
	}

	if containerJSON.NetworkSettings.IPAddress == "" {
		return nil, fmt.Errorf("No IP address found for container %s", head.ContainerID[:12])
	}

	containerUrl, err := url.Parse(fmt.Sprintf("http://%s:%d", containerJSON.NetworkSettings.IPAddress, deployment.Port))
	if err != nil {
		return nil, err
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL = containerUrl
			req.Host = containerUrl.Host
		},
		Transport: &http.Transport{
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 100,
		},
		ModifyResponse: func(resp *http.Response) error {
			atomic.AddInt64(&deployment.Proxy.activeRequests, -1)
			return nil
		},
	}

	return &DeploymentProxy{
		deployment:     deployment,
		currentHead:    head,
		proxy:          proxy,
		gracePeriod:    time.Second * 30,
		activeRequests: 0,
	}, nil
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
			log.Printf("Failed to remove container (%s): %v\n", container.ContainerID[:12], err)
		}
	}
}
