// Package client provides automated configuration injectors and diagnostic tools
// for local AI coding agents such as Pi, OpenCode, and Claude Code.
package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultClaudeSettingsPath verifies resolution of settings.json respecting CLAUDE_CONFIG_DIR.
func TestDefaultClaudeSettingsPath(t *testing.T) {
	customDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", customDir)

	resolved, err := DefaultClaudeSettingsPath()
	if err != nil {
		t.Fatalf("DefaultClaudeSettingsPath failed: %v", err)
	}

	expected := filepath.Join(customDir, "settings.json")
	if resolved != expected {
		t.Errorf("expected %s, got %s", expected, resolved)
	}
}

// TestSetupClaude_NewFileAndVerification verifies configuring settings.json from scratch
// including atomic write and mock test completion.
func TestSetupClaude_NewFileAndVerification(t *testing.T) {
	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, "settings.json")

	// Mock server answering /v1/messages
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":   "msg_123",
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": "ok"},
			},
			"model": "claude-sonnet-4-6",
		})
	}))
	defer mockServer.Close()

	ctx := t.Context()
	opts := ClaudeSetupOptions{
		SettingsPath: settingsPath,
		GatewayURL:   mockServer.URL,
		APIKey:       "test-key",
		Model:        "claude-sonnet-4-6",
		NoVerify:     false,
		DryRun:       false,
	}

	res, err := SetupClaude(ctx, opts)
	if err != nil {
		t.Fatalf("SetupClaude failed: %v", err)
	}

	if res.SettingsPath != settingsPath {
		t.Errorf("expected path %s, got %s", settingsPath, res.SettingsPath)
	}
	if res.TestResult == nil || !res.TestResult.Success {
		t.Errorf("expected successful test probe result, got %+v", res.TestResult)
	}

	// Verify written JSON
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("reading settings file: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parsing settings JSON: %v", err)
	}

	envMap, ok := parsed["env"].(map[string]any)
	if !ok {
		t.Fatalf("expected env map, got %v", parsed["env"])
	}

	if envMap["ANTHROPIC_BASE_URL"] != mockServer.URL {
		t.Errorf("expected ANTHROPIC_BASE_URL %s, got %v", mockServer.URL, envMap["ANTHROPIC_BASE_URL"])
	}
	if envMap["ANTHROPIC_AUTH_TOKEN"] != "test-key" {
		t.Errorf("expected ANTHROPIC_AUTH_TOKEN test-key, got %v", envMap["ANTHROPIC_AUTH_TOKEN"])
	}
	if envMap["ANTHROPIC_MODEL"] != "claude-sonnet-4-6" {
		t.Errorf("expected ANTHROPIC_MODEL claude-sonnet-4-6, got %v", envMap["ANTHROPIC_MODEL"])
	}
}

// TestSetupClaude_PreservesExistingSettings verifies that existing settings in settings.json
// are preserved while gateway env entries are merged.
func TestSetupClaude_PreservesExistingSettings(t *testing.T) {
	tmpDir := t.TempDir()
	settingsPath := filepath.Join(tmpDir, "settings.json")

	initial := map[string]any{
		"customSetting": "preservedValue",
		"env": map[string]any{
			"EXISTING_VAR": "leaveMeAlone",
		},
	}
	initialBytes, _ := json.Marshal(initial)
	if err := os.WriteFile(settingsPath, initialBytes, 0o600); err != nil {
		t.Fatalf("writing initial settings: %v", err)
	}

	ctx := t.Context()
	opts := ClaudeSetupOptions{
		SettingsPath: settingsPath,
		GatewayURL:   "http://127.0.0.1:51400",
		APIKey:       "test-token",
		NoVerify:     true,
		DryRun:       false,
	}

	res, err := SetupClaude(ctx, opts)
	if err != nil {
		t.Fatalf("SetupClaude failed: %v", err)
	}

	if res.BackupPath == "" {
		t.Error("expected backup path to be created for existing settings file")
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("reading updated settings file: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parsing updated settings: %v", err)
	}

	if parsed["customSetting"] != "preservedValue" {
		t.Errorf("expected customSetting preserved, got %v", parsed["customSetting"])
	}

	envMap := parsed["env"].(map[string]any)
	if envMap["EXISTING_VAR"] != "leaveMeAlone" {
		t.Errorf("expected EXISTING_VAR preserved, got %v", envMap["EXISTING_VAR"])
	}
	if envMap["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:51400" {
		t.Errorf("expected ANTHROPIC_BASE_URL injected, got %v", envMap["ANTHROPIC_BASE_URL"])
	}
}
