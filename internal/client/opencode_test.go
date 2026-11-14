// Package client provides automated configuration injectors and diagnostic tools
// for local AI coding agents such as Pi and OpenCode.
package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
)

// TestSetupOpenCode_NewFileAndVerification tests initializing opencode.json from scratch,
// registering models, and verifying connectivity via a test completion probe.
func TestSetupOpenCode_NewFileAndVerification(t *testing.T) {
	tempDir := t.TempDir()
	cfgPath := filepath.Join(tempDir, "opencode.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-api-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
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

	ctx := t.Context()
	models := []catalog.CatalogModel{
		{
			ID:          "gemini-3.1-pro-high",
			DisplayName: "Gemini 3.1 Pro High",
		},
	}

	opts := OpenCodeSetupOptions{
		ConfigPath: cfgPath,
		GatewayURL: srv.URL,
		APIKey:     "test-api-key",
		Models:     models,
	}

	res, err := SetupOpenCode(ctx, opts)
	if err != nil {
		t.Fatalf("SetupOpenCode failed: %v", err)
	}

	if res.ModelCount != 1 {
		t.Errorf("expected 1 model, got %d", res.ModelCount)
	}
	if res.TestResult == nil || !res.TestResult.Success {
		t.Errorf("expected successful test result, got: %+v", res.TestResult)
	}
	if res.BackupPath != "" {
		t.Errorf("expected no backup path on first run, got %s", res.BackupPath)
	}

	// Verify file content on disk
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading opencode.json: %v", err)
	}

	var parsed struct {
		Schema    string `json:"$schema"`
		Providers map[string]struct {
			Name     string `json:"name"`
			Package  string `json:"package"`
			Settings struct {
				BaseURL string `json:"baseURL"`
				APIKey  string `json:"apiKey"`
			} `json:"settings"`
			Models map[string]struct {
				Name string `json:"name"`
			} `json:"models"`
		} `json:"providers"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshaling opencode.json: %v", err)
	}

	if parsed.Schema != "https://opencode.ai/config.json" {
		t.Errorf("expected schema https://opencode.ai/config.json, got %s", parsed.Schema)
	}

	agwProv, ok := parsed.Providers["antigravity"]
	if !ok {
		t.Fatalf("expected provider antigravity in opencode.json")
	}
	if agwProv.Package != OpenCodePackageOpenAICompatible {
		t.Errorf("expected package %s, got %s", OpenCodePackageOpenAICompatible, agwProv.Package)
	}
	if agwProv.Settings.BaseURL != srv.URL+"/v1" {
		t.Errorf("expected baseURL %s, got %s", srv.URL+"/v1", agwProv.Settings.BaseURL)
	}
	if agwProv.Settings.APIKey != "test-api-key" {
		t.Errorf("expected apiKey test-api-key, got %s", agwProv.Settings.APIKey)
	}
	if m, ok := agwProv.Models["gemini-3.1-pro-high"]; !ok || m.Name != "Gemini 3.1 Pro High" {
		t.Errorf("unexpected model mapping: %+v", m)
	}

	// Second setup run: verifies backup creation
	res2, err := SetupOpenCode(ctx, opts)
	if err != nil {
		t.Fatalf("second SetupOpenCode run failed: %v", err)
	}
	if res2.BackupPath == "" {
		t.Errorf("expected backup path on second run")
	}
	if _, err := os.Stat(res2.BackupPath); err != nil {
		t.Errorf("backup file %s does not exist: %v", res2.BackupPath, err)
	}
}
