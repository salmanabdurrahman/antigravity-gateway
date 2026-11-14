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

	// Also verify Version constant matches config.Version
	if config.Version != "0.1.0" {
		t.Errorf("expected version 0.1.0, got %s", config.Version)
	}
}
