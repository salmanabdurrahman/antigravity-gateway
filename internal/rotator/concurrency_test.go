// Package rotator_test provides unit and concurrency test suites for the rotator package.
package rotator_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// TestRotatorConcurrency verifies that high-concurrency requests across multiple goroutines
// safely share the rotator pool and distribute requests without data races.
func TestRotatorConcurrency(t *testing.T) {
	ctx := t.Context()
	var requestCount atomic.Int64

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}
		requestCount.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer mockServer.Close()

	cfg := &config.Config{
		Strategy:  string(rotator.StrategyRoundRobin),
		TokenURL:  mockServer.URL + "/token",
		Endpoints: []string{mockServer.URL},
	}

	a1 := newTestAccount("u1@example.com")
	a2 := newTestAccount("u2@example.com")
	a3 := newTestAccount("u3@example.com")

	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{a1, a2, a3})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.Client())

	builder := func(s *rotator.AccountState) (any, error) {
		return map[string]string{"project": s.ProjectID()}, nil
	}

	const goroutines = 30
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			model := "gemini-pro"
			if idx%2 == 0 {
				model = "claude-sonnet"
			}
			res, err := r.Generate(ctx, model, builder)
			if err != nil {
				t.Errorf("goroutine %d failed: %v", idx, err)
				return
			}
			_ = res.Response.Body.Close()
		}(i)
	}

	wg.Wait()

	if requestCount.Load() != goroutines {
		t.Errorf("expected %d requests, got %d", goroutines, requestCount.Load())
	}
}

// TestRotatorConcurrent429AndReload verifies thread safety when handling concurrent 429 rate limit errors
// alongside dynamic account pool reloads from disk.
func TestRotatorConcurrent429AndReload(t *testing.T) {
	ctx := t.Context()
	var calls atomic.Int64

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}
		num := calls.Add(1)
		// Alternate between 429 and 200
		if num%3 == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"message": "Resource exhausted",
					"details": []any{
						map[string]any{
							"@type":      "type.googleapis.com/google.rpc.RetryInfo",
							"retryDelay": "1s",
						},
					},
				},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer mockServer.Close()

	cfg := &config.Config{
		Strategy:  string(rotator.StrategyRoundRobin),
		TokenURL:  mockServer.URL + "/token",
		Endpoints: []string{mockServer.URL},
	}

	a1 := newTestAccount("acct1@example.com")
	a2 := newTestAccount("acct2@example.com")
	a3 := newTestAccount("acct3@example.com")

	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{a1, a2, a3})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.Client())

	builder := func(s *rotator.AccountState) (any, error) {
		return map[string]string{"project": s.ProjectID()}, nil
	}

	var wg sync.WaitGroup
	// 20 workers concurrently calling Generate
	for i := range 20 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for range 5 {
				res, err := r.Generate(ctx, "gemini-pro", builder)
				if err == nil {
					_ = res.Response.Body.Close()
				}
			}
		}(i)
	}

	// Concurrently invoke SetAccounts and Snapshots to test race conditions
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_ = r.Pool().Snapshots()
				r.Pool().SetAccounts([]*auth.Account{a1, a2, a3})
				time.Sleep(2 * time.Millisecond)
			}
		}()
	}

	wg.Wait()
}
