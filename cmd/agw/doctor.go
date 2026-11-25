// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/client"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/updater"
	"github.com/spf13/cobra"
)

// doctorCmd runs environment health diagnostics across ports, permissions, accounts, and integrations.
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Run diagnostics on ports, permissions, accounts, and network connectivity",
	Long: `Doctor performs a comprehensive diagnostic check of the local Antigravity Gateway environment.
It inspects:
  - Local port availability (51400 for daemon, 51121 for OAuth PKCE callback)
  - POSIX filesystem permissions on ~/.agw (0700) and credential files (0600)
  - Configured Google accounts and token validity
  - Upstream network reachability to Google OAuth and Cloud Code Assist endpoints
  - Integration status in Pi Coding Agent and OpenCode configurations`,
	RunE: runDoctor,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
}

// CheckStatus represents the severity outcome of a diagnostic probe.
type CheckStatus string

const (
	// StatusOK indicates that the diagnostic check passed without issues.
	StatusOK CheckStatus = "OK"

	// StatusWarn indicates a non-critical discrepancy or recommendation.
	StatusWarn CheckStatus = "WARN"

	// StatusFail indicates a critical environment or configuration failure.
	StatusFail CheckStatus = "FAIL"
)

// CheckItem represents a single diagnostic evaluation.
type CheckItem struct {
	// Category groups related diagnostic checks (e.g. "Network", "Permissions").
	Category string

	// Name is the specific check identifier.
	Name string

	// Status represents check severity ("OK", "WARN", "FAIL").
	Status CheckStatus

	// Message is the detailed outcome explanation.
	Message string

	// Hint provides actionable recovery instructions if the check failed.
	Hint string
}

// runDoctor executes all diagnostic suites and renders an actionable CLI report.
func runDoctor(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	cmd.Println("Running Antigravity Gateway environment diagnostics...")
	cmd.Println()

	var results []CheckItem

	// 1. Ports Diagnostics
	results = append(results, checkPorts(ctx, cfg)...)

	// 2. Filesystem Permissions Diagnostics
	results = append(results, checkPermissions()...)
	results = append(results, checkAPIKey(cfg)...)

	// 3. Accounts Diagnostics
	results = append(results, checkAccounts()...)

	// 4. Upstream Google Connectivity
	results = append(results, checkConnectivity(ctx)...)

	// 5. Client Integrations
	results = append(results, checkClientIntegrations(cfg)...)

	// 6. Release & Version Status (Non-blocking probe)
	results = append(results, checkVersionRelease(ctx)...)

	// Render diagnostics report
	hasFailures := false
	hasWarnings := false

	var currentCategory string
	for _, item := range results {
		if item.Category != currentCategory {
			currentCategory = item.Category
			cmd.Printf("[%s]\n", currentCategory)
		}

		icon := "✓"
		switch item.Status {
		case StatusFail:
			icon = "✗"
			hasFailures = true
		case StatusWarn:
			icon = "!"
			hasWarnings = true
		}

		cmd.Printf("  %s %-32s : %s\n", icon, item.Name, item.Message)
		if item.Hint != "" {
			cmd.Printf("    └─ Hint: %s\n", item.Hint)
		}
	}

	cmd.Println()
	if hasFailures {
		cmd.Println("Doctor found issues requiring attention before full operation.")
		return errors.New("diagnostics completed with errors")
	} else if hasWarnings {
		cmd.Println("Doctor diagnostics completed with warnings. System is operational.")
	} else {
		cmd.Println("All diagnostics passed successfully! Antigravity Gateway is ready.")
	}

	return nil
}

// checkPorts verifies availability or health of daemon and callback ports.
func checkPorts(ctx context.Context, cfg *config.Config) []CheckItem {
	var items []CheckItem

	// Check Gateway Port (default 51400). The configured host is loopback-clamped and formatted with
	// IPv6-safe brackets so a "::1" configuration probes the interface the daemon actually binds.
	port := cfg.Port
	if port <= 0 {
		port = config.DefaultPort
	}
	host := config.LoopbackHost(cfg.Host)
	addr := config.ListenAddress(host, port)

	// Probe if AGW is already running
	healthURL := config.URL(host, port, "/health")
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	httpClient := &http.Client{Timeout: 1 * time.Second}
	resp, err := httpClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			var hResp struct {
				Name     string `json:"name"`
				Version  string `json:"version"`
				Accounts int    `json:"accounts"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&hResp)
			items = append(items, CheckItem{
				Category: "Network Ports",
				Name:     fmt.Sprintf("Gateway Port (%d)", port),
				Status:   StatusOK,
				Message:  fmt.Sprintf("Active (running %s v%s, %d account(s))", hResp.Name, hResp.Version, hResp.Accounts),
			})
		} else {
			items = append(items, CheckItem{
				Category: "Network Ports",
				Name:     fmt.Sprintf("Gateway Port (%d)", port),
				Status:   StatusWarn,
				Message:  fmt.Sprintf("Service returned HTTP %d on /health", resp.StatusCode),
			})
		}
	} else {
		// Test if port is available to bind
		ln, bindErr := net.Listen("tcp", addr)
		if bindErr == nil {
			_ = ln.Close()
			items = append(items, CheckItem{
				Category: "Network Ports",
				Name:     fmt.Sprintf("Gateway Port (%d)", port),
				Status:   StatusOK,
				Message:  "Available to bind (gateway daemon stopped)",
				Hint:     "Start daemon with 'agw start --detach'",
			})
		} else {
			items = append(items, CheckItem{
				Category: "Network Ports",
				Name:     fmt.Sprintf("Gateway Port (%d)", port),
				Status:   StatusFail,
				Message:  fmt.Sprintf("Port is in use by another process: %v", bindErr),
				Hint:     fmt.Sprintf("Free port %d or specify a custom port in config.json", port),
			})
		}
	}

	// Check OAuth callback port (51121)
	oauthAddr := "127.0.0.1:51121"
	lnOAuth, bindErr := net.Listen("tcp", oauthAddr)
	if bindErr == nil {
		_ = lnOAuth.Close()
		items = append(items, CheckItem{
			Category: "Network Ports",
			Name:     "OAuth Callback Port (51121)",
			Status:   StatusOK,
			Message:  "Available to bind",
		})
	} else {
		items = append(items, CheckItem{
			Category: "Network Ports",
			Name:     "OAuth Callback Port (51121)",
			Status:   StatusWarn,
			Message:  fmt.Sprintf("Port is occupied: %v", bindErr),
			Hint:     "Ensure no other OAuth listener is active when running 'agw login'",
		})
	}

	return items
}

// checkPermissions audits POSIX directory (0700) and file (0600) permissions on ~/.agw.
func checkPermissions() []CheckItem {
	var items []CheckItem

	dir, err := config.Dir()
	if err != nil {
		items = append(items, CheckItem{
			Category: "File System & Security",
			Name:     "Configuration Root Dir",
			Status:   StatusFail,
			Message:  fmt.Sprintf("Cannot resolve directory: %v", err),
		})
		return items
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			items = append(items, CheckItem{
				Category: "File System & Security",
				Name:     "Configuration Root Dir",
				Status:   StatusWarn,
				Message:  fmt.Sprintf("%s does not exist yet", dir),
				Hint:     "Will be created automatically on 'agw start' or 'agw login'",
			})
		} else {
			items = append(items, CheckItem{
				Category: "File System & Security",
				Name:     "Configuration Root Dir",
				Status:   StatusFail,
				Message:  fmt.Sprintf("Stat error: %v", err),
			})
		}
	} else {
		perm := dirInfo.Mode().Perm()
		if perm&0o077 != 0 {
			items = append(items, CheckItem{
				Category: "File System & Security",
				Name:     "Configuration Root Dir",
				Status:   StatusWarn,
				Message:  fmt.Sprintf("%s permissions %04o (recommended 0700)", dir, perm),
				Hint:     fmt.Sprintf("Run 'chmod 0700 %s'", dir),
			})
		} else {
			items = append(items, CheckItem{
				Category: "File System & Security",
				Name:     "Configuration Root Dir",
				Status:   StatusOK,
				Message:  fmt.Sprintf("%s has secure permissions (%04o)", dir, perm),
			})
		}
	}

	// Check config.json permissions
	cfgPath, _ := config.ConfigFilePath()
	if cfgInfo, err := os.Stat(cfgPath); err == nil {
		perm := cfgInfo.Mode().Perm()
		if perm&0o077 != 0 {
			items = append(items, CheckItem{
				Category: "File System & Security",
				Name:     "Config File (config.json)",
				Status:   StatusWarn,
				Message:  fmt.Sprintf("Permissions %04o (recommended 0600)", perm),
				Hint:     fmt.Sprintf("Run 'chmod 0600 %s'", cfgPath),
			})
		} else {
			items = append(items, CheckItem{
				Category: "File System & Security",
				Name:     "Config File (config.json)",
				Status:   StatusOK,
				Message:  fmt.Sprintf("Secure permissions (%04o)", perm),
			})
		}
	}

	// Check accounts.json permissions
	accPath, _ := config.AccountsFilePath()
	if accInfo, err := os.Stat(accPath); err == nil {
		perm := accInfo.Mode().Perm()
		if perm&0o077 != 0 {
			items = append(items, CheckItem{
				Category: "File System & Security",
				Name:     "Accounts File (accounts.json)",
				Status:   StatusWarn,
				Message:  fmt.Sprintf("Permissions %04o (recommended 0600)", perm),
				Hint:     fmt.Sprintf("Run 'chmod 0600 %s'", accPath),
			})
		} else {
			items = append(items, CheckItem{
				Category: "File System & Security",
				Name:     "Accounts File (accounts.json)",
				Status:   StatusOK,
				Message:  fmt.Sprintf("Secure permissions (%04o)", perm),
			})
		}
	}

	// Check optional state files if present: models-cache.json, agw.pid, agw.log
	extraFiles := []struct {
		name string
		path func() (string, error)
	}{
		{"Models Cache (models-cache.json)", config.ModelsCacheFilePath},
		{"Daemon PID File (agw.pid)", config.PIDFilePath},
		{"Daemon Log File (agw.log)", func() (string, error) {
			d, err := config.Dir()
			if err != nil {
				return "", err
			}
			return filepath.Join(d, "agw.log"), nil
		}},
	}

	for _, ef := range extraFiles {
		if p, err := ef.path(); err == nil {
			if info, err := os.Stat(p); err == nil {
				perm := info.Mode().Perm()
				if perm&0o077 != 0 {
					items = append(items, CheckItem{
						Category: "File System & Security",
						Name:     ef.name,
						Status:   StatusWarn,
						Message:  fmt.Sprintf("Permissions %04o (recommended 0600)", perm),
						Hint:     fmt.Sprintf("Run 'chmod 0600 %s'", p),
					})
				} else {
					items = append(items, CheckItem{
						Category: "File System & Security",
						Name:     ef.name,
						Status:   StatusOK,
						Message:  fmt.Sprintf("Secure permissions (%04o)", perm),
					})
				}
			}
		}
	}

	return items
}

// checkAccounts inspects accounts.json and verifies token presence.
func checkAccounts() []CheckItem {
	var items []CheckItem

	accPath, err := config.AccountsFilePath()
	if err != nil {
		items = append(items, CheckItem{
			Category: "Account Pool",
			Name:     "Stored Accounts",
			Status:   StatusFail,
			Message:  fmt.Sprintf("Cannot resolve accounts path: %v", err),
		})
		return items
	}

	store := auth.NewStore(accPath)
	accounts, err := store.Load()
	if err != nil {
		items = append(items, CheckItem{
			Category: "Account Pool",
			Name:     "Stored Accounts",
			Status:   StatusFail,
			Message:  fmt.Sprintf("Failed loading accounts: %v", err),
		})
		return items
	}

	if len(accounts) == 0 {
		hint := "Add accounts using 'agw login'"
		if sess, detectErr := auth.DetectLocalSession(context.Background()); detectErr == nil && sess != nil {
			hint = fmt.Sprintf("Local Antigravity session found for %s! Run 'agw login --local' to import", sess.Email)
		}
		items = append(items, CheckItem{
			Category: "Account Pool",
			Name:     "Google Accounts",
			Status:   StatusWarn,
			Message:  "0 accounts configured",
			Hint:     hint,
		})
		return items
	}

	usable := 0
	localSessions := 0
	for _, a := range accounts {
		if a.IsLocalDesktopSession() {
			localSessions++
		}
		if !a.Disabled && a.HasCredentials() && a.ProjectID != "" {
			usable++
		}
	}

	if usable == 0 {
		items = append(items, CheckItem{
			Category: "Account Pool",
			Name:     "Google Accounts",
			Status:   StatusFail,
			Message:  fmt.Sprintf("%d account(s) found but none are active with valid credentials", len(accounts)),
			Hint:     "Run 'agw login' to authenticate an account",
		})
	} else {
		disabledCount := len(accounts) - usable
		msg := fmt.Sprintf("%d active account(s) ready", usable)
		if disabledCount > 0 {
			msg += fmt.Sprintf(" (%d disabled)", disabledCount)
		}
		if localSessions > 0 {
			msg += fmt.Sprintf(" · %d imported desktop session(s) expire without refresh", localSessions)
		}
		items = append(items, CheckItem{
			Category: "Account Pool",
			Name:     "Google Accounts",
			Status:   StatusOK,
			Message:  msg,
			Hint:     localSessionHint(localSessions),
		})
	}

	return items
}

// localSessionHint returns the re-import guidance shown when imported desktop sessions are present,
// since those credentials carry no refresh token and cannot be renewed by the gateway.
func localSessionHint(localSessions int) string {
	if localSessions == 0 {
		return ""
	}
	return "Imported desktop tokens cannot be refreshed: re-run 'agw login --local' after they expire"
}

// connectivityDialer abstracts TCP dialing to external endpoints for testability and isolation.
var connectivityDialer = func(ctx context.Context, host string) (time.Duration, error) {
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 3 * time.Second},
		Config:    &tls.Config{MinVersion: tls.VersionTLS12},
	}
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", host)
	duration := time.Since(start)
	if err == nil {
		_ = conn.Close()
	}
	return duration, err
}

// checkConnectivity tests outbound network reachability to Google OAuth and Cloud Code Assist endpoints.
func checkConnectivity(ctx context.Context) []CheckItem {
	var items []CheckItem

	endpoints := []struct {
		name string
		host string
	}{
		{"Google OAuth 2.0 Endpoint", "oauth2.googleapis.com:443"},
		{"Cloud Code Assist PA Endpoint", "cloudcode-pa.googleapis.com:443"},
	}

	for _, ep := range endpoints {
		duration, err := connectivityDialer(ctx, ep.host)

		if err != nil {
			items = append(items, CheckItem{
				Category: "Upstream Connectivity",
				Name:     ep.name,
				Status:   StatusFail,
				Message:  fmt.Sprintf("Unreachable (%v)", err),
				Hint:     "Check your internet connection or DNS resolver",
			})
		} else {
			items = append(items, CheckItem{
				Category: "Upstream Connectivity",
				Name:     ep.name,
				Status:   StatusOK,
				Message:  fmt.Sprintf("Reachable (%s latency)", duration.Round(time.Millisecond)),
			})
		}
	}

	return items
}

// checkAPIKey verifies that the configured gateway API key still follows the generated format.
// The key value is never rendered: only its shape is reported, so a stale, truncated, or
// foreign key can be spotted before clients start failing with HTTP 401.
func checkAPIKey(cfg *config.Config) []CheckItem {
	if err := config.ValidateAPIKey(cfg.APIKey); err != nil {
		return []CheckItem{{
			Category: "File System & Security",
			Name:     "Gateway API Key",
			Status:   StatusWarn,
			Message:  fmt.Sprintf("Stored key does not match the generated format (%v)", err),
			Hint:     "Restore the original key or regenerate it, then re-run 'agw setup pi' to resynchronize clients",
		}}
	}

	return []CheckItem{{
		Category: "File System & Security",
		Name:     "Gateway API Key",
		Status:   StatusOK,
		Message:  fmt.Sprintf("Valid generated format (%d hexadecimal characters)", config.APIKeyHexLen),
	}}
}

// clientKeyMismatch reports whether a client stores a bearer key that differs from the gateway key.
// A mismatch is the direct cause of "401 Invalid API key" responses seen from an otherwise
// correctly configured agent, so doctor surfaces it without ever printing either key.
func clientKeyMismatch(clientKey, gatewayKey string) bool {
	return clientKey != "" && gatewayKey != "" && clientKey != gatewayKey
}

// checkClientIntegrations audits Pi Coding Agent, OpenCode, and Claude Code configuration files,
// reporting both whether the gateway provider is configured and whether the stored client key
// still matches the gateway key in config.json.
func checkClientIntegrations(cfg *config.Config) []CheckItem {
	var items []CheckItem

	// Check Pi Coding Agent configuration
	piDir, err := client.DefaultPiDir()
	if err == nil {
		modelsPath := filepath.Join(piDir, "models.json")
		if raw, err := os.ReadFile(modelsPath); err == nil {
			var parsed struct {
				Providers map[string]any `json:"providers"`
			}
			if parseErr := json.Unmarshal(raw, &parsed); parseErr == nil {
				if provider, ok := parsed.Providers[client.DefaultProviderID]; ok {
					key := ""
					if fields, isMap := provider.(map[string]any); isMap {
						key, _ = fields["apiKey"].(string)
					}

					if clientKeyMismatch(key, cfg.APIKey) {
						items = append(items, CheckItem{
							Category: "Client Integrations",
							Name:     "Pi Coding Agent",
							Status:   StatusWarn,
							Message:  "Configured but its API key differs from config.json",
							Hint:     "Run 'agw setup pi' to resynchronize the gateway key",
						})
					} else {
						items = append(items, CheckItem{
							Category: "Client Integrations",
							Name:     "Pi Coding Agent",
							Status:   StatusOK,
							Message:  fmt.Sprintf("Configured with %q provider", client.DefaultProviderID),
						})
					}
				} else {
					items = append(items, CheckItem{
						Category: "Client Integrations",
						Name:     "Pi Coding Agent",
						Status:   StatusWarn,
						Message:  "Installed but Antigravity Gateway is not configured",
						Hint:     "Run 'agw setup pi' to configure automatically",
					})
				}
			} else {
				items = append(items, CheckItem{
					Category: "Client Integrations",
					Name:     "Pi Coding Agent",
					Status:   StatusWarn,
					Message:  fmt.Sprintf("Configuration exists at %s but contains invalid JSON", modelsPath),
					Hint:     "Repair or re-run 'agw setup pi' to backup and replace",
				})
			}
		}
	}

	// Check OpenCode configuration
	opencodePath, err := client.DefaultOpenCodeConfigPath()
	if err == nil {
		if raw, err := os.ReadFile(opencodePath); err == nil {
			var parsed struct {
				Providers map[string]any `json:"providers"`
			}
			if parseErr := json.Unmarshal(raw, &parsed); parseErr == nil {
				if provider, ok := parsed.Providers[client.DefaultProviderID]; ok {
					key := ""
					if fields, isMap := provider.(map[string]any); isMap {
						if settings, isSettings := fields["settings"].(map[string]any); isSettings {
							key, _ = settings["apiKey"].(string)
						}
						if key == "" {
							if options, isOptions := fields["options"].(map[string]any); isOptions {
								key, _ = options["apiKey"].(string)
							}
						}
					}

					if clientKeyMismatch(key, cfg.APIKey) {
						items = append(items, CheckItem{
							Category: "Client Integrations",
							Name:     "OpenCode CLI",
							Status:   StatusWarn,
							Message:  "Configured but its API key differs from config.json",
							Hint:     "Run 'agw setup opencode' to resynchronize the gateway key",
						})
					} else {
						items = append(items, CheckItem{
							Category: "Client Integrations",
							Name:     "OpenCode CLI",
							Status:   StatusOK,
							Message:  fmt.Sprintf("Configured with %q provider", client.DefaultProviderID),
						})
					}
				} else {
					items = append(items, CheckItem{
						Category: "Client Integrations",
						Name:     "OpenCode CLI",
						Status:   StatusWarn,
						Message:  "Config exists but Antigravity Gateway is not configured",
						Hint:     "Run 'agw setup opencode' to configure automatically",
					})
				}
			} else {
				items = append(items, CheckItem{
					Category: "Client Integrations",
					Name:     "OpenCode CLI",
					Status:   StatusWarn,
					Message:  fmt.Sprintf("Configuration exists at %s but contains invalid JSON", opencodePath),
					Hint:     "Repair or re-run 'agw setup opencode' to backup and replace",
				})
			}
		}
	}

	// Check Claude Code CLI configuration
	claudePath, err := client.DefaultClaudeSettingsPath()
	if err == nil {
		if raw, err := os.ReadFile(claudePath); err == nil {
			var parsed struct {
				Env map[string]any `json:"env"`
			}
			if parseErr := json.Unmarshal(raw, &parsed); parseErr == nil {
				if baseURL, ok := parsed.Env["ANTHROPIC_BASE_URL"].(string); ok && baseURL != "" {
					token, _ := parsed.Env["ANTHROPIC_AUTH_TOKEN"].(string)
					if clientKeyMismatch(token, cfg.APIKey) {
						items = append(items, CheckItem{
							Category: "Client Integrations",
							Name:     "Claude Code CLI",
							Status:   StatusWarn,
							Message:  "Configured but ANTHROPIC_AUTH_TOKEN differs from config.json",
							Hint:     "Run 'agw setup claude' to resynchronize the gateway key",
						})
					} else {
						items = append(items, CheckItem{
							Category: "Client Integrations",
							Name:     "Claude Code CLI",
							Status:   StatusOK,
							Message:  fmt.Sprintf("Configured with ANTHROPIC_BASE_URL=%s", baseURL),
						})
					}
				} else {
					items = append(items, CheckItem{
						Category: "Client Integrations",
						Name:     "Claude Code CLI",
						Status:   StatusWarn,
						Message:  "Settings exist but ANTHROPIC_BASE_URL is not configured",
						Hint:     "Run 'agw setup claude' to configure automatically",
					})
				}
			} else {
				items = append(items, CheckItem{
					Category: "Client Integrations",
					Name:     "Claude Code CLI",
					Status:   StatusWarn,
					Message:  fmt.Sprintf("Settings exist at %s but contains invalid JSON", claudePath),
					Hint:     "Repair or re-run 'agw setup claude' to backup and replace",
				})
			}
		}
	}

	return items
}

// versionChecker abstracts release discovery for deterministic doctor unit testing.
var versionChecker = func(ctx context.Context) (*updater.ReleaseInfo, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return updater.CheckUpdate(probeCtx, updater.DefaultGitHubRepo, config.Version, "", nil)
}

// checkVersionRelease queries GitHub Releases non-blockingly to surface update notifications.
func checkVersionRelease(ctx context.Context) []CheckItem {
	rel, err := versionChecker(ctx)
	if err != nil {
		// Non-blocking: network timeout or offline environments must never fail doctor report
		return []CheckItem{{
			Category: "Release & Updates",
			Name:     "Gateway Version",
			Status:   StatusOK,
			Message:  fmt.Sprintf("Running v%s (unable to check for updates: %v)", config.Version, err),
		}}
	}

	if rel.UpdateAvailable {
		return []CheckItem{{
			Category: "Release & Updates",
			Name:     "Gateway Version",
			Status:   StatusWarn,
			Message:  fmt.Sprintf("v%s installed · newer release v%s is available", config.Version, rel.TargetVersion),
			Hint:     "Run 'agw update' to upgrade in-place automatically",
		}}
	}

	return []CheckItem{{
		Category: "Release & Updates",
		Name:     "Gateway Version",
		Status:   StatusOK,
		Message:  fmt.Sprintf("v%s (latest release)", config.Version),
	}}
}
