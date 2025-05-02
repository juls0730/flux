package proxyManagerService

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/juls0730/flux/internal/util"
	"go.uber.org/zap"
)

type DeploymentId int64

// this is the object that oversees the proxying of requests to the correct deployment
type ProxyManager struct {
	util.TypedMap[string, *Proxy]
	logger *zap.SugaredLogger
}

func NewProxyManager(logger *zap.SugaredLogger) *ProxyManager {
	return &ProxyManager{
		logger: logger,
	}
}

func (proxyManager *ProxyManager) ListenAndServe(host string) error {
	if host == "" {
		host = "0.0.0.0:7465"
	}

	proxyManager.logger.Infof("Proxy server starting on http://%s", host)
	if err := http.ListenAndServe(host, proxyManager); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("failed to start proxy server: %v", err)
	}
	return nil
}

// Stops forwarding traffic to a deployment
func (proxyManager *ProxyManager) RemoveDeployment(host string) {
	proxyManager.Delete(host)
}

// Starts forwarding traffic to a deployment. The deployment must be ready to recieve requests before this is called.
func (proxyManager *ProxyManager) AddProxy(host string, proxy *Proxy) {
	proxyManager.logger.Debugw("Adding proxy", zap.String("host", host))
	proxyManager.Store(host, proxy)
}

// This function is responsible for taking an http request and forwarding it to the correct deployment
func (proxyManager *ProxyManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host

	proxyManager.logger.Debugw("Proxying request", zap.String("host", host))
	proxy, ok := proxyManager.Load(host)
	if !ok {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}

	proxy.proxyFunc.ServeHTTP(w, r)
}

type Proxy struct {
	forwardingFor  url.URL
	proxyFunc      *httputil.ReverseProxy
	gracePeriod    time.Duration
	activeRequests int64
}

// type DeploymentProxy struct {
// 	deployment     *models.Deployment
// 	proxy          *httputil.ReverseProxy
// 	gracePeriod    time.Duration
// 	activeRequests int64
// }

// Creates a proxy for a given deployment
func NewDeploymentProxy(forwardingFor url.URL) (*Proxy, error) {
	proxy := &Proxy{
		forwardingFor:  forwardingFor,
		gracePeriod:    time.Second * 30,
		activeRequests: 0,
	}

	proxy.proxyFunc = &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL = &url.URL{
				Scheme: forwardingFor.Scheme,
				Host:   forwardingFor.Host,
				Path:   req.URL.Path,
			}
			req.Host = forwardingFor.Host
			atomic.AddInt64(&proxy.activeRequests, 1)
		},
		Transport: &http.Transport{
			MaxIdleConns:        100,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 100,
		},
		ModifyResponse: func(resp *http.Response) error {
			atomic.AddInt64(&proxy.activeRequests, -1)
			return nil
		},
	}

	return proxy, nil
}

// Drains connections from a proxy
func (p *Proxy) GracefulShutdown(shutdownFunc func()) {
	ctx, cancel := context.WithTimeout(context.Background(), p.gracePeriod)
	defer cancel()

	done := false
	for !done {
		select {
		case <-ctx.Done():
			done = true
		default:
			if atomic.LoadInt64(&p.activeRequests) == 0 {
				done = true
			}

			time.Sleep(time.Second)
		}
	}

	shutdownFunc()
}
