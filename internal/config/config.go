// Package config provides application settings, filesystem paths, and upstream HTTP header builders.
package config

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Version defines the current semantic version of the gateway application.
// Can be overridden at build time via -ldflags "-X github.com/salmanabdurrahman/antigravity-gateway/internal/config.Version=vX.Y.Z".
var Version = "0.3.0"

const (
	// DefaultHost defines the default local loopback binding address.
	DefaultHost = "127.0.0.1"

	// DefaultPort defines the standard HTTP port for the local reverse proxy.
	DefaultPort = 51400

	// DefaultStrategy defines the initial account rotation algorithm (sticky prompt caching).
	DefaultStrategy = "sticky"

	// DefaultClientID represents the OAuth application client ID used by the Antigravity IDE.
	DefaultClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"

	// DefaultClientSecret represents the OAuth client secret used by the Antigravity IDE.
	DefaultClientSecret = "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"

	// DefaultAuthURL is the Google OAuth 2.0 authorization endpoint.
	DefaultAuthURL = "https://accounts.google.com/o/oauth2/v2/auth"

	// DefaultTokenURL is the Google OAuth 2.0 token exchange endpoint.
	DefaultTokenURL = "https://oauth2.googleapis.com/token"

	// DefaultUserInfoURL is the Google endpoint used to retrieve authenticated account email and profile.
	DefaultUserInfoURL = "https://www.googleapis.com/oauth2/v1/userinfo?alt=json"

	// DefaultRedirectURI is the local loopback callback URI used during OAuth PKCE login.
	DefaultRedirectURI = "http://localhost:51121/oauth-callback"

	// APIKeyPrefix defines the human-readable prefix used for generated local gateway API keys.
	APIKeyPrefix = "agw_"

	// APIKeyHexLen defines the number of hexadecimal characters following APIKeyPrefix in a generated key.
	APIKeyHexLen = 64

	// EnvClientID overrides ClientID from an environment variable for BYOC configurations.
	EnvClientID = "AGW_CLIENT_ID"

	// EnvClientSecret overrides ClientSecret from an environment variable for BYOC configurations.
	EnvClientSecret = "AGW_CLIENT_SECRET"

	// DefaultMaxWaitSeconds defines how long the rotator absorbs account cooldowns before failing a request.
	DefaultMaxWaitSeconds = 20

	// DefaultCooldownSeconds defines the base 429 cooldown applied when upstream omits an explicit retry delay.
	DefaultCooldownSeconds = 60

	// DefaultEmptyStreamRetries defines how many times an empty upstream stream is retried before failing
	// under the manual profile.
	DefaultEmptyStreamRetries = 2

	// SafeEmptyStreamRetries is the empty-stream retry budget under the safe profile. One attempt plus
	// a single retry keeps a struggling stream from turning into a retry storm against upstream.
	SafeEmptyStreamRetries = 1

	// ProfileSafe is the default operating profile: one primary account serves traffic, cooldown
	// absorption is disabled, and background polling is kept to a minimum.
	ProfileSafe = "safe"

	// ProfileManual restores the legacy pooled rotation behaviour and the historical request defaults.
	ProfileManual = "manual"

	// DefaultProfile is the operating profile applied when config.json names none.
	DefaultProfile = ProfileSafe

	// RotationModeSingle serves every request from one primary account and never fails over to a
	// sibling credential.
	RotationModeSingle = "single"

	// RotationModePool keeps the legacy multi-account failover behaviour.
	RotationModePool = "pool"

	// DefaultCatalogMaxAgeMinutes is how long a cached model catalog stays fresh under the safe
	// profile before a background refresh is considered.
	DefaultCatalogMaxAgeMinutes = 60

	// LegacyCatalogMaxAgeMinutes is the catalog freshness window preserved under the manual profile.
	LegacyCatalogMaxAgeMinutes = 15

	// CatalogMaxAgeLimitMinutes caps the configurable catalog freshness window at 24 hours.
	CatalogMaxAgeLimitMinutes = 1440

	// DefaultSuspendPauseSeconds defines how long the rotator pauses all upstream dispatch after a
	// terminal account-suspension verdict, so one enforcement hit cannot sweep the rest of the pool.
	DefaultSuspendPauseSeconds = 900

	// MaxWaitLimitSeconds caps cooldown absorption so a mis-typed config cannot block a request for hours.
	MaxWaitLimitSeconds = 120

	// CooldownLimitSeconds caps the base 429 cooldown at one hour.
	CooldownLimitSeconds = 3600

	// EmptyStreamRetryLimit caps empty-stream retries to avoid unbounded upstream amplification.
	EmptyStreamRetryLimit = 5

	// SuspendPauseLimitSeconds caps the post-suspension protection window at 24 hours.
	SuspendPauseLimitSeconds = 86400
)

// Account tier identifiers normalized from the upstream loadCodeAssist subscription metadata.
const (
	// TierPersonal covers free, Plus, and Pro consumer subscriptions that route through the daily endpoint.
	TierPersonal = "personal"

	// TierBusiness covers Google Workspace business subscriptions that route through the production endpoint.
	TierBusiness = "business"

	// TierEnterprise covers enterprise subscriptions that route through the production endpoint.
	TierEnterprise = "enterprise"
)

var (
	// dailyEndpointHost is the Google Cloud Code Assist daily quota endpoint used by consumer tiers.
	dailyEndpointHost = "https://daily-cloudcode-pa.googleapis.com"

	// productionEndpointHost is the Google Cloud Code Assist production endpoint used by paid tiers.
	productionEndpointHost = "https://cloudcode-pa.googleapis.com"
)

// ErrInvalidAPIKey indicates that a gateway API key does not match the locally generated format
// (APIKeyPrefix followed by APIKeyHexLen lowercase hexadecimal characters).
var ErrInvalidAPIKey = errors.New("gateway API key does not match the generated format")

// DefaultEndpoints returns the default ordered list of Google Cloud Code Assist upstream endpoints.
func DefaultEndpoints() []string {
	return []string{dailyEndpointHost, productionEndpointHost}
}

// NormalizeAccountTier converts free-form upstream subscription identifiers into a stable tier label.
// Consumer identifiers (free, plus, pro, premium, personal) collapse to TierPersonal, while Workspace and
// enterprise identifiers retain their distinction because endpoint ordering differs between them.
func NormalizeAccountTier(raw string) string {
	lower := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case lower == "":
		return ""
	case strings.Contains(lower, "enterprise"):
		return TierEnterprise
	case strings.Contains(lower, "business"), strings.Contains(lower, "workspace"):
		return TierBusiness
	case containsWord(lower, "personal"),
		containsWord(lower, "free"),
		containsWord(lower, "plus"),
		containsWord(lower, "premium"),
		containsWord(lower, "pro"):
		return TierPersonal
	default:
		return ""
	}
}

// containsWord reports whether lower contains token as a whole word, so identifiers such as
// "production" or "product" are not mistaken for the "pro" consumer tier.
func containsWord(lower, token string) bool {
	fields := strings.FieldsFunc(lower, func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	return slices.Contains(fields, token)
}

// IsPaidTier reports whether the tier should prefer the production Cloud Code endpoint.
func IsPaidTier(tier string) bool {
	return tier == TierBusiness || tier == TierEnterprise
}

// EndpointsForTier returns the configured endpoints ordered for the given account tier.
// The standard default order is daily-first; business and enterprise accounts prefer the
// production endpoint instead. Any operator-supplied endpoint list that differs from the standard
// default order is returned untouched so explicit overrides always win.
func (c *Config) EndpointsForTier(tier string) []string {
	endpoints := DefaultEndpoints()
	if c != nil && len(c.Endpoints) > 0 {
		endpoints = c.Endpoints
	}

	if !slices.Equal(endpoints, DefaultEndpoints()) {
		return slices.Clone(endpoints)
	}

	if IsPaidTier(tier) {
		return []string{productionEndpointHost, dailyEndpointHost}
	}
	return []string{dailyEndpointHost, productionEndpointHost}
}

// Config represents runtime settings for the local daemon, rotation engine, and upstream integrations.
type Config struct {
	// Host defines the local loopback network interface to bind the HTTP reverse proxy daemon to.
	Host string `json:"host"`

	// Port defines the local TCP port number where the gateway listens for OpenAI-compatible requests.
	Port int `json:"port"`

	// APIKey is the local Bearer authorization secret required from clients connecting to the gateway.
	APIKey string `json:"api_key"`

	// Strategy specifies the account rotation algorithm used by the rotator (e.g. "sticky" or "round-robin").
	Strategy string `json:"strategy"`

	// Profile selects the operating profile: "safe" (default) keeps one primary account serving and
	// minimizes background traffic, while "manual" restores the legacy pooled behaviour.
	Profile string `json:"profile,omitempty"`

	// RotationMode overrides how many pooled accounts may serve requests: "single" or "pool".
	// When empty it follows the operating profile.
	RotationMode string `json:"rotation_mode,omitempty"`

	// CatalogMaxAgeMinutes overrides how long a cached model catalog stays fresh before a background
	// refresh is considered. A zero value follows the operating profile.
	CatalogMaxAgeMinutes int `json:"catalog_max_age_minutes,omitzero"`

	// ClientID is the Google OAuth 2.0 application client identifier utilized for PKCE authentication.
	ClientID string `json:"client_id"`

	// ClientSecret is the Google OAuth 2.0 application client secret paired with ClientID.
	ClientSecret string `json:"client_secret"`

	// AuthURL represents the Google OAuth 2.0 authorization endpoint initiating browser consent.
	AuthURL string `json:"auth_url"`

	// TokenURL represents the Google OAuth 2.0 endpoint for code exchange and token refreshes.
	TokenURL string `json:"token_url"`

	// UserInfoURL represents the Google userinfo endpoint used to verify account email addresses.
	UserInfoURL string `json:"user_info_url"`

	// RedirectURI is the local HTTP loopback address receiving the OAuth PKCE authorization callback.
	RedirectURI string `json:"redirect_uri"`

	// Endpoints holds the ordered list of upstream Cloud Code Assist endpoints with automatic failover.
	Endpoints []string `json:"endpoints"`

	// UserAgent optionally overrides the User-Agent header dispatched to upstream Google endpoints.
	UserAgent string `json:"user_agent,omitempty"`

	// APIClient optionally overrides the X-Goog-Api-Client header dispatched upstream.
	APIClient string `json:"api_client,omitempty"`

	// ClientMetadata optionally overrides the JSON Client-Metadata header dispatched upstream.
	ClientMetadata string `json:"client_metadata,omitempty"`

	// QuotaThreshold specifies the minimum fractional quota (0.0 to 1.0) before an account is deprioritized (default 0.05 = 5%).
	QuotaThreshold float64 `json:"quota_threshold,omitzero"`

	// ModelMapping maps custom model alias names to backend model IDs (e.g. {"gemini-4-argon": "gemini-4-argon-high"}).
	ModelMapping map[string]string `json:"model_mapping,omitempty"`

	// MaxWaitSeconds caps how long the rotator absorbs account cooldowns before failing a request.
	MaxWaitSeconds int `json:"max_wait_seconds,omitzero"`

	// DefaultCooldownSeconds is the base cooldown applied on HTTP 429 without an explicit retry delay.
	DefaultCooldownSeconds int `json:"default_cooldown_seconds,omitzero"`

	// EmptyStreamRetries is how many times an empty upstream stream is retried before surfacing an error.
	EmptyStreamRetries int `json:"empty_stream_retries,omitzero"`

	// SuspendPauseSeconds caps how long all upstream dispatch stays paused after a terminal
	// account-suspension verdict. A zero or absent value uses the default window.
	SuspendPauseSeconds int `json:"suspend_pause_seconds,omitzero"`

	// DisableSuspendPause turns the post-suspension protection window off entirely, restoring the
	// legacy behaviour of failing over to the next pooled account immediately.
	DisableSuspendPause bool `json:"disable_suspend_pause,omitzero"`
}

// MaxWaitDuration returns the configured cooldown absorption limit as a duration. The safe profile
// absorbs nothing: a cooled-down primary account fails the request instead of stalling it or rotating
// to a sibling credential.
func (c *Config) MaxWaitDuration() time.Duration {
	seconds := DefaultMaxWaitSeconds
	if c.EffectiveProfile() == ProfileSafe {
		seconds = 0
	}
	if c != nil && c.MaxWaitSeconds > 0 {
		seconds = min(c.MaxWaitSeconds, MaxWaitLimitSeconds)
	}
	return time.Duration(seconds) * time.Second
}

// CooldownDuration returns the configured base 429 cooldown as a duration.
func (c *Config) CooldownDuration() time.Duration {
	seconds := DefaultCooldownSeconds
	if c != nil && c.DefaultCooldownSeconds > 0 {
		seconds = min(c.DefaultCooldownSeconds, CooldownLimitSeconds)
	}
	return time.Duration(seconds) * time.Second
}

// EmptyStreamRetryCount returns the configured empty-stream retry budget. The safe profile defaults to
// a single retry so a struggling stream cannot amplify into a retry storm.
func (c *Config) EmptyStreamRetryCount() int {
	if c != nil && c.EmptyStreamRetries > 0 {
		return min(c.EmptyStreamRetries, EmptyStreamRetryLimit)
	}
	if c.EffectiveProfile() == ProfileSafe {
		return SafeEmptyStreamRetries
	}
	return DefaultEmptyStreamRetries
}

// EffectiveProfile returns the normalized operating profile, defaulting to the safe profile for an
// unset or unrecognized value. Integration builds and tests that construct Config literals directly
// therefore get the safe behaviour unless they opt into ProfileManual.
func (c *Config) EffectiveProfile() string {
	if c == nil {
		return DefaultProfile
	}
	switch strings.ToLower(strings.TrimSpace(c.Profile)) {
	case ProfileManual:
		return ProfileManual
	case ProfileSafe, "":
		return ProfileSafe
	default:
		return DefaultProfile
	}
}

// EffectiveRotationMode returns the resolved rotation mode. An explicit "single" or "pool" wins;
// otherwise the operating profile decides, with the safe profile serving one primary account.
func (c *Config) EffectiveRotationMode() string {
	if c != nil {
		switch strings.ToLower(strings.TrimSpace(c.RotationMode)) {
		case RotationModeSingle:
			return RotationModeSingle
		case RotationModePool:
			return RotationModePool
		}
	}
	if c.EffectiveProfile() == ProfileManual {
		return RotationModePool
	}
	return RotationModeSingle
}

// CatalogMaxAge returns how long a cached model catalog stays fresh before a background refresh is
// considered. The safe profile polls less often than the legacy fifteen-minute window, and an explicit
// catalog_max_age_minutes always wins.
func (c *Config) CatalogMaxAge() time.Duration {
	if c != nil && c.CatalogMaxAgeMinutes > 0 {
		return time.Duration(min(c.CatalogMaxAgeMinutes, CatalogMaxAgeLimitMinutes)) * time.Minute
	}
	if c.EffectiveProfile() == ProfileManual {
		return LegacyCatalogMaxAgeMinutes * time.Minute
	}
	return DefaultCatalogMaxAgeMinutes * time.Minute
}

// SuspendPauseDuration returns how long upstream dispatch stays paused after a terminal suspension
// verdict. A negative or explicitly disabled configuration returns zero, meaning the pool fails over
// to the next account immediately.
func (c *Config) SuspendPauseDuration() time.Duration {
	if c != nil && c.DisableSuspendPause {
		return 0
	}
	seconds := DefaultSuspendPauseSeconds
	if c != nil && c.SuspendPauseSeconds > 0 {
		seconds = min(c.SuspendPauseSeconds, SuspendPauseLimitSeconds)
	}
	return time.Duration(seconds) * time.Second
}

// String returns a redacted representation of Config to avoid leaking secrets in logs and diagnostic outputs.
func (c *Config) String() string {
	if c == nil {
		return "<nil>"
	}
	maskKey := "<empty>"
	if len(c.APIKey) > 8 {
		maskKey = c.APIKey[:4] + "••••••••"
	} else if c.APIKey != "" {
		maskKey = "••••••••"
	}
	maskSecret := "<empty>"
	if len(c.ClientSecret) > 8 {
		maskSecret = c.ClientSecret[:4] + "••••••••"
	} else if c.ClientSecret != "" {
		maskSecret = "••••••••"
	}
	return fmt.Sprintf("Config{Host:%s, Port:%d, Strategy:%s, ClientID:%s, ClientSecret:%s, APIKey:%s}",
		c.Host, c.Port, c.Strategy, c.ClientID, maskSecret, maskKey)
}

// DefaultConfig instantiates a Config initialized with standard production defaults and fallback endpoints.
func DefaultConfig() *Config {
	return &Config{
		Host:                   DefaultHost,
		Port:                   DefaultPort,
		Strategy:               DefaultStrategy,
		Profile:                DefaultProfile,
		ClientID:               cmp.Or(os.Getenv(EnvClientID), DefaultClientID),
		ClientSecret:           cmp.Or(os.Getenv(EnvClientSecret), DefaultClientSecret),
		AuthURL:                DefaultAuthURL,
		TokenURL:               DefaultTokenURL,
		UserInfoURL:            DefaultUserInfoURL,
		RedirectURI:            DefaultRedirectURI,
		Endpoints:              DefaultEndpoints(),
		QuotaThreshold:         0.05,
		ModelMapping:           make(map[string]string),
		DefaultCooldownSeconds: DefaultCooldownSeconds,
		SuspendPauseSeconds:    DefaultSuspendPauseSeconds,
	}
}

// GenerateAPIKey generates a cryptographically random local gateway Bearer API key.
func GenerateAPIKey() (string, error) {
	raw := make([]byte, APIKeyHexLen/2)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating random bytes for API key: %w", err)
	}
	return APIKeyPrefix + hex.EncodeToString(raw), nil
}

// ValidateAPIKey verifies that key follows the format produced by GenerateAPIKey, so operators
// can detect truncated, hand-edited, or foreign keys before clients start receiving HTTP 401.
// The key value itself is never echoed back in the returned error.
func ValidateAPIKey(key string) error {
	if key == "" {
		return fmt.Errorf("%w: key is empty", ErrInvalidAPIKey)
	}

	suffix, ok := strings.CutPrefix(key, APIKeyPrefix)
	if !ok {
		return fmt.Errorf("%w: missing %q prefix", ErrInvalidAPIKey, APIKeyPrefix)
	}
	if len(suffix) != APIKeyHexLen {
		return fmt.Errorf("%w: expected %d hexadecimal characters, got %d", ErrInvalidAPIKey, APIKeyHexLen, len(suffix))
	}
	if _, err := hex.DecodeString(suffix); err != nil {
		// The suffix is deliberately not echoed: it is the secret itself, and a partially valid key
		// would otherwise be partially printed in logs and diagnostic reports.
		return fmt.Errorf("%w: suffix contains non-hexadecimal characters", ErrInvalidAPIKey)
	}

	return nil
}

// Load reads and parses the JSON configuration from the given file path.
// If the configuration file does not exist, a default configuration is generated with
// a new local API key and written atomically with POSIX 0600 permissions.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg := DefaultConfig()
			apiKey, keyErr := GenerateAPIKey()
			if keyErr != nil {
				return nil, fmt.Errorf("generating initial API key: %w", keyErr)
			}
			cfg.APIKey = apiKey

			if saveErr := Save(path, cfg); saveErr != nil {
				return nil, fmt.Errorf("creating initial config file at %s: %w", path, saveErr)
			}
			return cfg, nil
		}
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	cfg := DefaultConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing config json from %s: %w", path, err)
	}

	// Ensure required zero-value fields are safely backfilled with defaults
	cfg.Host = cmp.Or(cfg.Host, DefaultHost)
	cfg.Port = cmp.Or(cfg.Port, DefaultPort)
	cfg.Strategy = cmp.Or(cfg.Strategy, DefaultStrategy)
	cfg.ClientID = cmp.Or(os.Getenv(EnvClientID), cfg.ClientID, DefaultClientID)
	cfg.ClientSecret = cmp.Or(os.Getenv(EnvClientSecret), cfg.ClientSecret, DefaultClientSecret)
	cfg.AuthURL = cmp.Or(cfg.AuthURL, DefaultAuthURL)
	cfg.TokenURL = cmp.Or(cfg.TokenURL, DefaultTokenURL)
	cfg.UserInfoURL = cmp.Or(cfg.UserInfoURL, DefaultUserInfoURL)
	cfg.RedirectURI = cmp.Or(cfg.RedirectURI, DefaultRedirectURI)
	if len(cfg.Endpoints) == 0 {
		cfg.Endpoints = DefaultEndpoints()
	}
	// MaxWaitSeconds and EmptyStreamRetries are intentionally not backfilled here: their effective
	// values follow the operating profile, so writing defaults into the file would pin a profile's
	// numbers as explicit user choices.
	cfg.DefaultCooldownSeconds = cmp.Or(cfg.DefaultCooldownSeconds, DefaultCooldownSeconds)
	if cfg.APIKey == "" {
		apiKey, keyErr := GenerateAPIKey()
		if keyErr != nil {
			return nil, fmt.Errorf("generating missing API key: %w", keyErr)
		}
		cfg.APIKey = apiKey
		if saveErr := Save(path, cfg); saveErr != nil {
			return nil, fmt.Errorf("persisting generated API key to %s: %w", path, saveErr)
		}
	}

	return cfg, nil
}

// SaveStrategy persists only the account rotation strategy into the configuration file at path.
// The file itself is the source of truth: it is read straight from disk and only the strategy key
// is replaced inside the raw JSON object, so values held by a long-running process (or a stale
// in-memory configuration) can never leak into the persisted file, and fields written by a newer
// agw release survive untouched. A missing configuration file is reported as an error instead of
// silently creating a new file with a freshly generated API key.
func SaveStrategy(path, strategy string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading config file %s before persisting strategy: %w", path, err)
	}

	fields := make(map[string]json.RawMessage)
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("parsing config json from %s before persisting strategy: %w", path, err)
	}

	encoded, err := json.Marshal(strategy)
	if err != nil {
		return fmt.Errorf("encoding strategy %q: %w", strategy, err)
	}
	fields["strategy"] = encoded

	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling configuration for %s: %w", path, err)
	}
	data = append(data, '\n')

	if err := writeFileAtomic(path, data); err != nil {
		return fmt.Errorf("writing strategy %q to %s: %w", strategy, path, err)
	}

	return nil
}

// Save writes the given configuration atomically to the destination path with POSIX 0600 permissions.
func Save(path string, cfg *Config) error {
	if cfg == nil {
		return errors.New("cannot save nil configuration")
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling configuration: %w", err)
	}
	data = append(data, '\n')

	return writeFileAtomic(path, data)
}

// writeFileAtomic replaces path with data using a temporary file in the same directory, so an
// interrupted write can never truncate or corrupt the destination. The temporary file is chmodded
// to 0600 before any payload is written and flushed before the rename.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, DirPerm); err != nil {
		return fmt.Errorf("ensuring directory %s exists: %w", dir, err)
	}

	tempFile, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temporary config file in %s: %w", dir, err)
	}
	tempPath := tempFile.Name()

	// Ensure cleanup in case of write failure
	defer func() {
		_ = os.Remove(tempPath)
	}()

	if err := tempFile.Chmod(FilePerm); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("setting file mode 0600 on %s: %w", tempPath, err)
	}

	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("writing configuration data to %s: %w", tempPath, err)
	}

	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("flushing configuration data to disk %s: %w", tempPath, err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("closing temporary config file %s: %w", tempPath, err)
	}

	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("renaming %s to destination %s: %w", tempPath, path, err)
	}

	return nil
}

// LoadOrCreate loads configuration from the resolved default path, initializing it if absent.
func LoadOrCreate() (*Config, error) {
	path, err := ConfigFilePath()
	if err != nil {
		return nil, fmt.Errorf("resolving config file path: %w", err)
	}

	return Load(path)
}

// Clone creates a deep copy of the configuration structure.
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	cp := *c
	cp.Endpoints = slices.Clone(c.Endpoints)
	if c.ModelMapping != nil {
		cp.ModelMapping = maps.Clone(c.ModelMapping)
	}
	return &cp
}

// ResolveModel returns the mapped model name if configured in ModelMapping, or the original model name.
func (c *Config) ResolveModel(model string) string {
	if c == nil || len(c.ModelMapping) == 0 {
		return model
	}
	if target, ok := c.ModelMapping[model]; ok && target != "" {
		return target
	}
	return model
}
