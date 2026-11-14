// Package config provides application settings, filesystem paths, and upstream HTTP header builders.
package config

import (
	"cmp"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
)

const (
	// DefaultAntigravityVersion specifies the spoofed Antigravity IDE version string.
	DefaultAntigravityVersion = "2.11.0"

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

	// EnvAltAntigravityVersion is the legacy environment variable for Antigravity IDE version.
	EnvAltAntigravityVersion = "PGA_ANTIGRAVITY_VERSION"
)

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

// DefaultUserAgent formats the standard Antigravity IDE User-Agent string matching the host architecture.
func DefaultUserAgent() string {
	version := cmp.Or(
		os.Getenv(EnvAntigravityVersion),
		os.Getenv(EnvAltAntigravityVersion),
		DefaultAntigravityVersion,
	)
	return fmt.Sprintf("antigravity/ide/%s (aidev_client; os_type=%s; arch=%s)", version, runtime.GOOS, runtime.GOARCH)
}

// DefaultAPIClient returns the standard Google Cloud SDK API client header value.
func DefaultAPIClient() string {
	return DefaultAPIClientValue
}

// DefaultClientMetadata formats the default JSON metadata string identifying the client as Antigravity IDE.
func DefaultClientMetadata() string {
	return fmt.Sprintf(`{"ideType":"ANTIGRAVITY","platform":"%s","pluginType":"GEMINI"}`, DefaultPlatform())
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

// BuildHTTPHeaders converts upstream headers into standard net/http Header structures.
func BuildHTTPHeaders(cfg *Config, accessToken string, model string) http.Header {
	raw := BuildUpstreamHeaders(cfg, accessToken, model)
	header := make(http.Header, len(raw))
	for k, v := range raw {
		header.Set(k, v)
	}
	return header
}
