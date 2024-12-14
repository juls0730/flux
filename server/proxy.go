package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

type Proxy struct {
	deployments sync.Map
}

func (p *Proxy) RemoveDeployment(deployment *Deployment) {
	p.deployments.Delete(deployment.URL)
}

func (p *Proxy) AddDeployment(deployment *Deployment) {
	logger.Debugw("Adding deployment", zap.String("url", deployment.URL))
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
	proxy          *httputil.ReverseProxy
	gracePeriod    time.Duration
	activeRequests int64
}

func (deployment *Deployment) NewDeploymentProxy() (*DeploymentProxy, error) {
	if deployment == nil {
		return nil, fmt.Errorf("deployment is nil")
	}

	containerJSON, err := Flux.dockerClient.ContainerInspect(context.Background(), string(deployment.Head.ContainerID[:]))
	if err != nil {
		return nil, err
	}

	if containerJSON.NetworkSettings.IPAddress == "" {
		return nil, fmt.Errorf("no IP address found for container %s", deployment.Head.ContainerID[:12])
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
		proxy:          proxy,
		gracePeriod:    time.Second * 30,
		activeRequests: 0,
	}, nil
}

func (dp *DeploymentProxy) GracefulShutdown(oldContainers []*Container) {
	ctx, cancel := context.WithTimeout(context.Background(), dp.gracePeriod)
	defer cancel()

	done := false
	for !done {
		select {
		case <-ctx.Done():
			done = true
		default:
			if atomic.LoadInt64(&dp.activeRequests) == 0 {
				done = true
			}

			time.Sleep(time.Second)
		}
	}

	for _, container := range oldContainers {
		err := RemoveDockerContainer(context.Background(), string(container.ContainerID[:]))
		if err != nil {
			logger.Errorw("Failed to remove container", zap.Error(err))
		}
	}
}
