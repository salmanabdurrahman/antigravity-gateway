// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/server"
)

// TestStartCommandValidation verifies input validation on strategy and port flags during start.
func TestStartCommandValidation(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	startStrategy = ""
	startDetach = false
	startPort = 0
	defer func() {
		startStrategy = ""
		startDetach = false
		startPort = 0
	}()

	outBuf := new(bytes.Buffer)
	errBuf := new(bytes.Buffer)

	rootCmd.SetOut(outBuf)
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"start", "--strategy", "unsupported-strategy"})

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error with unsupported rotation strategy, got nil")
	}
	if !strings.Contains(err.Error(), "must be 'sticky', 'round-robin', or 'hybrid'") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestStopCommandWhenNotRunning verifies running agw stop when no daemon is active.
func TestStopCommandWhenNotRunning(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	// Stop falls back to port discovery, so the isolated config must point at a reserved free port
	// instead of the default 51400 owned by any gateway running on the operator machine.
	cfg := config.DefaultConfig()
	cfg.Port = getFreePort(t)
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	outBuf := new(bytes.Buffer)
	errBuf := new(bytes.Buffer)

	rootCmd.SetOut(outBuf)
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"stop"})

	execErr := rootCmd.Execute()
	if execErr != nil {
		t.Fatalf("unexpected error running stop: %v", execErr)
	}

	if !strings.Contains(outBuf.String(), "No running antigravity-gateway daemon found") {
		t.Errorf("unexpected output: %s", outBuf.String())
	}
}

// helperBinaryPath returns the platform-correct path for a binary built by a test. Windows resolves
// executables by their .exe suffix, so a bare "agw" name is never launchable there.
func helperBinaryPath(dir, name string) string {
	if runtime.GOOS == "windows" {
		return filepath.Join(dir, name+".exe")
	}
	return filepath.Join(dir, name)
}

// getFreePort binds an ephemeral TCP listener on 127.0.0.1 to obtain an available port for isolated test servers.
func getFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestEndToEndDaemonLifecycle verifies the full detached daemon lifecycle: start -d -> health check -> stop.
func TestEndToEndDaemonLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	// Build real binary for detached execution
	binPath := helperBinaryPath(tempDir, "agw")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed building test binary: %v\nOutput: %s", err, string(out))
	}
	t.Setenv("AGW_BIN", binPath)

	port := getFreePort(t)
	cfg := config.DefaultConfig()
	cfg.Port = port
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	// 1. Start daemon detached
	pid, logFile, err := server.StartDetached(cfg, []string{"--port", fmt.Sprintf("%d", port)})
	if err != nil {
		t.Fatalf("StartDetached failed: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("expected positive PID, got %d", pid)
	}
	if !server.IsProcessAlive(pid) {
		t.Fatalf("process %d is not alive", pid)
	}

	// 2. Verify PID file was written
	pidFile := filepath.Join(tempDir, "agw.pid")
	readPID, err := server.ReadPID(pidFile)
	if err != nil {
		t.Fatalf("reading pid file failed: %v", err)
	}
	if readPID != pid {
		t.Fatalf("expected pid file to contain %d, got %d", pid, readPID)
	}

	// 3. Verify /health responds
	healthURL := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	health, err := server.CheckHealth(ctx, healthURL)
	if err != nil {
		t.Fatalf("CheckHealth failed: %v (see log: %s)", err, logFile)
	}
	if !health.OK || health.Name != server.ServerName {
		t.Errorf("unexpected health response: %+v", health)
	}

	// 4. Starting a second detached daemon reports the running instance and version
	if _, _, dupErr := server.StartDetached(cfg, []string{"--port", fmt.Sprintf("%d", port)}); dupErr == nil {
		t.Error("expected second StartDetached call to fail while daemon is running")
	} else if msg := dupErr.Error(); !strings.Contains(msg, "already running (PID") || !strings.Contains(msg, "version") {
		t.Errorf("expected already running message with version, got %q", msg)
	}

	// 5. Stop daemon
	stoppedPID, err := server.StopDetached(cfg)
	if err != nil {
		t.Fatalf("StopDetached failed: %v", err)
	}
	if stoppedPID != pid {
		t.Errorf("expected stopped PID %d, got %d", pid, stoppedPID)
	}

	// 6. Verify process is no longer running and PID file is removed
	if server.IsProcessAlive(pid) {
		t.Errorf("process %d is still alive after StopDetached", pid)
	}
	if _, err := server.ReadPID(pidFile); err == nil {
		t.Errorf("pid file %s should have been removed", pidFile)
	}
}

// TestStartForegroundAlreadyRunning verifies that starting in the foreground when a daemon is already active fails gracefully.
func TestStartForegroundAlreadyRunning(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	startStrategy = ""
	startDetach = false
	startPort = 0
	defer func() {
		startStrategy = ""
		startDetach = false
		startPort = 0
	}()

	// Spawn a real mock /health server on an ephemeral port
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(server.HealthResponse{
			OK:      true,
			Name:    server.ServerName,
			Version: config.Version,
		})
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	mockPort, _ := strconv.Atoi(u.Port())

	cfg := config.DefaultConfig()
	cfg.Port = mockPort
	cfgPath, _ := config.ConfigFilePath()
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config failed: %v", err)
	}

	pidFile := filepath.Join(tempDir, "agw.pid")
	// A live, unrelated process stands in for an init/launchd-managed daemon. The parent process is alive
	// on every platform, differs from this test process (the foreground guard ignores its own PID), and
	// unlike PID 1 also exists on Windows.
	activePID := os.Getppid()
	if err := server.WritePID(pidFile, activePID); err != nil {
		t.Fatalf("writing fake active pid failed: %v", err)
	}

	outBuf := new(bytes.Buffer)
	errBuf := new(bytes.Buffer)
	rootCmd.SetOut(outBuf)
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"start"})

	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected start to fail when daemon is already running, got nil")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("unexpected error: %v", err)
	}

	// Verify pid file was NOT deleted or overwritten
	currPID, err := server.ReadPID(pidFile)
	if err != nil {
		t.Fatalf("pid file was unexpectedly deleted: %v", err)
	}
	if currPID != activePID {
		t.Errorf("expected pid to remain %d, got %d", activePID, currPID)
	}
}

// TestEndToEndStopWithoutPIDFile verifies that a running daemon can still be stopped when agw.pid
// has been deleted, by rediscovering it through the /health identity probe on its configured port.
func TestEndToEndStopWithoutPIDFile(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	binPath := helperBinaryPath(tempDir, "agw")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed building test binary: %v\nOutput: %s", err, string(out))
	}
	t.Setenv("AGW_BIN", binPath)

	port := getFreePort(t)
	cfg := config.DefaultConfig()
	cfg.Port = port
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	pid, logFile, err := server.StartDetached(cfg, []string{"--port", fmt.Sprintf("%d", port)})
	if err != nil {
		t.Fatalf("StartDetached failed: %v (see log: %s)", err, logFile)
	}
	t.Cleanup(func() {
		if server.IsProcessAlive(pid) {
			if _, stopErr := server.StopDetached(cfg); stopErr != nil {
				t.Logf("cleanup stop failed: %v", stopErr)
			}
		}
	})

	pidFile := filepath.Join(tempDir, "agw.pid")
	if _, err := server.ReadPID(pidFile); err != nil {
		t.Fatalf("expected pid file before removal: %v", err)
	}
	if err := os.Remove(pidFile); err != nil {
		t.Fatalf("removing pid file: %v", err)
	}

	stoppedPID, err := server.StopDetached(cfg)
	if err != nil {
		t.Fatalf("StopDetached without pid file failed: %v", err)
	}
	if stoppedPID != pid {
		t.Errorf("expected rediscovered PID %d, got %d", pid, stoppedPID)
	}

	deadline := time.Now().Add(server.StopTimeout)
	for server.IsProcessAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if server.IsProcessAlive(pid) {
		t.Errorf("process %d is still alive after stop without pid file", pid)
	}
	if _, err := server.ReadPID(pidFile); err == nil {
		t.Errorf("pid file %s should not be recreated after stop", pidFile)
	}
}

// TestEndToEndStartDetectsOrphanDaemon verifies that starting a second daemon recovers the PID file
// of an already running daemon whose pid file was deleted, instead of spawning a process that
// cannot bind and reporting a wrong PID.
func TestEndToEndStartDetectsOrphanDaemon(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	binPath := helperBinaryPath(tempDir, "agw")
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed building test binary: %v\nOutput: %s", err, string(out))
	}
	t.Setenv("AGW_BIN", binPath)

	port := getFreePort(t)
	cfg := config.DefaultConfig()
	cfg.Port = port
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	pid, logFile, err := server.StartDetached(cfg, []string{"--port", fmt.Sprintf("%d", port)})
	if err != nil {
		t.Fatalf("StartDetached failed: %v (see log: %s)", err, logFile)
	}
	// Stop in-process so the daemon child is reaped by this process; spawning `agw stop` here would
	// leave a zombie child and make the stop wait for the full StopTimeout.
	t.Cleanup(func() {
		if server.IsProcessAlive(pid) {
			if _, stopErr := server.StopDetached(cfg); stopErr != nil {
				t.Logf("cleanup stop failed: %v", stopErr)
			}
		}
	})

	pidFile := filepath.Join(tempDir, "agw.pid")
	if err := os.Remove(pidFile); err != nil {
		t.Fatalf("removing pid file: %v", err)
	}

	secondPID, _, secondErr := server.StartDetached(cfg, []string{"--port", fmt.Sprintf("%d", port)})
	if secondErr == nil {
		t.Fatal("expected StartDetached to report the orphaned daemon instead of starting a second one")
	}
	if secondPID != pid {
		t.Errorf("expected recovered PID %d, got %d", pid, secondPID)
	}
	if msg := secondErr.Error(); !strings.Contains(msg, "already running (PID") {
		t.Errorf("expected already running message, got %q", msg)
	}

	restoredPID, err := server.ReadPID(pidFile)
	if err != nil {
		t.Fatalf("expected pid file recovery, got error: %v", err)
	}
	if restoredPID != pid {
		t.Errorf("expected restored pid file to contain %d, got %d", pid, restoredPID)
	}
}

// TestDetachArgsForwardConfig verifies the detached child receives every launch override, including a
// custom --config path, so it never silently falls back to the default configuration file.
func TestDetachArgsForwardConfig(t *testing.T) {
	args := detachArgs("round-robin", 51999, "/tmp/custom-config.json")
	want := []string{"--strategy", "round-robin", "--port", "51999", "--config", "/tmp/custom-config.json"}

	if len(args) != len(want) {
		t.Fatalf("detachArgs length = %d, want %d (%v)", len(args), len(want), args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("detachArgs[%d] = %q, want %q (full: %v)", i, args[i], want[i], args)
		}
	}

	// Defaults must never emit empty flag values.
	if got := detachArgs("", 0, ""); len(got) != 0 {
		t.Errorf("expected no forwarded flags for defaults, got %v", got)
	}
}
