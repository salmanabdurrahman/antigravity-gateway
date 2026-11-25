// Package config defines gateway configuration data structures, filesystem paths,
// environment overrides, and upstream Google HTTP headers.
package config

import (
	"cmp"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"testing"
	"time"
)

// TestIPFamilyRankOrdersIPv4First verifies the ordering helper used by the opt-in IPv4-first dialer.
func TestIPFamilyRankOrdersIPv4First(t *testing.T) {
	addresses := []net.IP{
		net.ParseIP("2001:db8::1"),
		net.ParseIP("192.0.2.1"),
		net.ParseIP("2001:db8::2"),
		net.ParseIP("198.51.100.7"),
	}
	ordered := append([]net.IP(nil), addresses...)
	slices.SortStableFunc(ordered, func(a, b net.IP) int {
		return cmp.Compare(ipFamilyRank(a), ipFamilyRank(b))
	})

	if rank := ipFamilyRank(ordered[0]); rank != 0 {
		t.Errorf("expected IPv4 address first, got %s", ordered[0])
	}
	if rank := ipFamilyRank(ordered[len(ordered)-1]); rank != 1 {
		t.Errorf("expected IPv6 address last, got %s", ordered[len(ordered)-1])
	}
}

// TestNewTransportPreferIPv4DialsHostname verifies that enabling AGW_PREFER_IPV4 still connects to a
// hostname that publishes an IPv6 address while only an IPv4 listener is reachable.
func TestNewTransportPreferIPv4DialsHostname(t *testing.T) {
	t.Setenv(PreferIPv4Env, "1")

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("binding IPv4 listener: %v", err)
	}
	defer listener.Close()

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}),
		ReadHeaderTimeout: 2 * time.Second,
	}
	go func() {
		_ = server.Serve(listener)
	}()
	defer func() {
		_ = server.Close()
	}()

	transport := NewTransport()
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	port := listener.Addr().(*net.TCPAddr).Port
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/", port))
	if err != nil {
		t.Fatalf("IPv4-first dial failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("unexpected response: %d %q", resp.StatusCode, body)
	}
}

// TestPreferIPv4EnvParsing verifies the opt-in flag accepts only explicit truthy values.
func TestPreferIPv4EnvParsing(t *testing.T) {
	cases := map[string]bool{
		"":       false,
		"0":      false,
		"false":  false,
		"1":      true,
		"true":   true,
		"TRUE":   true,
		" yes ":  true,
		"on":     true,
		"random": false,
	}
	for value, want := range cases {
		t.Run(value, func(t *testing.T) {
			t.Setenv(PreferIPv4Env, value)
			if got := preferIPv4(); got != want {
				t.Errorf("preferIPv4() with %q = %v; want %v", value, got, want)
			}
		})
	}
}

// TestDefaultHTTPClientReusesSharedTransport verifies that consecutive DefaultHTTPClient calls
// share the same underlying *http.Transport instead of allocating separate connection pools.
func TestDefaultHTTPClientReusesSharedTransport(t *testing.T) {
	t.Setenv(PreferIPv4Env, "0")
	c1 := DefaultHTTPClient(5 * time.Second)
	c2 := DefaultHTTPClient(15 * time.Second)
	if c1.Transport != c2.Transport {
		t.Fatalf("expected DefaultHTTPClient calls to share Transport, got %p vs %p", c1.Transport, c2.Transport)
	}

	t.Setenv(PreferIPv4Env, "1")
	c3 := DefaultHTTPClient(10 * time.Second)
	if c3.Transport == c1.Transport {
		t.Fatalf("expected AGW_PREFER_IPV4=1 to use the IPv4-first shared transport")
	}
}
