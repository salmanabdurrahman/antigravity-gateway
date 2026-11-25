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
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestAccountsCommandEmpty verifies that agw accounts outputs instructions when no accounts are stored.
func TestAccountsCommandEmpty(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	buf := new(bytes.Buffer)
	accountsCmd.SetOut(buf)
	accountsCmd.SetErr(buf)

	if err := accountsCmd.RunE(accountsCmd, []string{}); err != nil {
		t.Fatalf("accountsCmd.RunE failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "No accounts configured yet") {
		t.Errorf("expected empty state message, got %s", out)
	}
}

// TestAccountsAndLogoutCommands verifies listing stored accounts without secret leakage and removing an account via logout.
func TestAccountsAndLogoutCommands(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	accPath := filepath.Join(tempHome, "accounts.json")
	store := auth.NewStore(accPath)

	_, _ = store.Upsert(&auth.Account{
		Email:        "alpha@example.com",
		RefreshToken: "refresh-secret-alpha",
		ProjectID:    "project-alpha",
		AddedAt:      "2026-01-01T12:00:00Z",
	})
	_, _ = store.Upsert(&auth.Account{
		Email:        "beta@example.com",
		RefreshToken: "refresh-secret-beta",
		ProjectID:    "project-beta",
		Disabled:     true,
		LastError:    "invalid_grant",
	})

	// Test accounts command output
	buf := new(bytes.Buffer)
	accountsCmd.SetOut(buf)
	accountsCmd.SetErr(buf)

	if err := accountsCmd.RunE(accountsCmd, []string{}); err != nil {
		t.Fatalf("accountsCmd.RunE failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "alpha@example.com") || !strings.Contains(out, "project-alpha") {
		t.Errorf("expected alpha account listed, got %s", out)
	}
	if !strings.Contains(out, "beta@example.com") || !strings.Contains(out, "Disabled (invalid_grant)") {
		t.Errorf("expected beta disabled status listed, got %s", out)
	}
	// Verify zero token leakage
	if strings.Contains(out, "refresh-secret-alpha") || strings.Contains(out, "refresh-secret-beta") {
		t.Errorf("SECURITY LEAK: output contains raw refresh tokens: %s", out)
	}

	// Test logout command
	logoutBuf := new(bytes.Buffer)
	logoutCmd.SetOut(logoutBuf)
	logoutCmd.SetErr(logoutBuf)

	if err := logoutCmd.RunE(logoutCmd, []string{"alpha@example.com"}); err != nil {
		t.Fatalf("logoutCmd.RunE failed: %v", err)
	}

	logoutOut := logoutBuf.String()
	if !strings.Contains(logoutOut, "Successfully removed account: alpha@example.com") {
		t.Errorf("expected successful removal message, got %s", logoutOut)
	}

	// Verify alpha was removed
	accountsAfter, _ := store.Load()
	if len(accountsAfter) != 1 || accountsAfter[0].Email != "beta@example.com" {
		t.Errorf("expected only beta to remain, got %+v", accountsAfter)
	}

	// Test logout non-existent account
	logoutBuf.Reset()
	if err := logoutCmd.RunE(logoutCmd, []string{"nonexistent@example.com"}); err != nil {
		t.Fatalf("logoutCmd nonexistent failed: %v", err)
	}
	if !strings.Contains(logoutBuf.String(), "Account not found: nonexistent@example.com") {
		t.Errorf("expected not found message, got %s", logoutBuf.String())
	}
}

// TestLoginLocalCommand verifies importing local desktop session via agw login --local.
func TestLoginLocalCommand(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	dbPath := filepath.Join(tempHome, "state.vscdb")
	t.Setenv("ANTIGRAVITY_DB_PATH", dbPath)

	jsonPayload := `{"apiKey":"desktop-token-123","email":"desktop@example.com","name":"Desktop User"}`
	data := append([]byte("prefix_antigravityAuthStatus_pad"), []byte(jsonPayload)...)
	if err := os.WriteFile(dbPath, data, 0o600); err != nil {
		t.Fatalf("writing mock db: %v", err)
	}

	buf := new(bytes.Buffer)
	loginCmd.SetOut(buf)
	loginCmd.SetErr(buf)

	localImport = true
	defer func() { localImport = false }()

	if err := loginCmd.RunE(loginCmd, []string{}); err != nil {
		t.Fatalf("login --local failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Successfully imported local Antigravity account: desktop@example.com") {
		t.Errorf("unexpected output: %s", out)
	}

	// Verify account saved in accounts.json
	accPath, _ := config.AccountsFilePath()
	store := auth.NewStore(accPath)
	accounts, err := store.Load()
	if err != nil || len(accounts) != 1 {
		t.Fatalf("expected 1 imported account, got %d (err: %v)", len(accounts), err)
	}
	if accounts[0].Email != "desktop@example.com" {
		t.Errorf("expected desktop@example.com, got %s", accounts[0].Email)
	}
}

// TestAccountsCheckDoesNotReEnableWithoutOptIn verifies a live --check inspection leaves the disabled
// flag untouched unless the explicit --re-enable opt-in is supplied.
func TestAccountsCheckDoesNotReEnableWithoutOptIn(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	accPath := filepath.Join(tempHome, "accounts.json")
	store := auth.NewStore(accPath)
	if _, err := store.Upsert(&auth.Account{
		Email:        "disabled@example.com",
		RefreshToken: "refresh-secret",
		ProjectID:    "project-disabled",
		Disabled:     true,
		LastError:    "violation of terms of service",
	}); err != nil {
		t.Fatalf("seeding account: %v", err)
	}

	prevQuota, prevReenable := checkAccountsQuota, checkAccountsReenable
	t.Cleanup(func() {
		checkAccountsQuota, checkAccountsReenable = prevQuota, prevReenable
	})

	// Without --check the command only lists accounts, so no account state may change.
	checkAccountsQuota = false
	checkAccountsReenable = false

	buf := new(bytes.Buffer)
	accountsCmd.SetOut(buf)
	accountsCmd.SetErr(buf)
	if err := accountsCmd.RunE(accountsCmd, []string{}); err != nil {
		t.Fatalf("accountsCmd.RunE failed: %v", err)
	}

	accounts, err := store.Load()
	if err != nil {
		t.Fatalf("loading accounts: %v", err)
	}
	if len(accounts) != 1 || !accounts[0].Disabled {
		t.Fatalf("expected the disabled account to stay disabled, got %+v", accounts)
	}
	if !strings.Contains(buf.String(), "Disabled") {
		t.Errorf("expected disabled status in output, got %s", buf.String())
	}
}

// TestAccountsCommandDefaultTierSourceAndJSON verifies that default agw accounts surfaces tier and
// credential source columns and produces secret-free JSON when --json is passed.
func TestAccountsCommandDefaultTierSourceAndJSON(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	accPath := filepath.Join(tempHome, "accounts.json")
	store := auth.NewStore(accPath)
	_, _ = store.Upsert(&auth.Account{
		Email:        "oauth@example.com",
		RefreshToken: "refresh-secret-oauth",
		ProjectID:    "proj-oauth",
		Tier:         config.TierPersonal,
		AddedAt:      "2026-11-20T10:00:00Z",
	})
	_, _ = store.Upsert(&auth.Account{
		Email:            "desktop@example.com",
		AccessToken:      "access-secret-desktop",
		CredentialSource: auth.CredentialSourceLocalDesktop,
		ProjectID:        "proj-desktop",
		Tier:             config.TierBusiness,
		AddedAt:          "2026-11-21T10:00:00Z",
	})

	prevQuota, prevReenable, prevJSON := checkAccountsQuota, checkAccountsReenable, accountsJSON
	t.Cleanup(func() {
		checkAccountsQuota, checkAccountsReenable, accountsJSON = prevQuota, prevReenable, prevJSON
		accountsCmd.SetContext(nil)
	})
	checkAccountsQuota = false
	checkAccountsReenable = false
	accountsJSON = false

	buf := new(bytes.Buffer)
	accountsCmd.SetOut(buf)
	accountsCmd.SetErr(buf)
	accountsCmd.SetContext(t.Context())
	if err := accountsCmd.RunE(accountsCmd, []string{}); err != nil {
		t.Fatalf("accountsCmd.RunE table failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"TIER", "SOURCE", "personal", "business", "oauth", "local_desktop"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in table output:\n%s", want, out)
		}
	}

	accountsJSON = true
	buf.Reset()
	if err := accountsCmd.RunE(accountsCmd, []string{}); err != nil {
		t.Fatalf("accountsCmd.RunE --json failed: %v", err)
	}
	jsonOut := buf.String()
	if strings.Contains(jsonOut, "refresh-secret-oauth") || strings.Contains(jsonOut, "access-secret-desktop") {
		t.Fatalf("SECURITY LEAK: JSON output contains raw credentials: %s", jsonOut)
	}
	if !strings.Contains(jsonOut, `"source": "local_desktop"`) || !strings.Contains(jsonOut, `"tier": "business"`) {
		t.Errorf("expected structured source and tier in JSON output, got:\n%s", jsonOut)
	}
}

// TestAccountsCommandCheckWithQuotaWindowsAndJSON verifies parallel --check execution across
// multiple accounts, 5-hour/weekly quota windows, deterministic ordering, and --json serialization.
func TestAccountsCommandCheckWithQuotaWindowsAndJSON(t *testing.T) {
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
					"gemini-4-hydrogen-high": map[string]any{
						"quotaInfo": map[string]any{"remainingFraction": 0.45},
					},
					"claude-sonnet-4-6": map[string]any{
						"quotaInfo": map[string]any{"remainingFraction": 0.90},
					},
				},
			})
		case "/v1internal:retrieveUserQuotaSummary":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"fiveHour":        map[string]any{"remainingFraction": 0.30, "resetTime": "2099-01-01T12:00:00Z"},
				"weeklyQuotaInfo": map[string]any{"remainingFraction": 0.75, "resetTime": "2099-01-05T12:00:00Z"},
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
		Email:        "first@example.com",
		RefreshToken: "refresh-secret-first",
		ProjectID:    "proj-1",
		Tier:         config.TierPersonal,
	})
	_, _ = store.Upsert(&auth.Account{
		Email:        "second@example.com",
		RefreshToken: "refresh-secret-second",
		ProjectID:    "proj-2",
		Tier:         config.TierEnterprise,
	})

	prevQuota, prevReenable, prevJSON := checkAccountsQuota, checkAccountsReenable, accountsJSON
	t.Cleanup(func() {
		checkAccountsQuota, checkAccountsReenable, accountsJSON = prevQuota, prevReenable, prevJSON
		accountsCmd.SetContext(nil)
	})
	checkAccountsQuota = true
	checkAccountsReenable = false
	accountsJSON = false

	buf := new(bytes.Buffer)
	accountsCmd.SetOut(buf)
	accountsCmd.SetErr(buf)
	accountsCmd.SetContext(t.Context())
	if err := accountsCmd.RunE(accountsCmd, []string{}); err != nil {
		t.Fatalf("accounts --check failed: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"5H QUOTA", "WEEKLY QUOTA", "GEMINI QUOTA", "CLAUDE QUOTA", "30%", "75%", "45%", "90%"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in check output:\n%s", want, out)
		}
	}
	if strings.Index(out, "first@example.com") > strings.Index(out, "second@example.com") {
		t.Errorf("expected deterministic account ordering preserved:\n%s", out)
	}

	accountsJSON = true
	buf.Reset()
	if err := accountsCmd.RunE(accountsCmd, []string{}); err != nil {
		t.Fatalf("accounts --check --json failed: %v", err)
	}
	jsonOut := buf.String()
	if strings.Contains(jsonOut, "refresh-secret") || strings.Contains(jsonOut, "mock-access-token") {
		t.Fatalf("SECURITY LEAK: accounts --check --json leaked token: %s", jsonOut)
	}
	if !strings.Contains(jsonOut, `"five_hour_quota": "30%"`) || !strings.Contains(jsonOut, `"weekly_quota": "75%"`) {
		t.Errorf("expected 5h and weekly fields in JSON output:\n%s", jsonOut)
	}
}

// TestAccountsCommandCheckDegrades404AndMarksInvalidGrant verifies graceful degradation when
// retrieveUserQuotaSummary returns HTTP 404 and automatic disabling when /token reports invalid_grant.
func TestAccountsCommandCheckDegrades404AndMarksInvalidGrant(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err == nil && r.Form.Get("refresh_token") == "revoked-token" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error":             "invalid_grant",
					"error_description": "Token has been expired or revoked.",
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-access-token",
				"expires_in":   3600,
			})
		case "/v1internal:fetchAvailableModels":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": map[string]any{
					"gemini-4-hydrogen-high": map[string]any{
						"quotaInfo": map[string]any{"remainingFraction": 0.75},
					},
				},
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
		Email:        "ok@example.com",
		RefreshToken: "valid-token",
		ProjectID:    "proj-ok",
	})
	_, _ = store.Upsert(&auth.Account{
		Email:        "revoked@example.com",
		RefreshToken: "revoked-token",
		ProjectID:    "proj-revoked",
	})

	prevQuota, prevReenable, prevJSON := checkAccountsQuota, checkAccountsReenable, accountsJSON
	t.Cleanup(func() {
		checkAccountsQuota, checkAccountsReenable, accountsJSON = prevQuota, prevReenable, prevJSON
		accountsCmd.SetContext(nil)
	})
	checkAccountsQuota = true
	checkAccountsReenable = false
	accountsJSON = false

	buf := new(bytes.Buffer)
	accountsCmd.SetOut(buf)
	accountsCmd.SetErr(buf)
	accountsCmd.SetContext(t.Context())

	if err := accountsCmd.RunE(accountsCmd, []string{}); err != nil {
		t.Fatalf("accounts --check failed: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "ok@example.com") || !strings.Contains(out, "75%") {
		t.Errorf("expected ok account with 75%% quota, got:\n%s", out)
	}
	if !strings.Contains(out, "Disabled (refresh token revoked)") {
		t.Errorf("expected revoked account marked disabled in output:\n%s", out)
	}

	accountsAfter, err := store.Load()
	if err != nil {
		t.Fatalf("reloading accounts store: %v", err)
	}
	for _, a := range accountsAfter {
		if a.Email == "revoked@example.com" && !a.Disabled {
			t.Errorf("expected revoked account persisted as disabled in accounts.json")
		}
	}
}
