//go:build windows

// Package updater handles automated release discovery, cryptographic verification,
// archive extraction, and atomic in-place binary upgrades for Antigravity Gateway.
package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ApplyBinary replaces the executable at dstPath on Windows.
// Windows locks running executables, preventing direct overwrite; it allows renaming
// the currently running file to .old, writing the new binary into dstPath, and leaving
// the .old file to be cleaned up on the next run or reboot.
func ApplyBinary(dstPath string, newBinaryBytes []byte) error {
	dstPath, err := filepath.Abs(dstPath)
	if err != nil {
		return fmt.Errorf("resolving absolute path for %s: %w", dstPath, err)
	}

	dir := filepath.Dir(dstPath)
	oldPath := filepath.Join(dir, fmt.Sprintf("%s.old.%d", filepath.Base(dstPath), time.Now().UnixNano()))
	tmpPath := filepath.Join(dir, fmt.Sprintf("%s.tmp.%d", filepath.Base(dstPath), time.Now().UnixNano()))

	// Clean up any historical .old files in the directory best-effort
	if matches, err := filepath.Glob(filepath.Join(dir, "*.old.*")); err == nil {
		for _, m := range matches {
			_ = os.Remove(m)
		}
	}

	// Step 1: Write replacement bytes to a temp file in the same directory
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("creating temporary binary file %s: %w", tmpPath, err)
	}

	if _, err := f.Write(newBinaryBytes); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing replacement binary: %w", err)
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("syncing replacement binary: %w", err)
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing replacement binary: %w", err)
	}

	// Step 2: Move active executable to .old
	if err := os.Rename(dstPath, oldPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("moving active binary to %s: %w", oldPath, err)
	}

	// Step 3: Move new binary into destination path
	if err := os.Rename(tmpPath, dstPath); err != nil {
		// Roll back active binary if replacement rename fails
		_ = os.Rename(oldPath, dstPath)
		_ = os.Remove(tmpPath)
		return fmt.Errorf("placing new binary at %s: %w", dstPath, err)
	}

	// Step 4: Attempt immediate cleanup of .old file (best-effort)
	_ = os.Remove(oldPath)

	return nil
}
