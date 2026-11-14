// Package catalog discovers upstream model availability, limits, and live quota tracking.
package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestFetchCatalogSuccess verifies successfully retrieving and parsing live model catalogs from upstream.
func TestFetchCatalogSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:fetchAvailableModels" {
			http.NotFound(w, r)
			return
		}

		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		resp := rawCatalogResponse{
			Models: map[string]rawModelInfo{
				"gemini-3-flash": {
					DisplayName: "Gemini 3 Flash",
				},
			},
			DefaultAgentModelID: "gemini-3-flash",
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	cfg := &config.Config{
		Endpoints: []string{ts.URL},
	}

	snap, err := FetchCatalog(t.Context(), cfg, ts.Client(), "test-access-token", "test-project")
	if err != nil {
		t.Fatalf("FetchCatalog failed: %v", err)
	}

	if len(snap.Models) != 1 || snap.Models[0].ID != "gemini-3-flash" {
		t.Fatalf("unexpected snapshot: %+v", snap)
	}
	if snap.DefaultModel != "gemini-3-flash" {
		t.Fatalf("unexpected default model: %q", snap.DefaultModel)
	}
}

// TestFetchCatalogEndpointFallback verifies falling back to alternative endpoints when the first endpoint returns 404.
func TestFetchCatalogEndpointFallback(t *testing.T) {
	firstCalled := false
	secondCalled := false

	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalled = true
		http.Error(w, "Not Found", http.StatusNotFound)
	}))
	defer ts1.Close()

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalled = true
		w.Header().Set("Content-Type", "application/json")
		resp := rawCatalogResponse{
			Models: map[string]rawModelInfo{
				"gemini-3.8-flash-high": {},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts2.Close()

	cfg := &config.Config{
		Endpoints: []string{ts1.URL, ts2.URL},
	}

	snap, err := FetchCatalog(t.Context(), cfg, ts1.Client(), "test-token", "test-project")
	if err != nil {
		t.Fatalf("FetchCatalog failed: %v", err)
	}

	if !firstCalled || !secondCalled {
		t.Fatalf("expected both servers to be queried, first=%v second=%v", firstCalled, secondCalled)
	}

	if len(snap.Models) != 1 || snap.Models[0].ID != "gemini-3.8-flash-high" {
		t.Fatalf("unexpected models: %+v", snap)
	}
}

// TestFetchCatalogAuthErrorDoesNotFallback verifies that 401 authentication rejections do not trigger endpoint fallback.
func TestFetchCatalogAuthErrorDoesNotFallback(t *testing.T) {
	firstCalled := false
	secondCalled := false

	ts1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalled = true
		http.Error(w, `{"error":{"message":"Invalid Credentials"}}`, http.StatusUnauthorized)
	}))
	defer ts1.Close()

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalled = true
		http.Error(w, "ok", http.StatusOK)
	}))
	defer ts2.Close()

	cfg := &config.Config{
		Endpoints: []string{ts1.URL, ts2.URL},
	}

	_, err := FetchCatalog(t.Context(), cfg, ts1.Client(), "bad-token", "test-project")
	if err == nil {
		t.Fatal("expected error on 401 unauthorized")
	}

	if !firstCalled {
		t.Fatal("expected first server to be called")
	}
	if secondCalled {
		t.Fatal("expected second server not to be called on terminal 401")
	}
}

// TestFetchCatalogContextCanceled verifies that context cancellation immediately halts catalog retrieval.
func TestFetchCatalogContextCanceled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	cfg := &config.Config{
		Endpoints: []string{ts.URL},
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // Cancel before call

	_, err := FetchCatalog(ctx, cfg, ts.Client(), "test-token", "test-project")
	if err == nil {
		t.Fatal("expected error on canceled context")
	}
}
