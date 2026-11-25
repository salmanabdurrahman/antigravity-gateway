// Package config provides application settings, filesystem paths, and upstream HTTP header builders.
package config

import (
	"cmp"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
)

const (
	// DefaultAntigravityVersion specifies the Antigravity Hub release baseline advertised upstream.
	// Cloud Code Assist gates newer models behind a minimum Hub build, so this baseline must stay
	// at or above the gate until the operator overrides it explicitly.
	DefaultAntigravityVersion = "2.33.0"

	// DefaultAntigravityChangelist pairs the Hub baseline with its Piper changelist (`cl`) token.
	DefaultAntigravityChangelist = "1023160026"

	// DefaultAPIClientValue represents the default X-Goog-Api-Client header value.
	DefaultAPIClientValue = "google-cloud-sdk vscode_cloudshelleditor/0.1"

	// AnthropicBetaInterleavedThinking is the required beta header enabling Claude models
	// to stream interleaved thinking tokens and tool execution schemas over Cloud Code Assist.
	AnthropicBetaInterleavedThinking = "interleaved-thinking-2025-05-14"

	// EnvUserAgent is the primary environment variable overriding the User-Agent header.
	EnvUserAgent = "AGW_USER_AGENT"

	// EnvAltUserAgent is the legacy environment variable for User-Agent override.
	EnvAltUserAgent = "PGA_USER_AGENT"

	// EnvAPIClient is the primary environment variable overriding the X-Goog-Api-Client header.
	EnvAPIClient = "AGW_API_CLIENT"

	// EnvAltAPIClient is the legacy environment variable for X-Goog-Api-Client override.
	EnvAltAPIClient = "PGA_API_CLIENT"

	// EnvClientMetadata is the primary environment variable overriding the Client-Metadata header.
	EnvClientMetadata = "AGW_CLIENT_METADATA"

	// EnvAltClientMetadata is the legacy environment variable for Client-Metadata override.
	EnvAltClientMetadata = "PGA_CLIENT_METADATA"

	// EnvAntigravityVersion is the environment variable overriding the Antigravity IDE version.
	EnvAntigravityVersion = "AGW_ANTIGRAVITY_VERSION"

	// EnvAltAntigravityVersion is the legacy environment variable for the Antigravity Hub version.
	EnvAltAntigravityVersion = "PGA_ANTIGRAVITY_VERSION"

	// EnvAntigravityChangelist overrides the Hub Piper changelist (`cl`) token independently
	// from the version, for operators that pin an unmapped Hub build.
	EnvAntigravityChangelist = "AGW_ANTIGRAVITY_CHANGELIST"
)

// antigravityChangelists maps known Antigravity Hub releases to their Piper changelist token.
// Protocol Quirk: Cloud Code Assist fingerprints the `cl` value together with the Hub version,
// so a version override without its matching changelist can be rejected as an unknown build.
var antigravityChangelists = map[string]string{
	"2.29.0": "1013060670",
	"2.33.0": "1023160026",
	"2.34.0": "1026637980",
	"2.35.1": "1029194470",
}

// DefaultPlatform returns the normalized Google platform name corresponding to the local host OS.
func DefaultPlatform() string {
	switch runtime.GOOS {
	case "darwin":
		return "MACOS"
	case "linux":
		return "LINUX"
	case "windows":
		return "WINDOWS"
	default:
		return strings.ToUpper(runtime.GOOS)
	}
}

var (
	staticDefaultUserAgentOnce = sync.OnceValue(func() string {
		return userAgentForVersion(DefaultAntigravityVersion)
	})
	staticDefaultClientMetadataOnce = sync.OnceValue(func() string {
		return fmt.Sprintf(`{"ideType":"ANTIGRAVITY","platform":"%s","pluginType":"GEMINI"}`, DefaultPlatform())
	})
)

// ResolveAntigravityChangelist returns the Piper changelist token paired with a Hub version.
// Unknown versions fall back to the baseline changelist, and AGW_ANTIGRAVITY_CHANGELIST always wins.
func ResolveAntigravityChangelist(version string) string {
	if override := strings.TrimSpace(os.Getenv(EnvAntigravityChangelist)); override != "" {
		return override
	}
	if cl, ok := antigravityChangelists[version]; ok {
		return cl
	}
	return DefaultAntigravityChangelist
}

// userAgentForVersion builds the modern Antigravity Hub User-Agent fingerprint.
// Protocol Quirk: Cloud Code Assist expects `antigravity/hub/<version>` with the `cl` changelist
// token. The older `antigravity/ide/<version>` prefix is rejected by the current minimum
// client version gate.
func userAgentForVersion(version string) string {
	cl := ResolveAntigravityChangelist(version)
	return fmt.Sprintf("antigravity/hub/%s (aidev_client; os_type=%s; arch=%s; cl=%s)", version, runtime.GOOS, runtime.GOARCH, cl)
}

// DefaultUserAgent formats the standard Antigravity Hub User-Agent string matching the host architecture.
func DefaultUserAgent() string {
	v1 := os.Getenv(EnvAntigravityVersion)
	v2 := os.Getenv(EnvAltAntigravityVersion)
	if v1 == "" && v2 == "" {
		return staticDefaultUserAgentOnce()
	}
	version := cmp.Or(v1, v2, DefaultAntigravityVersion)
	return userAgentForVersion(version)
}

// DefaultAPIClient returns the standard Google Cloud SDK API client header value.
func DefaultAPIClient() string {
	return DefaultAPIClientValue
}

// DefaultClientMetadata formats the default JSON metadata string identifying the client as Antigravity IDE.
func DefaultClientMetadata() string {
	return staticDefaultClientMetadataOnce()
}

// ResolveUserAgent determines the effective User-Agent header, checking environment variables first,
// then the provided configuration struct, and finally defaulting to the host fingerprint.
func ResolveUserAgent(cfg *Config) string {
	var configVal string
	if cfg != nil {
		configVal = cfg.UserAgent
	}
	return cmp.Or(
		os.Getenv(EnvUserAgent),
		os.Getenv(EnvAltUserAgent),
		configVal,
		DefaultUserAgent(),
	)
}

// ResolveAPIClient determines the effective X-Goog-Api-Client header value.
func ResolveAPIClient(cfg *Config) string {
	var configVal string
	if cfg != nil {
		configVal = cfg.APIClient
	}
	return cmp.Or(
		os.Getenv(EnvAPIClient),
		os.Getenv(EnvAltAPIClient),
		configVal,
		DefaultAPIClient(),
	)
}

// ResolveClientMetadata determines the effective Client-Metadata header value.
func ResolveClientMetadata(cfg *Config) string {
	var configVal string
	if cfg != nil {
		configVal = cfg.ClientMetadata
	}
	return cmp.Or(
		os.Getenv(EnvClientMetadata),
		os.Getenv(EnvAltClientMetadata),
		configVal,
		DefaultClientMetadata(),
	)
}

// IsClaude reports whether the given model name refers to an Anthropic Claude model.
// Google Cloud Code Assist proxies Claude models under specific constraints, requiring
// custom protocol headers.
func IsClaude(model string) bool {
	return strings.Contains(strings.ToLower(model), "claude")
}

// BuildUpstreamHeaders constructs a key-value header map for HTTP requests directed to Google Cloud Code Assist.
// It injects standard OAuth authorization, IDE spoofing signatures, and model-specific protocol quirks.
//
// Protocol Quirk: Claude models on Google Cloud Code Assist strictly require the
// "anthropic-beta: interleaved-thinking-2025-05-14" header. Without this flag, Claude turns
// fail to stream reasoning blocks and reject interleaved tool calls.
func BuildUpstreamHeaders(cfg *Config, accessToken string, model string) map[string]string {
	headers := map[string]string{
		"Content-Type":      "application/json",
		"User-Agent":        ResolveUserAgent(cfg),
		"X-Goog-Api-Client": ResolveAPIClient(cfg),
		"Client-Metadata":   ResolveClientMetadata(cfg),
	}

	if accessToken != "" {
		headers["Authorization"] = "Bearer " + accessToken
	}

	// Injects required thinking protocol flag when routing to Claude models
	if model != "" && IsClaude(model) {
		headers["anthropic-beta"] = AnthropicBetaInterleavedThinking
	}

	return headers
}

// BuildHTTPHeaders directly populates standard net/http Header structures without intermediate map allocations.
func BuildHTTPHeaders(cfg *Config, accessToken string, model string) http.Header {
	header := make(http.Header, 5)
	header.Set("Content-Type", "application/json")
	header.Set("User-Agent", ResolveUserAgent(cfg))
	header.Set("X-Goog-Api-Client", ResolveAPIClient(cfg))
	header.Set("Client-Metadata", ResolveClientMetadata(cfg))

	if accessToken != "" {
		header.Set("Authorization", "Bearer "+accessToken)
	}

	if model != "" && IsClaude(model) {
		header.Set("anthropic-beta", AnthropicBetaInterleavedThinking)
	}

	return header
}
