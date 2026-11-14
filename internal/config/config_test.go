// Package config provides application settings, filesystem paths, and upstream HTTP header builders.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDefaultConfig verifies that DefaultConfig initializes all mandatory fields,
// ports, strategies, and upstream endpoints to standard expected defaults.
func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Host != DefaultHost {
		t.Errorf("Host = %q, expected %q", cfg.Host, DefaultHost)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("Port = %d, expected %d", cfg.Port, DefaultPort)
	}
	if cfg.Strategy != DefaultStrategy {
		t.Errorf("Strategy = %q, expected %q", cfg.Strategy, DefaultStrategy)
	}
	if cfg.ClientID != DefaultClientID {
		t.Errorf("ClientID = %q, expected %q", cfg.ClientID, DefaultClientID)
	}
	if len(cfg.Endpoints) != 2 {
		t.Errorf("expected 2 endpoints, got %d", len(cfg.Endpoints))
	}
}

// TestGenerateAPIKey verifies that generated gateway API keys have the proper prefix,
// high cryptographic entropy (64 hex characters), and unique generation across calls.
func TestGenerateAPIKey(t *testing.T) {
	key1, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() error: %v", err)
	}
	if !strings.HasPrefix(key1, APIKeyPrefix) {
		t.Errorf("API key missing prefix %q: %q", APIKeyPrefix, key1)
	}
	if len(key1) != len(APIKeyPrefix)+64 {
		t.Errorf("unexpected API key length %d, expected %d", len(key1), len(APIKeyPrefix)+64)
	}

	key2, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() second call error: %v", err)
	}
	if key1 == key2 {
		t.Error("expected generated keys to be unique")
	}
}

// TestLoadNonExistentInitializesFile verifies that calling Load on an absent configuration file path
// automatically generates a default config file with POSIX 0600 permissions and an auto-generated API key.
func TestLoadNonExistentInitializesFile(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	cfg, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() on absent file returned error: %v", err)
	}

	if cfg.APIKey == "" {
		t.Error("expected auto-generated API key")
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("os.Stat() on generated config file failed: %v", err)
	}

	// Verify POSIX 0600 file permissions
	perm := info.Mode().Perm()
	if perm != FilePerm {
		t.Errorf("expected file permissions %o, got %o", FilePerm, perm)
	}

	// Loading again should return the exact same configuration and key
	cfg2, err := Load(configPath)
	if err != nil {
		t.Fatalf("second Load() failed: %v", err)
	}
	if cfg2.APIKey != cfg.APIKey {
		t.Errorf("expected persistent API key %q, got %q", cfg.APIKey, cfg2.APIKey)
	}
}

// TestSaveAndLoadRoundtrip tests serialization and deserialization of modified configuration settings,
// ensuring POSIX 0600 file modes and faithful preservation of custom ports, keys, and strategies.
func TestSaveAndLoadRoundtrip(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "sub", "config.json")

	original := DefaultConfig()
	original.Port = 59999
	original.Strategy = "round-robin"
	original.APIKey = "agw_testkey123456789"
	original.UserAgent = "custom-agent/1.0"

	if err := Save(configPath, original); err != nil {
		t.Fatalf("Save() returned error: %v", err)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("os.Stat() failed: %v", err)
	}
	if info.Mode().Perm() != FilePerm {
		t.Errorf("expected permissions %o, got %o", FilePerm, info.Mode().Perm())
	}

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if loaded.Port != 59999 {
		t.Errorf("Port = %d, expected 59999", loaded.Port)
	}
	if loaded.Strategy != "round-robin" {
		t.Errorf("Strategy = %q, expected 'round-robin'", loaded.Strategy)
	}
	if loaded.APIKey != "agw_testkey123456789" {
		t.Errorf("APIKey = %q, expected 'agw_testkey123456789'", loaded.APIKey)
	}
	if loaded.UserAgent != "custom-agent/1.0" {
		t.Errorf("UserAgent = %q, expected 'custom-agent/1.0'", loaded.UserAgent)
	}
}

// TestClone verifies that Config.Clone produces an isolated deep copy of configuration slices.
func TestClone(t *testing.T) {
	var nilCfg *Config
	if nilCfg.Clone() != nil {
		t.Errorf("expected nilCfg.Clone() to return nil")
	}

	original := DefaultConfig()
	cloned := original.Clone()

	if cloned.Host != original.Host {
		t.Errorf("Host mismatch")
	}

	cloned.Endpoints[0] = "https://mutated.com"
	if original.Endpoints[0] == "https://mutated.com" {
		t.Errorf("mutation leaked into original endpoints slice")
	}
}

// TestSaveNilConfigFails verifies that attempting to save a nil Config pointer safely returns an error.
func TestSaveNilConfigFails(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	if err := Save(configPath, nil); err == nil {
		t.Error("expected Save(path, nil) to return error, got nil")
	}
}
