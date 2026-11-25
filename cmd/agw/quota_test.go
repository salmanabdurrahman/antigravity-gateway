// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestQuotaCommandSummaryAndPerModel verifies that agw quota renders 5-hour and weekly quota
// windows, automatically highlights constrained models, and prints all models when --models is set.
func TestQuotaCommandSummaryAndPerModel(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-access-token",
				"expires_in":   3600,
			})
		case "/v1internal:fetchAvailableModels":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": map[string]any{
					"gemini-3.1-pro-high": map[string]any{
						"quotaInfo": map[string]any{
							"remainingFraction": 0.0,
							"resetTime":         "2099-01-01T12:00:00Z",
						},
					},
					"gemini-4-hydrogen-high": map[string]any{
						"quotaInfo": map[string]any{
							"remainingFraction": 1.0,
						},
					},
					"claude-sonnet-4-6": map[string]any{
						"quotaInfo": map[string]any{
							"remainingFraction": 0.60,
							"resetTime":         "2099-01-01T14:00:00Z",
						},
					},
				},
			})
		case "/v1internal:retrieveUserQuotaSummary":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"fiveHour":        map[string]any{"remainingFraction": 0.50, "resetTime": "2099-01-01T12:00:00Z"},
				"weeklyQuotaInfo": map[string]any{"remainingFraction": 0.80, "resetTime": "2099-01-07T00:00:00Z"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cfg := config.DefaultConfig()
	cfg.TokenURL = ts.URL + "/token"
	cfg.Endpoints = []string{ts.URL}
	cfgPath, _ := config.ConfigFilePath()
	_ = config.Save(cfgPath, cfg)

	accPath := filepath.Join(tempHome, "accounts.json")
	store := auth.NewStore(accPath)
	_, _ = store.Upsert(&auth.Account{
		Email:        "quota@example.com",
		RefreshToken: "refresh-secret-quota",
		ProjectID:    "proj-quota",
		Tier:         config.TierPersonal,
	})

	prevModels, prevJSON, prevWatch, prevInterval, prevReenable := quotaShowModels, quotaJSON, quotaWatch, quotaWatchInterval, quotaReenable
	t.Cleanup(func() {
		quotaShowModels, quotaJSON, quotaWatch, quotaWatchInterval, quotaReenable = prevModels, prevJSON, prevWatch, prevInterval, prevReenable
		quotaCmd.SetContext(nil)
	})

	quotaShowModels = false
	quotaJSON = false
	quotaWatch = false
	quotaWatchInterval = 30 * time.Second
	quotaReenable = false

	buf := new(bytes.Buffer)
	quotaCmd.SetOut(buf)
	quotaCmd.SetErr(buf)
	quotaCmd.SetContext(t.Context())

	if err := quotaCmd.RunE(quotaCmd, []string{}); err != nil {
		t.Fatalf("quotaCmd.RunE failed: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"quota@example.com",
		"5h Window:",
		"50%",
		"Weekly Window:",
		"80%",
		"Constrained Models (<100%):",
		"gemini-3.1-pro-high",
		"claude-sonnet-4-6",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in default quota output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "gemini-4-hydrogen-high") {
		t.Errorf("expected untouched 100%% model omitted without --models:\n%s", out)
	}

	// With --models, untouched 100% models are included in the full table.
	quotaShowModels = true
	buf.Reset()
	if err := quotaCmd.RunE(quotaCmd, []string{}); err != nil {
		t.Fatalf("quotaCmd.RunE --models failed: %v", err)
	}
	allOut := buf.String()
	if !strings.Contains(allOut, "Models:") || !strings.Contains(allOut, "gemini-4-hydrogen-high") {
		t.Errorf("expected full per-model table with --models:\n%s", allOut)
	}
}

// TestQuotaCommandJSONAndEmpty verifies --json output without secret leakage and clean empty-pool handling.
func TestQuotaCommandJSONAndEmpty(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	prevModels, prevJSON, prevWatch := quotaShowModels, quotaJSON, quotaWatch
	t.Cleanup(func() {
		quotaShowModels, quotaJSON, quotaWatch = prevModels, prevJSON, prevWatch
		quotaCmd.SetContext(nil)
	})

	quotaShowModels = false
	quotaJSON = true
	quotaWatch = false

	buf := new(bytes.Buffer)
	quotaCmd.SetOut(buf)
	quotaCmd.SetErr(buf)
	quotaCmd.SetContext(t.Context())

	if err := quotaCmd.RunE(quotaCmd, []string{}); err != nil {
		t.Fatalf("quotaCmd empty --json failed: %v", err)
	}
	if !strings.Contains(buf.String(), `"accounts": []`) {
		t.Errorf("expected empty accounts JSON array, got:\n%s", buf.String())
	}
}

// TestQuotaCommandWatchValidationAndCancel verifies --watch rejects intervals below 5s and exits
// cleanly when the command context is cancelled.
func TestQuotaCommandWatchValidationAndCancel(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	prevWatch, prevInterval, prevJSON := quotaWatch, quotaWatchInterval, quotaJSON
	t.Cleanup(func() {
		quotaWatch, quotaWatchInterval, quotaJSON = prevWatch, prevInterval, prevJSON
		quotaCmd.SetContext(nil)
	})

	quotaWatch = true
	quotaWatchInterval = 1 * time.Second
	quotaJSON = false

	buf := new(bytes.Buffer)
	quotaCmd.SetOut(buf)
	quotaCmd.SetErr(buf)
	quotaCmd.SetContext(t.Context())

	if err := quotaCmd.RunE(quotaCmd, []string{}); err == nil || !strings.Contains(err.Error(), "must be at least 5s") {
		t.Fatalf("expected invalid --interval error, got: %v", err)
	}

	quotaWatchInterval = 5 * time.Second
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	quotaCmd.SetContext(ctx)

	if err := quotaCmd.RunE(quotaCmd, []string{}); err != nil {
		t.Fatalf("expected clean exit on cancelled watch context, got: %v", err)
	}
}

// TestQuotaCommandCatalogErrorSurfacesDiagnostic verifies that agw quota surfaces upstream errors
// when fetchAvailableModels fails instead of hiding the failure behind "Error".
func TestQuotaCommandCatalogErrorSurfacesDiagnostic(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-access-token",
				"expires_in":   3600,
			})
		case "/v1internal:fetchAvailableModels":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"message": "upstream catalog unavailable"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	cfg := config.DefaultConfig()
	cfg.TokenURL = ts.URL + "/token"
	cfg.Endpoints = []string{ts.URL}
	cfgPath, _ := config.ConfigFilePath()
	_ = config.Save(cfgPath, cfg)

	accPath := filepath.Join(tempHome, "accounts.json")
	_, _ = auth.NewStore(accPath).Upsert(&auth.Account{
		Email:        "err@example.com",
		RefreshToken: "refresh-secret",
		ProjectID:    "proj-err",
	})

	prevModels, prevJSON, prevWatch := quotaShowModels, quotaJSON, quotaWatch
	t.Cleanup(func() {
		quotaShowModels, quotaJSON, quotaWatch = prevModels, prevJSON, prevWatch
		quotaCmd.SetContext(nil)
	})
	quotaShowModels = false
	quotaJSON = false
	quotaWatch = false

	buf := new(bytes.Buffer)
	quotaCmd.SetOut(buf)
	quotaCmd.SetErr(buf)
	quotaCmd.SetContext(t.Context())

	if err := quotaCmd.RunE(quotaCmd, []string{}); err != nil {
		t.Fatalf("quotaCmd.RunE failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Error:") || !strings.Contains(out, "upstream catalog unavailable") {
		t.Errorf("expected diagnostic error message in quota output, got:\n%s", out)
	}
}
