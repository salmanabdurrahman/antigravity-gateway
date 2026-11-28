// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/updater"
)

// seedDoctorPool stores two active accounts plus a config profile under an isolated AGW_HOME, and stubs
// the release checker so doctor output stays deterministic and offline.
func seedDoctorPool(t *testing.T, profile string) {
	t.Helper()

	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	cfg := config.DefaultConfig()
	cfg.Profile = profile
	if err := config.Save(filepath.Join(tempHome, "config.json"), cfg); err != nil {
		t.Fatalf("seeding config file: %v", err)
	}

	store := auth.NewStore(filepath.Join(tempHome, "accounts.json"))
	for _, email := range []string{"first@example.com", "second@example.com"} {
		if _, err := store.Upsert(&auth.Account{
			Email:        email,
			RefreshToken: "refresh-" + email,
			ProjectID:    "project-" + email,
			AddedAt:      "2026-11-01T00:00:00Z",
		}); err != nil {
			t.Fatalf("seeding account %s: %v", email, err)
		}
	}

	originalChecker := versionChecker
	versionChecker = func(ctx context.Context) (*updater.ReleaseInfo, error) {
		return &updater.ReleaseInfo{
			CurrentVersion:  config.Version,
			TargetVersion:   config.Version,
			UpdateAvailable: false,
		}, nil
	}
	t.Cleanup(func() { versionChecker = originalChecker })
}

// runDoctorCaptured executes the doctor command and returns its rendered report.
func runDoctorCaptured(t *testing.T) string {
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

	if err := doctorCmd.RunE(doctorCmd, nil); err != nil {
		t.Fatalf("doctorCmd.RunE failed: %v", err)
	}
	return buf.String()
}

// TestDoctorWarnsOnManualProfileWithPool verifies that doctor flags pooled rotation under the manual
// profile, because consumer plans do not support automated account rotation.
func TestDoctorWarnsOnManualProfileWithPool(t *testing.T) {
	seedDoctorPool(t, config.ProfileManual)

	out := runDoctorCaptured(t)
	if !strings.Contains(out, "manual profile rotates 2 active accounts on failure") {
		t.Errorf("expected the manual-profile rotation warning, got:\n%s", out)
	}
	if !strings.Contains(out, "set profile to 'safe'") {
		t.Errorf("expected the safe-profile hint, got:\n%s", out)
	}
}

// TestDoctorReportsIdleAccountsUnderSafeProfile verifies that doctor explains extra accounts are idle when
// the safe profile serves a single primary account.
func TestDoctorReportsIdleAccountsUnderSafeProfile(t *testing.T) {
	seedDoctorPool(t, config.ProfileSafe)

	out := runDoctorCaptured(t)
	if !strings.Contains(out, "safe profile serves 1 primary account; 1 additional account(s) stay idle") {
		t.Errorf("expected the idle-account advisory, got:\n%s", out)
	}
	if !strings.Contains(out, "rotation_mode to 'pool'") {
		t.Errorf("expected the explicit rotation-mode hint, got:\n%s", out)
	}
}
