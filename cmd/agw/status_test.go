// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/server"
)

// TestStatusCommandStopped verifies that agw status reports stopped daemon state, configured
// strategy, stored accounts count, and clean JSON without leaking the local API key.
func TestStatusCommandStopped(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	cfg := config.DefaultConfig()
	cfg.Port = getFreePort(t)
	cfg.APIKey = config.APIKeyPrefix + strings.Repeat("a", config.APIKeyHexLen)
	cfg.Strategy = "hybrid"
	cfgPath, _ := config.ConfigFilePath()
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	accPath := filepath.Join(tempHome, "accounts.json")
	_, _ = auth.NewStore(accPath).Upsert(&auth.Account{
		Email:        "stored@example.com",
		RefreshToken: "refresh-secret",
		ProjectID:    "proj-stored",
	})

	prevPort, prevJSON := statusPort, statusJSON
	t.Cleanup(func() {
		statusPort, statusJSON = prevPort, prevJSON
		statusCmd.SetContext(nil)
	})

	statusPort = 0
	statusJSON = false

	buf := new(bytes.Buffer)
	statusCmd.SetOut(buf)
	statusCmd.SetErr(buf)
	statusCmd.SetContext(t.Context())

	if err := statusCmd.RunE(statusCmd, []string{}); err != nil {
		t.Fatalf("statusCmd.RunE failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Daemon:    Stopped", "Strategy:  hybrid", "Accounts:  1 usable / 1 total", "agw start --detach"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in stopped status output:\n%s", want, out)
		}
	}

	statusJSON = true
	buf.Reset()
	if err := statusCmd.RunE(statusCmd, []string{}); err != nil {
		t.Fatalf("statusCmd.RunE --json failed: %v", err)
	}
	jsonOut := buf.String()
	if strings.Contains(jsonOut, cfg.APIKey) || strings.Contains(jsonOut, "refresh-secret") {
		t.Fatalf("SECURITY LEAK: status --json leaked secret material: %s", jsonOut)
	}
	if !strings.Contains(jsonOut, `"running": false`) || !strings.Contains(jsonOut, `"strategy": "hybrid"`) {
		t.Errorf("unexpected stopped JSON output:\n%s", jsonOut)
	}
}

// TestStatusCommandRunning verifies that agw status probes /health and authenticated /v1/accounts,
// rendering the live strategy and per-account runtime metrics without leaking secrets.
func TestStatusCommandRunning(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	cfg := config.DefaultConfig()
	cfg.APIKey = config.APIKeyPrefix + strings.Repeat("b", config.APIKeyHexLen)
	cfgPath, _ := config.ConfigFilePath()
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(server.HealthResponse{
				OK:       true,
				Name:     server.ServerName,
				Version:  config.Version,
				Accounts: 1,
			})
		case "/v1/accounts":
			if got := r.Header.Get("Authorization"); got != "Bearer "+cfg.APIKey {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(server.AccountsResponse{
				Strategy: "round-robin",
				Accounts: []rotator.AccountSnapshot{
					{
						Email:      "live@example.com",
						ProjectID:  "proj-live",
						Tier:       config.TierPersonal,
						Health:     98,
						Requests:   17,
						RateLimits: 1,
						LastUsed:   time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339),
						Cooldowns:  map[string]string{"gemini-3.1-pro-high": "2099-01-01T12:00:00Z"},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	mockPort, _ := strconv.Atoi(u.Port())

	prevPort, prevJSON := statusPort, statusJSON
	t.Cleanup(func() {
		statusPort, statusJSON = prevPort, prevJSON
		statusCmd.SetContext(nil)
	})

	statusPort = mockPort
	statusJSON = false

	buf := new(bytes.Buffer)
	statusCmd.SetOut(buf)
	statusCmd.SetErr(buf)
	statusCmd.SetContext(t.Context())

	if err := statusCmd.RunE(statusCmd, []string{}); err != nil {
		t.Fatalf("statusCmd.RunE running failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Daemon:    Running",
		"Strategy:  round-robin",
		"live@example.com",
		"Cooling",
		"98%",
		"17",
		"gemini-3.1-pro-high",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in running status output:\n%s", want, out)
		}
	}
	if strings.Contains(out, cfg.APIKey) {
		t.Fatalf("SECURITY LEAK: status output contained gateway API key")
	}

	statusJSON = true
	buf.Reset()
	if err := statusCmd.RunE(statusCmd, []string{}); err != nil {
		t.Fatalf("statusCmd.RunE running --json failed: %v", err)
	}
	jsonOut := buf.String()
	if strings.Contains(jsonOut, cfg.APIKey) {
		t.Fatalf("SECURITY LEAK: status --json contained gateway API key")
	}
	if !strings.Contains(jsonOut, `"running": true`) || !strings.Contains(jsonOut, `"strategy": "round-robin"`) {
		t.Errorf("unexpected running JSON output:\n%s", jsonOut)
	}
}

// TestStatusCommandCustomPortIgnoresPrimaryPIDFile verifies that probing a custom --port never
// attributes an unrelated alive PID from ~/.agw/agw.pid to the probed server.
func TestStatusCommandCustomPortIgnoresPrimaryPIDFile(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	cfg := config.DefaultConfig()
	cfg.Port = getFreePort(t)
	cfg.APIKey = config.APIKeyPrefix + strings.Repeat("c", config.APIKeyHexLen)
	cfgPath, _ := config.ConfigFilePath()
	_ = config.Save(cfgPath, cfg)

	// Record an unrelated alive PID in agw.pid (e.g. primary daemon on another port).
	pidFile := filepath.Join(tempHome, "agw.pid")
	if err := server.WritePID(pidFile, os.Getppid()); err != nil {
		t.Fatalf("writing pid file: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(server.HealthResponse{
				OK:       true,
				Name:     server.ServerName,
				Version:  config.Version,
				Accounts: 0,
			})
		case "/v1/accounts":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(server.AccountsResponse{Strategy: "sticky"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	mockPort, _ := strconv.Atoi(u.Port())

	prevPort, prevJSON := statusPort, statusJSON
	t.Cleanup(func() {
		statusPort, statusJSON = prevPort, prevJSON
		statusCmd.SetContext(nil)
	})
	statusPort = mockPort
	statusJSON = true

	buf := new(bytes.Buffer)
	statusCmd.SetOut(buf)
	statusCmd.SetErr(buf)
	statusCmd.SetContext(t.Context())

	if err := statusCmd.RunE(statusCmd, []string{}); err != nil {
		t.Fatalf("statusCmd.RunE failed: %v", err)
	}

	var rep statusReport
	if err := json.Unmarshal(buf.Bytes(), &rep); err != nil {
		t.Fatalf("unmarshaling status JSON: %v", err)
	}
	if !rep.Running {
		t.Fatalf("expected running=true")
	}
	if rep.PID == os.Getppid() {
		t.Errorf("custom --port probe must not misattribute primary agw.pid (%d)", os.Getppid())
	}
}
