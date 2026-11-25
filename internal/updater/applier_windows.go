//go:build windows

// Package updater handles automated release discovery, cryptographic verification,
// archive extraction, and atomic in-place binary upgrades for Antigravity Gateway.
package updater

import (
	"fmt"
	"os"
	"path/filepath"
)

// ApplyBinary replaces the executable at dstPath on Windows.
// Windows locks running executables, preventing direct overwrite; it allows renaming the currently
// running file to a .old sibling, writing the new binary into dstPath, and leaving the .old file to be
// cleaned up on the next run or reboot. Every staged name comes from os.CreateTemp (O_EXCL) so a planted
// file or symlink in a shared binary directory is never written through.
func ApplyBinary(dstPath string, newBinaryBytes []byte) error {
	dstPath, err := filepath.Abs(dstPath)
	if err != nil {
		return fmt.Errorf("resolving absolute path for %s: %w", dstPath, err)
	}

	dir := filepath.Dir(dstPath)
	baseName := filepath.Base(dstPath)

	// Clean up historical rollback files for this binary only. A shared directory may hold unrelated
	// `*.old.*` entries that must never be deleted by an agw upgrade.
	if matches, err := filepath.Glob(filepath.Join(dir, baseName+".old.*")); err == nil {
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}

	// Reserve the rollback name with os.CreateTemp so it cannot collide with an attacker-created file.
	// The reserved placeholder is removed because Windows refuses to rename onto an existing target.
	oldFile, err := os.CreateTemp(dir, baseName+".old.*")
	if err != nil {
		return fmt.Errorf("reserving rollback name for %s: %w", dstPath, err)
	}
	oldPath := oldFile.Name()
	if err := oldFile.Close(); err != nil {
		_ = os.Remove(oldPath)
		return fmt.Errorf("closing rollback placeholder %s: %w", oldPath, err)
	}
	if err := os.Remove(oldPath); err != nil {
		return fmt.Errorf("releasing rollback placeholder %s: %w", oldPath, err)
	}

	// Step 1: Write replacement bytes to an O_EXCL temp file in the same directory.
	tmpFile, err := os.CreateTemp(dir, baseName+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temporary binary file in %s: %w", dir, err)
	}
	tmpPath := tmpFile.Name()

	if _, err := tmpFile.Write(newBinaryBytes); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing replacement binary: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("syncing replacement binary: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing replacement binary: %w", err)
	}

	// Step 2: Move active executable to the reserved .old name.
	if err := os.Rename(dstPath, oldPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("moving active binary to %s: %w", oldPath, err)
	}

	// Step 3: Move new binary into destination path.
	if err := os.Rename(tmpPath, dstPath); err != nil {
		// Roll back active binary if replacement rename fails.
		_ = os.Rename(oldPath, dstPath)
		_ = os.Remove(tmpPath)
		return fmt.Errorf("placing new binary at %s: %w", dstPath, err)
	}

	// Step 4: Attempt immediate cleanup of the rollback file (best-effort).
	_ = os.Remove(oldPath)

	return nil
}
