// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/spf13/cobra"
)

var (
	// debugJSON toggles structured JSON output for the read-only wire diagnostics.
	debugJSON bool

	// debugCmd groups read-only diagnostics that never mutate configuration, credentials, or accounts.
	debugCmd = &cobra.Command{
		Use:   "debug",
		Short: "Inspect the effective upstream wire identity without changing anything",
		Long: `Debug exposes the identity the gateway presents to Google Cloud Code Assist.

These commands are read-only: they resolve the active configuration, print the
headers and endpoints the daemon would send upstream, and never contact Google,
mutate config.json, or reveal API keys, OAuth secrets, access tokens, or refresh
tokens. Use them to verify what actually leaves the machine before blaming the
upstream for a rejection.`,
		Example: `  agw debug wire
  agw debug wire --json`,
	}

	// debugWireCmd prints the resolved upstream identity headers and request defaults.
	debugWireCmd = &cobra.Command{
		Use:   "wire",
		Short: "Print effective upstream User-Agent, client metadata, endpoints, and request defaults",
		Long: `Wire resolves the active configuration and prints the exact User-Agent, X-Goog-Api-Client,
and Client-Metadata values the gateway sends upstream, together with the upstream
endpoint order, the HTTP transport mode, and the retry and cooldown defaults that
shape request traffic.

Every value comes from the same resolvers the daemon uses, so the report cannot
drift from gateway behaviour. Secrets are never printed.`,
		Example: `  agw debug wire
  agw debug wire --json`,
		RunE: runDebugWire,
	}
)

func init() {
	debugWireCmd.Flags().BoolVar(&debugJSON, "json", false, "Output the resolved wire identity as JSON")
	debugCmd.AddCommand(debugWireCmd)
	rootCmd.AddCommand(debugCmd)
}

// wireIdentityView captures the secret-free upstream identity and request defaults the daemon
// presents to Google Cloud Code Assist.
type wireIdentityView struct {
	// ConfigPath is the resolved configuration file path this process loaded, empty when unresolvable.
	ConfigPath string `json:"config_path,omitempty"`
	// Endpoint is the clamped local loopback base URL clients connect to.
	Endpoint string `json:"endpoint"`
	// Strategy is the configured account rotation strategy.
	Strategy string `json:"strategy"`
	// UserAgent is the resolved User-Agent header value sent to upstream endpoints.
	UserAgent string `json:"user_agent"`
	// APIClient is the resolved X-Goog-Api-Client header value sent to upstream endpoints.
	APIClient string `json:"api_client"`
	// ClientMetadata is the resolved Client-Metadata header value sent to upstream endpoints.
	ClientMetadata string `json:"client_metadata"`
	// UpstreamTransport describes the HTTP version and connection-pool scope used upstream.
	UpstreamTransport string `json:"upstream_transport"`
	// QuotaThreshold is the fractional remaining-quota floor before an account is deprioritized.
	QuotaThreshold float64 `json:"quota_threshold"`
	// MaxWaitSeconds is the maximum cooldown wait the rotator absorbs before failing a request.
	MaxWaitSeconds int `json:"max_wait_seconds"`
	// DefaultCooldownSeconds is the base cooldown applied to a 429 without an explicit retry delay.
	DefaultCooldownSeconds int `json:"default_cooldown_seconds"`
	// EmptyStreamRetries is the retry budget for empty upstream streams.
	EmptyStreamRetries int `json:"empty_stream_retries"`
	// Endpoints lists the upstream Cloud Code Assist base URLs in dispatch order.
	Endpoints []string `json:"endpoints,omitempty"`
}

// buildWireIdentityView projects cfg into the secret-free upstream identity and request defaults
// reported by agw debug wire. Values are derived through the same resolvers the daemon uses, so
// environment overrides (AGW_USER_AGENT and friends) are reflected exactly as they are sent.
func buildWireIdentityView(path string, cfg *config.Config) wireIdentityView {
	cfg = cmp.Or(cfg, config.DefaultConfig())

	host := config.LoopbackHost(cfg.Host)
	port := cmp.Or(cfg.Port, config.DefaultPort)

	return wireIdentityView{
		ConfigPath:             path,
		Endpoint:               config.BaseURL(host, port),
		Strategy:               cmp.Or(cfg.Strategy, config.DefaultStrategy),
		UserAgent:              config.ResolveUserAgent(cfg),
		APIClient:              config.ResolveAPIClient(cfg),
		ClientMetadata:         config.ResolveClientMetadata(cfg),
		UpstreamTransport:      upstreamTransportMode(),
		QuotaThreshold:         cfg.QuotaThreshold,
		MaxWaitSeconds:         int(cfg.MaxWaitDuration().Seconds()),
		DefaultCooldownSeconds: int(cfg.CooldownDuration().Seconds()),
		EmptyStreamRetries:     cfg.EmptyStreamRetryCount(),
		Endpoints:              slices.Clone(cfg.Endpoints),
	}
}

// upstreamTransportMode reports the HTTP version and connection-pool scope the process-wide
// upstream transport negotiates. The answer is derived from the transport the daemon actually
// builds, so the diagnostic cannot drift from gateway behaviour after a transport change.
func upstreamTransportMode() string {
	transport := config.NewTransport()
	if transport.ForceAttemptHTTP2 {
		return "http/2 (process-wide shared pool)"
	}
	return "http/1.1 (process-wide shared pool)"
}

// runDebugWire renders the resolved upstream identity for the active configuration.
func runDebugWire(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	view := buildWireIdentityView(activeConfigPath(), cfg)
	if debugJSON {
		return writeJSONOutput(cmd, view)
	}

	if view.ConfigPath != "" {
		cmd.Printf("Config File:           %s\n", view.ConfigPath)
	}
	cmd.Printf("Gateway Endpoint:      %s\n", view.Endpoint)
	cmd.Printf("Strategy:              %s\n", view.Strategy)
	cmd.Printf("Quota Threshold:       %.0f%%\n", view.QuotaThreshold*100)
	cmd.Printf("User-Agent:            %s\n", view.UserAgent)
	cmd.Printf("X-Goog-Api-Client:     %s\n", view.APIClient)
	cmd.Printf("Client-Metadata:       %s\n", view.ClientMetadata)
	cmd.Printf("Upstream Transport:    %s\n", view.UpstreamTransport)
	cmd.Printf("Max Cooldown Wait:     %ds\n", view.MaxWaitSeconds)
	cmd.Printf("Default 429 Cooldown:  %ds\n", view.DefaultCooldownSeconds)
	cmd.Printf("Empty Stream Retries:  %d\n", view.EmptyStreamRetries)
	if len(view.Endpoints) > 0 {
		cmd.Println("Upstream Endpoints:")
		for i, endpoint := range view.Endpoints {
			cmd.Printf("  %d. %s\n", i+1, endpoint)
		}
	}

	cmd.Println("\nRun 'agw config show' for the redacted configuration view.")
	return nil
}
