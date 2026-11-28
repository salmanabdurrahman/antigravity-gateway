// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
	"github.com/spf13/cobra"
)

var (
	// configJSON outputs redacted configuration values as structured JSON.
	configJSON bool

	// configCmd inspects and updates gateway configuration stored in config.json.
	configCmd = &cobra.Command{
		Use:   "config",
		Short: "Inspect and update gateway configuration",
		Long: `Config displays redacted gateway settings from ~/.agw/config.json (or --config),
reports the active configuration file path, and validates or updates the persisted
account rotation strategy without exposing API keys or OAuth client secrets.`,
		Example: `  agw config show
  agw config show --json
  agw config path
  agw config strategy hybrid`,
		RunE: runConfigShow,
	}

	// configShowCmd explicitly prints the redacted gateway configuration.
	configShowCmd = &cobra.Command{
		Use:     "show",
		Short:   "Display redacted gateway settings",
		Long:    `Show loads the active configuration file and prints non-sensitive settings with secrets masked.`,
		Example: `  agw config show`,
		RunE:    runConfigShow,
	}

	// configPathCmd prints the resolved path of the active configuration file.
	configPathCmd = &cobra.Command{
		Use:     "path",
		Short:   "Print the resolved config.json file path",
		Long:    `Path resolves and prints the configuration file path honoring --config and $AGW_HOME.`,
		Example: `  agw config path`,
		RunE:    runConfigPath,
	}

	// configStrategyCmd inspects or persists the rotation strategy in config.json.
	configStrategyCmd = &cobra.Command{
		Use:   "strategy [sticky|round-robin|hybrid]",
		Short: "Get or set the persisted account rotation strategy",
		Long: `Strategy prints the persisted rotation strategy when called without arguments, or
updates only the "strategy" field in config.json while keeping all other settings intact.`,
		Example: `  agw config strategy
  agw config strategy hybrid`,
		Args: cobra.MaximumNArgs(1),
		RunE: runConfigStrategy,
	}
)

func init() {
	configCmd.PersistentFlags().BoolVar(&configJSON, "json", false, "Output configuration details as JSON")
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configPathCmd)
	configCmd.AddCommand(configStrategyCmd)
	rootCmd.AddCommand(configCmd)
}

// redactedConfigView holds safe, secret-redacted configuration values for CLI and JSON display.
type redactedConfigView struct {
	// Path is the resolved configuration file path on disk.
	Path string `json:"path,omitempty"`

	// Endpoint is the clamped local loopback URL where the gateway listens.
	Endpoint string `json:"endpoint"`

	// Host is the configured loopback host address.
	Host string `json:"host"`

	// Port is the configured TCP listening port.
	Port int `json:"port"`

	// Strategy is the persisted account rotation strategy.
	Strategy string `json:"strategy"`

	// Profile is the resolved operating profile ("safe" or "manual").
	Profile string `json:"profile"`

	// RotationMode is the resolved rotation mode ("single" or "pool").
	RotationMode string `json:"rotation_mode"`

	// CatalogMaxAgeMinutes is the effective catalog freshness window in minutes.
	CatalogMaxAgeMinutes int `json:"catalog_max_age_minutes"`

	// APIKeyStatus reports whether the local API key is configured and valid without revealing it.
	APIKeyStatus string `json:"api_key_status"`

	// ClientSecretStatus reports whether a custom or default OAuth client secret is present.
	ClientSecretStatus string `json:"client_secret_status"`

	// QuotaThreshold is the hybrid remaining-quota floor before parking an account.
	QuotaThreshold float64 `json:"quota_threshold"`

	// MaxWaitSeconds is the maximum cooldown absorption wait in seconds.
	MaxWaitSeconds int `json:"max_wait_seconds"`

	// DefaultCooldownSeconds is the base 429 cooldown duration in seconds.
	DefaultCooldownSeconds int `json:"default_cooldown_seconds"`

	// EmptyStreamRetries is the retry budget for empty upstream streams.
	EmptyStreamRetries int `json:"empty_stream_retries"`

	// SuspendPauseSeconds is the effective window that pauses all upstream dispatch after a terminal
	// account-suspension verdict, zero when the protection window is disabled.
	SuspendPauseSeconds int `json:"suspend_pause_seconds"`

	// SuspendPauseDisabled reports whether the post-suspension protection window is turned off.
	SuspendPauseDisabled bool `json:"suspend_pause_disabled"`

	// MaxInFlightPerAccount is the resolved per-credential concurrency ceiling, zero when unbounded.
	MaxInFlightPerAccount int `json:"max_in_flight_per_account"`

	// MaxDispatchesPerRequest is the explicit per-request dispatch budget, zero when the profile decides.
	MaxDispatchesPerRequest int `json:"max_dispatches_per_request"`

	// PromptRejectionLimit is the resolved bare-rejection threshold, zero when the guard is off.
	PromptRejectionLimit int `json:"prompt_rejection_limit"`

	// PromptRejectionCooldownSeconds is the resolved park duration for a rejection storm.
	PromptRejectionCooldownSeconds int `json:"prompt_rejection_cooldown_seconds"`

	// PromptPreflightEnabled reports whether harness-fingerprint rewrites are applied to instructions.
	PromptPreflightEnabled bool `json:"prompt_preflight_enabled"`

	// Endpoints lists configured upstream Cloud Code Assist base URLs.
	Endpoints []string `json:"endpoints,omitempty"`

	// ModelMapping maps custom model aliases to upstream model IDs.
	ModelMapping map[string]string `json:"model_mapping,omitempty"`
}

// buildRedactedConfigView creates a secret-free summary of cfg at path.
func buildRedactedConfigView(path string, cfg *config.Config) redactedConfigView {
	host := config.LoopbackHost(cfg.Host)
	port := cmp.Or(cfg.Port, config.DefaultPort)

	apiKeyStatus := "<empty>"
	if cfg.APIKey != "" {
		if err := config.ValidateAPIKey(cfg.APIKey); err == nil {
			apiKeyStatus = fmt.Sprintf("configured (%s••••••••, valid)", config.APIKeyPrefix)
		} else {
			apiKeyStatus = "configured (custom format)"
		}
	}

	secretStatus := "<empty>"
	if cfg.ClientSecret != "" {
		secretStatus = "configured (redacted)"
	}

	var mapping map[string]string
	if len(cfg.ModelMapping) > 0 {
		mapping = maps.Clone(cfg.ModelMapping)
	}

	promptRejectionLimit, promptRejectionCooldown := cfg.PromptRejectionGuard()

	return redactedConfigView{
		Path:                           path,
		Endpoint:                       config.BaseURL(host, port),
		Host:                           host,
		Port:                           port,
		Strategy:                       cmp.Or(cfg.Strategy, config.DefaultStrategy),
		Profile:                        cfg.EffectiveProfile(),
		RotationMode:                   cfg.EffectiveRotationMode(),
		CatalogMaxAgeMinutes:           int(cfg.CatalogMaxAge() / time.Minute),
		APIKeyStatus:                   apiKeyStatus,
		ClientSecretStatus:             secretStatus,
		QuotaThreshold:                 cfg.QuotaThreshold,
		MaxWaitSeconds:                 int(cfg.MaxWaitDuration() / time.Second),
		DefaultCooldownSeconds:         int(cfg.CooldownDuration() / time.Second),
		EmptyStreamRetries:             cfg.EmptyStreamRetryCount(),
		SuspendPauseSeconds:            int(cfg.SuspendPauseDuration() / time.Second),
		SuspendPauseDisabled:           cfg.DisableSuspendPause,
		MaxInFlightPerAccount:          cfg.MaxInFlightPerAccountLimit(),
		MaxDispatchesPerRequest:        cfg.MaxDispatchesPerRequest,
		PromptRejectionLimit:           promptRejectionLimit,
		PromptRejectionCooldownSeconds: int(promptRejectionCooldown / time.Second),
		PromptPreflightEnabled:         !cfg.DisablePromptPreflight,
		Endpoints:                      slices.Clone(cfg.Endpoints),
		ModelMapping:                   mapping,
	}
}

// runConfigShow loads the active configuration and renders redacted settings.
func runConfigShow(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	view := buildRedactedConfigView(activeConfigPath(), cfg)
	if configJSON {
		return writeJSONOutput(cmd, view)
	}

	if view.Path != "" {
		cmd.Printf("Config File:           %s\n", view.Path)
	}
	cmd.Printf("Gateway Endpoint:      %s\n", view.Endpoint)
	cmd.Printf("Strategy:              %s\n", view.Strategy)
	cmd.Printf("Profile:               %s\n", view.Profile)
	cmd.Printf("Rotation Mode:         %s\n", view.RotationMode)
	cmd.Printf("API Key:               %s\n", view.APIKeyStatus)
	cmd.Printf("OAuth Client Secret:   %s\n", view.ClientSecretStatus)
	cmd.Printf("Quota Threshold:       %.0f%%\n", view.QuotaThreshold*100)
	cmd.Printf("Max Cooldown Wait:     %ds\n", view.MaxWaitSeconds)
	cmd.Printf("Default 429 Cooldown:  %ds\n", view.DefaultCooldownSeconds)
	cmd.Printf("Empty Stream Retries:  %d\n", view.EmptyStreamRetries)
	cmd.Printf("Catalog Refresh:       %dm\n", view.CatalogMaxAgeMinutes)
	if view.SuspendPauseDisabled {
		cmd.Printf("Suspension Pause:      disabled\n")
	} else {
		cmd.Printf("Suspension Pause:      %ds\n", view.SuspendPauseSeconds)
	}
	if view.MaxInFlightPerAccount > 0 {
		cmd.Printf("Max In Flight:         %d per account\n", view.MaxInFlightPerAccount)
	} else {
		cmd.Printf("Max In Flight:         unbounded\n")
	}
	if view.MaxDispatchesPerRequest > 0 {
		cmd.Printf("Dispatch Budget:       %d per request\n", view.MaxDispatchesPerRequest)
	} else {
		cmd.Printf("Dispatch Budget:       profile default\n")
	}
	if view.PromptRejectionLimit > 0 {
		cmd.Printf("Prompt Rejection Guard: %d bare 429s / %ds\n", view.PromptRejectionLimit, view.PromptRejectionCooldownSeconds)
	} else {
		cmd.Printf("Prompt Rejection Guard: off\n")
	}
	if view.PromptPreflightEnabled {
		cmd.Printf("Prompt Preflight:      enabled\n")
	} else {
		cmd.Printf("Prompt Preflight:      disabled\n")
	}
	if len(view.Endpoints) > 0 {
		cmd.Printf("Upstream Endpoints:    %s\n", strings.Join(view.Endpoints, ", "))
	}
	if len(view.ModelMapping) > 0 {
		cmd.Println("Model Aliases:")
		for _, alias := range slices.Sorted(maps.Keys(view.ModelMapping)) {
			cmd.Printf("  %s -> %s\n", alias, view.ModelMapping[alias])
		}
	}

	cmd.Println("\nUpdate rotation strategy with: agw config strategy <sticky|round-robin|hybrid>")
	return nil
}

// runConfigPath prints the resolved active configuration file path.
func runConfigPath(cmd *cobra.Command, args []string) error {
	path := activeConfigPath()
	if path == "" {
		return fmt.Errorf("resolving configuration file path")
	}
	if configJSON {
		return writeJSONOutput(cmd, map[string]string{"path": path})
	}
	cmd.Println(path)
	return nil
}

// runConfigStrategy reads or updates the persisted account rotation strategy in config.json.
func runConfigStrategy(cmd *cobra.Command, args []string) error {
	if len(args) == 1 {
		target := strings.TrimSpace(args[0])
		if target != string(rotator.StrategySticky) &&
			target != string(rotator.StrategyRoundRobin) &&
			target != string(rotator.StrategyHybrid) {
			return fmt.Errorf("invalid strategy %q: must be 'sticky', 'round-robin', or 'hybrid'", target)
		}
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	path := activeConfigPath()
	if len(args) == 0 {
		current := cmp.Or(cfg.Strategy, config.DefaultStrategy)
		if configJSON {
			return writeJSONOutput(cmd, map[string]string{"strategy": current, "path": path})
		}
		cmd.Printf("Current strategy: %s\n", current)
		cmd.Println("Available strategies: sticky, round-robin, hybrid")
		return nil
	}

	target := strings.TrimSpace(args[0])

	if err := config.SaveStrategy(path, target); err != nil {
		return fmt.Errorf("persisting strategy: %w", err)
	}

	if configJSON {
		return writeJSONOutput(cmd, map[string]string{"strategy": target, "path": path})
	}

	cmd.Printf("✓ Strategy updated to %q in %s\n", target, path)

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if _, running := daemonHealth(ctx, cfg); running {
		cmd.Println("Note: daemon is currently running; restart it ('agw stop && agw start -d') to apply the new strategy.")
	}
	return nil
}
