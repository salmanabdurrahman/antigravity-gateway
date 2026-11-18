// Package config defines gateway configuration data structures, filesystem paths,
// environment overrides, and upstream Google HTTP headers.
package config

import (
	"net"
	"net/http"
	"time"
)

// NewTransport constructs an optimized *http.Transport configured for multiplexed,
// low-latency communication with Google Cloud Code Assist and Google OAuth endpoints.
// It sets high per-host connection pools to avoid connection churn, enables HTTP/2,
// bounds response header latency, and honors standard proxy environment variables.
func NewTransport() *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// DefaultHTTPClient returns an *http.Client configured with NewTransport and the provided timeout.
func DefaultHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: NewTransport(),
		Timeout:   timeout,
	}
}
