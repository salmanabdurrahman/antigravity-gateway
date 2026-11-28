// Package config provides application settings, filesystem paths, and upstream HTTP header builders.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

	// Verify POSIX 0600 file permissions. Windows does not model POSIX bits, so this contract is
	// asserted on POSIX hosts only.
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm != FilePerm {
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
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm != FilePerm {
		t.Errorf("expected permissions %o, got %o", FilePerm, perm)
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
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm != FilePerm {
		t.Errorf("expected permissions %o, got %o", FilePerm, perm)
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

// TestConfigBYOCEnvOverrides verifies that AGW_CLIENT_ID and AGW_CLIENT_SECRET environment variables
// override DefaultClientID and DefaultClientSecret during DefaultConfig and Load.
func TestConfigBYOCEnvOverrides(t *testing.T) {
	t.Setenv(EnvClientID, "custom-client-id-999")
	t.Setenv(EnvClientSecret, "custom-client-secret-999")

	cfg := DefaultConfig()
	if cfg.ClientID != "custom-client-id-999" {
		t.Errorf("expected ClientID custom-client-id-999, got %s", cfg.ClientID)
	}
	if cfg.ClientSecret != "custom-client-secret-999" {
		t.Errorf("expected ClientSecret custom-client-secret-999, got %s", cfg.ClientSecret)
	}

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.json")
	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if loaded.ClientID != "custom-client-id-999" {
		t.Errorf("expected loaded ClientID custom-client-id-999, got %s", loaded.ClientID)
	}
	if loaded.ClientSecret != "custom-client-secret-999" {
		t.Errorf("expected loaded ClientSecret custom-client-secret-999, got %s", loaded.ClientSecret)
	}
}

// TestConfigStringRedaction verifies that Config.String() redacts APIKey and ClientSecret
// to prevent accidental credential leakage in log statements and diagnostics.
func TestConfigStringRedaction(t *testing.T) {
	cfg := &Config{
		Host:         "127.0.0.1",
		Port:         51400,
		Strategy:     "sticky",
		ClientID:     "test-client-id",
		ClientSecret: "super-secret-client-secret-val",
		APIKey:       "agw_1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
	}

	rendered := cfg.String()
	if strings.Contains(rendered, "super-secret") {
		t.Errorf("SECURITY LEAK: Config.String() exposed ClientSecret: %s", rendered)
	}
	if strings.Contains(rendered, "1234567890abcdef") {
		t.Errorf("SECURITY LEAK: Config.String() exposed APIKey: %s", rendered)
	}
	if !strings.Contains(rendered, "test-client-id") {
		t.Errorf("Config.String() missing ClientID: %s", rendered)
	}
}

// TestEndpointsForTierOrdering verifies tier-aware endpoint preference while preserving explicit
// operator-supplied endpoint lists.
func TestEndpointsForTierOrdering(t *testing.T) {
	cfg := DefaultConfig()

	personal := cfg.EndpointsForTier(TierPersonal)
	if len(personal) != 2 || personal[0] != "https://daily-cloudcode-pa.googleapis.com" {
		t.Errorf("personal endpoints = %v; want daily endpoint first", personal)
	}

	business := cfg.EndpointsForTier(TierBusiness)
	if len(business) != 2 || business[0] != "https://cloudcode-pa.googleapis.com" {
		t.Errorf("business endpoints = %v; want production endpoint first", business)
	}

	enterprise := cfg.EndpointsForTier(TierEnterprise)
	if len(enterprise) != 2 || enterprise[0] != "https://cloudcode-pa.googleapis.com" {
		t.Errorf("enterprise endpoints = %v; want production endpoint first", enterprise)
	}

	custom := &Config{Endpoints: []string{"https://custom-b.example", "https://custom-a.example"}}
	overridden := custom.EndpointsForTier(TierEnterprise)
	if len(overridden) != 2 || overridden[0] != "https://custom-b.example" {
		t.Errorf("explicit endpoints must keep their order, got %v", overridden)
	}
}

// TestConfigDurationDefaults verifies the configurable rotator thresholds follow the operating profile:
// the safe profile absorbs no cooldown wait and keeps one empty-stream retry, while the manual profile
// preserves the historical defaults. Explicit values always win over the profile.
func TestConfigDurationDefaults(t *testing.T) {
	safe := DefaultConfig()
	if got := safe.EffectiveProfile(); got != ProfileSafe {
		t.Fatalf("DefaultConfig().EffectiveProfile() = %q; want %q", got, ProfileSafe)
	}
	if got := safe.MaxWaitDuration(); got != 0 {
		t.Errorf("safe MaxWaitDuration() = %v; want 0", got)
	}
	if got := safe.EmptyStreamRetryCount(); got != SafeEmptyStreamRetries {
		t.Errorf("safe EmptyStreamRetryCount() = %d; want %d", got, SafeEmptyStreamRetries)
	}

	manual := &Config{Profile: ProfileManual}
	if got := manual.MaxWaitDuration(); got != 20*time.Second {
		t.Errorf("manual MaxWaitDuration() = %v; want 20s", got)
	}
	if got := manual.EmptyStreamRetryCount(); got != 2 {
		t.Errorf("manual EmptyStreamRetryCount() = %d; want 2", got)
	}
	if got := manual.CooldownDuration(); got != 60*time.Second {
		t.Errorf("manual CooldownDuration() = %v; want 60s", got)
	}

	tuned := &Config{Profile: ProfileManual, MaxWaitSeconds: 5, DefaultCooldownSeconds: 30, EmptyStreamRetries: 4}
	if got := tuned.MaxWaitDuration(); got != 5*time.Second {
		t.Errorf("tuned MaxWaitDuration() = %v; want 5s", got)
	}
	if got := tuned.CooldownDuration(); got != 30*time.Second {
		t.Errorf("tuned CooldownDuration() = %v; want 30s", got)
	}
	if got := tuned.EmptyStreamRetryCount(); got != 4 {
		t.Errorf("tuned EmptyStreamRetryCount() = %d; want 4", got)
	}

	// An explicit value overrides the safe profile too, so an operator can re-enable absorption on a
	// single account without switching profiles.
	safeTuned := &Config{MaxWaitSeconds: 8, EmptyStreamRetries: 3}
	if got := safeTuned.MaxWaitDuration(); got != 8*time.Second {
		t.Errorf("safe tuned MaxWaitDuration() = %v; want 8s", got)
	}
	if got := safeTuned.EmptyStreamRetryCount(); got != 3 {
		t.Errorf("safe tuned EmptyStreamRetryCount() = %d; want 3", got)
	}

	capped := &Config{Profile: ProfileManual, MaxWaitSeconds: 86400, DefaultCooldownSeconds: 999999, EmptyStreamRetries: 1000}
	if got := capped.MaxWaitDuration(); got != MaxWaitLimitSeconds*time.Second {
		t.Errorf("capped MaxWaitDuration() = %v; want %v", got, MaxWaitLimitSeconds*time.Second)
	}
	if got := capped.CooldownDuration(); got != CooldownLimitSeconds*time.Second {
		t.Errorf("capped CooldownDuration() = %v; want %v", got, CooldownLimitSeconds*time.Second)
	}
	if got := capped.EmptyStreamRetryCount(); got != EmptyStreamRetryLimit {
		t.Errorf("capped EmptyStreamRetryCount() = %d; want %d", got, EmptyStreamRetryLimit)
	}
}

// TestConfigProfileResolvers verifies that the operating profile drives rotation mode and catalog
// freshness, that explicit overrides win, and that unknown values degrade to the safe profile.
func TestConfigProfileResolvers(t *testing.T) {
	tests := []struct {
		// name labels the configuration shape under test.
		name string
		// cfg is the configuration to resolve.
		cfg *Config
		// wantProfile is the normalized profile.
		wantProfile string
		// wantMode is the resolved rotation mode.
		wantMode string
		// wantCatalogAge is the resolved catalog freshness window.
		wantCatalogAge time.Duration
	}{
		{
			name:           "nil config",
			cfg:            nil,
			wantProfile:    ProfileSafe,
			wantMode:       RotationModeSingle,
			wantCatalogAge: DefaultCatalogMaxAgeMinutes * time.Minute,
		},
		{
			name:           "unset profile defaults to safe",
			cfg:            &Config{},
			wantProfile:    ProfileSafe,
			wantMode:       RotationModeSingle,
			wantCatalogAge: DefaultCatalogMaxAgeMinutes * time.Minute,
		},
		{
			name:           "explicit safe profile",
			cfg:            &Config{Profile: ProfileSafe},
			wantProfile:    ProfileSafe,
			wantMode:       RotationModeSingle,
			wantCatalogAge: DefaultCatalogMaxAgeMinutes * time.Minute,
		},
		{
			name:           "manual profile restores pool rotation",
			cfg:            &Config{Profile: ProfileManual},
			wantProfile:    ProfileManual,
			wantMode:       RotationModePool,
			wantCatalogAge: LegacyCatalogMaxAgeMinutes * time.Minute,
		},
		{
			name:           "explicit rotation mode overrides profile",
			cfg:            &Config{Profile: ProfileManual, RotationMode: RotationModeSingle},
			wantProfile:    ProfileManual,
			wantMode:       RotationModeSingle,
			wantCatalogAge: LegacyCatalogMaxAgeMinutes * time.Minute,
		},
		{
			name:           "explicit catalog window overrides profile",
			cfg:            &Config{Profile: ProfileManual, CatalogMaxAgeMinutes: 5},
			wantProfile:    ProfileManual,
			wantMode:       RotationModePool,
			wantCatalogAge: 5 * time.Minute,
		},
		{
			name:           "unknown profile degrades to safe",
			cfg:            &Config{Profile: "yolo"},
			wantProfile:    ProfileSafe,
			wantMode:       RotationModeSingle,
			wantCatalogAge: DefaultCatalogMaxAgeMinutes * time.Minute,
		},
		{
			name:           "catalog window is capped",
			cfg:            &Config{Profile: ProfileManual, CatalogMaxAgeMinutes: 999999},
			wantProfile:    ProfileManual,
			wantMode:       RotationModePool,
			wantCatalogAge: CatalogMaxAgeLimitMinutes * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.EffectiveProfile(); got != tt.wantProfile {
				t.Errorf("EffectiveProfile() = %q; want %q", got, tt.wantProfile)
			}
			if got := tt.cfg.EffectiveRotationMode(); got != tt.wantMode {
				t.Errorf("EffectiveRotationMode() = %q; want %q", got, tt.wantMode)
			}
			if got := tt.cfg.CatalogMaxAge(); got != tt.wantCatalogAge {
				t.Errorf("CatalogMaxAge() = %v; want %v", got, tt.wantCatalogAge)
			}
		})
	}
}

// TestNormalizeAccountTier verifies normalization of upstream subscription identifiers.
func TestNormalizeAccountTier(t *testing.T) {
	tests := map[string]string{
		"":                    "",
		"tier-free":           TierPersonal,
		"Free Tier":           TierPersonal,
		"tier-premium":        TierPersonal,
		"Google Workspace":    TierBusiness,
		"business-standard":   TierBusiness,
		"enterprise-standard": TierEnterprise,
		"unknown-product":     "",
	}
	for raw, want := range tests {
		if got := NormalizeAccountTier(raw); got != want {
			t.Errorf("NormalizeAccountTier(%q) = %q; want %q", raw, got, want)
		}
	}
}

// TestConfigPacingResolvers verifies the safe profile caps in-flight streams and dispatch attempts and
// enables the bare-rejection guard, while the manual profile preserves legacy behaviour and explicit
// values override each knob.
func TestConfigPacingResolvers(t *testing.T) {
	safe := DefaultConfig()
	if got := safe.MaxInFlightPerAccountLimit(); got != DefaultMaxInFlightPerAccount {
		t.Errorf("safe MaxInFlightPerAccountLimit() = %d; want %d", got, DefaultMaxInFlightPerAccount)
	}
	if got := safe.DispatchBudget(10); got != SafeMaxDispatchesPerRequest {
		t.Errorf("safe DispatchBudget(10) = %d; want %d", got, SafeMaxDispatchesPerRequest)
	}
	if got := safe.DispatchBudget(1); got != 1 {
		t.Errorf("safe DispatchBudget(1) = %d; want the legacy budget of 1", got)
	}
	limit, cooldown := safe.PromptRejectionGuard()
	if limit != DefaultPromptRejectionLimit {
		t.Errorf("safe PromptRejectionGuard limit = %d; want %d", limit, DefaultPromptRejectionLimit)
	}
	if cooldown != time.Duration(DefaultPromptRejectionCooldownSeconds)*time.Second {
		t.Errorf("safe PromptRejectionGuard cooldown = %v; want %ds", cooldown, DefaultPromptRejectionCooldownSeconds)
	}

	manual := &Config{Profile: ProfileManual}
	if got := manual.MaxInFlightPerAccountLimit(); got != 0 {
		t.Errorf("manual MaxInFlightPerAccountLimit() = %d; want 0 (unbounded)", got)
	}
	if got := manual.DispatchBudget(7); got != 7 {
		t.Errorf("manual DispatchBudget(7) = %d; want the legacy budget", got)
	}
	if limit, cooldown := manual.PromptRejectionGuard(); limit != 0 || cooldown != 0 {
		t.Errorf("manual PromptRejectionGuard() = (%d, %v); want the guard disabled", limit, cooldown)
	}

	explicit := &Config{
		MaxInFlightPerAccount:          MaxInFlightLimit + 100,
		MaxDispatchesPerRequest:        DispatchBudgetLimit + 100,
		PromptRejectionLimit:           PromptRejectionLimitCap + 100,
		PromptRejectionCooldownSeconds: PromptRejectionCooldownLimitSeconds + 100,
	}
	if got := explicit.MaxInFlightPerAccountLimit(); got != MaxInFlightLimit {
		t.Errorf("explicit MaxInFlightPerAccountLimit() = %d; want the cap %d", got, MaxInFlightLimit)
	}
	if got := explicit.DispatchBudget(3); got != DispatchBudgetLimit {
		t.Errorf("explicit DispatchBudget(3) = %d; want the cap %d", got, DispatchBudgetLimit)
	}
	limit, cooldown = explicit.PromptRejectionGuard()
	if limit != PromptRejectionLimitCap {
		t.Errorf("explicit guard limit = %d; want the cap %d", limit, PromptRejectionLimitCap)
	}
	if cooldown != time.Duration(PromptRejectionCooldownLimitSeconds)*time.Second {
		t.Errorf("explicit guard cooldown = %v; want the cap %ds", cooldown, PromptRejectionCooldownLimitSeconds)
	}

	tuned := &Config{MaxInFlightPerAccount: 4, MaxDispatchesPerRequest: 3, PromptRejectionLimit: 5, PromptRejectionCooldownSeconds: 60}
	if got := tuned.MaxInFlightPerAccountLimit(); got != 4 {
		t.Errorf("tuned MaxInFlightPerAccountLimit() = %d; want 4", got)
	}
	if got := tuned.DispatchBudget(9); got != 3 {
		t.Errorf("tuned DispatchBudget(9) = %d; want 3", got)
	}
	limit, cooldown = tuned.PromptRejectionGuard()
	if limit != 5 || cooldown != time.Minute {
		t.Errorf("tuned PromptRejectionGuard() = (%d, %v); want (5, 1m)", limit, cooldown)
	}

	// An explicit limit enables the guard even under the manual profile, matching the override rule the
	// other profile-derived defaults follow.
	manualOverride := &Config{Profile: ProfileManual, PromptRejectionLimit: 2}
	limit, cooldown = manualOverride.PromptRejectionGuard()
	if limit != 2 {
		t.Errorf("manual override guard limit = %d; want 2", limit)
	}
	if cooldown != time.Duration(DefaultPromptRejectionCooldownSeconds)*time.Second {
		t.Errorf("manual override guard cooldown = %v; want the default window", cooldown)
	}
}
