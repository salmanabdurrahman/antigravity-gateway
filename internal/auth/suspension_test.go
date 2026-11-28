// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStoreSetSuspendedPersistsVerdict verifies that a terminal suspension verdict removes the account
// from rotation and stores the upstream kind, reason, timestamp, and appeal link for later inspection.
func TestStoreSetSuspendedPersistsVerdict(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "accounts.json"))
	account := &Account{
		Email:        "suspended@example.com",
		RefreshToken: "refresh-suspended",
		ProjectID:    "project-suspended",
		AddedAt:      "2026-11-01T00:00:00Z",
	}
	if _, err := store.Upsert(account); err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}

	if err := store.SetSuspended(account.Email, "tos_violation", "This service has been disabled in this account.", "https://forms.gle/abc"); err != nil {
		t.Fatalf("SetSuspended failed: %v", err)
	}

	accounts, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}

	stored := accounts[0]
	if !stored.Disabled {
		t.Error("suspended account is not disabled")
	}
	if !stored.Suspended() {
		t.Error("Suspended() = false; want true for a suspended account")
	}
	if stored.SuspendKind != "tos_violation" {
		t.Errorf("SuspendKind = %q; want tos_violation", stored.SuspendKind)
	}
	if stored.SuspendReason == "" || stored.LastError == "" {
		t.Errorf("suspension reason not recorded: suspend_reason=%q last_error=%q", stored.SuspendReason, stored.LastError)
	}
	if stored.AppealURL != "https://forms.gle/abc" {
		t.Errorf("AppealURL = %q; want the validated appeal link", stored.AppealURL)
	}
	if stored.SuspendedAt == "" {
		t.Error("SuspendedAt is empty; want an RFC 3339 timestamp")
	}
}

// TestStoreReEnableClearsSuspension verifies that the operator re-enable path clears suspension metadata,
// so a verdict that Google lifted cannot keep showing as current state.
func TestStoreReEnableClearsSuspension(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "accounts.json"))
	account := &Account{
		Email:        "appealed@example.com",
		RefreshToken: "refresh-appealed",
		ProjectID:    "project-appealed",
		AddedAt:      "2026-11-01T00:00:00Z",
	}
	if _, err := store.Upsert(account); err != nil {
		t.Fatalf("Upsert failed: %v", err)
	}
	if err := store.SetSuspended(account.Email, "tos_violation", "disabled for a terms violation", "https://forms.gle/abc"); err != nil {
		t.Fatalf("SetSuspended failed: %v", err)
	}

	if err := store.SetDisabled(account.Email, false, ""); err != nil {
		t.Fatalf("SetDisabled(false) failed: %v", err)
	}

	accounts, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	stored := accounts[0]
	if stored.Disabled {
		t.Error("account is still disabled after re-enable")
	}
	if stored.Suspended() {
		t.Errorf("Suspended() = true after re-enable; want false (kind=%q)", stored.SuspendKind)
	}
	if stored.SuspendReason != "" || stored.AppealURL != "" || stored.SuspendedAt != "" {
		t.Errorf("stale suspension metadata survived re-enable: %+v", stored)
	}
}

// TestStoreLoadsLegacyAccountWithoutSuspensionFields verifies backward compatibility: accounts written
// before suspension metadata existed still load cleanly and report themselves as not suspended.
func TestStoreLoadsLegacyAccountWithoutSuspensionFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	legacy := `{"version":1,"accounts":[{"email":"legacy@example.com","refresh_token":"refresh-legacy","project_id":"project-legacy","added_at":"2026-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("writing legacy accounts file: %v", err)
	}

	accounts, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load of legacy accounts file failed: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 legacy account, got %d", len(accounts))
	}
	if accounts[0].Suspended() {
		t.Error("legacy account reports Suspended() = true; want false")
	}
	if accounts[0].SuspendedAt != "" || accounts[0].SuspendKind != "" || accounts[0].SuspendReason != "" || accounts[0].AppealURL != "" {
		t.Errorf("legacy account carries unexpected suspension metadata: %+v", accounts[0])
	}
}

// TestLegacySuspensionRecordIsRecognized verifies that a disabled record written before structured verdicts
// existed is still treated as a suspension, without rewriting the stored file.
func TestLegacySuspensionRecordIsRecognized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	legacy := `{"version":1,"accounts":[{"email":"legacy@example.com","refresh_token":"rt-legacy","project_id":"p-legacy","added_at":"2026-01-01T00:00:00Z","disabled":true,"last_error":"HTTP 403 permanent suspension: This service has been disabled in this account for violation of Terms of Service."}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("writing legacy accounts file: %v", err)
	}

	accounts, err := NewStore(path).Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if !accounts[0].Suspended() {
		t.Error("legacy suspension record is not recognized as suspended")
	}
	if got := accounts[0].SuspensionKind(); got != "tos_violation" {
		t.Errorf("SuspensionKind() = %q; want the derived legacy kind", got)
	}

	// The file is read-only from the store's perspective: no migration may rewrite it on load.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-reading accounts file: %v", err)
	}
	if string(raw) != legacy {
		t.Error("load rewrote the legacy accounts file; derived state must stay in memory")
	}
}

// TestLegacyMarkerOnEnabledAccountIsNotSuspension verifies that re-enabling an account clears the derived
// suspension state even when the old message is still stored.
func TestLegacyMarkerOnEnabledAccountIsNotSuspension(t *testing.T) {
	account := &Account{Email: "active@example.com", Disabled: false, LastError: "HTTP 403 permanent suspension: disabled"}
	if account.Suspended() {
		t.Error("enabled account must not be classified as suspended from a stale message")
	}
	if got := account.SuspensionKind(); got != "" {
		t.Errorf("SuspensionKind() = %q; want empty", got)
	}
}

// TestLegacyValidationRequiredKind verifies the derived kind follows the recorded message, because the old
// rotator wrote every permanent 403 under one prefix.
func TestLegacyValidationRequiredKind(t *testing.T) {
	account := &Account{
		Email:     "verify@example.com",
		Disabled:  true,
		LastError: "HTTP 403 permanent suspension: validation_required — verify your account",
	}
	if !account.Suspended() {
		t.Fatal("legacy validation record is not recognized as suspended")
	}
	if got := account.SuspensionKind(); got != "validation_required" {
		t.Errorf("SuspensionKind() = %q; want validation_required", got)
	}
}

// TestLegacyMarkerRequiresSuspensionWording verifies that a disabled account for an unrelated reason is not
// mislabelled as suspended just because its message mentions a disabled resource.
func TestLegacyMarkerRequiresSuspensionWording(t *testing.T) {
	account := &Account{Email: "billing@example.com", Disabled: true, LastError: "project has been disabled for billing review"}
	if account.Suspended() {
		t.Error("unrelated disabled reason was classified as a suspension")
	}
	if got := account.SuspensionKind(); got != "" {
		t.Errorf("SuspensionKind() = %q; want empty", got)
	}
}
