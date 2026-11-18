// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestRootCommandHelp verifies that the root agw command renders CLI usage instructions.
func TestRootCommandHelp(t *testing.T) {
	buf := new(bytes.Buffer)
	rootCmd.SetOut(buf)
	rootCmd.SetErr(buf)
	rootCmd.SetArgs([]string{"--help"})

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("rootCmd.Execute() error: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Antigravity Gateway") {
		t.Errorf("expected root command help to contain 'Antigravity Gateway', got: %s", output)
	}
}

// TestVersionCommand verifies that agw version outputs the compiled semantic version string.
func TestVersionCommand(t *testing.T) {
	buf := new(bytes.Buffer)
	versionCmd.SetOut(buf)
	versionCmd.SetErr(buf)
	versionCmd.SetArgs([]string{})

	versionCmd.Run(versionCmd, []string{})

	output := buf.String()
	expected := "agw version " + config.Version + "\n"
	if output != expected {
		t.Errorf("version output = %q, expected %q", output, expected)
	}

	// Verify that the version string is populated and non-empty.
	if strings.TrimSpace(config.Version) == "" {
		t.Error("expected non-empty config.Version")
	}
}

// TestGlobalConfigFlagRouting verifies that setting cfgFile through the global --config flag
// causes loadConfig to load the specified custom configuration file instead of default locations.
func TestGlobalConfigFlagRouting(t *testing.T) {
	tempDir := t.TempDir()
	customPath := tempDir + "/custom-config.json"

	customCfg := config.DefaultConfig()
	customCfg.Port = 59999
	customCfg.Strategy = "hybrid"
	if err := config.Save(customPath, customCfg); err != nil {
		t.Fatalf("saving custom config: %v", err)
	}

	origCfgFile := cfgFile
	defer func() { cfgFile = origCfgFile }()

	cfgFile = customPath
	loaded, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig with --config flag failed: %v", err)
	}

	if loaded.Port != 59999 {
		t.Errorf("expected loaded port 59999 from custom config, got %d", loaded.Port)
	}
	if loaded.Strategy != "hybrid" {
		t.Errorf("expected loaded strategy 'hybrid', got %s", loaded.Strategy)
	}
}
