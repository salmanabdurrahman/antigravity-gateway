// Package rotator_test provides unit and concurrency test suites for the rotator package.
package rotator_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// wireCapture records the protocol version and header set of upstream requests for wire-shape assertions.
type wireCapture struct {
	// mu guards the captured slices across the token and generation handlers.
	mu sync.Mutex
	// protoMajors lists the HTTP major version of each captured request.
	protoMajors []int
	// headerKeys lists the sorted header names of the generation request only.
	headerKeys []string
}

// record appends the request protocol and, when generation is true, its sorted header names.
func (c *wireCapture) record(r *http.Request, generation bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.protoMajors = append(c.protoMajors, r.ProtoMajor)
	if !generation {
		return
	}
	keys := make([]string, 0, len(r.Header))
	for key := range r.Header {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	c.headerKeys = keys
}

// TestRotatorUpstreamWireShape verifies that generation traffic uses HTTP/1.1 and sends only the header
// set the native client sends: no client headers leak through and no ALPN-negotiated HTTP/2 is used.
func TestRotatorUpstreamWireShape(t *testing.T) {
	capture := &wireCapture{}

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			capture.record(r, false)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-access-token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}

		capture.record(r, true)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}}\n\n"))
	}))
	defer mockServer.Close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		Strategy:  string(rotator.StrategySticky),
		TokenURL:  mockServer.URL + "/token",
		Endpoints: []string{mockServer.URL},
	}
	pool, err := rotator.NewPool(nil, cfg, time.Now)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}
	pool.SetAccounts([]*auth.Account{newTestAccount("wire@example.com")})

	// No pinned HTTP client: the rotator must select the credential-scoped HTTP/1.1 wire transport.
	r := rotator.NewRotator(pool, cfg, nil, time.Now)

	res, err := r.Generate(t.Context(), "gemini-pro", func(s *rotator.AccountState) (any, error) {
		return map[string]any{"project": s.ProjectID()}, nil
	})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	_ = res.Response.Body.Close()

	capture.mu.Lock()
	defer capture.mu.Unlock()

	if len(capture.protoMajors) == 0 {
		t.Fatal("no upstream requests captured")
	}
	for i, major := range capture.protoMajors {
		if major != 1 {
			t.Errorf("upstream request %d used HTTP/%d; want HTTP/1.1", i, major)
		}
	}

	// Accept-Encoding and Content-Length are added by net/http itself; everything else is what the
	// gateway explicitly sets. A client header leaking through would appear here and fail the match.
	wantHeaders := []string{
		"Accept",
		"Accept-Encoding",
		"Authorization",
		"Client-Metadata",
		"Content-Length",
		"Content-Type",
		"User-Agent",
		"X-Goog-Api-Client",
	}
	if !slices.Equal(capture.headerKeys, wantHeaders) {
		t.Errorf("upstream generation headers = %v; want %v", capture.headerKeys, wantHeaders)
	}
}

// TestAccountCredentialKeyIsStableAndDistinct verifies the pool key exposed by account state stays stable
// for one credential and separates two identities, which is what keeps their connection pools apart.
func TestAccountCredentialKeyIsStableAndDistinct(t *testing.T) {
	first := rotator.NewAccountState(newTestAccount("first@example.com"))
	second := rotator.NewAccountState(newTestAccount("second@example.com"))

	key := first.CredentialKey()
	if key == "" {
		t.Fatal("CredentialKey returned an empty key for a credentialed account")
	}
	if key != first.CredentialKey() {
		t.Error("CredentialKey is not stable across calls")
	}
	if key == second.CredentialKey() {
		t.Error("CredentialKey collided across two accounts")
	}
	if strings.Contains(key, "first@example.com") || strings.Contains(key, "rt-first@example.com") {
		t.Errorf("CredentialKey leaked credential material: %q", key)
	}
	if got := rotator.NewAccountState(&auth.Account{}).CredentialKey(); got != "" {
		t.Errorf("CredentialKey for an empty account = %q; want empty", got)
	}
}

// TestRotatorIsolatesCredentialConnections verifies that two pooled accounts never share one TCP
// connection: each dispatch runs over a distinct remote address, which is what keeps two Google
// identities from appearing as a single long-lived client.
func TestRotatorIsolatesCredentialConnections(t *testing.T) {
	type dispatch struct {
		// remoteAddr is the client address the upstream saw.
		remoteAddr string
		// authorization is the bearer token, which identifies the serving account.
		authorization string
	}

	var (
		mu         sync.Mutex
		dispatches []dispatch
	)

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "token-for-" + r.FormValue("refresh_token"),
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}

		mu.Lock()
		dispatches = append(dispatches, dispatch{remoteAddr: r.RemoteAddr, authorization: r.Header.Get("Authorization")})
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}}\n\n"))
	}))
	defer mockServer.Close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		Strategy:  string(rotator.StrategyRoundRobin),
		TokenURL:  mockServer.URL + "/token",
		Endpoints: []string{mockServer.URL},
	}
	pool, err := rotator.NewPool(nil, cfg, time.Now)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}
	pool.SetAccounts([]*auth.Account{newTestAccount("pool-a@example.com"), newTestAccount("pool-b@example.com")})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)

	for range 2 {
		res, err := r.Generate(t.Context(), "gemini-pro", func(s *rotator.AccountState) (any, error) {
			return map[string]any{"project": s.ProjectID()}, nil
		})
		if err != nil {
			t.Fatalf("Generate failed: %v", err)
		}
		_ = res.Response.Body.Close()
	}

	mu.Lock()
	defer mu.Unlock()

	if len(dispatches) != 2 {
		t.Fatalf("captured %d dispatch(es); want 2", len(dispatches))
	}
	if dispatches[0].authorization == dispatches[1].authorization {
		t.Errorf("both dispatches used the same credential %q; want one per account", dispatches[0].authorization)
	}
	if dispatches[0].remoteAddr == dispatches[1].remoteAddr {
		t.Errorf("both credentials used the same upstream connection %q; want isolated pools", dispatches[0].remoteAddr)
	}
}
