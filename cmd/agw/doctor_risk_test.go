// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/updater"
)

// runDoctorTolerant executes doctor and returns its report even when diagnostics exit non-zero, which a
// suspended pool triggers by leaving no usable account.
func runDoctorTolerant(t *testing.T) string {
	t.Helper()

	buf := new(bytes.Buffer)
	doctorCmd.SetOut(buf)
	doctorCmd.SetErr(buf)
	doctorCmd.SetContext(t.Context())
	t.Cleanup(func() {
		doctorCmd.SetOut(nil)
		doctorCmd.SetErr(nil)
		doctorCmd.SetContext(nil)
	})

	_ = doctorCmd.RunE(doctorCmd, nil)
	return buf.String()
}

// TestDoctorReportsSuspendedAccounts verifies that doctor surfaces suspended credentials with their appeal
// link, since they stay out of rotation until the operator appeals and clears them locally.
func TestDoctorReportsSuspendedAccounts(t *testing.T) {
	seedSuspendedAccount(t, "suspended@example.com", "https://forms.gle/DOCRISK")
	seedActiveAccountInCurrentHome(t, "active@example.com")

	originalChecker := versionChecker
	versionChecker = func(ctx context.Context) (*updater.ReleaseInfo, error) {
		return &updater.ReleaseInfo{
			CurrentVersion:  config.Version,
			TargetVersion:   config.Version,
			UpdateAvailable: false,
		}, nil
	}
	t.Cleanup(func() { versionChecker = originalChecker })

	out := runDoctorTolerant(t)

	for _, want := range []string{
		"Suspended Accounts",
		"1 suspended: s***@example.com (tos_violation)",
		"Appeal once: https://forms.gle/DOCRISK",
		"1 active account(s) ready (1 suspended)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "suspended@example.com") {
		t.Errorf("doctor output leaked the full email address:\n%s", out)
	}
}

// TestDoctorStaysQuietWithoutSuspensions verifies that the suspension advisory only appears when a verdict
// exists, so a healthy pool keeps a short report.
func TestDoctorStaysQuietWithoutSuspensions(t *testing.T) {
	seedActiveAccount(t, "active@example.com")

	originalChecker := versionChecker
	versionChecker = func(ctx context.Context) (*updater.ReleaseInfo, error) {
		return &updater.ReleaseInfo{
			CurrentVersion:  config.Version,
			TargetVersion:   config.Version,
			UpdateAvailable: false,
		}, nil
	}
	t.Cleanup(func() { versionChecker = originalChecker })

	out := runDoctorTolerant(t)
	if strings.Contains(out, "Suspended Accounts") {
		t.Errorf("doctor reported a suspension advisory for a healthy pool:\n%s", out)
	}
}

// seedActiveAccountInCurrentHome stores one healthy account inside the AGW_HOME the caller already isolated.
func seedActiveAccountInCurrentHome(t *testing.T, email string) {
	t.Helper()

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
		t.Fatalf("seeding active account: %v", err)
	}
}
