// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/client"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/risk"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/updater"
	"github.com/spf13/cobra"
)

// doctorCmd runs environment health diagnostics across ports, permissions, accounts, and integrations.
var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Run diagnostics on ports, permissions, accounts, and network connectivity",
	Long: `Doctor performs a comprehensive diagnostic check of the local Antigravity Gateway environment.
It inspects:
  - Local port availability (gateway daemon port and OAuth PKCE callback port from config)
  - POSIX filesystem permissions on ~/.agw (0700) and credential files (0600)
  - Configured Google accounts and token validity
  - Upstream network reachability to Google OAuth and Cloud Code Assist endpoints
  - Integration status in Pi Coding Agent, OpenCode, and Claude Code configurations`,
	Example: `  agw doctor
  agw --config /etc/agw/config.json doctor`,
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
	results = append(results, checkAccounts(cfg)...)

	// 4. Upstream Identity Diagnostics
	results = append(results, checkUpstreamIdentity(cfg)...)
	results = append(results, checkPromptPreflight(cfg))

	// 5. Upstream Google Connectivity
	results = append(results, checkConnectivity(ctx)...)

	// 6. Client Integrations
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
	port := config.DefaultPort
	hostRaw := config.DefaultHost
	redirectURI := config.DefaultRedirectURI
	if cfg != nil {
		if cfg.Port > 0 {
			port = cfg.Port
		}
		if cfg.Host != "" {
			hostRaw = cfg.Host
		}
		redirectURI = cmp.Or(cfg.RedirectURI, config.DefaultRedirectURI)
	}
	host := config.LoopbackHost(hostRaw)
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

	// Check OAuth callback port resolved from configured RedirectURI (default 127.0.0.1:51121).
	if cfg != nil && cfg.RedirectURI != "" {
		if parsed, parseErr := url.Parse(cfg.RedirectURI); parseErr != nil || parsed.Scheme == "" || parsed.Host == "" {
			items = append(items, CheckItem{
				Category: "Network Ports",
				Name:     "OAuth Redirect URI",
				Status:   StatusWarn,
				Message:  fmt.Sprintf("Malformed redirect_uri %q; falling back to default callback listener", cfg.RedirectURI),
				Hint:     "Set a valid loopback redirect_uri such as http://127.0.0.1:51121/oauth-callback in config.json",
			})
		}
	}
	oauthAddr, _ := auth.ResolveCallbackEndpoint(redirectURI)
	_, oauthPort, splitErr := net.SplitHostPort(oauthAddr)
	if splitErr != nil || oauthPort == "" {
		oauthPort = "51121"
	}

	lnOAuth, bindErr := net.Listen("tcp", oauthAddr)
	if bindErr == nil {
		_ = lnOAuth.Close()
		items = append(items, CheckItem{
			Category: "Network Ports",
			Name:     fmt.Sprintf("OAuth Callback Port (%s)", oauthPort),
			Status:   StatusOK,
			Message:  "Available to bind",
		})
	} else {
		items = append(items, CheckItem{
			Category: "Network Ports",
			Name:     fmt.Sprintf("OAuth Callback Port (%s)", oauthPort),
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
// checkAccounts inspects the stored account pool and reports how the operating profile uses it.
func checkAccounts(cfg *config.Config) []CheckItem {
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

	items = append(items, accountProfileCheck(cfg, usable)...)
	items = append(items, suspendedAccountCheck(accounts)...)

	return items
}

// suspendedAccountCheck reports accounts Google suspended, because they stay out of rotation until the
// operator appeals and clears them locally. The first appeal link is surfaced so doctor output is
// actionable without a second command.
func suspendedAccountCheck(accounts []*auth.Account) []CheckItem {
	var suspended []*auth.Account
	for _, acc := range accounts {
		if acc.Suspended() {
			suspended = append(suspended, acc)
		}
	}
	if len(suspended) == 0 {
		return nil
	}

	kinds := make([]string, 0, len(suspended))
	for _, acc := range suspended {
		kinds = append(kinds, fmt.Sprintf("%s (%s)", risk.MaskEmail(acc.Email), acc.SuspendKind))
	}

	hint := "Appeal once, then revoke third-party access at https://myaccount.google.com/permissions before signing in again"
	if suspended[0].AppealURL != "" {
		hint = fmt.Sprintf("Appeal once: %s — then revoke third-party access before signing in again", suspended[0].AppealURL)
	}

	return []CheckItem{{
		Category: "Account Pool",
		Name:     "Suspended Accounts",
		Status:   StatusWarn,
		Message:  fmt.Sprintf("%d suspended: %s", len(suspended), strings.Join(kinds, ", ")),
		Hint:     hint,
	}}
}

// accountProfileCheck reports how the operating profile uses the stored pool: the safe profile serves a
// single primary account, while the manual profile rotates every active account on failure.
func accountProfileCheck(cfg *config.Config, usable int) []CheckItem {
	if usable <= 1 {
		return nil
	}

	profile := cfg.EffectiveProfile()
	if profile == config.ProfileManual {
		return []CheckItem{{
			Category: "Account Pool",
			Name:     "Operating Profile",
			Status:   StatusWarn,
			Message:  fmt.Sprintf("manual profile rotates %d active accounts on failure", usable),
			Hint:     "Consumer plans do not support automated account rotation: set profile to 'safe' to serve one primary account",
		}}
	}

	return []CheckItem{{
		Category: "Account Pool",
		Name:     "Operating Profile",
		Status:   StatusOK,
		Message:  fmt.Sprintf("safe profile serves 1 primary account; %d additional account(s) stay idle", usable-1),
		Hint:     "Set rotation_mode to 'pool' only if you accept pooled failover against the provider's consumer terms",
	}}
}

// semverPattern matches the first MAJOR.MINOR.PATCH token in a version banner.
var semverPattern = regexp.MustCompile(`\d+\.\d+\.\d+`)

// agyVersionDetector resolves the locally installed Antigravity CLI version. It is injectable so the
// diagnostic stays offline and deterministic in tests.
var agyVersionDetector = detectLocalAgyVersion

// detectLocalAgyVersion runs `agy --version` with a short timeout and extracts its semantic version.
func detectLocalAgyVersion(ctx context.Context) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	output, err := exec.CommandContext(cmdCtx, "agy", "--version").Output()
	if err != nil {
		return "", err
	}
	return parseSemanticVersion(string(output)), nil
}

// parseSemanticVersion returns the first MAJOR.MINOR.PATCH token in raw, or an empty string when the
// banner contains none.
func parseSemanticVersion(raw string) string {
	return semverPattern.FindString(strings.TrimSpace(raw))
}

// checkUpstreamIdentity reports the client identity the gateway advertises and, separately, the locally
// installed Antigravity CLI. The CLI and the Hub advertise different version tracks, so they are reported
// side by side instead of being compared numerically.
func checkUpstreamIdentity(cfg *config.Config) []CheckItem {
	items := []CheckItem{checkAdvertisedIdentity(cfg)}
	if item, ok := checkLocalAntigravityCLI(); ok {
		items = append(items, item)
	}
	return items
}

// checkAdvertisedIdentity reports the Hub version and changelist the gateway sends upstream, warning when
// an operator override left the version without its paired changelist.
func checkAdvertisedIdentity(cfg *config.Config) CheckItem {
	item := CheckItem{Category: "Upstream Identity", Name: "Antigravity Client Version", Status: StatusOK}

	if cfg != nil && strings.TrimSpace(cfg.UserAgent) != "" {
		item.Message = "custom user_agent configured; the version baseline is operator-managed"
		item.Hint = "Keep the advertised version and its cl changelist paired with the installed Antigravity build"
		return item
	}

	version := config.ResolveAntigravityVersion()
	changelist, known := config.LookupAntigravityChangelist(version)
	changelistOverride := strings.TrimSpace(os.Getenv(config.EnvAntigravityChangelist))

	switch {
	case known && changelistOverride == "":
		item.Message = fmt.Sprintf("advertising antigravity/hub/%s (cl=%s)", version, changelist)
	case !known && changelistOverride != "":
		item.Message = fmt.Sprintf("advertising antigravity/hub/%s (cl=%s)", version, changelistOverride)
	case !known:
		item.Status = StatusWarn
		item.Message = fmt.Sprintf("version override %s has no known changelist; the baseline cl would be sent", version)
		item.Hint = fmt.Sprintf("Set %s to the Piper changelist paired with that Hub build", config.EnvAntigravityChangelist)
	default:
		item.Message = fmt.Sprintf("advertising antigravity/hub/%s (cl override %s)", version, changelistOverride)
	}

	return item
}

// checkPromptPreflight reports whether harness-fingerprint rewrites are active. Disabling them is a valid
// debugging choice, but it re-exposes the exact instruction patterns upstream rejects with a bare 429.
func checkPromptPreflight(cfg *config.Config) CheckItem {
	item := CheckItem{Category: "Upstream Identity", Name: "Prompt Preflight", Status: StatusOK}
	if cfg != nil && cfg.DisablePromptPreflight {
		item.Status = StatusWarn
		item.Message = "disabled; harness fingerprints stay in system instructions"
		item.Hint = "Remove disable_prompt_preflight from config.json to rewrite known wrapper banners and anchors"
		return item
	}
	item.Message = "enabled; known wrapper banners and tool anchors are rewritten"
	return item
}

// checkLocalAntigravityCLI reports the locally installed Antigravity CLI version when one is available.
// It never warns on absence: the CLI and the IDE Hub are separate installs, and only the Hub version is
// advertised upstream.
func checkLocalAntigravityCLI() (CheckItem, bool) {
	item := CheckItem{Category: "Upstream Identity", Name: "Local Antigravity CLI", Status: StatusOK}

	local, err := agyVersionDetector(context.Background())
	if err != nil || local == "" {
		item.Message = "local agy CLI not detected"
		item.Hint = "Install the Antigravity CLI for local sessions, or set AGW_ANTIGRAVITY_VERSION manually if upstream gates the advertised Hub build"
		return item, true
	}

	item.Message = fmt.Sprintf("local agy CLI %s detected (CLI and Hub versions are separate tracks)", local)
	return item, true
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

// addressFamilyResolver abstracts DNS resolution for the IPv4 vs IPv6 health checks, keeping the
// diagnostic hermetic under tests and offline sandboxes.
var addressFamilyResolver = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// familyDialer abstracts single-address-family TCP dialing for the IPv4 vs IPv6 health checks.
var familyDialer = func(ctx context.Context, network, host string) (time.Duration, error) {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	start := time.Now()
	conn, err := dialer.DialContext(ctx, network, host)
	duration := time.Since(start)
	if err == nil {
		_ = conn.Close()
	}
	return duration, err
}

// checkConnectivity tests outbound network reachability to Google OAuth and Cloud Code Assist endpoints,
// reports address-family health for the Cloud Code hosts, and surfaces proxy configuration without
// echoing proxy credentials.
func checkConnectivity(ctx context.Context) []CheckItem {
	var items []CheckItem

	endpoints := []struct {
		name string
		host string
		port string
		// families enables per-address-family probing for hosts whose IPv6 path commonly stalls.
		families bool
	}{
		{name: "Google OAuth 2.0 Endpoint", host: "oauth2.googleapis.com", port: "443"},
		{name: "Cloud Code Assist Daily Endpoint", host: "daily-cloudcode-pa.googleapis.com", port: "443", families: true},
		{name: "Cloud Code Assist PA Endpoint", host: "cloudcode-pa.googleapis.com", port: "443", families: true},
	}

	for _, ep := range endpoints {
		hostPort := net.JoinHostPort(ep.host, ep.port)
		duration, err := connectivityDialer(ctx, hostPort)

		if err != nil {
			items = append(items, CheckItem{
				Category: "Upstream Connectivity",
				Name:     ep.name,
				Status:   StatusFail,
				Message:  fmt.Sprintf("Unreachable (%v)", err),
				Hint:     "Check your internet connection, DNS resolver, and HTTPS_PROXY settings",
			})
			continue
		}

		items = append(items, CheckItem{
			Category: "Upstream Connectivity",
			Name:     ep.name,
			Status:   StatusOK,
			Message:  fmt.Sprintf("Reachable (%s latency)", duration.Round(time.Millisecond)),
		})

		if ep.families {
			items = append(items, addressFamilyChecks(ctx, ep.name, ep.host, ep.port)...)
		}
	}

	return append(items, proxyEnvironmentChecks()...)
}

// addressFamilyChecks probes IPv4 and IPv6 reachability independently. A family with no published
// address is reported as informational rather than a failure, while a published but unreachable family
// is a warning: it is the usual cause of multi-second Happy Eyeballs stalls.
func addressFamilyChecks(ctx context.Context, endpointName, host, port string) []CheckItem {
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	addrs, err := addressFamilyResolver(lookupCtx, host)
	cancel()
	if err != nil {
		return []CheckItem{{
			Category: "Upstream Connectivity",
			Name:     endpointName + " address families",
			Status:   StatusWarn,
			Message:  fmt.Sprintf("Could not resolve address families (%v)", err),
			Hint:     "Verify DNS resolution; per-family health cannot be determined",
		}}
	}

	var items []CheckItem
	for _, family := range []struct {
		label   string
		network string
		isIPv4  bool
	}{
		{label: "IPv4", network: "tcp4", isIPv4: true},
		{label: "IPv6", network: "tcp6", isIPv4: false},
	} {
		published := slices.ContainsFunc(addrs, func(addr net.IPAddr) bool {
			return (addr.IP.To4() != nil) == family.isIPv4
		})
		if !published {
			items = append(items, CheckItem{
				Category: "Upstream Connectivity",
				Name:     fmt.Sprintf("%s over %s", endpointName, family.label),
				Status:   StatusOK,
				Message:  fmt.Sprintf("No %s address published by DNS", family.label),
			})
			continue
		}

		dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
		duration, dialErr := familyDialer(dialCtx, family.network, net.JoinHostPort(host, port))
		dialCancel()
		if dialErr != nil {
			items = append(items, CheckItem{
				Category: "Upstream Connectivity",
				Name:     fmt.Sprintf("%s over %s", endpointName, family.label),
				Status:   StatusWarn,
				Message:  fmt.Sprintf("Unreachable (%v)", dialErr),
				Hint:     "If IPv6 blackholes traffic, set AGW_PREFER_IPV4=1 to dial IPv4 first",
			})
			continue
		}

		items = append(items, CheckItem{
			Category: "Upstream Connectivity",
			Name:     fmt.Sprintf("%s over %s", endpointName, family.label),
			Status:   StatusOK,
			Message:  fmt.Sprintf("Reachable (%s latency)", duration.Round(time.Millisecond)),
		})
	}

	return items
}

// proxyEnvironmentChecks reports whether HTTPS_PROXY routes upstream traffic and masks any credentials
// embedded in the proxy URL.
func proxyEnvironmentChecks() []CheckItem {
	raw := cmp.Or(os.Getenv("HTTPS_PROXY"), os.Getenv("https_proxy"))
	if raw == "" {
		return []CheckItem{{
			Category: "Upstream Connectivity",
			Name:     "HTTPS Proxy",
			Status:   StatusOK,
			Message:  "Direct connection (no HTTPS_PROXY configured)",
		}}
	}

	item := CheckItem{
		Category: "Upstream Connectivity",
		Name:     "HTTPS Proxy",
		Status:   StatusOK,
		Message:  fmt.Sprintf("Configured (%s)", redactProxyURL(raw)),
		Hint:     "Connectivity probes above dial Google directly; a proxy must allow the Cloud Code and OAuth hosts",
	}
	if noProxy := cmp.Or(os.Getenv("NO_PROXY"), os.Getenv("no_proxy")); noProxy != "" && !strings.Contains(noProxy, "googleapis.com") {
		item.Message += fmt.Sprintf("; NO_PROXY=%s", noProxy)
	}
	return []CheckItem{item}
}

// redactProxyURL renders a proxy URL without its userinfo, so embedded proxy credentials never reach
// the terminal or a diagnostic report.
func redactProxyURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "<unparseable proxy url>"
	}
	return fmt.Sprintf("%s://%s", parsed.Scheme, parsed.Host)
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
