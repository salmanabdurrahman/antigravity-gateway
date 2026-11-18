// Package rotator_test provides unit and concurrency test suites for the rotator package.
package rotator_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// TestRotatorEndpointFallbackOn5xx verifies automatic failover to fallback upstream endpoints
// when the primary endpoint returns HTTP 503 Service Unavailable.
func TestRotatorEndpointFallbackOn5xx(t *testing.T) {
	ctx := t.Context()
	var ep1Calls, ep2Calls atomic.Int64

	// Endpoint 1: Always returns 503 Service Unavailable
	ep1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ep1Calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "backend unavailable"}})
	}))
	defer ep1.Close()

	// Endpoint 2: Healthy, returns 200 SSE stream
	ep2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ep2Calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"status\":\"ok\"}\n\n"))
	}))
	defer ep2.Close()

	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 3600})
	}))
	defer tokenServer.Close()

	cfg := &config.Config{
		Strategy:  string(rotator.StrategySticky),
		TokenURL:  tokenServer.URL,
		Endpoints: []string{ep1.URL, ep2.URL},
	}

	a1 := newTestAccount("user@example.com")
	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{a1})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)

	builder := func(s *rotator.AccountState) (any, error) {
		return map[string]string{"project": s.ProjectID()}, nil
	}

	res, err := r.Generate(ctx, "gemini-pro", builder)
	if err != nil {
		t.Fatalf("expected failover to secondary endpoint, got error: %v", err)
	}
	defer res.Response.Body.Close()

	if ep1Calls.Load() != 1 {
		t.Errorf("expected 1 call to endpoint 1, got %d", ep1Calls.Load())
	}
	if ep2Calls.Load() != 1 {
		t.Errorf("expected 1 call to endpoint 2, got %d", ep2Calls.Load())
	}
}

// TestRotatorAccountFallbackOnPersistent5xx verifies rotating to alternative accounts
// when all endpoints persistently return HTTP 5xx errors for a single account.
func TestRotatorAccountFallbackOnPersistent5xx(t *testing.T) {
	ctx := t.Context()
	var calls []string

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 3600})
			return
		}

		var body struct {
			Project string `json:"project"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		calls = append(calls, body.Project)

		if body.Project == "proj-broken@example.com" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "internal error"}})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer mockServer.Close()

	cfg := &config.Config{
		Strategy:  string(rotator.StrategySticky),
		TokenURL:  mockServer.URL + "/token",
		Endpoints: []string{mockServer.URL},
	}

	a1 := newTestAccount("broken@example.com")
	a2 := newTestAccount("healthy@example.com")

	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{a1, a2})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.Client())

	builder := func(s *rotator.AccountState) (any, error) {
		return map[string]string{"project": s.ProjectID()}, nil
	}

	res, err := r.Generate(ctx, "gemini-pro", builder)
	if err != nil {
		t.Fatalf("expected fallback to healthy account, got: %v", err)
	}
	defer res.Response.Body.Close()

	if res.State.Email() != "healthy@example.com" {
		t.Errorf("expected response from healthy account, got %s", res.State.Email())
	}
}

// TestRotatorAccountFallbackOnTransportError verifies rotating to alternative accounts
// when an account encounters a network transport failure, verifying that skip[s] is marked.
func TestRotatorAccountFallbackOnTransportError(t *testing.T) {
	ctx := t.Context()
	var attempts atomic.Int64

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "expires_in": 3600})
			return
		}

		var body struct {
			Project string `json:"project"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		if body.Project == "proj-network-fail@example.com" {
			attempts.Add(1)
			// Close connection abruptly to cause client transport error
			hj, ok := w.(http.Hijacker)
			if ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
				return
			}
			w.WriteHeader(http.StatusBadGateway)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer mockServer.Close()

	cfg := &config.Config{
		Strategy:  string(rotator.StrategySticky),
		TokenURL:  mockServer.URL + "/token",
		Endpoints: []string{mockServer.URL},
	}

	a1 := newTestAccount("network-fail@example.com")
	a2 := newTestAccount("healthy-fallback@example.com")

	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{a1, a2})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.Client())
	r.SetSleepFn(func(ctx context.Context, d time.Duration) error { return nil })

	builder := func(s *rotator.AccountState) (any, error) {
		return map[string]string{"project": s.ProjectID()}, nil
	}

	res, err := r.Generate(ctx, "gemini-pro", builder)
	if err != nil {
		t.Fatalf("expected fallback to healthy account after transport failure, got: %v", err)
	}
	defer res.Response.Body.Close()

	if res.State.Email() != "healthy-fallback@example.com" {
		t.Errorf("expected response from healthy-fallback account, got %s", res.State.Email())
	}
}
