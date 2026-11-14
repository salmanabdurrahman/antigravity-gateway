// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	if perm := info.Mode().Perm(); perm != 0o600 {
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
		name       string
		handler    http.HandlerFunc
		wantErr    bool
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

	cfg := config.DefaultConfig()

	// Stopping with no pid file returns 0, nil
	stoppedPID, err := StopDetached(cfg)
	if err != nil {
		t.Fatalf("StopDetached failed: %v", err)
	}
	if stoppedPID != 0 {
		t.Fatalf("expected stopped PID 0, got %d", stoppedPID)
	}
}
