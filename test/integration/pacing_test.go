//go:build integration

// Package integration_test provides end-to-end integration tests for Antigravity Gateway.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/server"
)

// pacingGateway is a running gateway wired to a mock upstream, ready for HTTP-level assertions.
type pacingGateway struct {
	// url is the loopback base URL clients call.
	url string
	// apiKey authenticates client requests.
	apiKey string
}

// newPacingGateway builds a single-account gateway whose upstream delegates to handler, mutating the
// configuration through mutate before the rotator is constructed.
func newPacingGateway(t *testing.T, mutate func(cfg *config.Config), handler http.HandlerFunc) *pacingGateway {
	t.Helper()

	mockGoogle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-access-token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}
		if r.URL.Path != "" && !bytes.Contains([]byte(r.URL.Path), []byte("streamGenerateContent")) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"models": map[string]any{}})
			return
		}
		handler(w, r)
	}))
	t.Cleanup(mockGoogle.Close)

	tempDir := t.TempDir()
	cfg := &config.Config{
		Host:      "127.0.0.1",
		Port:      0,
		APIKey:    "agw_test_pacing_key",
		Strategy:  "sticky",
		TokenURL:  mockGoogle.URL + "/token",
		Endpoints: []string{mockGoogle.URL},
	}
	if mutate != nil {
		mutate(cfg)
	}

	store := auth.NewStore(filepath.Join(tempDir, "accounts.json"))
	if _, err := store.Upsert(&auth.Account{
		Email:        "paced@example.com",
		RefreshToken: "ref-paced",
		ProjectID:    "proj-paced",
		AddedAt:      "2026-01-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seeding account: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := rotator.NewPool(store, cfg, time.Now)
	if err != nil {
		t.Fatalf("creating account pool: %v", err)
	}

	rot := rotator.NewRotator(pool, cfg, logger, time.Now)
	cat := catalog.NewModelCatalog(cfg, rot, filepath.Join(tempDir, "models-cache.json"), logger)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("creating tcp listener: %v", err)
	}

	gatewayServer, err := server.NewServer(server.Options{
		Config:   cfg,
		Rotator:  rot,
		Catalog:  cat,
		Logger:   logger,
		Listener: listener,
	})
	if err != nil {
		t.Fatalf("creating gateway server: %v", err)
	}

	go func() { _ = gatewayServer.Start() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shutdownCancel()
		_ = gatewayServer.Shutdown(shutdownCtx)
	})

	url := "http://" + listener.Addr().String()
	waitForHealth(t, ctx, url)

	return &pacingGateway{url: url, apiKey: cfg.APIKey}
}

// postChat dispatches one non-streaming chat completion and returns the status code.
func (g *pacingGateway) postChat(ctx context.Context) (int, error) {
	payload, err := json.Marshal(map[string]any{
		"model":    "gemini-3.1-pro-high",
		"messages": []map[string]string{{"role": "user", "content": "pacing check"}},
		"stream":   false,
	})
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.url+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+g.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// TestGatewayPacingSerializesConcurrentRequests verifies end to end that one credential never runs more
// upstream streams than its in-flight ceiling, even when two clients request at the same time.
func TestGatewayPacingSerializesConcurrentRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	var (
		mu           sync.Mutex
		active       int
		maxActive    int
		gate         = make(chan struct{})
		firstEntered = make(chan struct{}, 1)
	)

	gateway := newPacingGateway(t, func(cfg *config.Config) {
		cfg.MaxInFlightPerAccount = 1
	}, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()

		select {
		case firstEntered <- struct{}{}:
		default:
		}

		<-gate

		mu.Lock()
		active--
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}}\n\n"))
	})

	results := make(chan int, 2)
	errs := make(chan error, 2)
	go func() {
		status, err := gateway.postChat(ctx)
		results <- status
		errs <- err
	}()

	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never reached upstream")
	}

	go func() {
		status, err := gateway.postChat(ctx)
		results <- status
		errs <- err
	}()

	// While the first request holds the only slot, the second must wait instead of opening a stream.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	observed := maxActive
	mu.Unlock()
	if observed != 1 {
		t.Errorf("max concurrent upstream streams = %d; want 1 with max_in_flight_per_account=1", observed)
	}

	close(gate)

	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("paced request failed: %v", err)
		}
		if status := <-results; status != http.StatusOK {
			t.Fatalf("paced request status = %d; want 200", status)
		}
	}
}

// TestGatewayDispatchBudgetStopsPoolCycling verifies end to end that a hard upstream failure with an
// explicit budget of one dispatch answers 502 after a single upstream attempt.
func TestGatewayDispatchBudgetStopsPoolCycling(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	var dispatches atomic.Int64

	gateway := newPacingGateway(t, func(cfg *config.Config) {
		cfg.MaxDispatchesPerRequest = 1
	}, func(w http.ResponseWriter, r *http.Request) {
		dispatches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"internal error","status":"INTERNAL"}}`))
	})

	status, err := gateway.postChat(ctx)
	if err != nil {
		t.Fatalf("dispatching chat request: %v", err)
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502 after the dispatch budget is exhausted", status)
	}
	if got := dispatches.Load(); got != 1 {
		t.Errorf("upstream dispatches = %d; want 1 with max_dispatches_per_request=1", got)
	}
}
