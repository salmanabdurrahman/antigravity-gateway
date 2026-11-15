// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

const (
	// DefaultHealthTimeout defines the timeout for local daemon health check verification.
	DefaultHealthTimeout = 2 * time.Second

	// DetachedStartupTimeout defines the maximum duration to wait for a detached daemon to answer /health.
	DetachedStartupTimeout = 10 * time.Second

	// StopTimeout defines the maximum duration to wait for a daemon process to exit gracefully after SIGTERM.
	StopTimeout = 8 * time.Second
)

// ReadPID reads and parses the process ID stored in the specified PID file.
func ReadPID(pidFile string) (int, error) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}

	trimmed := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("parsing pid from %s: %w", pidFile, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid %d in %s", pid, pidFile)
	}

	return pid, nil
}

// WritePID writes a process ID to the target file atomically with POSIX 0600 permissions.
func WritePID(pidFile string, pid int) error {
	dir := filepath.Dir(pidFile)
	if err := os.MkdirAll(dir, config.DirPerm); err != nil {
		return fmt.Errorf("creating directory for pid file %s: %w", dir, err)
	}

	tmpFile := fmt.Sprintf("%s.tmp.%d", pidFile, time.Now().UnixNano())
	content := fmt.Sprintf("%d\n", pid)

	if err := os.WriteFile(tmpFile, []byte(content), config.FilePerm); err != nil {
		return fmt.Errorf("writing temp pid file: %w", err)
	}

	if err := os.Rename(tmpFile, pidFile); err != nil {
		_ = os.Remove(tmpFile) // Best-effort cleanup of temporary pid file if atomic rename fails.
		return fmt.Errorf("renaming temp pid file to %s: %w", pidFile, err)
	}

	return nil
}

// ClearPID removes the specified PID file, ignoring errors if the file does not exist.
func ClearPID(pidFile string) error {
	err := os.Remove(pidFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// CheckHealth queries the /health endpoint of a local gateway instance and verifies identity.
func CheckHealth(ctx context.Context, healthURL string) (*HealthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building health request: %w", err)
	}

	client := &http.Client{Timeout: DefaultHealthTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing health check against %s: %w", healthURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status HTTP %d from %s", resp.StatusCode, healthURL)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading health response body: %w", err)
	}

	var health HealthResponse
	if err := json.Unmarshal(body, &health); err != nil {
		return nil, fmt.Errorf("parsing health response json: %w", err)
	}

	if !health.OK || health.Name != ServerName {
		return nil, fmt.Errorf("foreign server answered /health: name=%q ok=%v", health.Name, health.OK)
	}

	return &health, nil
}

// StartDetached forks and executes the agw server process in the background, redirecting
// output to ~/.agw/agw.log and waiting until the server responds healthy.
func StartDetached(cfg *config.Config, extraArgs []string) (int, string, error) {
	dir, err := config.EnsureDir()
	if err != nil {
		return 0, "", fmt.Errorf("ensuring configuration directory: %w", err)
	}

	pidFile, err := config.PIDFilePath()
	if err != nil {
		return 0, "", fmt.Errorf("resolving pid file path: %w", err)
	}

	logFile := filepath.Join(dir, "agw.log")
	logWriter, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, config.FilePerm)
	if err != nil {
		return 0, "", fmt.Errorf("opening log file %s: %w", logFile, err)
	}
	defer logWriter.Close()

	// Check if already running
	if existingPID, readErr := ReadPID(pidFile); readErr == nil && IsProcessAlive(existingPID) {
		host := cmp.Or(cfg.Host, config.DefaultHost)
		port := cmp.Or(cfg.Port, config.DefaultPort)
		healthURL := fmt.Sprintf("http://%s:%d/health", host, port)

		ctx, cancel := context.WithTimeout(context.Background(), DefaultHealthTimeout)
		defer cancel()
		if _, err := CheckHealth(ctx, healthURL); err == nil {
			return existingPID, logFile, fmt.Errorf("antigravity-gateway daemon is already running (PID: %d)", existingPID)
		}
		// Stale or dead process; clean up pid file
		_ = ClearPID(pidFile) // Best-effort removal of stale pid file.
	}

	execPath := os.Getenv("AGW_BIN")
	if execPath == "" {
		var err error
		execPath, err = os.Executable()
		if err != nil {
			return 0, "", fmt.Errorf("resolving executable path: %w", err)
		}
	}

	args := append([]string{"start"}, extraArgs...)
	cmd := exec.Command(execPath, args...)
	cmd.Stdout = logWriter
	cmd.Stderr = logWriter
	cmd.Stdin = nil
	cmd.Env = os.Environ()
	cmd.SysProcAttr = detachedSysProcAttr()

	if err := cmd.Start(); err != nil {
		return 0, "", fmt.Errorf("spawning background server process: %w", err)
	}

	pid := cmd.Process.Pid
	if err := WritePID(pidFile, pid); err != nil {
		_ = cmd.Process.Kill() // Terminate spawned process if pid file writing fails.
		return 0, "", fmt.Errorf("saving pid file %s: %w", pidFile, err)
	}

	host := cmp.Or(cfg.Host, config.DefaultHost)
	port := cmp.Or(cfg.Port, config.DefaultPort)
	healthURL := fmt.Sprintf("http://%s:%d/health", host, port)

	// Poll until server answers /health or deadline expires
	deadline := time.Now().Add(DetachedStartupTimeout)
	for time.Now().Before(deadline) {
		if !IsProcessAlive(pid) {
			_ = ClearPID(pidFile) // Remove stale pid file if daemon process died unexpectedly.
			return 0, logFile, fmt.Errorf("daemon process exited immediately. See log: %s", logFile)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, err := CheckHealth(ctx, healthURL)
		cancel()
		if err == nil {
			return pid, logFile, nil
		}

		time.Sleep(200 * time.Millisecond)
	}

	_ = cmd.Process.Kill() // Terminate process that failed to become healthy within deadline.
	_ = ClearPID(pidFile)  // Remove pid file after failed startup.
	return 0, logFile, fmt.Errorf("daemon did not respond healthy within %s. See log: %s", DetachedStartupTimeout, logFile)
}

// StopDetached stops the background daemon process identified by the PID file.
// It initiates process termination (SIGTERM on Unix, forceful termination on Windows),
// waits for graceful exit, and falls back to SIGKILL on Unix if necessary.
func StopDetached(cfg *config.Config) (int, error) {
	pidFile, err := config.PIDFilePath()
	if err != nil {
		return 0, fmt.Errorf("resolving pid file path: %w", err)
	}

	pid, err := ReadPID(pidFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	if !IsProcessAlive(pid) {
		_ = ClearPID(pidFile) // Clean up pid file if process is already dead.
		return 0, nil
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		_ = ClearPID(pidFile) // Clean up pid file if process handle resolution fails.
		return 0, nil
	}

	// Send termination signal
	if err := terminateProcess(proc); err != nil {
		if isProcessDoneErr(err) {
			// Process exited concurrently before termination signal was delivered.
			_ = ClearPID(pidFile) // Best-effort cleanup of stale pid file.
			return pid, nil
		}
		return 0, fmt.Errorf("terminating process pid %d: %w", pid, err)
	}

	// Wait up to StopTimeout for graceful termination
	deadline := time.Now().Add(StopTimeout)
	for time.Now().Before(deadline) {
		if !IsProcessAlive(pid) {
			_ = ClearPID(pidFile) // Clean up pid file once process has exited.
			return pid, nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Force kill if process failed to terminate gracefully
	_ = proc.Kill()       // Force kill if process failed to terminate within deadline.
	_ = ClearPID(pidFile) // Clean up pid file after forceful termination.
	return pid, nil
}
