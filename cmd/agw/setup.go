// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/client"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/spf13/cobra"
)

var (
	// setupURL overrides the gateway target URL during client configuration.
	setupURL string

	// setupAPIKey overrides the Bearer API key injected into client configuration files.
	setupAPIKey string

	// setupModelsFilter filters registered models using glob patterns.
	setupModelsFilter string

	// setupNoVerify disables the 1-token connectivity verification probe.
	setupNoVerify bool

	// setupDryRun outputs generated configurations without modifying files on disk.
	setupDryRun bool

	// setupPiDir specifies a custom ~/.pi/agent directory path.
	setupPiDir string

	// setupPiCleanLegacy enables removing old legacy proxy blocks from Pi configuration.
	setupPiCleanLegacy bool

	// setupOpenCodePath specifies a custom opencode.json path.
	setupOpenCodePath string

	// setupClaudeSettingsPath specifies a custom Claude CLI settings.json path.
	setupClaudeSettingsPath string

	// setupClaudeModel specifies the model configured for Claude Code CLI.
	setupClaudeModel string

	// setupCmd registers gateway settings into local AI coding agent configurations.
	setupCmd = &cobra.Command{
		Use:   "setup",
		Short: "Configure AI coding agents to use Antigravity Gateway",
		Long: `Setup automatically registers Antigravity Gateway as an OpenAI-compatible
model provider in your coding agent configurations (Pi Coding Agent, OpenCode, or Claude Code CLI).
It preserves existing custom settings, creates timestamped backups of existing files,
and performs a quick 1-token connectivity test against the local gateway.

Other OpenAI-compatible clients (such as Cursor) can point directly at
http://127.0.0.1:51400/v1 using the local API key from ~/.agw/config.json.`,
		Example: `  agw setup pi
  agw setup pi --models "gemini-*,!*-low"
  agw setup opencode --dry-run
  agw setup claude --model claude-sonnet-5-5-high`,
	}

	// setupPiCmd configures Pi Coding Agent's models.json and auth.json.
	setupPiCmd = &cobra.Command{
		Use:   "pi",
		Short: "Configure Pi Coding Agent (~/.pi/agent/models.json)",
		Long: `Pi configures Pi Coding Agent to use the local Antigravity Gateway reverse proxy.
It adds the "antigravity" provider block into ~/.pi/agent/models.json, backs up the previous
configuration, and tests connectivity by sending a 1-token chat completion.`,
		Example: `  agw setup pi
  agw setup pi --models "gemini-*,!*-low"
  agw setup pi --dry-run`,
		RunE: runSetupPi,
	}

	// setupOpenCodeCmd configures OpenCode's opencode.json file.
	setupOpenCodeCmd = &cobra.Command{
		Use:   "opencode",
		Short: "Configure OpenCode (~/.config/opencode/opencode.json)",
		Long: `OpenCode configures the OpenCode CLI to use the local Antigravity Gateway reverse proxy.
It registers the "@opencode/ai/providers/openai-compatible" provider block into
~/.config/opencode/opencode.json, backs up previous configuration, and verifies connectivity.`,
		Example: `  agw setup opencode
  agw setup opencode --dry-run`,
		RunE: runSetupOpenCode,
	}

	// setupClaudeCmd configures Claude Code CLI's settings.json file.
	setupClaudeCmd = &cobra.Command{
		Use:   "claude",
		Short: "Configure Claude Code CLI (~/.claude/settings.json)",
		Long: `Claude configures Claude Code CLI to route requests through Antigravity Gateway's
Anthropic Messages API endpoint (/v1/messages). It registers ANTHROPIC_BASE_URL and
ANTHROPIC_AUTH_TOKEN into ~/.claude/settings.json and verifies connectivity.`,
		Example: `  agw setup claude
  agw setup claude --model claude-sonnet-5-5-high`,
		RunE: runSetupClaude,
	}
)

func init() {
	setupCmd.PersistentFlags().StringVar(&setupURL, "url", "", "Antigravity Gateway base URL (default from config or http://127.0.0.1:51400)")
	setupCmd.PersistentFlags().StringVar(&setupAPIKey, "api-key", "", "API key override (default is from ~/.agw/config.json)")
	setupCmd.PersistentFlags().StringVar(&setupModelsFilter, "models", "", "Filter models by comma-separated patterns (e.g. 'gemini-*,!*-low')")
	setupCmd.PersistentFlags().BoolVar(&setupNoVerify, "no-verify", false, "Skip 1-token connectivity test")
	setupCmd.PersistentFlags().BoolVar(&setupDryRun, "dry-run", false, "Output generated configuration without writing to disk")

	setupPiCmd.Flags().StringVar(&setupPiDir, "pi-dir", "", "Pi configuration directory (default ~/.pi/agent or $PI_AGENT_DIR)")
	setupPiCmd.Flags().BoolVar(&setupPiCleanLegacy, "clean-legacy", true, "Clean legacy loopback proxy entries and placeholder tokens")

	setupOpenCodeCmd.Flags().StringVar(&setupOpenCodePath, "config-file", "", "OpenCode configuration file (default ~/.config/opencode/opencode.json)")

	setupClaudeCmd.Flags().StringVar(&setupClaudeSettingsPath, "settings", "", "Claude settings file (default ~/.claude/settings.json)")
	setupClaudeCmd.Flags().StringVar(&setupClaudeModel, "model", client.DefaultClaudeModel, "Default model configured for Claude CLI")

	setupCmd.AddCommand(setupPiCmd)
	setupCmd.AddCommand(setupOpenCodeCmd)
	setupCmd.AddCommand(setupClaudeCmd)
	rootCmd.AddCommand(setupCmd)
}

// resolveGatewayEndpoint determines the target gateway URL and API key from flags, running daemon, or
// config file. An explicitly supplied --api-key is validated up front so a malformed key fails the
// command instead of being written into every client configuration.
func resolveGatewayEndpoint(cfg *config.Config) (string, string, error) {
	baseURL := setupURL
	if baseURL == "" {
		host := config.LoopbackHost(cfg.Host)
		port := cfg.Port
		if port <= 0 {
			port = config.DefaultPort
		}
		baseURL = config.BaseURL(host, port)
	}
	baseURL = strings.TrimRight(baseURL, "/")

	apiKey := cmp.Or(setupAPIKey, cfg.APIKey)
	if setupAPIKey != "" {
		if err := config.ValidateAPIKey(setupAPIKey); err != nil {
			return "", "", fmt.Errorf("invalid --api-key: %w", err)
		}
	}
	return baseURL, apiKey, nil
}

// loadAvailableModels retrieves models from the running daemon, local cache, or family defaults.
func loadAvailableModels(ctx context.Context, cfg *config.Config, baseURL string, apiKey string) ([]catalog.CatalogModel, error) {
	// First: try live probe from running gateway
	modelsEndpoint := baseURL + "/v1/models"
	probeReq, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsEndpoint, nil)
	if err == nil {
		if apiKey != "" {
			probeReq.Header.Set("Authorization", "Bearer "+apiKey)
		}
		// Probe clients never follow redirects so the local Authorization/x-api-key headers can never be
		// forwarded to a cross-origin location.
		httpClient := client.NewNoRedirectClient(3 * time.Second)
		resp, probeErr := httpClient.Do(probeReq)
		if probeErr == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var envelope struct {
					Data []struct {
						ID            string `json:"id"`
						ContextWindow int    `json:"context_window"`
						Meta          struct {
							DisplayName     string `json:"display_name"`
							Thinking        bool   `json:"thinking"`
							Multimodal      bool   `json:"multimodal"`
							MaxOutputTokens int    `json:"max_output_tokens"`
						} `json:"meta"`
					} `json:"data"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&envelope); err == nil && len(envelope.Data) > 0 {
					var models []catalog.CatalogModel
					for _, d := range envelope.Data {
						models = append(models, catalog.CatalogModel{
							ID:              d.ID,
							DisplayName:     d.Meta.DisplayName,
							ContextWindow:   d.ContextWindow,
							MaxOutputTokens: d.Meta.MaxOutputTokens,
							Thinking:        d.Meta.Thinking,
							Images:          d.Meta.Multimodal,
						})
					}
					return models, nil
				}
			}
		}
	}

	// Second: fallback to cached models
	cachePath, err := config.ModelsCacheFilePath()
	if err == nil {
		cat := catalog.NewModelCatalog(cfg, nil, cachePath, nil)
		if loadErr := cat.LoadCache(); loadErr == nil {
			snap, _ := cat.Snapshot()
			if len(snap.Models) > 0 {
				return snap.Models, nil
			}
		}
	}

	// Third: provide the bundled offline model manifest if uninitialized
	return catalog.FallbackModels(), nil
}

// runSetupPi executes configuration injection and legacy cleanup for the Pi Coding Agent.
func runSetupPi(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	baseURL, apiKey, err := resolveGatewayEndpoint(cfg)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	models, err := loadAvailableModels(ctx, cfg, baseURL, apiKey)
	if err != nil {
		return fmt.Errorf("loading available models: %w", err)
	}

	piDir := setupPiDir
	if piDir == "" {
		piDir, err = client.DefaultPiDir()
		if err != nil {
			return fmt.Errorf("resolving pi directory: %w", err)
		}
	}

	opts := client.PiSetupOptions{
		PiDir:       piDir,
		GatewayURL:  baseURL,
		APIKey:      apiKey,
		Models:      models,
		ModelFilter: setupModelsFilter,
		NoVerify:    setupNoVerify,
		DryRun:      setupDryRun,
		CleanLegacy: setupPiCleanLegacy,
	}

	res, err := client.SetupPi(ctx, opts)
	if err != nil {
		return err
	}

	if setupDryRun {
		cmd.Println(redactSetupJSON(res.WrittenJSON, apiKey))
		return nil
	}

	cmd.Printf("✓ Configured Pi Coding Agent with %d model(s)\n", res.ModelCount)
	cmd.Printf("  Target config: %s\n", res.ModelsPath)
	if res.BackupPath != "" {
		cmd.Printf("  Backup created: %s\n", filepath.Base(res.BackupPath))
	}
	for _, item := range res.CleanedItems {
		cmd.Printf("  Cleaned: %s\n", item)
	}

	if res.TestResult != nil && res.TestResult.Success {
		cmd.Printf("✓ Verified end-to-end connectivity (%s in %v)\n", res.TestResult.Model, res.TestResult.Duration.Round(time.Millisecond))
	}

	cmd.Println("\nRestart Pi Coding Agent to load the new \"antigravity\" models.")
	return nil
}

// runSetupOpenCode executes configuration injection for the OpenCode agent.
func runSetupOpenCode(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	baseURL, apiKey, err := resolveGatewayEndpoint(cfg)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	models, err := loadAvailableModels(ctx, cfg, baseURL, apiKey)
	if err != nil {
		return fmt.Errorf("loading available models: %w", err)
	}

	cfgPath := setupOpenCodePath
	if cfgPath == "" {
		cfgPath, err = client.DefaultOpenCodeConfigPath()
		if err != nil {
			return fmt.Errorf("resolving opencode config path: %w", err)
		}
	}

	opts := client.OpenCodeSetupOptions{
		ConfigPath:  cfgPath,
		GatewayURL:  baseURL,
		APIKey:      apiKey,
		Models:      models,
		ModelFilter: setupModelsFilter,
		NoVerify:    setupNoVerify,
		DryRun:      setupDryRun,
	}

	res, err := client.SetupOpenCode(ctx, opts)
	if err != nil {
		return err
	}

	if setupDryRun {
		cmd.Println(redactSetupJSON(res.WrittenJSON, apiKey))
		return nil
	}

	cmd.Printf("✓ Configured OpenCode with %d model(s)\n", res.ModelCount)
	cmd.Printf("  Target config: %s\n", res.ConfigPath)
	if res.BackupPath != "" {
		cmd.Printf("  Backup created: %s\n", filepath.Base(res.BackupPath))
	}

	if res.TestResult != nil && res.TestResult.Success {
		cmd.Printf("✓ Verified end-to-end connectivity (%s in %v)\n", res.TestResult.Model, res.TestResult.Duration.Round(time.Millisecond))
	}

	cmd.Println("\nRun \"opencode\" and select an antigravity model under /models.")
	return nil
}

// runSetupClaude executes configuration injection for Claude Code CLI.
func runSetupClaude(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	baseURL, apiKey, err := resolveGatewayEndpoint(cfg)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	settingsPath := setupClaudeSettingsPath
	if settingsPath == "" {
		settingsPath, err = client.DefaultClaudeSettingsPath()
		if err != nil {
			return fmt.Errorf("resolving claude settings path: %w", err)
		}
	}

	opts := client.ClaudeSetupOptions{
		SettingsPath: settingsPath,
		GatewayURL:   baseURL,
		APIKey:       apiKey,
		Model:        setupClaudeModel,
		NoVerify:     setupNoVerify,
		DryRun:       setupDryRun,
	}

	res, err := client.SetupClaude(ctx, opts)
	if err != nil {
		return err
	}

	if setupDryRun {
		cmd.Println(redactSetupJSON(res.WrittenJSON, apiKey))
		return nil
	}

	cmd.Println("✓ Configured Claude Code CLI (~/.claude/settings.json)")
	cmd.Printf("  Target config: %s\n", res.SettingsPath)
	if res.BackupPath != "" {
		cmd.Printf("  Backup created: %s\n", filepath.Base(res.BackupPath))
	}

	if res.TestResult != nil && res.TestResult.Success {
		cmd.Printf("✓ Verified end-to-end connectivity (%s in %v)\n", res.TestResult.Model, res.TestResult.Duration.Round(time.Millisecond))
	}

	cmd.Println("\nRun \"claude\" in your terminal to use Antigravity Gateway.")
	return nil
}

// redactSetupJSON masks sensitive API keys and tokens in configuration JSON before printing to stdout.
// Masking never depends on the secret length: a short placeholder token is still a credential, and a
// partially printed key is a leak.
func redactSetupJSON(raw string, gatewayKey string) string {
	if gatewayKey != "" {
		raw = strings.ReplaceAll(raw, gatewayKey, "agw_••••••••")
	}
	re := regexp.MustCompile(`("(?:apiKey|api_key|ANTHROPIC_AUTH_TOKEN|ANTHROPIC_API_KEY|OPENAI_API_KEY|token)"):\s*"([^"]*)"`)
	raw = re.ReplaceAllStringFunc(raw, func(match string) string {
		sub := re.FindStringSubmatch(match)
		if len(sub) != 3 || sub[2] == "" {
			return match
		}
		return fmt.Sprintf(`%s: "<redacted:%d chars>"`, sub[1], len(sub[2]))
	})
	return raw
}
