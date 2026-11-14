// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestDoctorCommand verifies that agw doctor executes all diagnostic categories and renders structured output.
func TestDoctorCommand(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	// Populate accounts.json so account diagnostic is happy
	accPath := filepath.Join(tempHome, "accounts.json")
	store := auth.NewStore(accPath)
	_, _ = store.Upsert(&auth.Account{
		Email:        "doctor-test@example.com",
		RefreshToken: "refresh-token-xyz",
		ProjectID:    "companion-project-123",
		AddedAt:      "2026-01-01T00:00:00Z",
	})

	buf := new(bytes.Buffer)
	doctorCmd.SetOut(buf)
	doctorCmd.SetErr(buf)

	// runDoctor may return error if upstream google connectivity fails in offline environments,
	// but diagnostics report must always be produced.
	_ = doctorCmd.RunE(doctorCmd, []string{})

	out := buf.String()
	if !strings.Contains(out, "Running Antigravity Gateway environment diagnostics") {
		t.Errorf("expected diagnostics header in output, got: %s", out)
	}
	if !strings.Contains(out, "[Network Ports]") {
		t.Errorf("expected Network Ports section, got: %s", out)
	}
	if !strings.Contains(out, "[File System & Security]") {
		t.Errorf("expected File System & Security section, got: %s", out)
	}
	if !strings.Contains(out, "[Account Pool]") {
		t.Errorf("expected Account Pool section, got: %s", out)
	}
}
