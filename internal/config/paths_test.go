// Package config provides application settings, filesystem paths, and upstream HTTP header builders.
package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDirDefault verifies that the default configuration directory resolves to ~/.agw.
func TestDirDefault(t *testing.T) {
	t.Setenv(EnvHome, "")

	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir() returned unexpected error: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir() returned error: %v", err)
	}

	expected := filepath.Join(home, DefaultDirName)
	if dir != expected {
		t.Errorf("Dir() = %q, expected %q", dir, expected)
	}
}

// TestDirCustomEnv verifies that setting the AGW_HOME environment variable overrides the default config directory.
func TestDirCustomEnv(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(EnvHome, tempDir)

	dir, err := Dir()
	if err != nil {
		t.Fatalf("Dir() with AGW_HOME returned error: %v", err)
	}

	if dir != filepath.Clean(tempDir) {
		t.Errorf("Dir() = %q, expected %q", dir, filepath.Clean(tempDir))
	}
}

// TestEnsureDir verifies that EnsureDir creates missing directories with strict POSIX 0700 permissions.
func TestEnsureDir(t *testing.T) {
	tempDir := filepath.Join(t.TempDir(), "nested", "agw")
	t.Setenv(EnvHome, tempDir)

	created, err := EnsureDir()
	if err != nil {
		t.Fatalf("EnsureDir() failed: %v", err)
	}

	if created != tempDir {
		t.Errorf("EnsureDir() returned %q, expected %q", created, tempDir)
	}

	info, err := os.Stat(tempDir)
	if err != nil {
		t.Fatalf("os.Stat() failed: %v", err)
	}

	if !info.IsDir() {
		t.Errorf("expected %q to be a directory", tempDir)
	}

	// Verify POSIX permissions (0700)
	perm := info.Mode().Perm()
	if perm != DirPerm {
		t.Errorf("expected permissions %o, got %o", DirPerm, perm)
	}
}

// TestPathsHelpers verifies that path helper functions generate consistent absolute file paths under the root directory.
func TestPathsHelpers(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv(EnvHome, tempDir)

	cfgPath, err := ConfigFilePath()
	if err != nil {
		t.Fatalf("ConfigFilePath() error: %v", err)
	}
	if cfgPath != filepath.Join(tempDir, "config.json") {
		t.Errorf("ConfigFilePath() = %q, expected %q", cfgPath, filepath.Join(tempDir, "config.json"))
	}

	accPath, err := AccountsFilePath()
	if err != nil {
		t.Fatalf("AccountsFilePath() error: %v", err)
	}
	if accPath != filepath.Join(tempDir, "accounts.json") {
		t.Errorf("AccountsFilePath() = %q, expected %q", accPath, filepath.Join(tempDir, "accounts.json"))
	}

	cachePath, err := ModelsCacheFilePath()
	if err != nil {
		t.Fatalf("ModelsCacheFilePath() error: %v", err)
	}
	if cachePath != filepath.Join(tempDir, "models-cache.json") {
		t.Errorf("ModelsCacheFilePath() = %q, expected %q", cachePath, filepath.Join(tempDir, "models-cache.json"))
	}

	pidPath, err := PIDFilePath()
	if err != nil {
		t.Fatalf("PIDFilePath() error: %v", err)
	}
	if pidPath != filepath.Join(tempDir, "agw.pid") {
		t.Errorf("PIDFilePath() = %q, expected %q", pidPath, filepath.Join(tempDir, "agw.pid"))
	}

	resolved, err := ResolvePath("custom.txt")
	if err != nil {
		t.Fatalf("ResolvePath() error: %v", err)
	}
	if resolved != filepath.Join(tempDir, "custom.txt") {
		t.Errorf("ResolvePath() = %q, expected %q", resolved, filepath.Join(tempDir, "custom.txt"))
	}
}
