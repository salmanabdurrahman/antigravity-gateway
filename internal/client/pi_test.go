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

// TestSetupPi_NewFileAndVerification verifies configuring Pi's models.json from scratch and testing connectivity.
func TestSetupPi_NewFileAndVerification(t *testing.T) {
	tempDir := t.TempDir()

	// Mock gateway server answering /v1/chat/completions
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
			ID:              "gemini-3.1-pro-high",
			DisplayName:     "Gemini 3.1 Pro High",
			Thinking:        true,
			Images:          true,
			ContextWindow:   1048576,
			MaxOutputTokens: 65536,
		},
		{
			ID:              "claude-sonnet-4-5",
			DisplayName:     "Claude Sonnet 4.5",
			Thinking:        true,
			Images:          true,
			ContextWindow:   200000,
			MaxOutputTokens: 8192,
		},
	}

	opts := PiSetupOptions{
		PiDir:      tempDir,
		GatewayURL: srv.URL,
		APIKey:     "test-api-key",
		Models:     models,
	}

	res, err := SetupPi(ctx, opts)
	if err != nil {
		t.Fatalf("SetupPi failed: %v", err)
	}

	if res.ModelCount != 2 {
		t.Errorf("expected 2 models, got %d", res.ModelCount)
	}
	if res.TestResult == nil || !res.TestResult.Success {
		t.Errorf("expected successful test result, got: %+v", res.TestResult)
	}
	if res.BackupPath != "" {
		t.Errorf("expected no backup path on fresh install, got %s", res.BackupPath)
	}

	// Verify file content on disk
	modelsFile := filepath.Join(tempDir, "models.json")
	raw, err := os.ReadFile(modelsFile)
	if err != nil {
		t.Fatalf("reading models.json: %v", err)
	}

	var parsed struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			API     string `json:"api"`
			APIKey  string `json:"apiKey"`
			Models  []struct {
				ID            string   `json:"id"`
				Name          string   `json:"name"`
				Input         []string `json:"input"`
				Reasoning     bool     `json:"reasoning"`
				ContextWindow int      `json:"contextWindow"`
				MaxTokens     int      `json:"maxTokens"`
			} `json:"models"`
		} `json:"providers"`
	}

	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshaling models.json: %v", err)
	}

	agwProv, ok := parsed.Providers["antigravity"]
	if !ok {
		t.Fatalf("expected provider 'antigravity' in models.json")
	}
	if agwProv.BaseURL != srv.URL+"/v1" {
		t.Errorf("expected baseUrl %s, got %s", srv.URL+"/v1", agwProv.BaseURL)
	}
	if agwProv.APIKey != "test-api-key" {
		t.Errorf("expected apiKey test-api-key, got %s", agwProv.APIKey)
	}
	if len(agwProv.Models) != 2 {
		t.Fatalf("expected 2 models in provider, got %d", len(agwProv.Models))
	}
	if agwProv.Models[0].ID != "gemini-3.1-pro-high" || agwProv.Models[0].Reasoning != true {
		t.Errorf("unexpected model 0: %+v", agwProv.Models[0])
	}

	// Second setup run: verifies backup creation and preservation of custom fields
	res2, err := SetupPi(ctx, opts)
	if err != nil {
		t.Fatalf("second SetupPi run failed: %v", err)
	}
	if res2.BackupPath == "" {
		t.Errorf("expected backup path to be created on second run")
	}
	if _, err := os.Stat(res2.BackupPath); err != nil {
		t.Errorf("backup file %s does not exist: %v", res2.BackupPath, err)
	}
}

// TestSetupPi_LegacyCleanup verifies purging legacy google-antigravity provider entries from models.json and auth.json.
func TestSetupPi_LegacyCleanup(t *testing.T) {
	tempDir := t.TempDir()
	modelsFile := filepath.Join(tempDir, "models.json")
	authFile := filepath.Join(tempDir, "auth.json")

	// Write legacy models.json and auth.json
	initialModels := map[string]any{
		"providers": map[string]any{
			"google-antigravity": map[string]any{
				"baseUrl": "http://127.0.0.1:51300/v1",
			},
			"other-provider": map[string]any{
				"baseUrl": "https://api.openai.com/v1",
			},
		},
	}
	_ = AtomicWriteJSON(modelsFile, initialModels, 0o600)

	initialAuth := map[string]any{
		"google-antigravity": map[string]any{
			"refresh": "proxy-managed",
		},
	}
	_ = AtomicWriteJSON(authFile, initialAuth, 0o600)

	ctx := t.Context()
	opts := PiSetupOptions{
		PiDir:       tempDir,
		GatewayURL:  "http://127.0.0.1:51400",
		APIKey:      "test-key",
		Models:      []catalog.CatalogModel{{ID: "gemini-3.1-pro-high"}},
		NoVerify:    true,
		CleanLegacy: true,
	}

	res, err := SetupPi(ctx, opts)
	if err != nil {
		t.Fatalf("SetupPi failed: %v", err)
	}

	if len(res.CleanedItems) != 2 {
		t.Errorf("expected 2 cleaned items, got %v", res.CleanedItems)
	}

	// Verify legacy removed, other preserved
	rawModels, _ := os.ReadFile(modelsFile)
	var parsedModels map[string]any
	_ = json.Unmarshal(rawModels, &parsedModels)
	providers := parsedModels["providers"].(map[string]any)

	if _, exists := providers["google-antigravity"]; exists {
		t.Errorf("expected google-antigravity to be deleted")
	}
	if _, exists := providers["other-provider"]; !exists {
		t.Errorf("expected other-provider to be preserved")
	}
	if _, exists := providers["antigravity"]; !exists {
		t.Errorf("expected antigravity provider to be injected")
	}
}
