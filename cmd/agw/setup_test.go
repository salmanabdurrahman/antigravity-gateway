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

// validTestAPIKey is a generated-format key, required because setup validates any --api-key override.
var validTestAPIKey = config.APIKeyPrefix + strings.Repeat("a", config.APIKeyHexLen)

// TestSetupRejectsInvalidAPIKeyOverride verifies a malformed --api-key fails the command instead of
// being written into every generated client configuration.
func TestSetupRejectsInvalidAPIKeyOverride(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())

	buf := new(bytes.Buffer)
	setupPiCmd.SetOut(buf)
	setupPiCmd.SetErr(buf)

	setupURL = "http://127.0.0.1:51400"
	setupAPIKey = "not-a-generated-key"
	setupNoVerify = true
	setupDryRun = true
	setupPiDir = t.TempDir()
	setupPiCleanLegacy = false
	setupModelsFilter = ""

	err := setupPiCmd.RunE(setupPiCmd, []string{})
	if err == nil {
		t.Fatal("expected an invalid --api-key to fail the command")
	}
	if !strings.Contains(err.Error(), "invalid --api-key") {
		t.Errorf("expected a clear --api-key error, got %v", err)
	}
	if strings.Contains(buf.String(), "not-a-generated-key") {
		t.Error("invalid key value must not be echoed")
	}
	if _, statErr := os.Stat(filepath.Join(setupPiDir, "models.json")); statErr == nil {
		t.Error("invalid key must not write a client configuration")
	}
}

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
	setupAPIKey = validTestAPIKey
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
	setupAPIKey = validTestAPIKey
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
	setupAPIKey = validTestAPIKey
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
	if !strings.Contains(out, `"apiKey": "<redacted:`) {
		t.Errorf("expected redacted gateway key indicator in dry-run output, got: %s", out)
	}
}

// TestSetupClaudeCommandDynamicModelAndExperimentalMCPOptOut verifies that agw setup claude
// resolves Claude models from /v1/models when --model is omitted and honors --experimental-mcp=false.
func TestSetupClaudeCommandDynamicModelAndExperimentalMCPOptOut(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	claudeSettings := filepath.Join(tempHome, "claude", "settings.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data": []map[string]any{
					{"id": "claude-sonnet-4-6", "context_window": 1000000},
					{"id": "claude-opus-4-6-thinking", "context_window": 1000000},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	prevURL, prevKey, prevNoVerify, prevDryRun := setupURL, setupAPIKey, setupNoVerify, setupDryRun
	prevSettings, prevModel, prevExpMCP := setupClaudeSettingsPath, setupClaudeModel, setupClaudeExperimentalMCP
	t.Cleanup(func() {
		setupURL, setupAPIKey, setupNoVerify, setupDryRun = prevURL, prevKey, prevNoVerify, prevDryRun
		setupClaudeSettingsPath, setupClaudeModel, setupClaudeExperimentalMCP = prevSettings, prevModel, prevExpMCP
		setupClaudeCmd.SetContext(nil)
	})

	setupURL = srv.URL
	setupAPIKey = validTestAPIKey
	setupNoVerify = true
	setupDryRun = false
	setupClaudeSettingsPath = claudeSettings
	setupClaudeModel = ""
	setupClaudeExperimentalMCP = false

	buf := new(bytes.Buffer)
	setupClaudeCmd.SetOut(buf)
	setupClaudeCmd.SetErr(buf)
	setupClaudeCmd.SetContext(t.Context())

	if err := setupClaudeCmd.RunE(setupClaudeCmd, []string{}); err != nil {
		t.Fatalf("setupClaudeCmd failed: %v", err)
	}

	raw, err := os.ReadFile(claudeSettings)
	if err != nil {
		t.Fatalf("reading settings.json: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("parsing settings.json: %v", err)
	}
	envMap, _ := parsed["env"].(map[string]any)
	if envMap["ANTHROPIC_MODEL"] != "claude-sonnet-4-6" {
		t.Errorf("ANTHROPIC_MODEL = %v; want claude-sonnet-4-6", envMap["ANTHROPIC_MODEL"])
	}
	if envMap["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "claude-opus-4-6-thinking" {
		t.Errorf("ANTHROPIC_DEFAULT_OPUS_MODEL = %v; want claude-opus-4-6-thinking", envMap["ANTHROPIC_DEFAULT_OPUS_MODEL"])
	}
	if _, exists := envMap["ENABLE_EXPERIMENTAL_MCP_CLI"]; exists {
		t.Errorf("expected ENABLE_EXPERIMENTAL_MCP_CLI omitted when --experimental-mcp=false")
	}
}
