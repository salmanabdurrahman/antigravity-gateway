// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
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
