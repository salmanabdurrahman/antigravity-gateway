// Package config provides application settings, filesystem paths, and upstream HTTP header builders.
package config

import (
	"encoding/json"
	"errors"
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
	original.ModelMapping = map[string]string{
		"gemini-4-argon": "gemini-4-argon-high",
	}
	cloned := original.Clone()

	if cloned.Host != original.Host {
		t.Errorf("Host mismatch")
	}

	cloned.Endpoints[0] = "https://mutated.com"
	if original.Endpoints[0] == "https://mutated.com" {
		t.Errorf("mutation leaked into original endpoints slice")
	}

	cloned.ModelMapping["gemini-4-argon"] = "mutated"
	if original.ModelMapping["gemini-4-argon"] == "mutated" {
		t.Errorf("mutation leaked into original model mapping map")
	}

	if original.ResolveModel("gemini-4-argon") != "gemini-4-argon-high" {
		t.Errorf("expected gemini-4-argon-high, got %s", original.ResolveModel("gemini-4-argon"))
	}
	if original.ResolveModel("other-model") != "other-model" {
		t.Errorf("expected other-model, got %s", original.ResolveModel("other-model"))
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

// TestValidateAPIKey verifies the gateway API key format validator accepts keys produced by
// GenerateAPIKey and rejects empty, foreign, truncated, and non-hexadecimal keys. The rejected
// error must stay generic so the secret value is never echoed back to callers or logs.
func TestValidateAPIKey(t *testing.T) {
	validKey, err := GenerateAPIKey()
	if err != nil {
		t.Fatalf("GenerateAPIKey() error: %v", err)
	}
	if err := ValidateAPIKey(validKey); err != nil {
		t.Fatalf("ValidateAPIKey(generated) returned error: %v", err)
	}

	cases := []struct {
		name string
		key  string
	}{
		{"empty", ""},
		{"missing prefix", strings.Repeat("a", APIKeyHexLen)},
		{"foreign prefix", "sk-" + strings.Repeat("a", APIKeyHexLen)},
		{"truncated", validKey[:len(validKey)-1]},
		{"non hexadecimal", APIKeyPrefix + strings.Repeat("z", APIKeyHexLen)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verr := ValidateAPIKey(tc.key)
			if verr == nil {
				t.Fatalf("expected error for %s key, got nil", tc.name)
			}
			if !errors.Is(verr, ErrInvalidAPIKey) {
				t.Errorf("expected error wrapping ErrInvalidAPIKey, got %v", verr)
			}
			if tc.key != "" && strings.Contains(verr.Error(), tc.key) {
				t.Errorf("validation error leaked the raw key value: %v", verr)
			}
		})
	}
}

// TestSaveStrategyPreservesUnrelatedFields verifies that persisting a new rotation strategy only
// rewrites the strategy field: port, API key, endpoints, custom headers, and model mapping stored
// on disk must survive untouched, guarding against whole-config overwrites by stale processes.
func TestSaveStrategyPreservesUnrelatedFields(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	original := DefaultConfig()
	original.Port = 59999
	original.APIKey = APIKeyPrefix + strings.Repeat("a", APIKeyHexLen)
	original.Endpoints = []string{"https://example.invalid/upstream"}
	original.UserAgent = "custom-agent/1.0"
	original.ModelMapping = map[string]string{"custom-model": "gemini-4-argon-high"}

	if err := Save(configPath, original); err != nil {
		t.Fatalf("Save() returned error: %v", err)
	}

	if err := SaveStrategy(configPath, "hybrid"); err != nil {
		t.Fatalf("SaveStrategy() returned error: %v", err)
	}

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() returned error: %v", err)
	}

	if loaded.Strategy != "hybrid" {
		t.Errorf("Strategy = %q, expected 'hybrid'", loaded.Strategy)
	}
	if loaded.Port != 59999 {
		t.Errorf("Port = %d, expected 59999", loaded.Port)
	}
	if loaded.APIKey != original.APIKey {
		t.Errorf("APIKey = %q, expected the on-disk key to be preserved", loaded.APIKey)
	}
	if len(loaded.Endpoints) != 1 || loaded.Endpoints[0] != "https://example.invalid/upstream" {
		t.Errorf("Endpoints = %v, expected the on-disk endpoints to be preserved", loaded.Endpoints)
	}
	if loaded.UserAgent != "custom-agent/1.0" {
		t.Errorf("UserAgent = %q, expected 'custom-agent/1.0'", loaded.UserAgent)
	}
	if got := loaded.ResolveModel("custom-model"); got != "gemini-4-argon-high" {
		t.Errorf("ResolveModel('custom-model') = %q, expected 'gemini-4-argon-high'", got)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("os.Stat() failed: %v", err)
	}
	if info.Mode().Perm() != FilePerm {
		t.Errorf("expected permissions %o, got %o", FilePerm, info.Mode().Perm())
	}
}

// TestSaveStrategyPreservesUnknownFields verifies that persisting a strategy leaves JSON keys this
// binary does not model untouched, so configuration written by a newer agw release is never
// silently dropped by a running daemon.
func TestSaveStrategyPreservesUnknownFields(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")

	seeded := `{
  "strategy": "sticky",
  "api_key": "agw_` + strings.Repeat("a", APIKeyHexLen) + `",
  "future_feature": {"enabled": true, "weight": 0.5},
  "future_list": ["alpha", "beta"]
}`
	if err := os.WriteFile(configPath, []byte(seeded), FilePerm); err != nil {
		t.Fatalf("seeding config file: %v", err)
	}

	if err := SaveStrategy(configPath, "hybrid"); err != nil {
		t.Fatalf("SaveStrategy() returned error: %v", err)
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading persisted config: %v", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("parsing persisted config: %v", err)
	}
	if got := string(fields["strategy"]); got != `"hybrid"` {
		t.Errorf("persisted strategy = %s, expected \"hybrid\"", got)
	}
	if _, ok := fields["future_feature"]; !ok {
		t.Error("expected unknown key future_feature to survive strategy persistence")
	}
	if _, ok := fields["future_list"]; !ok {
		t.Error("expected unknown key future_list to survive strategy persistence")
	}

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() after SaveStrategy returned error: %v", err)
	}
	if loaded.Strategy != "hybrid" {
		t.Errorf("Strategy = %q, expected 'hybrid'", loaded.Strategy)
	}
}

// TestSaveStrategyMissingFileFails verifies that SaveStrategy refuses to create a configuration
// file from scratch, since doing so would mint a new API key and invalidate configured clients.
func TestSaveStrategyMissingFileFails(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "missing", "config.json")

	err := SaveStrategy(configPath, "hybrid")
	if err == nil {
		t.Fatal("expected SaveStrategy() on absent file to return error, got nil")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected error wrapping os.ErrNotExist, got %v", err)
	}
	if _, statErr := os.Stat(configPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("expected config file to remain absent, stat error: %v", statErr)
	}
}
