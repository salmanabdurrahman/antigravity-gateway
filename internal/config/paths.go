// Package config provides application settings, filesystem paths, and upstream HTTP header builders.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// DefaultDirName defines the default configuration directory under the user's home directory.
	DefaultDirName = ".agw"

	// DirPerm defines the POSIX file permissions for the configuration directory (owner read/write/execute).
	DirPerm os.FileMode = 0o700

	// FilePerm defines the POSIX file permissions for sensitive configuration and credential files (owner read/write).
	FilePerm os.FileMode = 0o600

	// EnvHome defines the environment variable used to override the configuration root directory.
	EnvHome = "AGW_HOME"
)

// Dir resolves and returns the root configuration directory path.
// It prioritizes the AGW_HOME environment variable, falling back to ~/.agw.
func Dir() (string, error) {
	if custom := os.Getenv(EnvHome); custom != "" {
		return filepath.Clean(custom), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving user home directory: %w", err)
	}

	return filepath.Join(home, DefaultDirName), nil
}

// EnsureDir verifies that the configuration root directory exists, creating it with 0700 permissions if absent.
func EnsureDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}

	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if createErr := os.MkdirAll(dir, DirPerm); createErr != nil {
				return "", fmt.Errorf("creating configuration directory %s: %w", dir, createErr)
			}
			return dir, nil
		}
		return "", fmt.Errorf("inspecting configuration directory %s: %w", dir, err)
	}

	if !info.IsDir() {
		return "", fmt.Errorf("configuration path %s exists but is not a directory", dir)
	}

	return dir, nil
}

// ConfigFilePath returns the absolute path to the main daemon configuration file (config.json).
func ConfigFilePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// AccountsFilePath returns the absolute path to the credential storage file (accounts.json).
func AccountsFilePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "accounts.json"), nil
}

// ModelsCacheFilePath returns the absolute path to the cached catalog file (models-cache.json).
func ModelsCacheFilePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "models-cache.json"), nil
}

// RiskFilePath returns the absolute path to the local risk telemetry file (risk.json).
func RiskFilePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "risk.json"), nil
}

// PIDFilePath returns the absolute path to the background daemon process ID file (agw.pid).
func PIDFilePath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "agw.pid"), nil
}

// ResolvePath returns an absolute path for a filename located within the configuration directory.
func ResolvePath(filename string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, filename), nil
}
