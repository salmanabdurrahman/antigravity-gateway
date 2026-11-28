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
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/risk"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/server"
)

// prepareStatusRiskTest seeds an isolated home with one suspended account and returns its risk file path.
func prepareStatusRiskTest(t *testing.T) string {
	t.Helper()

	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	cfg := config.DefaultConfig()
	cfg.Port = getFreePort(t)
	cfg.APIKey = config.APIKeyPrefix + strings.Repeat("a", config.APIKeyHexLen)
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	accPath, err := config.AccountsFilePath()
	if err != nil {
		t.Fatalf("resolving accounts path: %v", err)
	}
	store := auth.NewStore(accPath)
	if _, upsertErr := store.Upsert(&auth.Account{
		Email:        "risk@example.com",
		RefreshToken: "refresh-risk-secret",
		ProjectID:    "proj-risk",
		AddedAt:      "2026-11-01T00:00:00Z",
	}); upsertErr != nil {
		t.Fatalf("seeding account: %v", upsertErr)
	}
	if suspendErr := store.SetSuspended("risk@example.com", "tos_violation", "This service has been disabled.", "https://forms.gle/RISKCASE"); suspendErr != nil {
		t.Fatalf("seeding suspension: %v", suspendErr)
	}

	prevPort, prevJSON, prevRisk := statusPort, statusJSON, statusRisk
	t.Cleanup(func() {
		statusPort, statusJSON, statusRisk = prevPort, prevJSON, prevRisk
		statusCmd.SetOut(nil)
		statusCmd.SetErr(nil)
		statusCmd.SetContext(nil)
	})
	statusPort = 0
	statusJSON = false
	statusRisk = true

	riskPath, err := config.RiskFilePath()
	if err != nil {
		t.Fatalf("resolving risk path: %v", err)
	}
	return riskPath
}

// runStatusRisk executes agw status with --risk and returns the rendered output.
func runStatusRisk(t *testing.T) string {
	t.Helper()

	buf := new(bytes.Buffer)
	statusCmd.SetOut(buf)
	statusCmd.SetErr(buf)
	statusCmd.SetContext(t.Context())
	if err := statusCmd.RunE(statusCmd, nil); err != nil {
		t.Fatalf("statusCmd.RunE failed: %v", err)
	}
	return buf.String()
}

// TestStatusRiskPersistsTelemetry verifies that agw status --risk prints the local risk posture and writes a
// bounded, owner-only telemetry snapshot with masked identity and no credential material.
func TestStatusRiskPersistsTelemetry(t *testing.T) {
	riskPath := prepareStatusRiskTest(t)

	out := runStatusRisk(t)
	for _, want := range []string{
		"[Risk Telemetry]",
		"Posture:   safe profile, single rotation, 2 per account in flight, guard 3 bare 429s / 900s",
		"r***@example.com",
		"Suspended (tos_violation)",
		riskPath,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status --risk output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "risk@example.com") {
		t.Errorf("status --risk printed the full email address:\n%s", out)
	}

	info, err := os.Stat(riskPath)
	if err != nil {
		t.Fatalf("stat risk telemetry file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("risk telemetry mode = %o; want 600", mode)
		}
	}

	document, err := risk.NewStore(riskPath).Load()
	if err != nil {
		t.Fatalf("loading risk telemetry: %v", err)
	}
	if len(document.Snapshots) != 1 {
		t.Fatalf("snapshots = %d; want 1", len(document.Snapshots))
	}
	snapshot := document.Snapshots[0]
	if snapshot.Profile != config.ProfileSafe || snapshot.RotationMode != config.RotationModeSingle {
		t.Errorf("snapshot posture = (%q, %q); want safe/single", snapshot.Profile, snapshot.RotationMode)
	}
	if len(snapshot.Accounts) != 1 {
		t.Fatalf("account entries = %d; want 1", len(snapshot.Accounts))
	}
	entry := snapshot.Accounts[0]
	if entry.Email != "r***@example.com" {
		t.Errorf("stored email = %q; want the masked form", entry.Email)
	}
	if !entry.Suspended || entry.SuspendKind != "tos_violation" {
		t.Errorf("stored suspension = (%v, %q); want a tos_violation verdict", entry.Suspended, entry.SuspendKind)
	}
	if entry.Credential == "" {
		t.Error("stored credential digest is empty")
	}

	raw, err := os.ReadFile(riskPath)
	if err != nil {
		t.Fatalf("reading risk telemetry: %v", err)
	}
	for _, forbidden := range []string{"refresh-risk-secret", "risk@example.com"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("SECURITY LEAK: risk telemetry contains %q", forbidden)
		}
	}
}

// TestStatusRiskJSONIncludesSnapshot verifies that structured output carries the captured snapshot and its
// storage path.
func TestStatusRiskJSONIncludesSnapshot(t *testing.T) {
	riskPath := prepareStatusRiskTest(t)
	statusJSON = true

	out := runStatusRisk(t)

	var report statusReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decoding status JSON: %v\n%s", err, out)
	}
	if report.Risk == nil {
		t.Fatal("risk snapshot missing from JSON output")
	}
	if report.RiskPath != riskPath {
		t.Errorf("risk_path = %q; want %q", report.RiskPath, riskPath)
	}
	if report.RiskError != "" {
		t.Errorf("risk_error = %q; want empty", report.RiskError)
	}
	if report.RiskGuardCooldownSeconds != config.DefaultPromptRejectionCooldownSeconds {
		t.Errorf("risk_guard_cooldown_seconds = %d; want %d", report.RiskGuardCooldownSeconds, config.DefaultPromptRejectionCooldownSeconds)
	}
	if len(report.Risk.Accounts) != 1 || !report.Risk.Accounts[0].Suspended {
		t.Errorf("risk accounts = %+v; want one suspended entry", report.Risk.Accounts)
	}
	if filepath.Base(riskPath) != "risk.json" {
		t.Errorf("risk path basename = %q; want risk.json", filepath.Base(riskPath))
	}
}

// TestStatusRiskMergesRuntimeCounters verifies that a running daemon's live counters land in the persisted
// snapshot, and that quoted addresses inside error text are masked before they reach disk.
func TestStatusRiskMergesRuntimeCounters(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	cfg := config.DefaultConfig()
	cfg.APIKey = config.APIKeyPrefix + strings.Repeat("a", config.APIKeyHexLen)
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	accPath, err := config.AccountsFilePath()
	if err != nil {
		t.Fatalf("resolving accounts path: %v", err)
	}
	if _, upsertErr := auth.NewStore(accPath).Upsert(&auth.Account{
		Email:        "live@example.com",
		RefreshToken: "refresh-live",
		ProjectID:    "proj-live",
		AddedAt:      "2026-11-01T00:00:00Z",
	}); upsertErr != nil {
		t.Fatalf("seeding account: %v", upsertErr)
	}

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(server.HealthResponse{OK: true, Name: server.ServerName, Version: config.Version, Accounts: 1})
		case "/v1/accounts":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(server.AccountsResponse{
				Strategy: "sticky",
				Accounts: []rotator.AccountSnapshot{{
					Email:            "live@example.com",
					ProjectID:        "proj-live",
					Requests:         17,
					RateLimits:       1,
					PromptRejections: 2,
					LastUpstreamOK:   "2026-11-28T00:00:00Z",
					LastError:        "quota exhausted for live@example.com",
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()

	parsed, err := url.Parse(daemon.URL)
	if err != nil {
		t.Fatalf("parsing fake daemon URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parsing fake daemon port: %v", err)
	}

	prevPort, prevJSON, prevRisk := statusPort, statusJSON, statusRisk
	t.Cleanup(func() {
		statusPort, statusJSON, statusRisk = prevPort, prevJSON, prevRisk
		statusCmd.SetOut(nil)
		statusCmd.SetErr(nil)
		statusCmd.SetContext(nil)
	})
	statusPort = port
	statusJSON = false
	statusRisk = true

	out := runStatusRisk(t)
	for _, want := range []string{"[Risk Telemetry]", "17", "l***@example.com"} {
		if !strings.Contains(out, want) {
			t.Errorf("status --risk output missing %q:\n%s", want, out)
		}
	}
	riskSection := out[strings.Index(out, "[Risk Telemetry]"):]
	if end := strings.Index(riskSection, "\nEMAIL"); end >= 0 {
		riskSection = riskSection[:end]
	}
	if strings.Contains(riskSection, "live@example.com") {
		t.Errorf("risk telemetry block leaked the full address:\n%s", riskSection)
	}

	riskPath, err := config.RiskFilePath()
	if err != nil {
		t.Fatalf("resolving risk path: %v", err)
	}
	document, err := risk.NewStore(riskPath).Load()
	if err != nil {
		t.Fatalf("loading risk telemetry: %v", err)
	}
	entry := document.Snapshots[0].Accounts[0]
	if entry.Requests != 17 || entry.RateLimits != 1 || entry.PromptRejections != 2 {
		t.Errorf("persisted counters = (%d, %d, %d); want (17, 1, 2)", entry.Requests, entry.RateLimits, entry.PromptRejections)
	}
	if entry.LastUpstreamOK != "2026-11-28T00:00:00Z" {
		t.Errorf("LastUpstreamOK = %q; want the runtime timestamp", entry.LastUpstreamOK)
	}
	if entry.LastError != "quota exhausted for l***@example.com" {
		t.Errorf("LastError = %q; want the masked runtime error", entry.LastError)
	}
}

// TestStatusRiskWithoutAccounts verifies that an empty pool still produces a posture snapshot without an
// account table, so the capture never fails on a fresh install.
func TestStatusRiskWithoutAccounts(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	cfg := config.DefaultConfig()
	cfg.Port = getFreePort(t)
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	prevPort, prevJSON, prevRisk := statusPort, statusJSON, statusRisk
	t.Cleanup(func() {
		statusPort, statusJSON, statusRisk = prevPort, prevJSON, prevRisk
		statusCmd.SetOut(nil)
		statusCmd.SetErr(nil)
		statusCmd.SetContext(nil)
	})
	statusPort = 0
	statusJSON = false
	statusRisk = true

	out := runStatusRisk(t)
	if !strings.Contains(out, "[Risk Telemetry]") || !strings.Contains(out, "Posture:") {
		t.Errorf("expected the risk posture block for an empty pool:\n%s", out)
	}
	if strings.Contains(out, "ACCOUNT\t") {
		t.Errorf("empty pool printed an account table:\n%s", out)
	}

	riskPath, err := config.RiskFilePath()
	if err != nil {
		t.Fatalf("resolving risk path: %v", err)
	}
	document, err := risk.NewStore(riskPath).Load()
	if err != nil {
		t.Fatalf("loading risk telemetry: %v", err)
	}
	if len(document.Snapshots) != 1 {
		t.Fatalf("snapshots = %d; want 1", len(document.Snapshots))
	}
	if len(document.Snapshots[0].Accounts) != 0 {
		t.Errorf("accounts = %d; want none for an empty pool", len(document.Snapshots[0].Accounts))
	}
}

// TestStatusRuntimeTableLabelsSuspension verifies that the runtime account table reports a suspended verdict
// instead of the generic disabled label, so it agrees with the risk block printed below it.
func TestStatusRuntimeTableLabelsSuspension(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	cfg := config.DefaultConfig()
	cfg.APIKey = config.APIKeyPrefix + strings.Repeat("a", config.APIKeyHexLen)
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(server.HealthResponse{OK: true, Name: server.ServerName, Version: config.Version, Accounts: 0})
		case "/v1/accounts":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(server.AccountsResponse{
				Strategy: "sticky",
				Accounts: []rotator.AccountSnapshot{{
					Email:       "flagged@example.com",
					Disabled:    true,
					SuspendKind: "tos_violation",
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer daemon.Close()

	parsed, err := url.Parse(daemon.URL)
	if err != nil {
		t.Fatalf("parsing fake daemon URL: %v", err)
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		t.Fatalf("parsing fake daemon port: %v", err)
	}

	prevPort, prevJSON, prevRisk := statusPort, statusJSON, statusRisk
	t.Cleanup(func() {
		statusPort, statusJSON, statusRisk = prevPort, prevJSON, prevRisk
		statusCmd.SetOut(nil)
		statusCmd.SetErr(nil)
		statusCmd.SetContext(nil)
	})
	statusPort = port
	statusJSON = false
	statusRisk = false

	buf := new(bytes.Buffer)
	statusCmd.SetOut(buf)
	statusCmd.SetErr(buf)
	statusCmd.SetContext(t.Context())
	if err := statusCmd.RunE(statusCmd, nil); err != nil {
		t.Fatalf("statusCmd.RunE failed: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "Suspended (tos_violation)") {
		t.Errorf("runtime table did not label the suspension:\n%s", out)
	}
	// The local runtime table deliberately keeps the operator's own full address; only the persisted risk
	// block masks it.
}
