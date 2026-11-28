// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// stubAgyVersion replaces the local Antigravity CLI detector for the duration of a test.
func stubAgyVersion(t *testing.T, version string, err error) {
	t.Helper()

	original := agyVersionDetector
	agyVersionDetector = func(ctx context.Context) (string, error) {
		return version, err
	}
	t.Cleanup(func() { agyVersionDetector = original })
}

// clearingIdentityEnv neutralizes Hub version and changelist overrides so identity assertions stay
// deterministic regardless of the developer's shell.
func clearingIdentityEnv(t *testing.T) {
	t.Helper()

	t.Setenv(config.EnvAntigravityVersion, "")
	t.Setenv(config.EnvAltAntigravityVersion, "")
	t.Setenv(config.EnvAntigravityChangelist, "")
}

// TestCheckAdvertisedIdentityReportsBaseline verifies the default report names the Hub version together
// with its mapped changelist.
func TestCheckAdvertisedIdentityReportsBaseline(t *testing.T) {
	clearingIdentityEnv(t)

	item := checkAdvertisedIdentity(config.DefaultConfig())
	if item.Status != StatusOK {
		t.Errorf("Status = %v; want OK", item.Status)
	}
	want := "advertising antigravity/hub/" + config.DefaultAntigravityVersion + " (cl=" + config.DefaultAntigravityChangelist + ")"
	if item.Message != want {
		t.Errorf("Message = %q; want %q", item.Message, want)
	}
}

// TestCheckAdvertisedIdentityWarnsOnUnpairedVersion verifies that an operator version override without a
// changelist is flagged, because upstream gates the two together.
func TestCheckAdvertisedIdentityWarnsOnUnpairedVersion(t *testing.T) {
	clearingIdentityEnv(t)
	t.Setenv(config.EnvAntigravityVersion, "9.9.9")

	item := checkAdvertisedIdentity(config.DefaultConfig())
	if item.Status != StatusWarn {
		t.Errorf("Status = %v; want WARN for an unmapped version override", item.Status)
	}
	if !strings.Contains(item.Message, "9.9.9") || !strings.Contains(item.Message, "no known changelist") {
		t.Errorf("Message = %q; want the unpaired-version warning", item.Message)
	}
	if !strings.Contains(item.Hint, config.EnvAntigravityChangelist) {
		t.Errorf("Hint = %q; want the changelist environment hint", item.Hint)
	}
}

// TestCheckAdvertisedIdentityAcceptsPairedOverride verifies that supplying both halves of the identity
// (known version, or explicit changelist) clears the warning.
func TestCheckAdvertisedIdentityAcceptsPairedOverride(t *testing.T) {
	cases := []struct {
		// name labels the override shape.
		name string
		// version is the AGW_ANTIGRAVITY_VERSION value.
		version string
		// changelist is the AGW_ANTIGRAVITY_CHANGELIST value.
		changelist string
	}{
		{name: "mapped hub release", version: "2.34.0"},
		{name: "unknown release with explicit changelist", version: "9.9.9", changelist: "9999999999"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearingIdentityEnv(t)
			t.Setenv(config.EnvAntigravityVersion, tc.version)
			t.Setenv(config.EnvAntigravityChangelist, tc.changelist)

			item := checkAdvertisedIdentity(config.DefaultConfig())
			if item.Status != StatusOK {
				t.Errorf("Status = %v; want OK for a paired override (message: %s)", item.Status, item.Message)
			}
			if !strings.Contains(item.Message, tc.version) {
				t.Errorf("Message = %q; want the advertised version %q", item.Message, tc.version)
			}
		})
	}
}

// TestCheckAdvertisedIdentityRespectsCustomUserAgent verifies that an operator-supplied User-Agent leaves
// the identity operator-managed instead of nagging about the baseline.
func TestCheckAdvertisedIdentityRespectsCustomUserAgent(t *testing.T) {
	clearingIdentityEnv(t)

	cfg := config.DefaultConfig()
	cfg.UserAgent = "custom-agent/1.0"
	item := checkAdvertisedIdentity(cfg)
	if item.Status != StatusOK {
		t.Errorf("Status = %v; want OK for an operator-managed identity", item.Status)
	}
	if !strings.Contains(item.Message, "custom user_agent configured") {
		t.Errorf("Message = %q; want the operator-managed phrasing", item.Message)
	}
}

// TestCheckLocalAntigravityCLIReportsVersion verifies the CLI is reported on its own version track rather
// than compared numerically with the Hub release.
func TestCheckLocalAntigravityCLIReportsVersion(t *testing.T) {
	stubAgyVersion(t, "1.5.18", nil)

	item, ok := checkLocalAntigravityCLI()
	if !ok {
		t.Fatal("expected a CLI diagnostic item")
	}
	if item.Status != StatusOK {
		t.Errorf("Status = %v; want OK", item.Status)
	}
	if !strings.Contains(item.Message, "local agy CLI 1.5.18 detected") {
		t.Errorf("Message = %q; want the detected CLI version", item.Message)
	}
	if !strings.Contains(item.Message, "separate tracks") {
		t.Errorf("Message = %q; want the separate-version-tracks clarification", item.Message)
	}
}

// TestCheckLocalAntigravityCLIHandlesMissingBinary verifies a machine without the CLI stays informational.
func TestCheckLocalAntigravityCLIHandlesMissingBinary(t *testing.T) {
	stubAgyVersion(t, "", errors.New("exec: \"agy\": executable file not found in $PATH"))

	item, ok := checkLocalAntigravityCLI()
	if !ok {
		t.Fatal("expected a CLI diagnostic item")
	}
	if item.Status != StatusOK {
		t.Errorf("Status = %v; want OK when the local CLI is absent", item.Status)
	}
	if !strings.Contains(item.Message, "local agy CLI not detected") {
		t.Errorf("Message = %q; want the not-detected phrasing", item.Message)
	}
	if !strings.Contains(item.Hint, config.EnvAntigravityVersion) {
		t.Errorf("Hint = %q; want the version override hint", item.Hint)
	}
}

// TestParseSemanticVersion verifies extraction from typical `agy --version` banners.
func TestParseSemanticVersion(t *testing.T) {
	tests := map[string]string{
		"1.5.15":                           "1.5.15",
		"agy version 2.33.0":               "2.33.0",
		"Antigravity CLI v2.34.0 (darwin)": "2.34.0",
		"no version here":                  "",
		"":                                 "",
	}

	for raw, want := range tests {
		if got := parseSemanticVersion(raw); got != want {
			t.Errorf("parseSemanticVersion(%q) = %q; want %q", raw, got, want)
		}
	}
}
