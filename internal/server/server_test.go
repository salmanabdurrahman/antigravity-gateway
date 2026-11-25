// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// TestServerWritesAndClearsPIDFile verifies that the daemon records its own PID once the listener is
// bound and removes the file on graceful shutdown, so `agw stop` also works for foreground daemons.
func TestServerWritesAndClearsPIDFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	pidFile := filepath.Join(tempDir, "agw.pid")
	srv, ln := newTestServerWithPIDFile(t, pidFile)

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- srv.Start()
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	waitForHealthy(t, ctx, ln.Addr().String())

	pid, err := ReadPID(pidFile)
	if err != nil {
		t.Fatalf("reading pid file written by daemon: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("expected pid file to contain %d, got %d", os.Getpid(), pid)
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

	if _, err := ReadPID(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected pid file to be removed on shutdown, stat error: %v", err)
	}
}

// TestServerBindErrorReportsOwningProcess verifies that a bind failure caused by an occupied port
// names the owning process and the releasing command, while still wrapping the original error.
func TestServerBindErrorReportsOwningProcess(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving listener: %v", err)
	}
	defer occupied.Close()

	cfg := &config.Config{Host: "127.0.0.1", Port: occupied.Addr().(*net.TCPAddr).Port, Strategy: "sticky"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newTestServerWithConfig(t, cfg, logger)

	startErr := srv.Start()
	if startErr == nil {
		t.Fatalf("expected bind failure on occupied port %d", cfg.Port)
	}
	// The wrapped errno differs by platform: POSIX reports EADDRINUSE while Windows reports
	// WSAEADDRINUSE (10048), which syscall.EADDRINUSE never matches there. Assert the platform-neutral
	// predicate the daemon itself uses.
	if !isAddrInUse(startErr) {
		t.Errorf("expected an address-in-use bind failure, got %v", startErr)
	}

	message := startErr.Error()
	if !strings.Contains(message, "already in use") {
		t.Errorf("expected human readable bind failure, got %q", message)
	}
	if !strings.Contains(message, "'agw stop'") {
		t.Errorf("expected 'agw stop' recovery hint, got %q", message)
	}
	if !strings.Contains(message, fmt.Sprintf("pid %d", os.Getpid())) {
		t.Errorf("expected owning pid %d in message, got %q", os.Getpid(), message)
	}
}

// newTestServerWithConfig builds a Server around an in-memory account store and the given config.
func newTestServerWithConfig(t *testing.T, cfg *config.Config, logger *slog.Logger) *Server {
	t.Helper()

	store := auth.NewStore(filepath.Join(t.TempDir(), "accounts.json"))
	pool, err := rotator.NewPool(store, cfg, time.Now)
	if err != nil {
		t.Fatalf("failed creating pool: %v", err)
	}
	rot := rotator.NewRotator(pool, cfg, logger, time.Now)
	cat := catalog.NewModelCatalog(cfg, rot, filepath.Join(t.TempDir(), "models-cache.json"), logger)

	srv, err := NewServer(Options{Config: cfg, Rotator: rot, Catalog: cat, Logger: logger})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	return srv
}

// newTestServerWithPIDFile builds a listening Server that manages the given PID file path.
func newTestServerWithPIDFile(t *testing.T, pidFile string) (*Server, net.Listener) {
	t.Helper()

	cfg := &config.Config{Host: "127.0.0.1", Strategy: "sticky"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed creating listener: %v", err)
	}

	store := auth.NewStore(filepath.Join(t.TempDir(), "accounts.json"))
	pool, err := rotator.NewPool(store, cfg, time.Now)
	if err != nil {
		t.Fatalf("failed creating pool: %v", err)
	}
	rot := rotator.NewRotator(pool, cfg, logger, time.Now)
	cat := catalog.NewModelCatalog(cfg, rot, filepath.Join(t.TempDir(), "models-cache.json"), logger)

	srv, err := NewServer(Options{
		Config:   cfg,
		Rotator:  rot,
		Catalog:  cat,
		Logger:   logger,
		Listener: ln,
		PIDFile:  pidFile,
	})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}
	return srv, ln
}

// waitForHealthy polls /health until the daemon answers or the context deadline expires.
func waitForHealthy(t *testing.T, ctx context.Context, addr string) {
	t.Helper()

	healthURL := "http://" + addr + "/health"
	for {
		if ctx.Err() != nil {
			t.Fatalf("timed out waiting for server to answer /health: %v", ctx.Err())
		}
		if _, err := CheckHealth(ctx, healthURL); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestServerConfigSnapshotCopyOnWrite verifies that runtime configuration updates replace an
// immutable snapshot: concurrent readers always observe a fully populated struct and the caller's
// configuration passed to NewServer is never mutated.
func TestServerConfigSnapshotCopyOnWrite(t *testing.T) {
	tempDir := t.TempDir()

	callerCfg := &config.Config{
		Host:         "127.0.0.1",
		Port:         51400,
		Strategy:     "sticky",
		ModelMapping: map[string]string{"alias": "backend"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	store := auth.NewStore(filepath.Join(tempDir, "accounts.json"))
	pool, err := rotator.NewPool(store, callerCfg, time.Now)
	if err != nil {
		t.Fatalf("failed creating pool: %v", err)
	}
	rot := rotator.NewRotator(pool, callerCfg, logger, time.Now)
	cat := catalog.NewModelCatalog(callerCfg, rot, filepath.Join(tempDir, "models-cache.json"), logger)

	srv, err := NewServer(Options{Config: callerCfg, Rotator: rot, Catalog: cat, Logger: logger})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(2)
		go func(strategy string) {
			defer wg.Done()
			srv.setConfig(func(c *config.Config) { c.Strategy = strategy })
		}([]string{"sticky", "round-robin", "hybrid"}[i%3])
		go func() {
			defer wg.Done()
			for range 50 {
				snapshot := srv.config()
				if snapshot == nil {
					t.Error("config snapshot must never be nil")
					return
				}
				if snapshot.Host == "" || snapshot.Port == 0 {
					t.Errorf("observed partially populated config snapshot: %+v", snapshot)
					return
				}
				_ = snapshot.ModelMapping["alias"]
			}
		}()
	}
	wg.Wait()

	if callerCfg.Strategy != "sticky" {
		t.Errorf("caller configuration must not be mutated, got strategy %q", callerCfg.Strategy)
	}
	if got := srv.config().ModelMapping["alias"]; got != "backend" {
		t.Errorf("expected model mapping to survive updates, got %q", got)
	}
}

// TestServerIPv6LoopbackBinding verifies a "::1" configuration produces a bracketed listen address
// and that the daemon can actually bind and answer /health over IPv6 loopback.
func TestServerIPv6LoopbackBinding(t *testing.T) {
	probe, err := net.Listen("tcp", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable on this host: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	tempDir := t.TempDir()
	cfg := &config.Config{
		Host:     "::1",
		Port:     port,
		APIKey:   "test-key",
		Strategy: "sticky",
	}

	store := auth.NewStore(filepath.Join(tempDir, "accounts.json"))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := rotator.NewPool(store, cfg, time.Now)
	if err != nil {
		t.Fatalf("failed creating pool: %v", err)
	}
	rot := rotator.NewRotator(pool, cfg, logger, time.Now)
	cat := catalog.NewModelCatalog(cfg, rot, filepath.Join(tempDir, "models-cache.json"), logger)

	srv, err := NewServer(Options{Config: cfg, Rotator: rot, Catalog: cat, Logger: logger})
	if err != nil {
		t.Fatalf("NewServer failed: %v", err)
	}

	wantAddr := fmt.Sprintf("[::1]:%d", port)
	if got := srv.Addr(); got != wantAddr {
		t.Fatalf("expected bracketed IPv6 listen address %q, got %q", wantAddr, got)
	}

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- srv.Start()
	}()

	healthURL := config.URL("::1", port, "/health")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	var health *HealthResponse
	for {
		if ctx.Err() != nil {
			t.Fatalf("timed out waiting for IPv6 /health probe: %v", ctx.Err())
		}
		probed, probeErr := CheckHealth(ctx, healthURL)
		if probeErr == nil {
			health = probed
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !health.OK || health.Name != ServerName {
		t.Errorf("unexpected IPv6 health response: %+v", health)
	}

	// Bracketed IPv6 loopback Host authorities must be accepted on /health as well.
	handler := srv.buildHandler()
	healthReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	healthReq.Host = wantAddr
	healthRec := httptest.NewRecorder()
	handler.ServeHTTP(healthRec, healthReq)
	if healthRec.Code != http.StatusOK {
		t.Errorf("expected /health to accept IPv6 loopback host, got %d: %s", healthRec.Code, healthRec.Body.String())
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("srv.Shutdown failed: %v", err)
	}

	select {
	case err := <-serverErrCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Fatalf("unexpected server exit error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server to exit after shutdown")
	}
}
