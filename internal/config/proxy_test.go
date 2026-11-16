// Package config defines gateway configuration data structures, filesystem paths,
// environment overrides, and upstream Google HTTP headers.
package config

import (
	"net/http"
	"net/url"
	"testing"
)

// TestProxyFromEnvironmentHonored verifies that standard HTTP clients created throughout the gateway
// honor the HTTP_PROXY and HTTPS_PROXY environment variables by default.
func TestProxyFromEnvironmentHonored(t *testing.T) {
	fakeHTTPProxy := "http://127.0.0.1:8888"
	fakeHTTPSProxy := "http://127.0.0.1:8443"

	t.Setenv("HTTP_PROXY", fakeHTTPProxy)
	t.Setenv("http_proxy", fakeHTTPProxy)
	t.Setenv("HTTPS_PROXY", fakeHTTPSProxy)
	t.Setenv("https_proxy", fakeHTTPSProxy)

	req, err := http.NewRequest(http.MethodGet, "http://example.com/api", nil)
	if err != nil {
		t.Fatalf("creating test request: %v", err)
	}

	proxyURL, err := http.ProxyFromEnvironment(req)
	if err != nil {
		t.Fatalf("http.ProxyFromEnvironment failed: %v", err)
	}
	if proxyURL == nil {
		t.Fatal("expected proxy URL from environment, got nil")
	}

	expectedHTTP, _ := url.Parse(fakeHTTPProxy)
	if proxyURL.String() != expectedHTTP.String() {
		t.Errorf("expected proxy %s, got %s", expectedHTTP, proxyURL)
	}

	httpsReq, err := http.NewRequest(http.MethodPost, "https://cloudcode-pa.googleapis.com/v1internal", nil)
	if err != nil {
		t.Fatalf("creating test https request: %v", err)
	}

	httpsProxyURL, err := http.ProxyFromEnvironment(httpsReq)
	if err != nil {
		t.Fatalf("http.ProxyFromEnvironment failed for https: %v", err)
	}
	if httpsProxyURL == nil {
		t.Fatal("expected https proxy URL from environment, got nil")
	}

	expectedHTTPS, _ := url.Parse(fakeHTTPSProxy)
	if httpsProxyURL.String() != expectedHTTPS.String() {
		t.Errorf("expected https proxy %s, got %s", expectedHTTPS, httpsProxyURL)
	}
}
