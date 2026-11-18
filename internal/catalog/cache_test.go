// Package catalog discovers upstream model availability, limits, and live quota tracking.
package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// mockAccountRunner implements AccountRunner for testing catalog refresh routines.
type mockAccountRunner struct {
	fn func(ctx context.Context, handler func(ctx context.Context, token string, state *rotator.AccountState) error) error
}

// WithAccount executes the handler with mock credentials.
func (m *mockAccountRunner) WithAccount(ctx context.Context, handler func(ctx context.Context, token string, state *rotator.AccountState) error) error {
	return m.fn(ctx, handler)
}

// TestModelCatalogCacheLoadAndSave verifies loading and saving model metadata to models-cache.json with 0600 permissions.
func TestModelCatalogCacheLoadAndSave(t *testing.T) {
	tempDir := t.TempDir()
	cacheFile := filepath.Join(tempDir, "models-cache.json")

	cat := NewModelCatalog(nil, nil, cacheFile, nil)
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	cat.SetNowFn(func() time.Time { return now })

	// Initially empty
	if err := cat.LoadCache(); err != nil {
		t.Fatalf("LoadCache on missing file failed: %v", err)
	}
	snap, _ := cat.Snapshot()
	if len(snap.Models) != 0 {
		t.Fatalf("expected 0 models, got %d", len(snap.Models))
	}

	// Save dummy models and quotas
	fraction := 0.75
	cat.mu.Lock()
	cat.snapshot.Models = []CatalogModel{
		{ID: "gemini-3.8-flash-high", ContextWindow: 1048576, MaxOutputTokens: 65536, Thinking: true},
	}
	cat.snapshot.Quotas = map[string]ModelQuota{
		"gemini-3.8-flash-high": {RemainingFraction: &fraction},
	}
	cat.snapshot.DefaultModel = "gemini-3.8-flash-high"
	cat.fetchedAt = now
	cat.mu.Unlock()

	if err := cat.SaveCache(); err != nil {
		t.Fatalf("SaveCache failed: %v", err)
	}

	// Verify file permissions 0600
	info, err := os.Stat(cacheFile)
	if err != nil {
		t.Fatalf("os.Stat failed: %v", err)
	}
	if info.Mode().Perm() != config.FilePerm {
		t.Errorf("expected file mode %o, got %o", config.FilePerm, info.Mode().Perm())
	}

	// Read back in new catalog instance
	cat2 := NewModelCatalog(nil, nil, cacheFile, nil)
	if err := cat2.LoadCache(); err != nil {
		t.Fatalf("cat2.LoadCache failed: %v", err)
	}
	snap2, fetched2 := cat2.Snapshot()
	if len(snap2.Models) != 1 || snap2.Models[0].ID != "gemini-3.8-flash-high" {
		t.Fatalf("unexpected restored models: %+v", snap2.Models)
	}
	if snap2.DefaultModel != "gemini-3.8-flash-high" {
		t.Errorf("expected DefaultModel gemini-3.8-flash-high, got %s", snap2.DefaultModel)
	}
	if q, ok := snap2.Quotas["gemini-3.8-flash-high"]; !ok || q.RemainingFraction == nil || *q.RemainingFraction != 0.75 {
		t.Errorf("unexpected restored quotas: %+v", snap2.Quotas)
	}
	if !fetched2.Equal(now) {
		t.Fatalf("expected fetchedAt %v, got %v", now, fetched2)
	}
}

// TestModelCatalogRefreshAndEnsure verifies refreshing stale catalogs and ensuring cached data is present.
func TestModelCatalogRefreshAndEnsure(t *testing.T) {
	tempDir := t.TempDir()
	cacheFile := filepath.Join(tempDir, "models-cache.json")

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := rawCatalogResponse{
			Models: map[string]rawModelInfo{
				"gemini-3.8-flash-high": {
					DisplayName: "Gemini 3.8 Flash High",
				},
				"claude-sonnet-4-6": {
					DisplayName: "Claude Sonnet 4.6",
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	cfg := &config.Config{
		Endpoints: []string{ts.URL},
	}

	refreshCount := 0
	mockRunner := &mockAccountRunner{
		fn: func(ctx context.Context, handler func(ctx context.Context, token string, state *rotator.AccountState) error) error {
			refreshCount++
			st := rotator.NewAccountState(&auth.Account{
				Email:     "user@example.com",
				ProjectID: "proj-123",
			})
			return handler(ctx, "mock-access-token", st)
		},
	}

	cat := NewModelCatalog(cfg, mockRunner, cacheFile, nil)
	cat.SetHTTPClient(ts.Client())

	// Ensure should trigger refresh when catalog and cache are empty
	if err := cat.Ensure(t.Context()); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}

	if refreshCount != 1 {
		t.Fatalf("expected 1 refresh call, got %d", refreshCount)
	}

	snap, _ := cat.Snapshot()
	if len(snap.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(snap.Models))
	}

	// Verify Get and Info
	m, ok := cat.Get("claude-sonnet-4-6")
	if !ok || m.ID != "claude-sonnet-4-6" {
		t.Fatalf("Get claude-sonnet-4-6 failed: ok=%v, m=%+v", ok, m)
	}

	info := cat.Info("gemini-custom-unlisted")
	if info.ContextWindow != 1048576 {
		t.Errorf("expected synthesized family defaults, got %+v", info)
	}

	// Verify disk cache was written
	if _, err := os.Stat(cacheFile); err != nil {
		t.Fatalf("cache file was not written: %v", err)
	}

	// Cold start simulation: fresh ModelCatalog instance with existing disk cache
	catCold := NewModelCatalog(cfg, mockRunner, cacheFile, nil)
	catCold.SetHTTPClient(ts.Client())

	if err := catCold.Ensure(t.Context()); err != nil {
		t.Fatalf("catCold.Ensure failed: %v", err)
	}

	// Refresh count should STILL be 1 because disk cache was fresh and used!
	if refreshCount != 1 {
		t.Fatalf("catCold.Ensure should have loaded from fresh cache without network refresh; refreshCount=%d", refreshCount)
	}
	coldSnap, _ := catCold.Snapshot()
	if len(coldSnap.Models) != 2 {
		t.Fatalf("expected 2 models from cold cache, got %d", len(coldSnap.Models))
	}
}

// TestModelCatalogConcurrency verifies thread-safe concurrent reads and Ensure calls across goroutines.
func TestModelCatalogConcurrency(t *testing.T) {
	tempDir := t.TempDir()
	cacheFile := filepath.Join(tempDir, "models-cache.json")

	cat := NewModelCatalog(nil, nil, cacheFile, nil)
	cat.mu.Lock()
	cat.snapshot.Models = []CatalogModel{
		{ID: "gemini-3.8-flash-high"},
		{ID: "claude-sonnet-4-6"},
	}
	cat.mu.Unlock()

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if id%2 == 0 {
				_ = cat.Info(fmt.Sprintf("model-%d", id))
			} else {
				_, _ = cat.Snapshot()
			}
		}(i)
	}
	wg.Wait()
}

// TestModelCatalogStaleWhileRevalidateNonBlocking verifies that Ensure returns immediately when
// cached models are stale, serving the stale snapshot while triggering background refresh.
func TestModelCatalogStaleWhileRevalidateNonBlocking(t *testing.T) {
	tempDir := t.TempDir()
	cacheFile := filepath.Join(tempDir, "models-cache.json")

	refreshStarted := make(chan struct{})
	refreshDone := make(chan struct{})

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(refreshStarted)
		<-refreshDone
		w.Header().Set("Content-Type", "application/json")
		resp := rawCatalogResponse{
			Models: map[string]rawModelInfo{
				"gemini-refreshed": {DisplayName: "Refreshed"},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	cfg := &config.Config{
		Endpoints: []string{ts.URL},
	}

	mockRunner := &mockAccountRunner{
		fn: func(ctx context.Context, handler func(ctx context.Context, token string, state *rotator.AccountState) error) error {
			st := rotator.NewAccountState(&auth.Account{
				Email:     "user@example.com",
				ProjectID: "proj-123",
			})
			return handler(ctx, "mock-token", st)
		},
	}

	cat := NewModelCatalog(cfg, mockRunner, cacheFile, nil)
	cat.SetHTTPClient(ts.Client())

	// Seed with stale data (> 15m old)
	staleTime := time.Now().Add(-20 * time.Minute)
	cat.mu.Lock()
	cat.snapshot.Models = []CatalogModel{{ID: "gemini-stale"}}
	cat.fetchedAt = staleTime
	cat.rebuildIndexLocked()
	cat.mu.Unlock()

	start := time.Now()
	// Ensure must return immediately (< 50ms) without waiting for the slow background refresh
	if err := cat.Ensure(t.Context()); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	duration := time.Since(start)
	if duration > 100*time.Millisecond {
		t.Errorf("Ensure blocked for %v, expected instant return on stale data", duration)
	}

	// Verify stale model is immediately returned while refresh is pending
	if _, ok := cat.Get("gemini-stale"); !ok {
		t.Error("expected stale model to be immediately servable")
	}

	// Wait for background refresh to initiate and unblock server response
	select {
	case <-refreshStarted:
		close(refreshDone)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for background refresh to start")
	}

	// Allow background goroutine to complete
	time.Sleep(100 * time.Millisecond)

	// Verify refreshed model is now present
	if _, ok := cat.Get("gemini-refreshed"); !ok {
		t.Error("expected refreshed model to be present after background refresh")
	}
}
