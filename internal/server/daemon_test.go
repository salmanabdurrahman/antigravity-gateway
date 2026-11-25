// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestPIDFileLifecycle verifies reading, writing, and clearing agw.pid with POSIX 0600 permissions.
func TestPIDFileLifecycle(t *testing.T) {
	tempDir := t.TempDir()
	pidFile := filepath.Join(tempDir, "test.pid")

	// Read non-existent
	_, err := ReadPID(pidFile)
	if err == nil {
		t.Fatal("expected error reading non-existent pid file")
	}

	// Write PID
	testPID := 12345
	if err := WritePID(pidFile, testPID); err != nil {
		t.Fatalf("WritePID failed: %v", err)
	}

	// Verify permissions
	info, err := os.Stat(pidFile)
	if err != nil {
		t.Fatalf("stat pid file failed: %v", err)
	}
	// Windows does not model POSIX permission bits, so the 0600 contract is asserted on POSIX only.
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o600 {
		t.Errorf("expected permissions 0600, got %#o", perm)
	}

	// Read PID back
	readPID, err := ReadPID(pidFile)
	if err != nil {
		t.Fatalf("ReadPID failed: %v", err)
	}
	if readPID != testPID {
		t.Fatalf("expected pid %d, got %d", testPID, readPID)
	}

	// Clear PID
	if err := ClearPID(pidFile); err != nil {
		t.Fatalf("ClearPID failed: %v", err)
	}

	// Clearing again should be a no-op
	if err := ClearPID(pidFile); err != nil {
		t.Fatalf("second ClearPID failed: %v", err)
	}
}

// TestIsProcessAlive verifies detecting active and dead OS process IDs via signaling.
func TestIsProcessAlive(t *testing.T) {
	currentPID := os.Getpid()
	if !IsProcessAlive(currentPID) {
		t.Errorf("current process %d should report alive", currentPID)
	}

	if IsProcessAlive(-1) {
		t.Error("pid -1 should not report alive")
	}

	if IsProcessAlive(0) {
		t.Error("pid 0 should not report alive")
	}

	// High PID unlikely to exist
	if IsProcessAlive(9999999) {
		t.Log("warning: pid 9999999 reported alive, skipping dead pid assertion")
	}
}

// TestCheckHealth verifies health check probing against simulated endpoints.
func TestCheckHealth(t *testing.T) {
	tests := []struct {
		name         string
		handler      http.HandlerFunc
		wantErr      bool
		wantAccounts int
	}{
		{
			name: "healthy response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(fmt.Sprintf(`{"ok":true,"name":%q,"version":"0.1.0","accounts":2}`, ServerName)))
			},
			wantErr:      false,
			wantAccounts: 2,
		},
		{
			name: "foreign server response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"ok":true,"name":"foreign-app","version":"1.0"}`))
			},
			wantErr: true,
		},
		{
			name: "server error response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"server down"}`))
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			res, err := CheckHealth(ctx, server.URL)
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckHealth err = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && res.Accounts != tc.wantAccounts {
				t.Errorf("expected %d accounts, got %d", tc.wantAccounts, res.Accounts)
			}
		})
	}
}

// TestStopDetachedNonExistentPID verifies error handling when stopping an already dead daemon PID.
func TestStopDetachedNonExistentPID(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	// The configured port must be a reserved free port: stop falls back to port discovery, so using
	// the default 51400 would target a real daemon running on the operator machine.
	cfg := config.DefaultConfig()
	cfg.Port = getFreePort(t)

	// Stopping with no pid file returns 0, nil
	stoppedPID, err := StopDetached(cfg)
	if err != nil {
		t.Fatalf("StopDetached failed: %v", err)
	}
	if stoppedPID != 0 {
		t.Fatalf("expected stopped PID 0, got %d", stoppedPID)
	}
}

// TestStopDetachedRefusesToKillUnrelatedLiveProcess verifies that StopDetached does not kill an
// arbitrary live process when agw.pid holds a stale recycled OS PID and /health check fails.
func TestStopDetachedRefusesToKillUnrelatedLiveProcess(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	pidFile, err := config.PIDFilePath()
	if err != nil {
		t.Fatalf("resolving pid file path: %v", err)
	}

	// Use parent process PID (which is guaranteed alive and definitely not our daemon)
	parentPID := os.Getppid()
	if err := WritePID(pidFile, parentPID); err != nil {
		t.Fatalf("writing pid file: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Port = getFreePort(t)

	stoppedPID, err := StopDetached(cfg)
	if err != nil {
		t.Fatalf("StopDetached failed: %v", err)
	}
	if stoppedPID != 0 {
		t.Fatalf("expected stopped PID 0 (did not kill parent), got %d", stoppedPID)
	}

	// Stale PID file should have been cleaned up
	if _, err := os.Stat(pidFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected stale pid file to be removed, got err: %v", err)
	}
}

// TestStopDetachedVerifiesPortOwnershipDespiteHealthyDaemon verifies that a passing /health probe never
// authorizes signalling a pid file entry that does not own the daemon port.
func TestStopDetachedVerifiesPortOwnershipDespiteHealthyDaemon(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(config.EnvHome, tempDir)

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"name":"` + ServerName + `","version":"test","accounts":1}`))
	}))
	defer gateway.Close()
	port := gateway.Listener.Addr().(*net.TCPAddr).Port

	pidFile, err := config.PIDFilePath()
	if err != nil {
		t.Fatalf("resolving pid file path: %v", err)
	}
	// The parent process is alive and unrelated, but the listening socket belongs to this test process.
	unrelatedPID := os.Getppid()
	if err := WritePID(pidFile, unrelatedPID); err != nil {
		t.Fatalf("writing pid file: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Port = port

	stoppedPID, stopErr := StopDetached(cfg)
	if stopErr == nil {
		t.Fatalf("expected StopDetached to refuse terminating the current process, got pid %d", stoppedPID)
	}
	if stoppedPID != 0 {
		t.Errorf("expected no reported PID on refusal, got %d", stoppedPID)
	}
	if !IsProcessAlive(unrelatedPID) {
		t.Fatal("the unrelated process recorded in the pid file must never be signalled")
	}
}

// TestProcessListeningOnPortResolvesOwnProcess verifies that the socket table lookup attributes a
// live loopback listener to the process that owns it, which is the recovery path used when agw.pid
// has been deleted while the daemon keeps running.
func TestProcessListeningOnPortResolvesOwnProcess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	port := srv.Listener.Addr().(*net.TCPAddr).Port

	pid, err := processListeningOnPort(port)
	if err != nil {
		t.Fatalf("processListeningOnPort(%d) returned error: %v", port, err)
	}
	if pid != os.Getpid() {
		t.Errorf("expected owning pid %d, got %d", os.Getpid(), pid)
	}
}

// TestProcessListeningOnPortUnknownPort verifies that an unused port is reported as
// ErrProcessNotFound instead of returning a bogus process ID.
func TestProcessListeningOnPortUnknownPort(t *testing.T) {
	freePort := getFreePort(t)

	pid, err := processListeningOnPort(freePort)
	if err == nil {
		t.Fatalf("expected error for unused port %d, got pid %d", freePort, pid)
	}
	if !errors.Is(err, ErrProcessNotFound) {
		t.Errorf("expected error wrapping ErrProcessNotFound, got %v", err)
	}
}

// TestResolveRunningDaemonIdentity verifies that port based daemon discovery accepts a genuine
// gateway /health identity and rejects a foreign server sharing the same loopback port, so the
// recovery path can never signal an unrelated process.
func TestResolveRunningDaemonIdentity(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"name":"` + ServerName + `","version":"test","accounts":2}`))
	}))
	defer gateway.Close()

	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"name":"some-other-service"}`))
	}))
	defer foreign.Close()

	cases := []struct {
		name     string
		url      string
		wantPID  bool
		wantErr  bool
		wantName string
	}{
		{name: "gateway daemon", url: gateway.URL, wantPID: true, wantName: ServerName},
		{name: "foreign listener", url: foreign.URL, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u, err := url.Parse(tc.url)
			if err != nil {
				t.Fatalf("parsing test url: %v", err)
			}
			port, err := strconv.Atoi(u.Port())
			if err != nil {
				t.Fatalf("parsing test port: %v", err)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			pid, health, err := ResolveRunningDaemon(ctx, "127.0.0.1", port)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected discovery error, got pid %d", pid)
				}
				if !errors.Is(err, ErrDaemonNotRunning) {
					t.Errorf("expected error wrapping ErrDaemonNotRunning, got %v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("ResolveRunningDaemon returned error: %v", err)
			}
			if tc.wantPID && pid != os.Getpid() {
				t.Errorf("expected pid %d, got %d", os.Getpid(), pid)
			}
			if health == nil || health.Name != tc.wantName {
				t.Errorf("unexpected health identity: %+v", health)
			}
			if health.Accounts != 2 {
				t.Errorf("expected 2 accounts, got %d", health.Accounts)
			}
		})
	}
}

// getFreePort reserves and releases a loopback port to obtain a number nothing is listening on.
func getFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving free port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// TestParseLsofPID verifies extraction of the first positive process ID from lsof output,
// tolerating blank lines and non-numeric noise emitted by wrapper scripts.
func TestParseLsofPID(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   int
	}{
		{name: "single pid", output: "4242\n", want: 4242},
		{name: "blank lines", output: "\n\n4242\n", want: 4242},
		{name: "multiple pids", output: "4242\n5252\n", want: 4242},
		{name: "noise", output: "lsof: warning\n4242\n", want: 4242},
		{name: "empty", output: "", want: 0},
		{name: "only noise", output: "lsof: no process\n", want: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLsofPID(tc.output); got != tc.want {
				t.Errorf("parseLsofPID(%q) = %d, expected %d", tc.output, got, tc.want)
			}
		})
	}
}

// TestParseNetstatPID verifies parsing of the Windows netstat socket table so the Windows-only
// discovery path stays covered on every platform.
func TestParseNetstatPID(t *testing.T) {
	output := `
Active Connections

  Proto  Local Address          Foreign Address        State           PID
  TCP    127.0.0.1:51400        0.0.0.0:0              LISTENING       4242
  TCP    127.0.0.1:51401        0.0.0.0:0              LISTENING       5252
  TCP    127.0.0.1:51400        127.0.0.1:52345        ESTABLISHED     4242
  TCP    0.0.0.0:8080           0.0.0.0:0              LISTENING       6262
`
	cases := []struct {
		name string
		port int
		want int
	}{
		{name: "listening daemon port", port: 51400, want: 4242},
		{name: "second listening port", port: 51401, want: 5252},
		{name: "established only", port: 52345, want: 0},
		{name: "unused port", port: 51499, want: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseNetstatPID(output, tc.port); got != tc.want {
				t.Errorf("parseNetstatPID(port %d) = %d, expected %d", tc.port, got, tc.want)
			}
		})
	}
}
