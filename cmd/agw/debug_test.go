// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// debugWireFixtureConfig returns a configuration with distinctive, non-secret wire identity values so
// the diagnostic assertions can prove the report is resolved from configuration rather than defaults.
func debugWireFixtureConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Strategy = "hybrid"
	cfg.APIKey = config.APIKeyPrefix + strings.Repeat("a", config.APIKeyHexLen)
	cfg.ClientSecret = "super-secret-oauth-client-secret"
	cfg.UserAgent = "antigravity/hub/2.34.0 (aidev_client; os_type=darwin; arch=arm64; cl=1026637980)"
	cfg.APIClient = "google-cloud-sdk vscode/1.87.0"
	cfg.ClientMetadata = `{"ideType":"ANTIGRAVITY","platform":"MACOS","pluginType":"GEMINI"}`
	cfg.Endpoints = []string{"https://daily-cloudcode-pa.googleapis.com", "https://cloudcode-pa.googleapis.com"}
	cfg.MaxWaitSeconds = 15
	cfg.DefaultCooldownSeconds = 45
	cfg.EmptyStreamRetries = 2
	cfg.QuotaThreshold = 0.1
	return cfg
}

// prepareDebugWireTest isolates configuration state for agw debug wire, neutralizes wire identity
// environment overrides so assertions stay deterministic, and restores the shared JSON flag.
func prepareDebugWireTest(t *testing.T) {
	t.Helper()

	t.Setenv(config.EnvHome, t.TempDir())
	t.Setenv(config.EnvUserAgent, "")
	t.Setenv(config.EnvAltUserAgent, "")
	t.Setenv(config.EnvAPIClient, "")
	t.Setenv(config.EnvAltAPIClient, "")
	t.Setenv(config.EnvClientMetadata, "")
	t.Setenv(config.EnvAltClientMetadata, "")

	cfg := debugWireFixtureConfig()
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
	}

	previousJSON := debugJSON
	t.Cleanup(func() {
		debugJSON = previousJSON
		debugWireCmd.SetOut(nil)
		debugWireCmd.SetErr(nil)
		debugWireCmd.SetContext(nil)
	})
}

// runDebugWireCaptured executes agw debug wire with structured JSON output disabled and returns the
// rendered text report.
func runDebugWireCaptured(t *testing.T) string {
	t.Helper()

	debugJSON = false
	buf := new(bytes.Buffer)
	debugWireCmd.SetOut(buf)
	debugWireCmd.SetErr(buf)
	debugWireCmd.SetContext(t.Context())

	if err := debugWireCmd.RunE(debugWireCmd, nil); err != nil {
		t.Fatalf("debugWireCmd.RunE failed: %v", err)
	}
	return buf.String()
}

// TestDebugWireReportsResolvedIdentity verifies that agw debug wire prints the resolved upstream
// User-Agent, API client, client metadata, transport mode, endpoint order, and request defaults, and
// that the report leaks neither the local API key nor the OAuth client secret.
func TestDebugWireReportsResolvedIdentity(t *testing.T) {
	prepareDebugWireTest(t)

	out := runDebugWireCaptured(t)

	wants := []string{
		"Gateway Endpoint:      http://127.0.0.1:",
		"Strategy:              hybrid",
		"Profile:               safe",
		"Rotation Mode:         single",
		"Quota Threshold:       10%",
		"User-Agent:            antigravity/hub/2.34.0 (aidev_client; os_type=darwin; arch=arm64; cl=1026637980)",
		"X-Goog-Api-Client:     google-cloud-sdk vscode/1.87.0",
		`Client-Metadata:       {"ideType":"ANTIGRAVITY","platform":"MACOS","pluginType":"GEMINI"}`,
		"Upstream Transport:    http/",
		"Max Cooldown Wait:     15s",
		"Default 429 Cooldown:  45s",
		"Empty Stream Retries:  2",
		"Catalog Refresh:       60m",
		"Max In Flight:         2 per account",
		"Dispatch Budget:       profile default",
		"Prompt Rejection Guard: 3 bare 429s / 900s",
		"  1. https://daily-cloudcode-pa.googleapis.com",
		"  2. https://cloudcode-pa.googleapis.com",
	}
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("debug wire output missing %q:\n%s", want, out)
		}
	}

	if strings.Contains(out, strings.Repeat("a", config.APIKeyHexLen)) || strings.Contains(out, "super-secret-oauth-client-secret") {
		t.Fatalf("SECURITY LEAK: debug wire printed a secret value:\n%s", out)
	}
}

// TestDebugWireJSONMatchesTextReport verifies that agw debug wire --json emits the same resolved
// identity as the text report, remains parseable as a wireIdentityView, and still exposes no secrets.
func TestDebugWireJSONMatchesTextReport(t *testing.T) {
	prepareDebugWireTest(t)

	debugJSON = true
	buf := new(bytes.Buffer)
	debugWireCmd.SetOut(buf)
	debugWireCmd.SetErr(buf)
	debugWireCmd.SetContext(t.Context())

	if err := debugWireCmd.RunE(debugWireCmd, nil); err != nil {
		t.Fatalf("debugWireCmd.RunE failed: %v", err)
	}

	raw := buf.String()
	if strings.Contains(raw, strings.Repeat("a", config.APIKeyHexLen)) || strings.Contains(raw, "super-secret-oauth-client-secret") {
		t.Fatalf("SECURITY LEAK: debug wire --json printed a secret value:\n%s", raw)
	}

	var view wireIdentityView
	if err := json.Unmarshal([]byte(raw), &view); err != nil {
		t.Fatalf("decoding debug wire JSON output: %v\n%s", err, raw)
	}

	if view.UserAgent != "antigravity/hub/2.34.0 (aidev_client; os_type=darwin; arch=arm64; cl=1026637980)" {
		t.Errorf("user_agent = %q; want the configured value", view.UserAgent)
	}
	if view.APIClient != "google-cloud-sdk vscode/1.87.0" {
		t.Errorf("api_client = %q; want the configured value", view.APIClient)
	}
	if view.Strategy != "hybrid" {
		t.Errorf("strategy = %q; want hybrid", view.Strategy)
	}
	if view.MaxWaitSeconds != 15 || view.DefaultCooldownSeconds != 45 || view.EmptyStreamRetries != 2 {
		t.Errorf("request defaults = (%d, %d, %d); want (15, 45, 2)",
			view.MaxWaitSeconds, view.DefaultCooldownSeconds, view.EmptyStreamRetries)
	}
	if len(view.Endpoints) != 2 || view.Endpoints[0] != "https://daily-cloudcode-pa.googleapis.com" {
		t.Errorf("endpoints = %v; want the configured dispatch order", view.Endpoints)
	}
	if !strings.HasPrefix(view.UpstreamTransport, "http/") {
		t.Errorf("upstream_transport = %q; want an HTTP version prefix", view.UpstreamTransport)
	}
	if view.ConfigPath == "" {
		t.Errorf("config_path is empty; want the resolved configuration path")
	}
}

// TestUpstreamTransportModeFollowsWireMode verifies that the reported transport shape follows the active
// wire mode, so the rollback escape hatch and the default cannot drift from one another.
func TestUpstreamTransportModeFollowsWireMode(t *testing.T) {
	t.Setenv(config.WireModeEnv, config.WireModeHTTP1)
	if mode := upstreamTransportMode(); !strings.HasPrefix(mode, "http/1.1") {
		t.Errorf("upstreamTransportMode() = %q; want an http/1.1 label in the default wire mode", mode)
	}

	t.Setenv(config.WireModeEnv, config.WireModeAuto)
	if mode := upstreamTransportMode(); !strings.HasPrefix(mode, "http/2") {
		t.Errorf("upstreamTransportMode() = %q; want an http/2 label in auto wire mode", mode)
	}
}
