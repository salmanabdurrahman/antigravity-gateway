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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/server"
)

// TestGatewaySafeProfileDoesNotRotateToSibling verifies the default safe profile end to end: when the
// primary account returns a quota rejection, the client receives that HTTP 429 instead of a transparent
// failover to the second stored account, and the sibling never sees upstream traffic.
func TestGatewaySafeProfileDoesNotRotateToSibling(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	var primaryCalls atomic.Int64
	var secondaryCalls atomic.Int64

	mockGoogle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "token-for-" + r.FormValue("refresh_token"),
				"token_type":   "Bearer",
				"expires_in":   3600,
			})
			return
		}

		var body struct {
			Project string `json:"project"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		// Catalog refreshes and other metadata calls must not be counted as generation dispatches.
		if !strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"models": map[string]any{}})
			return
		}

		switch body.Project {
		case "proj-primary":
			primaryCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{
				"error": {
					"code": 429,
					"message": "Individual quota reached. Resets in 1h.",
					"status": "RESOURCE_EXHAUSTED",
					"details": [{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "3600s"}]
				}
			}`))
		case "proj-secondary":
			secondaryCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"secondary served\"}]},\"finishReason\":\"STOP\"}]}}\n\n"))
		default:
			http.Error(w, "unexpected project: "+body.Project, http.StatusBadRequest)
		}
	}))
	defer mockGoogle.Close()

	tempDir := t.TempDir()
	// No profile is set, so the gateway resolves the safe default: single-account dispatch.
	cfg := &config.Config{
		Host:      "127.0.0.1",
		Port:      0,
		APIKey:    "agw_test_safe_profile_key",
		Strategy:  "sticky",
		TokenURL:  mockGoogle.URL + "/token",
		Endpoints: []string{mockGoogle.URL},
	}
	if got := cfg.EffectiveRotationMode(); got != config.RotationModeSingle {
		t.Fatalf("resolved rotation mode = %q; want %q", got, config.RotationModeSingle)
	}

	store := auth.NewStore(filepath.Join(tempDir, "accounts.json"))
	for _, account := range []struct {
		// email identifies the stored credential.
		email string
		// refreshToken seeds the OAuth refresh token.
		refreshToken string
		// project is the Cloud Code companion project the upstream mock keys off.
		project string
	}{
		{email: "primary@example.com", refreshToken: "ref-primary", project: "proj-primary"},
		{email: "secondary@example.com", refreshToken: "ref-secondary", project: "proj-secondary"},
	} {
		if _, err := store.Upsert(&auth.Account{
			Email:        account.email,
			RefreshToken: account.refreshToken,
			ProjectID:    account.project,
			AddedAt:      "2026-01-01T00:00:00Z",
		}); err != nil {
			t.Fatalf("seeding account %s: %v", account.email, err)
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := rotator.NewPool(store, cfg, time.Now)
	if err != nil {
		t.Fatalf("failed creating account pool: %v", err)
	}

	rot := rotator.NewRotator(pool, cfg, logger, time.Now)
	cat := catalog.NewModelCatalog(cfg, rot, filepath.Join(tempDir, "models-cache.json"), logger)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed creating tcp listener: %v", err)
	}

	gatewayServer, err := server.NewServer(server.Options{
		Config:   cfg,
		Rotator:  rot,
		Catalog:  cat,
		Logger:   logger,
		Listener: listener,
	})
	if err != nil {
		t.Fatalf("failed creating gateway server: %v", err)
	}

	go func() { _ = gatewayServer.Start() }()
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer shutdownCancel()
		_ = gatewayServer.Shutdown(shutdownCtx)
	})

	gatewayURL := "http://" + listener.Addr().String()
	waitForHealth(t, ctx, gatewayURL)

	payload, err := json.Marshal(map[string]any{
		"model":    "gemini-3.1-pro-high",
		"messages": []map[string]string{{"role": "user", "content": "safe profile check"}},
		"stream":   false,
	})
	if err != nil {
		t.Fatalf("marshaling chat request: %v", err)
	}

	post := func() (int, string) {
		t.Helper()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/chat/completions", bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("building chat request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("dispatching chat request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("reading chat response: %v", err)
		}
		return resp.StatusCode, string(raw)
	}

	status, body := post()
	if status != http.StatusTooManyRequests {
		t.Fatalf("first request status = %d; want 429 (no failover under the safe profile), body: %s", status, body)
	}
	if primaryCalls.Load() != 1 {
		t.Errorf("primary upstream calls = %d; want 1", primaryCalls.Load())
	}
	if secondaryCalls.Load() != 0 {
		t.Errorf("secondary upstream calls = %d; want 0 (safe profile must not rotate)", secondaryCalls.Load())
	}

	// The primary is cooling with no cooldown absorption, so the follow-up is refused locally.
	status, body = post()
	if status != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d; want 429, body: %s", status, body)
	}
	if primaryCalls.Load() != 1 || secondaryCalls.Load() != 0 {
		t.Errorf("upstream calls after the second request = (%d primary, %d secondary); want (1, 0)",
			primaryCalls.Load(), secondaryCalls.Load())
	}
}
