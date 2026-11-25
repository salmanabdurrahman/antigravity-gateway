//go:build !windows

// Package updater handles automated release discovery, cryptographic verification,
// archive extraction, and atomic in-place binary upgrades for Antigravity Gateway.
package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ApplyBinary atomically replaces the executable at dstPath with newBinaryBytes on POSIX systems.
// It creates a temp file in the same directory, syncs to disk, sets 0755 executable permissions,
// and atomically renames it over dstPath.
func ApplyBinary(dstPath string, newBinaryBytes []byte) error {
	dstPath, err := filepath.Abs(dstPath)
	if err != nil {
		return fmt.Errorf("resolving absolute path for %s: %w", dstPath, err)
	}

	dir := filepath.Dir(dstPath)
	tmpPath := filepath.Join(dir, fmt.Sprintf(".agw.new.%d", time.Now().UnixNano()))

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

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("setting executable permissions on %s: %w", tmpPath, err)
	}

	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomically renaming %s to %s: %w", tmpPath, dstPath, err)
	}

	return nil
}
