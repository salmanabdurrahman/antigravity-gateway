// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestConfigShowReportsOperatingProfile verifies that the redacted configuration report exposes the
// resolved operating profile, rotation mode, and catalog freshness so operators can confirm the safe
// defaults without reading config.json.
func TestConfigShowReportsOperatingProfile(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())

	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	if err := config.Save(cfgPath, config.DefaultConfig()); err != nil {
		t.Fatalf("saving default config: %v", err)
	}

	previousJSON := configJSON
	t.Cleanup(func() {
		configJSON = previousJSON
		configShowCmd.SetOut(nil)
		configShowCmd.SetErr(nil)
		configShowCmd.SetContext(nil)
	})

	render := func() string {
		t.Helper()

		buf := new(bytes.Buffer)
		configShowCmd.SetOut(buf)
		configShowCmd.SetErr(buf)
		configShowCmd.SetContext(t.Context())
		if err := configShowCmd.RunE(configShowCmd, nil); err != nil {
			t.Fatalf("configShowCmd.RunE failed: %v", err)
		}
		return buf.String()
	}

	configJSON = false
	out := render()
	for _, want := range []string{
		"Profile:               safe",
		"Rotation Mode:         single",
		"Catalog Refresh:       60m",
		"Max In Flight:         2 per account",
		"Prompt Rejection Guard: 3 bare 429s / 900s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("config show output missing %q:\n%s", want, out)
		}
	}

	manual := config.DefaultConfig()
	manual.Profile = config.ProfileManual
	if err := config.Save(cfgPath, manual); err != nil {
		t.Fatalf("saving manual profile config: %v", err)
	}
	if out := render(); !strings.Contains(out, "Profile:               manual") ||
		!strings.Contains(out, "Rotation Mode:         pool") ||
		!strings.Contains(out, "Max In Flight:         unbounded") ||
		!strings.Contains(out, "Prompt Rejection Guard: off") {
		t.Errorf("manual profile was not reported with its legacy pacing posture:\n%s", out)
	}

	configJSON = true
	buf := new(bytes.Buffer)
	configShowCmd.SetOut(buf)
	configShowCmd.SetErr(buf)
	configShowCmd.SetContext(t.Context())
	if err := configShowCmd.RunE(configShowCmd, nil); err != nil {
		t.Fatalf("configShowCmd.RunE failed for JSON output: %v", err)
	}

	var view redactedConfigView
	if err := json.Unmarshal(buf.Bytes(), &view); err != nil {
		t.Fatalf("decoding config JSON: %v\n%s", err, buf.String())
	}
	if view.Profile != config.ProfileManual {
		t.Errorf("profile = %q; want %q", view.Profile, config.ProfileManual)
	}
	if view.RotationMode != config.RotationModePool {
		t.Errorf("rotation_mode = %q; want %q", view.RotationMode, config.RotationModePool)
	}
	if view.CatalogMaxAgeMinutes != config.LegacyCatalogMaxAgeMinutes {
		t.Errorf("catalog_max_age_minutes = %d; want %d", view.CatalogMaxAgeMinutes, config.LegacyCatalogMaxAgeMinutes)
	}
}
