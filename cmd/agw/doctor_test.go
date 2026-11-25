// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/updater"
)

// TestDoctorCommand verifies that agw doctor executes all diagnostic categories and renders structured output.
func TestDoctorCommand(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	origDialer := connectivityDialer
	connectivityDialer = func(ctx context.Context, host string) (time.Duration, error) {
		return 5 * time.Millisecond, nil
	}
	defer func() { connectivityDialer = origDialer }()

	// Keep the diagnostic offline: address-family resolution and per-family dialing are stubbed.
	origResolver := addressFamilyResolver
	addressFamilyResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return nil, errors.New("stubbed resolver")
	}
	defer func() { addressFamilyResolver = origResolver }()

	origFamilyDialer := familyDialer
	familyDialer = func(ctx context.Context, network, host string) (time.Duration, error) {
		return time.Millisecond, nil
	}
	defer func() { familyDialer = origFamilyDialer }()

	origChecker := versionChecker
	versionChecker = func(ctx context.Context) (*updater.ReleaseInfo, error) {
		return &updater.ReleaseInfo{
			CurrentVersion:  config.Version,
			TargetVersion:   config.Version,
			UpdateAvailable: false,
		}, nil
	}
	defer func() { versionChecker = origChecker }()

	// Populate accounts.json so account diagnostic is happy
	accPath := filepath.Join(tempHome, "accounts.json")
	store := auth.NewStore(accPath)
	_, _ = store.Upsert(&auth.Account{
		Email:        "doctor-test@example.com",
		RefreshToken: "refresh-token-xyz",
		ProjectID:    "companion-project-123",
		AddedAt:      "2026-01-01T00:00:00Z",
	})

	buf := new(bytes.Buffer)
	doctorCmd.SetOut(buf)
	doctorCmd.SetErr(buf)

	// runDoctor may return error if upstream google connectivity fails in offline environments,
	// but diagnostics report must always be produced.
	_ = doctorCmd.RunE(doctorCmd, []string{})

	out := buf.String()
	if !strings.Contains(out, "Running Antigravity Gateway environment diagnostics") {
		t.Errorf("expected diagnostics header in output, got: %s", out)
	}
	if !strings.Contains(out, "[Network Ports]") {
		t.Errorf("expected Network Ports section, got: %s", out)
	}
	if !strings.Contains(out, "[File System & Security]") {
		t.Errorf("expected File System & Security section, got: %s", out)
	}
	if !strings.Contains(out, "[Account Pool]") {
		t.Errorf("expected Account Pool section, got: %s", out)
	}
}

// TestDoctorReportsInvalidAPIKey verifies that doctor flags a stored gateway key whose shape no
// longer matches the generated format, without echoing the key value into the report.
func TestDoctorReportsInvalidAPIKey(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	origChecker := versionChecker
	versionChecker = func(ctx context.Context) (*updater.ReleaseInfo, error) {
		return &updater.ReleaseInfo{
			CurrentVersion:  config.Version,
			TargetVersion:   config.Version,
			UpdateAvailable: false,
		}, nil
	}
	defer func() { versionChecker = origChecker }()

	cfg := config.DefaultConfig()
	cfg.APIKey = "test-gateway-key-999"
	if err := config.Save(filepath.Join(tempHome, "config.json"), cfg); err != nil {
		t.Fatalf("seeding config file: %v", err)
	}

	buf := new(bytes.Buffer)
	doctorCmd.SetOut(buf)
	doctorCmd.SetErr(buf)
	_ = doctorCmd.RunE(doctorCmd, []string{})

	out := buf.String()
	if !strings.Contains(out, "Gateway API Key") {
		t.Errorf("expected Gateway API Key diagnostic, got: %s", out)
	}
	if !strings.Contains(out, "does not match the generated format") {
		t.Errorf("expected invalid key warning, got: %s", out)
	}
	if strings.Contains(out, "test-gateway-key-999") {
		t.Errorf("doctor must never render the raw API key, got: %s", out)
	}
}

// TestDoctorDetectsClientKeyMismatch verifies that doctor warns when a configured client stores a
// different gateway key than config.json, which is the direct cause of HTTP 401 responses.
func TestDoctorDetectsClientKeyMismatch(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	origChecker := versionChecker
	versionChecker = func(ctx context.Context) (*updater.ReleaseInfo, error) {
		return &updater.ReleaseInfo{
			CurrentVersion:  config.Version,
			TargetVersion:   config.Version,
			UpdateAvailable: false,
		}, nil
	}
	defer func() { versionChecker = origChecker }()

	gatewayKey, err := config.GenerateAPIKey()
	if err != nil {
		t.Fatalf("generating gateway key: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.APIKey = gatewayKey
	if err := config.Save(filepath.Join(tempHome, "config.json"), cfg); err != nil {
		t.Fatalf("seeding config file: %v", err)
	}

	piDir := t.TempDir()
	t.Setenv("PI_AGENT_DIR", piDir)
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	opencodePath := filepath.Join(t.TempDir(), "opencode.json")
	t.Setenv("OPENCODE_CONFIG", opencodePath)

	staleKey := "agw_" + strings.Repeat("b", config.APIKeyHexLen)
	mustWriteFile(t, filepath.Join(piDir, "models.json"), `{"providers":{"antigravity":{"apiKey":"`+staleKey+`"}}}`)
	mustWriteFile(t, opencodePath, `{"providers":{"antigravity":{"settings":{"apiKey":"`+staleKey+`"}}}}`)
	mustWriteFile(t, filepath.Join(claudeDir, "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:51400","ANTHROPIC_AUTH_TOKEN":"`+staleKey+`"}}`)

	buf := new(bytes.Buffer)
	doctorCmd.SetOut(buf)
	doctorCmd.SetErr(buf)
	_ = doctorCmd.RunE(doctorCmd, []string{})

	out := buf.String()
	for _, clientName := range []string{"Pi Coding Agent", "OpenCode CLI", "Claude Code CLI"} {
		if !strings.Contains(out, clientName) {
			t.Fatalf("expected %s diagnostic in output, got: %s", clientName, out)
		}
	}
	if got := strings.Count(out, "differs from config.json"); got < 2 {
		t.Errorf("expected mismatch warnings for each configured client, found %d: %s", got, out)
	}
	if strings.Contains(out, staleKey) || strings.Contains(out, gatewayKey) {
		t.Errorf("doctor must never render raw API keys, got: %s", out)
	}
}

// TestDoctorReportsReleaseUpdatesSurfacesUpdateNotification verifies that agw doctor warns
// when a newer release is available and suggests running 'agw update'.
func TestDoctorReportsReleaseUpdatesSurfacesUpdateNotification(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	origChecker := versionChecker
	versionChecker = func(ctx context.Context) (*updater.ReleaseInfo, error) {
		return &updater.ReleaseInfo{
			CurrentVersion:  config.Version,
			TargetVersion:   "0.2.0",
			UpdateAvailable: true,
		}, nil
	}
	defer func() { versionChecker = origChecker }()

	buf := new(bytes.Buffer)
	doctorCmd.SetOut(buf)
	doctorCmd.SetErr(buf)
	_ = doctorCmd.RunE(doctorCmd, []string{})

	out := buf.String()
	if !strings.Contains(out, "[Release & Updates]") {
		t.Errorf("expected [Release & Updates] category in doctor report, got: %s", out)
	}
	if !strings.Contains(out, "newer release v0.2.0 is available") {
		t.Errorf("expected update notification in doctor report, got: %s", out)
	}
	if !strings.Contains(out, "Run 'agw update' to upgrade in-place automatically") {
		t.Errorf("expected update hint in doctor report, got: %s", out)
	}
}

// mustWriteFile writes fixture content to path, failing the test when the write cannot complete.
func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing fixture %s: %v", path, err)
	}
}

// TestConnectivityAddressFamiliesAndProxy verifies per-family reachability reporting, the daily Cloud
// Code host probe, and credential masking for a configured HTTPS proxy.
func TestConnectivityAddressFamiliesAndProxy(t *testing.T) {
	origDialer := connectivityDialer
	connectivityDialer = func(ctx context.Context, host string) (time.Duration, error) {
		return 4 * time.Millisecond, nil
	}
	defer func() { connectivityDialer = origDialer }()

	origResolver := addressFamilyResolver
	addressFamilyResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("192.0.2.10")}, {IP: net.ParseIP("2001:db8::10")}}, nil
	}
	defer func() { addressFamilyResolver = origResolver }()

	origFamilyDialer := familyDialer
	familyDialer = func(ctx context.Context, network, host string) (time.Duration, error) {
		if network == "tcp6" {
			return 3 * time.Millisecond, errors.New("network is unreachable")
		}
		return 2 * time.Millisecond, nil
	}
	defer func() { familyDialer = origFamilyDialer }()

	t.Setenv("HTTPS_PROXY", "http://proxy-user:s3cr3t-proxy-pass@proxy.internal:8080")

	items := checkConnectivity(t.Context())

	var dailyFound, ipv4OK, ipv6Warn, proxyFound bool
	for _, item := range items {
		switch item.Name {
		case "Cloud Code Assist Daily Endpoint":
			dailyFound = item.Status == StatusOK
		case "Cloud Code Assist Daily Endpoint over IPv4":
			ipv4OK = item.Status == StatusOK
		case "Cloud Code Assist Daily Endpoint over IPv6":
			ipv6Warn = item.Status == StatusWarn && strings.Contains(item.Hint, "AGW_PREFER_IPV4")
		case "HTTPS Proxy":
			proxyFound = item.Status == StatusOK
			if strings.Contains(item.Message, "s3cr3t-proxy-pass") || strings.Contains(item.Message, "proxy-user") {
				t.Errorf("proxy diagnostics leaked credentials: %s", item.Message)
			}
			if !strings.Contains(item.Message, "proxy.internal:8080") {
				t.Errorf("expected proxy host reported, got %s", item.Message)
			}
		}
	}

	if !dailyFound {
		t.Error("expected daily-cloudcode-pa endpoint to be probed")
	}
	if !ipv4OK {
		t.Error("expected IPv4 family probe to report reachability")
	}
	if !ipv6Warn {
		t.Error("expected unreachable IPv6 family to warn with the IPv4 dial hint")
	}
	if !proxyFound {
		t.Error("expected HTTPS proxy configuration to be reported")
	}
}
