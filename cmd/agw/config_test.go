// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/server"
)

// TestConfigShowAndPath verifies that agw config show redacts secrets (API key and OAuth client
// secret) in both text and --json output, and agw config path prints the active configuration path.
func TestConfigShowAndPath(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	cfg := config.DefaultConfig()
	cfg.APIKey = config.APIKeyPrefix + strings.Repeat("d", config.APIKeyHexLen)
	cfg.ClientSecret = "super-secret-oauth-client-secret"
	cfg.Strategy = "hybrid"
	cfg.ModelMapping = map[string]string{"fast": "gemini-4-hydrogen-high"}
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	prevJSON := configJSON
	t.Cleanup(func() {
		configJSON = prevJSON
		configCmd.SetContext(nil)
		configShowCmd.SetContext(nil)
		configPathCmd.SetContext(nil)
	})

	configJSON = false
	buf := new(bytes.Buffer)
	configShowCmd.SetOut(buf)
	configShowCmd.SetErr(buf)
	configShowCmd.SetContext(t.Context())

	if err := configShowCmd.RunE(configShowCmd, []string{}); err != nil {
		t.Fatalf("configShowCmd.RunE failed: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, cfg.APIKey) || strings.Contains(out, cfg.ClientSecret) {
		t.Fatalf("SECURITY LEAK: config show leaked secret values:\n%s", out)
	}
	for _, want := range []string{"Strategy:              hybrid", "fast -> gemini-4-hydrogen-high", "configured (agw_"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in config show output:\n%s", want, out)
		}
	}

	configJSON = true
	buf.Reset()
	if err := configShowCmd.RunE(configShowCmd, []string{}); err != nil {
		t.Fatalf("configShowCmd.RunE --json failed: %v", err)
	}
	jsonOut := buf.String()
	if strings.Contains(jsonOut, cfg.APIKey) || strings.Contains(jsonOut, cfg.ClientSecret) {
		t.Fatalf("SECURITY LEAK: config show --json leaked secret values:\n%s", jsonOut)
	}
	if !strings.Contains(jsonOut, `"strategy": "hybrid"`) {
		t.Errorf("expected strategy in JSON output, got:\n%s", jsonOut)
	}

	configJSON = false
	buf.Reset()
	configPathCmd.SetOut(buf)
	configPathCmd.SetErr(buf)
	configPathCmd.SetContext(t.Context())
	if err := configPathCmd.RunE(configPathCmd, []string{}); err != nil {
		t.Fatalf("configPathCmd.RunE failed: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != filepath.Clean(cfgPath) {
		t.Errorf("config path = %q, want %q", got, filepath.Clean(cfgPath))
	}
}

// TestConfigStrategyReadWriteAndRestartHint verifies reading, validating, and persisting rotation
// strategy via agw config strategy without mutating unrelated fields, and warning when daemon runs.
func TestConfigStrategyReadWriteAndRestartHint(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(server.HealthResponse{
				OK:      true,
				Name:    server.ServerName,
				Version: config.Version,
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	mockPort, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parsing test server port: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Port = mockPort
	cfg.APIKey = config.APIKeyPrefix + strings.Repeat("e", config.APIKeyHexLen)
	cfg.Strategy = "sticky"
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	prevJSON := configJSON
	t.Cleanup(func() {
		configJSON = prevJSON
		configStrategyCmd.SetContext(nil)
	})
	configJSON = false

	buf := new(bytes.Buffer)
	configStrategyCmd.SetOut(buf)
	configStrategyCmd.SetErr(buf)
	configStrategyCmd.SetContext(t.Context())

	// 1. Read current strategy
	if err := configStrategyCmd.RunE(configStrategyCmd, []string{}); err != nil {
		t.Fatalf("config strategy read failed: %v", err)
	}
	if !strings.Contains(buf.String(), "Current strategy: sticky") {
		t.Errorf("expected current strategy sticky, got:\n%s", buf.String())
	}

	// 2. Reject invalid strategy without leaving side effects
	if err := configStrategyCmd.RunE(configStrategyCmd, []string{"invalid"}); err == nil {
		t.Fatal("expected error for invalid strategy, got nil")
	}

	// 3. Persist valid strategy and warn because mock daemon answered /health
	buf.Reset()
	if err := configStrategyCmd.RunE(configStrategyCmd, []string{"round-robin"}); err != nil {
		t.Fatalf("config strategy write failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `Strategy updated to "round-robin"`) {
		t.Errorf("expected update confirmation, got:\n%s", out)
	}
	if !strings.Contains(out, "restart it") {
		t.Errorf("expected running-daemon restart hint, got:\n%s", out)
	}

	reloaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reloading config: %v", err)
	}
	if reloaded.Strategy != "round-robin" {
		t.Errorf("persisted strategy = %q, want round-robin", reloaded.Strategy)
	}
	if reloaded.APIKey != cfg.APIKey || reloaded.Port != cfg.Port {
		t.Errorf("unrelated config fields were unexpectedly altered")
	}
}
