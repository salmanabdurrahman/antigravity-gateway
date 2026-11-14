// Package client provides automated configuration injectors and diagnostic tools
// for local AI coding agents such as Pi and OpenCode.
package client

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPrettyModelName verifies formatting raw model IDs into clean human-readable names.
func TestPrettyModelName(t *testing.T) {
	cases := []struct {
		id   string
		want string
	}{
		{"gemini-3.1-pro-high", "Gemini 3.1 Pro High"},
		{"gemini-3.8-flash-medium", "Gemini 3.8 Flash Medium"},
		{"claude-sonnet-4-5", "Claude Sonnet 4.5"},
		{"claude-opus-4-5", "Claude Opus 4.5"},
		{"gpt-oss-120b-medium", "GPT OSS 120B Medium"},
	}

	for _, tc := range cases {
		got := PrettyModelName(tc.id)
		if got != tc.want {
			t.Errorf("PrettyModelName(%q) = %q; want %q", tc.id, got, tc.want)
		}
	}
}

// TestModelFilter verifies glob inclusion and exclusion matching against model IDs.
func TestModelFilter(t *testing.T) {
	mf, err := NewModelFilter("gemini-*,!*-low")
	if err != nil {
		t.Fatalf("NewModelFilter failed: %v", err)
	}

	if !mf.Matches("gemini-3.1-pro-high") {
		t.Errorf("expected gemini-3.1-pro-high to match")
	}
	if mf.Matches("gemini-3.1-pro-low") {
		t.Errorf("expected gemini-3.1-pro-low to be excluded")
	}
	if mf.Matches("claude-sonnet-4-5") {
		t.Errorf("expected claude-sonnet-4-5 to not match include filter")
	}
}

// TestAtomicWriteAndBackup verifies atomic file writes with 0600 permissions and timestamped backups.
func TestAtomicWriteAndBackup(t *testing.T) {
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "test.json")

	initialData := map[string]string{"foo": "bar"}
	if err := AtomicWriteJSON(filePath, initialData, 0o600); err != nil {
		t.Fatalf("AtomicWriteJSON failed: %v", err)
	}

	info, err := os.Stat(filePath)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("expected perm 0600, got %o", info.Mode().Perm())
	}

	backupPath, err := BackupFile(filePath, "20261114-120000")
	if err != nil {
		t.Fatalf("BackupFile failed: %v", err)
	}
	if backupPath == "" {
		t.Fatalf("expected non-empty backup path")
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Errorf("backup file %s not found: %v", backupPath, err)
	}

	// Backup non-existent file returns empty string, no error
	missing := filepath.Join(tempDir, "nonexistent.json")
	b, err := BackupFile(missing, "20261114-120000")
	if err != nil || b != "" {
		t.Errorf("expected empty string and nil error for missing file, got %q, %v", b, err)
	}
}
