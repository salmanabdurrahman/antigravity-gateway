// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// seedActiveAccount stores one healthy account under an isolated AGW_HOME for logout coverage.
func seedActiveAccount(t *testing.T, email string) {
	t.Helper()

	t.Setenv(config.EnvHome, t.TempDir())
	accPath, err := config.AccountsFilePath()
	if err != nil {
		t.Fatalf("resolving accounts path: %v", err)
	}
	if _, err := auth.NewStore(accPath).Upsert(&auth.Account{
		Email:        email,
		RefreshToken: "refresh-" + email,
		ProjectID:    "project-" + email,
		AddedAt:      "2026-11-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("seeding account: %v", err)
	}
}

// TestLogoutPrintsSuspensionRemediation verifies that removing a suspended account explains the upstream
// verdict, the appeal link, and the third-party access revocation that must happen before signing in
// again, since background traffic after reinstatement can escalate to a permanent suspension.
func TestLogoutPrintsSuspensionRemediation(t *testing.T) {
	restoreAccountsFlags(t)
	seedSuspendedAccount(t, "suspended@example.com", "https://forms.gle/LOGOUTCASE")

	buf := new(bytes.Buffer)
	logoutCmd.SetOut(buf)
	logoutCmd.SetErr(buf)
	logoutCmd.SetContext(t.Context())
	t.Cleanup(func() {
		logoutCmd.SetOut(nil)
		logoutCmd.SetErr(nil)
		logoutCmd.SetContext(nil)
	})

	if err := logoutCmd.RunE(logoutCmd, []string{"suspended@example.com"}); err != nil {
		t.Fatalf("logoutCmd.RunE failed: %v", err)
	}

	out := buf.String()
	wants := []string{
		"Successfully removed account: suspended@example.com",
		"suspended upstream (tos_violation)",
		"https://myaccount.google.com/permissions",
		"Appeal once: https://forms.gle/LOGOUTCASE",
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("logout output missing %q:\n%s", want, out)
		}
	}
}

// TestLogoutStaysQuietForActiveAccount verifies that removing a healthy account keeps the previous output
// unchanged, so the suspension guidance never appears for accounts that were never suspended.
func TestLogoutStaysQuietForActiveAccount(t *testing.T) {
	restoreAccountsFlags(t)
	seedActiveAccount(t, "active@example.com")

	buf := new(bytes.Buffer)
	logoutCmd.SetOut(buf)
	logoutCmd.SetErr(buf)
	logoutCmd.SetContext(t.Context())
	t.Cleanup(func() {
		logoutCmd.SetOut(nil)
		logoutCmd.SetErr(nil)
		logoutCmd.SetContext(nil)
	})

	if err := logoutCmd.RunE(logoutCmd, []string{"active@example.com"}); err != nil {
		t.Fatalf("logoutCmd.RunE failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Successfully removed account: active@example.com") {
		t.Errorf("expected the standard removal message, got:\n%s", out)
	}
	if strings.Contains(out, "myaccount.google.com/permissions") || strings.Contains(out, "Appeal once") {
		t.Errorf("active account removal printed suspension guidance:\n%s", out)
	}
}
