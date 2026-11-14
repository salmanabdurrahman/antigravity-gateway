// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
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
	if !strings.Contains(err.Error(), "must be 'sticky' or 'round-robin'") {
		t.Errorf("unexpected error message: %v", err)
	}
}

// TestStopCommandWhenNotRunning verifies running agw stop when no daemon is active.
func TestStopCommandWhenNotRunning(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	outBuf := new(bytes.Buffer)
	errBuf := new(bytes.Buffer)

	rootCmd.SetOut(outBuf)
	rootCmd.SetErr(errBuf)
	rootCmd.SetArgs([]string{"stop"})

	err := rootCmd.Execute()
	if err != nil {
		t.Fatalf("unexpected error running stop: %v", err)
	}

	if !strings.Contains(outBuf.String(), "No running antigravity-gateway daemon found") {
		t.Errorf("unexpected output: %s", outBuf.String())
	}
}

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
	binPath := filepath.Join(tempDir, "agw")
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

	// 4. Stop daemon
	stoppedPID, err := server.StopDetached(cfg)
	if err != nil {
		t.Fatalf("StopDetached failed: %v", err)
	}
	if stoppedPID != pid {
		t.Errorf("expected stopped PID %d, got %d", pid, stoppedPID)
	}

	// 5. Verify process is no longer running and PID file is removed
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

	pidFile := filepath.Join(tempDir, "agw.pid")
	// Simulate an active process
	activePID := 1 // init/launchd always exists
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
