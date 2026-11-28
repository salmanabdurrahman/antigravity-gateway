// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestConfigShowReportsSuspensionPause verifies that the redacted configuration report exposes the
// effective post-suspension pause window, including the disabled state, so operators can confirm the
// protection without reading config.json or source code.
func TestConfigShowReportsSuspensionPause(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())

	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.SuspendPauseSeconds = 120
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config: %v", err)
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
	if out := render(); !strings.Contains(out, "Suspension Pause:      120s") {
		t.Errorf("expected the configured pause window in the report, got:\n%s", out)
	}

	cfg.DisableSuspendPause = true
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving disabled-pause config: %v", err)
	}
	if out := render(); !strings.Contains(out, "Suspension Pause:      disabled") {
		t.Errorf("expected the disabled pause state in the report, got:\n%s", out)
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
	if !view.SuspendPauseDisabled {
		t.Error("suspend_pause_disabled = false; want true")
	}
	if view.SuspendPauseSeconds != 0 {
		t.Errorf("suspend_pause_seconds = %d; want 0 while the pause is disabled", view.SuspendPauseSeconds)
	}
}
