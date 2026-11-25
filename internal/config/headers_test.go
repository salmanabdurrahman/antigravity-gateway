// Package config provides application settings, filesystem paths, and upstream HTTP header builders.
package config

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// TestDefaultHeaders verifies default header generation for standard Gemini models, ensuring
// proper User-Agent construction, Cloud SDK identity headers, and omission of Claude-specific beta headers.
func TestDefaultHeaders(t *testing.T) {
	// Clear any environment overrides
	t.Setenv(EnvUserAgent, "")
	t.Setenv(EnvAltUserAgent, "")
	t.Setenv(EnvAPIClient, "")
	t.Setenv(EnvAltAPIClient, "")
	t.Setenv(EnvClientMetadata, "")
	t.Setenv(EnvAltClientMetadata, "")
	t.Setenv(EnvAntigravityVersion, "")
	t.Setenv(EnvAltAntigravityVersion, "")

	cfg := DefaultConfig()
	headers := BuildUpstreamHeaders(cfg, "test-access-token", "gemini-2.5-pro")

	expectedUA := fmt.Sprintf("antigravity/hub/2.33.0 (aidev_client; os_type=%s; arch=%s; cl=1023160026)", runtime.GOOS, runtime.GOARCH)
	if headers["User-Agent"] != expectedUA {
		t.Errorf("User-Agent = %q, expected %q", headers["User-Agent"], expectedUA)
	}

	if headers["X-Goog-Api-Client"] != DefaultAPIClientValue {
		t.Errorf("X-Goog-Api-Client = %q, expected %q", headers["X-Goog-Api-Client"], DefaultAPIClientValue)
	}

	if !strings.Contains(headers["Client-Metadata"], `"ideType":"ANTIGRAVITY"`) {
		t.Errorf("Client-Metadata missing ideType: %s", headers["Client-Metadata"])
	}

	if headers["Authorization"] != "Bearer test-access-token" {
		t.Errorf("Authorization = %q, expected 'Bearer test-access-token'", headers["Authorization"])
	}

	// Non-Claude model must NOT have anthropic-beta header
	if _, ok := headers["anthropic-beta"]; ok {
		t.Errorf("unexpected anthropic-beta header for Gemini model")
	}
}

// TestClaudeInterleavedThinkingHeader verifies that Claude family models always inject the mandatory
// anthropic-beta interleaved-thinking header required by Google Cloud Code Assist.
func TestClaudeInterleavedThinkingHeader(t *testing.T) {
	cfg := DefaultConfig()

	claudeModels := []string{
		"claude-3-7-sonnet-20250219",
		"claude-3-5-sonnet",
		"Claude-3-Haiku",
	}

	for _, m := range claudeModels {
		headers := BuildUpstreamHeaders(cfg, "token", m)
		val, ok := headers["anthropic-beta"]
		if !ok {
			t.Errorf("model %q missing anthropic-beta header", m)
		}
		if val != AnthropicBetaInterleavedThinking {
			t.Errorf("model %q anthropic-beta = %q, expected %q", m, val, AnthropicBetaInterleavedThinking)
		}
	}
}

// TestEnvironmentOverrides verifies that environment variables for User-Agent, X-Goog-Api-Client,
// and Client-Metadata take precedence over default header generation.
func TestEnvironmentOverrides(t *testing.T) {
	t.Setenv(EnvUserAgent, "custom-agent-override")
	t.Setenv(EnvAPIClient, "custom-api-client")
	t.Setenv(EnvClientMetadata, `{"custom":true}`)

	cfg := DefaultConfig()
	headers := BuildUpstreamHeaders(cfg, "", "gemini-3.8-flash")

	if headers["User-Agent"] != "custom-agent-override" {
		t.Errorf("User-Agent = %q, expected override", headers["User-Agent"])
	}
	if headers["X-Goog-Api-Client"] != "custom-api-client" {
		t.Errorf("X-Goog-Api-Client = %q, expected override", headers["X-Goog-Api-Client"])
	}
	if headers["Client-Metadata"] != `{"custom":true}` {
		t.Errorf("Client-Metadata = %q, expected override", headers["Client-Metadata"])
	}
}

// TestAntigravityVersionOverrides verifies that a Hub version override keeps its matching
// Piper changelist token, that an unknown version falls back to the baseline changelist, and that
// AGW_ANTIGRAVITY_CHANGELIST always wins.
func TestAntigravityVersionOverrides(t *testing.T) {
	t.Setenv(EnvAntigravityChangelist, "")

	tests := []struct {
		name            string
		version         string
		changelist      string
		expectedCL      string
		expectedVersion string
	}{
		{
			name:            "known version resolves its changelist",
			version:         "2.34.0",
			expectedCL:      "1026637980",
			expectedVersion: "2.34.0",
		},
		{
			name:            "unknown version falls back to baseline changelist",
			version:         "9.9.9",
			expectedCL:      DefaultAntigravityChangelist,
			expectedVersion: "9.9.9",
		},
		{
			name:            "explicit changelist override wins",
			version:         "2.34.0",
			changelist:      "9999999999",
			expectedCL:      "9999999999",
			expectedVersion: "2.34.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvAntigravityVersion, tt.version)
			t.Setenv(EnvAntigravityChangelist, tt.changelist)

			got := DefaultUserAgent()
			expected := fmt.Sprintf("antigravity/hub/%s (aidev_client; os_type=%s; arch=%s; cl=%s)",
				tt.expectedVersion, runtime.GOOS, runtime.GOARCH, tt.expectedCL)
			if got != expected {
				t.Errorf("DefaultUserAgent() = %q, expected %q", got, expected)
			}
		})
	}
}

// TestBuildHTTPHeaders verifies conversion from raw header key-value maps to standard net/http Header objects.
func TestBuildHTTPHeaders(t *testing.T) {
	cfg := DefaultConfig()
	httpHeaders := BuildHTTPHeaders(cfg, "token123", "claude-3-7-sonnet")

	if httpHeaders.Get("Authorization") != "Bearer token123" {
		t.Errorf("Authorization = %q", httpHeaders.Get("Authorization"))
	}
	if httpHeaders.Get("anthropic-beta") != AnthropicBetaInterleavedThinking {
		t.Errorf("anthropic-beta = %q", httpHeaders.Get("anthropic-beta"))
	}
	if httpHeaders.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", httpHeaders.Get("Content-Type"))
	}
}
