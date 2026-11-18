// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestSetupPiCommand verifies executing agw setup pi to configure Pi Coding Agent.
func TestSetupPiCommand(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	piDir := filepath.Join(tempHome, "pi-agent")
	t.Setenv("PI_AGENT_DIR", piDir)

	// Mock gateway answering /v1/chat/completions
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": "ok",
					},
				},
			},
		})
	}))
	defer srv.Close()

	buf := new(bytes.Buffer)
	setupPiCmd.SetOut(buf)
	setupPiCmd.SetErr(buf)

	setupURL = srv.URL
	setupAPIKey = "test-key"
	setupNoVerify = false
	setupDryRun = false
	setupPiDir = piDir
	setupPiCleanLegacy = true
	setupModelsFilter = ""

	if err := setupPiCmd.RunE(setupPiCmd, []string{}); err != nil {
		t.Fatalf("setupPiCmd failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Configured Pi Coding Agent") {
		t.Errorf("expected success message, got: %s", out)
	}
	if !strings.Contains(out, "Verified end-to-end connectivity") {
		t.Errorf("expected connectivity verification, got: %s", out)
	}

	// Verify models.json exists on disk
	modelsPath := filepath.Join(piDir, "models.json")
	if _, err := os.Stat(modelsPath); err != nil {
		t.Fatalf("models.json was not created: %v", err)
	}
}

// TestSetupOpenCodeCommand verifies executing agw setup opencode to configure OpenCode.
func TestSetupOpenCodeCommand(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	opencodeConfig := filepath.Join(tempHome, "opencode", "opencode.json")

	// Mock gateway answering /v1/chat/completions
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": "ok",
					},
				},
			},
		})
	}))
	defer srv.Close()

	buf := new(bytes.Buffer)
	setupOpenCodeCmd.SetOut(buf)
	setupOpenCodeCmd.SetErr(buf)

	setupURL = srv.URL
	setupAPIKey = "test-key"
	setupNoVerify = false
	setupDryRun = false
	setupOpenCodePath = opencodeConfig
	setupModelsFilter = ""

	if err := setupOpenCodeCmd.RunE(setupOpenCodeCmd, []string{}); err != nil {
		t.Fatalf("setupOpenCodeCmd failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Configured OpenCode") {
		t.Errorf("expected success message, got: %s", out)
	}
	if !strings.Contains(out, "Verified end-to-end connectivity") {
		t.Errorf("expected connectivity verification, got: %s", out)
	}

	if _, err := os.Stat(opencodeConfig); err != nil {
		t.Fatalf("opencode.json was not created: %v", err)
	}
}

// TestSetupClaudeCommand verifies executing agw setup claude to configure Claude Code CLI.
func TestSetupClaudeCommand(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	claudeSettings := filepath.Join(tempHome, "claude", "settings.json")

	// Mock gateway answering /v1/messages
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   "msg_test",
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "ok"},
			},
			"model": "claude-sonnet-4-6",
		})
	}))
	defer srv.Close()

	buf := new(bytes.Buffer)
	setupClaudeCmd.SetOut(buf)
	setupClaudeCmd.SetErr(buf)

	setupURL = srv.URL
	setupAPIKey = "test-key"
	setupNoVerify = false
	setupDryRun = false
	setupClaudeSettingsPath = claudeSettings
	setupClaudeModel = "claude-sonnet-4-6"

	if err := setupClaudeCmd.RunE(setupClaudeCmd, []string{}); err != nil {
		t.Fatalf("setupClaudeCmd failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Configured Claude Code CLI") {
		t.Errorf("expected success message, got: %s", out)
	}
	if !strings.Contains(out, "Verified end-to-end connectivity") {
		t.Errorf("expected connectivity verification, got: %s", out)
	}

	if _, err := os.Stat(claudeSettings); err != nil {
		t.Fatalf("settings.json was not created: %v", err)
	}
}

// TestSetupDryRunRedactsSecrets verifies that running setup commands in --dry-run mode masks
// the gateway API key and auth token before printing the generated JSON payload to stdout.
func TestSetupDryRunRedactsSecrets(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	piDir := filepath.Join(tempHome, "pi-agent")
	t.Setenv("PI_AGENT_DIR", piDir)

	buf := new(bytes.Buffer)
	setupPiCmd.SetOut(buf)
	setupPiCmd.SetErr(buf)

	rawKey := "agw_abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	setupURL = "http://127.0.0.1:51400"
	setupAPIKey = rawKey
	setupNoVerify = true
	setupDryRun = true
	setupPiDir = piDir
	setupPiCleanLegacy = false
	setupModelsFilter = ""

	if err := setupPiCmd.RunE(setupPiCmd, []string{}); err != nil {
		t.Fatalf("setupPiCmd in dry-run mode failed: %v", err)
	}

	out := buf.String()
	if strings.Contains(out, rawKey) {
		t.Errorf("SECURITY LEAK: --dry-run output contains plaintext gateway API key: %s", out)
	}
	if !strings.Contains(out, "agw_••••••••") {
		t.Errorf("expected redacted gateway key indicator in dry-run output, got: %s", out)
	}
}
