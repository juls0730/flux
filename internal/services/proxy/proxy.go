package proxyManagerService

import (
	"net/http"
	"net/url"
	"time"
)

func GetTransport(target string) *http.Transport {
	return &http.Transport{
		Proxy: func(r *http.Request) (*url.URL, error) {
			return url.Parse(target)
		},
		MaxIdleConns:        100,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 100,
	}
}
