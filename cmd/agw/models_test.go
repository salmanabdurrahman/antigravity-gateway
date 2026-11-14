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

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestModelsCommandEmpty verifies that agw models outputs instructions when the model cache is empty.
func TestModelsCommandEmpty(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	buf := new(bytes.Buffer)
	modelsCmd.SetOut(buf)
	modelsCmd.SetErr(buf)

	if err := modelsCmd.RunE(modelsCmd, []string{}); err != nil {
		t.Fatalf("modelsCmd.RunE failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "No chat models available") {
		t.Errorf("expected empty state message, got %s", out)
	}
}

// TestModelsListFromCache verifies formatting cached models from models-cache.json into a table.
func TestModelsListFromCache(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	cachePath := filepath.Join(tempHome, "models-cache.json")

	// Save fixture to cache
	tb := 4000
	snap := catalog.CatalogSnapshot{
		Models: []catalog.CatalogModel{
			{
				ID:              "gemini-3.8-flash-high",
				DisplayName:     "Gemini 3.8 Flash High",
				ContextWindow:   1048576,
				MaxOutputTokens: 65536,
				Thinking:        true,
				ThinkingBudget:  &tb,
				Images:          true,
			},
			{
				ID:              "gpt-oss-120b-medium",
				DisplayName:     "GPT OSS 120B Medium",
				ContextWindow:   131072,
				MaxOutputTokens: 32768,
				Thinking:        true,
				Images:          false,
			},
		},
	}

	payload := catalog.CachePayload{
		FetchedAt: 1731580000000,
		Models:    snap.Models,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshaling cache fixture: %v", err)
	}
	if err := os.WriteFile(cachePath, data, config.FilePerm); err != nil {
		t.Fatalf("writing cache file: %v", err)
	}

	buf := new(bytes.Buffer)
	modelsCmd.SetOut(buf)
	modelsCmd.SetErr(buf)

	if err := modelsCmd.RunE(modelsCmd, []string{}); err != nil {
		t.Fatalf("modelsCmd.RunE failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "gemini-3.8-flash-high") || !strings.Contains(out, "Gemini 3.8 Flash High") {
		t.Errorf("expected gemini model listed, got: %s", out)
	}
	if !strings.Contains(out, "gpt-oss-120b-medium") || !strings.Contains(out, "GPT OSS 120B Medium") {
		t.Errorf("expected gpt-oss model listed, got: %s", out)
	}
	if !strings.Contains(out, "Yes (4000)") {
		t.Errorf("expected thinking budget formatted, got: %s", out)
	}
}

// TestAccountsCommandCheck verifies that agw accounts --check queries upstream quotas and formats remaining fractions.
func TestAccountsCommandCheck(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-access-token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
		case "/v1internal:fetchAvailableModels":
			w.Header().Set("Content-Type", "application/json")
			remaining := 0.85
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": map[string]any{
					"gemini-3.8-flash-high": map[string]any{
						"maxTokens":        1048576,
						"maxOutputTokens":  65536,
						"supportsThinking": true,
						"supportsImages":   true,
						"quotaInfo": map[string]any{
							"remainingFraction": remaining,
						},
					},
					"claude-sonnet-4-6": map[string]any{
						"maxTokens":        1000000,
						"maxOutputTokens":  64000,
						"supportsThinking": true,
						"supportsImages":   true,
						"quotaInfo": map[string]any{
							"remainingFraction": 1.0,
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	// Write custom config with test server URLs
	cfg := config.DefaultConfig()
	cfg.TokenURL = ts.URL + "/token"
	cfg.Endpoints = []string{ts.URL}
	cfgPath, _ := config.ConfigFilePath()
	_ = config.Save(cfgPath, cfg)

	accPath := filepath.Join(tempHome, "accounts.json")
	store := auth.NewStore(accPath)
	_, _ = store.Upsert(&auth.Account{
		Email:        "user@example.com",
		RefreshToken: "refresh-secret-123",
		ProjectID:    "proj-mock",
	})

	// Test accounts with checkAccountsQuota = true
	checkAccountsQuota = true
	defer func() { checkAccountsQuota = false }()

	buf := new(bytes.Buffer)
	accountsCmd.SetOut(buf)
	accountsCmd.SetErr(buf)

	if err := accountsCmd.RunE(accountsCmd, []string{}); err != nil {
		t.Fatalf("accountsCmd.RunE failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "GEMINI QUOTA") || !strings.Contains(out, "CLAUDE QUOTA") {
		t.Errorf("expected quota headers in check output: %s", out)
	}
	if !strings.Contains(out, "user@example.com") || !strings.Contains(out, "85%") || !strings.Contains(out, "100%") {
		t.Errorf("expected account quotas listed: %s", out)
	}
}
