// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// seedSuspendedAccount stores one suspended account under an isolated AGW_HOME and returns its store.
func seedSuspendedAccount(t *testing.T, email, appealURL string) *auth.Store {
	t.Helper()

	t.Setenv(config.EnvHome, t.TempDir())
	accPath, err := config.AccountsFilePath()
	if err != nil {
		t.Fatalf("resolving accounts path: %v", err)
	}
	store := auth.NewStore(accPath)
	if _, err := store.Upsert(&auth.Account{
		Email:        email,
		RefreshToken: "refresh-" + email,
		ProjectID:    "project-" + email,
		AddedAt:      "2026-11-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seeding account: %v", err)
	}
	if err := store.SetSuspended(email, "tos_violation", "This service has been disabled in this account for violation of Terms of Service.", appealURL); err != nil {
		t.Fatalf("seeding suspension: %v", err)
	}
	return store
}

// restoreAccountsFlags restores the shared agw accounts flag state after a test mutates it.
func restoreAccountsFlags(t *testing.T) {
	t.Helper()

	prevQuota, prevReenable, prevJSON := checkAccountsQuota, checkAccountsReenable, accountsJSON
	t.Cleanup(func() {
		checkAccountsQuota, checkAccountsReenable, accountsJSON = prevQuota, prevReenable, prevJSON
		accountsCmd.SetOut(nil)
		accountsCmd.SetErr(nil)
		accountsCmd.SetContext(nil)
	})
}

// runAccountsCommand executes agw accounts with the shared command instance and returns its output.
func runAccountsCommand(t *testing.T) string {
	t.Helper()

	buf := new(bytes.Buffer)
	accountsCmd.SetOut(buf)
	accountsCmd.SetErr(buf)
	accountsCmd.SetContext(t.Context())
	if err := accountsCmd.RunE(accountsCmd, nil); err != nil {
		t.Fatalf("accountsCmd.RunE failed: %v", err)
	}
	return buf.String()
}

// TestAccountsShowsSuspendedVerdict verifies that a suspended account is listed with its verdict and the
// upstream appeal link, so an operator can act without re-contacting Google.
func TestAccountsShowsSuspendedVerdict(t *testing.T) {
	restoreAccountsFlags(t)
	seedSuspendedAccount(t, "suspended@example.com", "https://forms.gle/CLICASE")
	checkAccountsQuota = false
	checkAccountsReenable = false
	accountsJSON = false

	out := runAccountsCommand(t)

	if !strings.Contains(out, "Suspended (tos_violation)") {
		t.Errorf("expected the suspension verdict in the status column, got:\n%s", out)
	}
	if !strings.Contains(out, "https://forms.gle/CLICASE") {
		t.Errorf("expected the appeal link in the report, got:\n%s", out)
	}
	if !strings.Contains(out, "Suspended accounts stay out of rotation") {
		t.Errorf("expected the suspension remediation hint, got:\n%s", out)
	}
}

// TestAccountsJSONReportsSuspension verifies that structured output keeps the suspension verdict and the
// validated appeal link for tooling, without exposing credentials.
func TestAccountsJSONReportsSuspension(t *testing.T) {
	restoreAccountsFlags(t)
	seedSuspendedAccount(t, "suspended@example.com", "https://forms.gle/CLICASE")
	checkAccountsQuota = false
	checkAccountsReenable = false
	accountsJSON = true

	out := runAccountsCommand(t)

	var payload struct {
		Accounts []accountReportEntry `json:"accounts"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("decoding accounts JSON: %v\n%s", err, out)
	}
	if len(payload.Accounts) != 1 {
		t.Fatalf("expected 1 account entry, got %d", len(payload.Accounts))
	}
	entry := payload.Accounts[0]
	if entry.Status != "Suspended (tos_violation)" {
		t.Errorf("status = %q; want Suspended (tos_violation)", entry.Status)
	}
	if entry.AppealURL != "https://forms.gle/CLICASE" {
		t.Errorf("appeal_url = %q; want the stored appeal link", entry.AppealURL)
	}
	if !entry.Disabled {
		t.Error("suspended account entry is not marked disabled")
	}
	if strings.Contains(out, "refresh-suspended@example.com") {
		t.Fatalf("SECURITY LEAK: accounts JSON exposed a refresh token:\n%s", out)
	}
}

// TestAccountsCheckSkipsSuspendedAccount verifies that a live quota inspection reports the suspension
// without probing upstream, and that clearing it stays an explicit --re-enable action.
func TestAccountsCheckSkipsSuspendedAccount(t *testing.T) {
	restoreAccountsFlags(t)
	store := seedSuspendedAccount(t, "suspended@example.com", "https://forms.gle/CLICASE")
	checkAccountsQuota = true
	checkAccountsReenable = false
	accountsJSON = false

	out := runAccountsCommand(t)
	if !strings.Contains(out, "Suspended (tos_violation)") {
		t.Errorf("expected the suspension verdict from a live check, got:\n%s", out)
	}
	if strings.Contains(out, "Error") {
		t.Errorf("suspended account was probed instead of skipped, got:\n%s", out)
	}

	// The stored verdict must survive a read-only inspection.
	accounts, err := store.Load()
	if err != nil {
		t.Fatalf("reloading accounts: %v", err)
	}
	if !accounts[0].Suspended() {
		t.Error("a read-only quota check cleared the stored suspension verdict")
	}
}

// TestAccountsReEnableClearsSuspension verifies that the explicit opt-in clears the local suspension
// verdict without sending upstream traffic, so an operator can resume after Google lifts the suspension.
func TestAccountsReEnableClearsSuspension(t *testing.T) {
	restoreAccountsFlags(t)
	store := seedSuspendedAccount(t, "suspended@example.com", "https://forms.gle/CLICASE")
	checkAccountsQuota = true
	checkAccountsReenable = true
	accountsJSON = false

	out := runAccountsCommand(t)
	if !strings.Contains(out, "suspension cleared") {
		t.Errorf("expected the re-enable confirmation, got:\n%s", out)
	}

	accounts, err := store.Load()
	if err != nil {
		t.Fatalf("reloading accounts: %v", err)
	}
	stored := accounts[0]
	if stored.Disabled || stored.Suspended() {
		t.Errorf("re-enable did not clear the suspension: disabled=%v kind=%q", stored.Disabled, stored.SuspendKind)
	}
	if stored.AppealURL != "" || stored.SuspendReason != "" {
		t.Errorf("re-enable left stale suspension metadata: %+v", stored)
	}
}
