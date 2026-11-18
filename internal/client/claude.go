// Package client provides automated configuration injectors and diagnostic tools
// for local AI coding agents such as Pi, OpenCode, and Claude Code.
package client

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultClaudeModel specifies the primary model configured for Claude Code CLI.
const DefaultClaudeModel = "claude-sonnet-4-6"

// ClaudeSetupOptions encapsulates parameters for configuring Claude Code CLI (~/.claude/settings.json).
type ClaudeSetupOptions struct {
	// SettingsPath overrides the destination settings.json file path (defaults to ~/.claude/settings.json or $CLAUDE_CONFIG_DIR).
	SettingsPath string

	// GatewayURL is the base HTTP endpoint of the local gateway proxy (e.g. http://127.0.0.1:51400).
	GatewayURL string

	// APIKey is the Bearer key required to authenticate against the gateway.
	APIKey string

	// Model specifies the default ANTHROPIC_MODEL configured in settings.json.
	Model string

	// NoVerify when true skips the post-setup live completion test probe.
	NoVerify bool

	// DryRun when true generates configuration without modifying disk.
	DryRun bool
}

// ClaudeSetupResult contains execution results, backup paths, and verification outcomes for Claude CLI setup.
type ClaudeSetupResult struct {
	// SettingsPath is the target settings.json path.
	SettingsPath string

	// BackupPath is the timestamped backup created before mutation.
	BackupPath string

	// WrittenJSON contains the serialized JSON payload for dry-run inspection.
	WrittenJSON string

	// TestResult holds the outcome of the 1-token test completion probe.
	TestResult *TestResult
}

// DefaultClaudeSettingsPath resolves ~/.claude/settings.json respecting $CLAUDE_CONFIG_DIR and $HOME.
func DefaultClaudeSettingsPath() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving user home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// SetupClaude configures Claude Code CLI's settings.json to route requests through Antigravity Gateway.
// It creates a timestamped backup before modifying disk and verifies connectivity with a 1-token probe.
func SetupClaude(ctx context.Context, opts ClaudeSetupOptions) (*ClaudeSetupResult, error) {
	settingsPath := opts.SettingsPath
	if settingsPath == "" {
		var err error
		settingsPath, err = DefaultClaudeSettingsPath()
		if err != nil {
			return nil, err
		}
	}

	gatewayURL := strings.TrimRight(opts.GatewayURL, "/")
	if gatewayURL == "" {
		gatewayURL = "http://127.0.0.1:51400"
	}

	apiKey := cmp.Or(opts.APIKey, "test")
	model := cmp.Or(opts.Model, DefaultClaudeModel)

	settingsMap := make(map[string]any)
	if existingBytes, err := os.ReadFile(settingsPath); err == nil && len(existingBytes) > 0 {
		if unmarshalErr := json.Unmarshal(existingBytes, &settingsMap); unmarshalErr != nil {
			return nil, fmt.Errorf("parsing existing claude settings %s: %w", settingsPath, unmarshalErr)
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("reading existing claude settings %s: %w", settingsPath, err)
	}

	envMap, ok := settingsMap["env"].(map[string]any)
	if !ok || envMap == nil {
		envMap = make(map[string]any)
	}

	envMap["ANTHROPIC_BASE_URL"] = gatewayURL
	envMap["ANTHROPIC_AUTH_TOKEN"] = apiKey
	envMap["ANTHROPIC_MODEL"] = model
	envMap["ANTHROPIC_DEFAULT_OPUS_MODEL"] = "claude-opus-4-6-thinking"
	envMap["ANTHROPIC_DEFAULT_SONNET_MODEL"] = model
	envMap["ANTHROPIC_DEFAULT_HAIKU_MODEL"] = model
	envMap["CLAUDE_CODE_SUBAGENT_MODEL"] = model
	envMap["ENABLE_EXPERIMENTAL_MCP_CLI"] = "true"

	settingsMap["env"] = envMap

	payload, _ := json.MarshalIndent(settingsMap, "", "  ")
	res := &ClaudeSetupResult{
		SettingsPath: settingsPath,
		WrittenJSON:  string(payload),
	}

	if !opts.DryRun {
		backup, err := BackupFile(settingsPath, "")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("creating backup: %w", err)
		}
		res.BackupPath = backup

		if err := AtomicWriteJSON(settingsPath, settingsMap, 0o600); err != nil {
			return nil, fmt.Errorf("writing settings to %s: %w", settingsPath, err)
		}
	}

	if !opts.DryRun && !opts.NoVerify {
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		testRes, err := TestAnthropicCompletion(probeCtx, gatewayURL, apiKey, model)
		if err != nil {
			return res, fmt.Errorf("connectivity verification failed: %w", err)
		}
		res.TestResult = testRes
	}

	return res, nil
}
