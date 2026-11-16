// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// TestServerLifecycle verifies server initialization, health probing, and clean graceful shutdown.
func TestServerLifecycle(t *testing.T) {
	tempDir := t.TempDir()

	cfg := &config.Config{
		Host:     "127.0.0.1",
		Port:     0, // Will use custom listener
		APIKey:   "test-key",
		Strategy: "sticky",
	}

	accFile := filepath.Join(tempDir, "accounts.json")
	store := auth.NewStore(accFile)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := rotator.NewPool(store, cfg, time.Now)
	if err != nil {
		t.Fatalf("failed creating pool: %v", err)
	}

	rot := rotator.NewRotator(pool, cfg, logger, time.Now)
	cacheFile := filepath.Join(tempDir, "models-cache.json")
	cat := catalog.NewModelCatalog(cfg, rot, cacheFile, logger)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed creating listener: %v", err)
	}

	srv, err := NewServer(Options{
		Config:   cfg,
		Rotator:  rot,
		Catalog:  cat,
		Logger:   logger,
		Listener: ln,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- srv.Start()
	}()

	// Query /health on active listener
	healthURL := "http://" + ln.Addr().String() + "/health"
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	var healthResp *HealthResponse
	for {
		if ctx.Err() != nil {
			t.Fatalf("timed out waiting for server to answer /health: %v", ctx.Err())
		}
		resp, err := CheckHealth(ctx, healthURL)
		if err == nil {
			healthResp = resp
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !healthResp.OK || healthResp.Name != ServerName {
		t.Errorf("unexpected health response: %+v", healthResp)
	}

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Second)
	defer shutdownCancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("srv.Shutdown failed: %v", err)
	}

	select {
	case err := <-serverErrCh:
		if err != nil && err != http.ErrServerClosed {
			t.Fatalf("unexpected server exit error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server to exit after shutdown")
	}
}

// TestServerLoopbackEnforcement verifies that non-loopback host addresses (0.0.0.0, public IPs)
// are strictly coerced back to 127.0.0.1 for security.
func TestServerLoopbackEnforcement(t *testing.T) {
	tempDir := t.TempDir()

	testHosts := []string{"0.0.0.0", "", "192.168.1.100", "example.com"}
	for _, h := range testHosts {
		cfg := &config.Config{
			Host:     h,
			Port:     51400,
			APIKey:   "key",
			Strategy: "sticky",
		}

		accFile := filepath.Join(tempDir, "accounts.json")
		store := auth.NewStore(accFile)
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		pool, _ := rotator.NewPool(store, cfg, time.Now)
		rot := rotator.NewRotator(pool, cfg, logger, time.Now)
		cat := catalog.NewModelCatalog(cfg, rot, "", logger)

		srv, err := NewServer(Options{
			Config:  cfg,
			Rotator: rot,
			Catalog: cat,
			Logger:  logger,
		})
		if err != nil {
			t.Fatalf("NewServer failed: %v", err)
		}

		if srv.Addr() != "127.0.0.1:51400" {
			t.Errorf("expected host %q to enforce loopback '127.0.0.1:51400', got %q", h, srv.Addr())
		}
	}
}

// TestServerStartWarnsOnMalformedAPIKey verifies that startup logs the active configuration file
// path and warns when the configured gateway key does not match the generated format, so a key
// mismatch with clients is visible in the daemon log instead of only as client-side HTTP 401.
func TestServerStartWarnsOnMalformedAPIKey(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	cfg := &config.Config{
		Host:     "127.0.0.1",
		APIKey:   "test-gateway-key-999",
		Strategy: "sticky",
	}

	accFile := filepath.Join(tempDir, "accounts.json")
	store := auth.NewStore(accFile)

	logBuffer := new(bytes.Buffer)
	logger := slog.New(slog.NewTextHandler(logBuffer, nil))

	pool, err := rotator.NewPool(store, cfg, time.Now)
	if err != nil {
		t.Fatalf("failed creating pool: %v", err)
	}
	rot := rotator.NewRotator(pool, cfg, logger, time.Now)
	cat := catalog.NewModelCatalog(cfg, rot, filepath.Join(tempDir, "models-cache.json"), logger)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed creating listener: %v", err)
	}

	srv, err := NewServer(Options{
		Config:   cfg,
		Rotator:  rot,
		Catalog:  cat,
		Logger:   logger,
		Listener: ln,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- srv.Start()
	}()

	healthURL := "http://" + ln.Addr().String() + "/health"
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		if ctx.Err() != nil {
			t.Fatalf("timed out waiting for server to answer /health: %v", ctx.Err())
		}
		if _, err := CheckHealth(ctx, healthURL); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("srv.Shutdown failed: %v", err)
	}

	select {
	case err := <-serverErrCh:
		if err != nil && err != http.ErrServerClosed {
			t.Fatalf("unexpected server exit error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server to exit after shutdown")
	}

	logs := logBuffer.String()
	if !strings.Contains(logs, "active configuration file") {
		t.Errorf("expected configuration path log entry, got: %s", logs)
	}
	if !strings.Contains(logs, "not in the generated format") {
		t.Errorf("expected malformed API key warning, got: %s", logs)
	}
	if strings.Contains(logs, "test-gateway-key-999") {
		t.Errorf("startup logs must never render the raw API key, got: %s", logs)
	}
}
