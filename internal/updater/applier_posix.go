//go:build !windows

// Package updater handles automated release discovery, cryptographic verification,
// archive extraction, and atomic in-place binary upgrades for Antigravity Gateway.
package updater

import (
	"fmt"
	"os"
	"path/filepath"
)

// ApplyBinary atomically replaces the executable at dstPath with newBinaryBytes on POSIX systems.
// It stages the payload in an O_EXCL temp file in the same directory, syncs it to disk, sets 0755
// executable permissions, and atomically renames it over dstPath. Staging through os.CreateTemp keeps
// the write from following a pre-planted symlink in a shared binary directory.
func ApplyBinary(dstPath string, newBinaryBytes []byte) error {
	dstPath, err := filepath.Abs(dstPath)
	if err != nil {
		return fmt.Errorf("resolving absolute path for %s: %w", dstPath, err)
	}

	dir := filepath.Dir(dstPath)
	tmpFile, err := os.CreateTemp(dir, ".agw.new-*")
	if err != nil {
		return fmt.Errorf("creating temporary binary file in %s: %w", dir, err)
	}
	tmpPath := tmpFile.Name()

	cleanup := func() {
		_ = os.Remove(tmpPath)
	}

	if _, err := tmpFile.Write(newBinaryBytes); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return fmt.Errorf("writing replacement binary: %w", err)
	}

	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return fmt.Errorf("syncing replacement binary: %w", err)
	}

	if err := tmpFile.Chmod(0o755); err != nil {
		_ = tmpFile.Close()
		cleanup()
		return fmt.Errorf("setting executable permissions on %s: %w", tmpPath, err)
	}

	if err := tmpFile.Close(); err != nil {
		cleanup()
		return fmt.Errorf("closing replacement binary: %w", err)
	}

	if err := os.Rename(tmpPath, dstPath); err != nil {
		cleanup()
		return fmt.Errorf("atomically renaming %s to %s: %w", tmpPath, dstPath, err)
	}

	return nil
}
