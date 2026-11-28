// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/client"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/risk"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/server"
	"github.com/spf13/cobra"
)

var (
	// statusPort overrides the gateway TCP port probed by agw status.
	statusPort int

	// statusJSON toggles structured JSON output for daemon and runtime metrics.
	statusJSON bool

	// statusRisk captures and persists local risk telemetry alongside the status report.
	statusRisk bool

	// statusCmd inspects the running daemon's health, active rotation strategy, and runtime pool metrics.
	statusCmd = &cobra.Command{
		Use:   "status",
		Short: "Show gateway daemon status, active strategy, and runtime metrics",
		Long: `Status probes the local Antigravity Gateway daemon (/health and /v1/accounts)
to report whether the proxy is running, its PID, active rotation strategy,
cached catalog summary, and per-account runtime request/429/cooldown metrics.`,
		Example: `  agw status
  agw status --port 51401
  agw status --json`,
		RunE: runStatus,
	}
)

func init() {
	statusCmd.Flags().IntVarP(&statusPort, "port", "p", 0, "Gateway HTTP port to inspect (default from config or 51400)")
	statusCmd.Flags().BoolVar(&statusJSON, "json", false, "Output daemon status and runtime metrics as JSON")
	statusCmd.Flags().BoolVar(&statusRisk, "risk", false, "Capture risk telemetry into ~/.agw/risk.json and print the local risk posture")
	rootCmd.AddCommand(statusCmd)
}

// statusReport captures non-sensitive runtime status for terminal rendering and JSON output.
type statusReport struct {
	// Running indicates whether a verified antigravity-gateway daemon answered /health.
	Running bool `json:"running"`

	// PID records the daemon process ID when available and alive.
	PID int `json:"pid,omitzero"`

	// URL is the local loopback base URL probed for the daemon.
	URL string `json:"url"`

	// Version reports the daemon version when running, or the CLI build version otherwise.
	Version string `json:"version"`

	// Strategy is the active rotation strategy reported by the daemon or config.json.
	Strategy string `json:"strategy"`

	// ConfigPath is the resolved configuration file path inspected by the CLI.
	ConfigPath string `json:"config_path,omitempty"`

	// UsableAccounts reports the count of usable accounts reported by /health or stored on disk.
	UsableAccounts int `json:"usable_accounts"`

	// TotalAccounts reports the total number of stored accounts in accounts.json.
	TotalAccounts int `json:"total_accounts"`

	// CachedModels reports the number of models present in models-cache.json.
	CachedModels int `json:"cached_models"`

	// CacheFetchedAt records when models-cache.json was last refreshed, if cached.
	CacheFetchedAt string `json:"cache_fetched_at,omitempty"`

	// RuntimeAccounts holds live per-account snapshots from GET /v1/accounts when running.
	RuntimeAccounts []rotator.AccountSnapshot `json:"accounts,omitempty"`

	// StoredAccounts holds stored disk account summaries when the daemon is stopped.
	StoredAccounts []accountReportEntry `json:"stored_accounts,omitempty"`

	// DiagnosticsError records a non-fatal error querying /v1/accounts while /health succeeded.
	DiagnosticsError string `json:"diagnostics_error,omitempty"`

	// Risk holds the captured risk posture when --risk was requested.
	Risk *risk.Snapshot `json:"risk,omitempty"`

	// RiskPath records where the captured snapshot was persisted, empty when persistence failed.
	RiskPath string `json:"risk_path,omitempty"`

	// RiskError records why the risk snapshot could not be persisted.
	RiskError string `json:"risk_error,omitempty"`

	// RiskGuardCooldownSeconds is the resolved park duration for a bare-rejection storm.
	RiskGuardCooldownSeconds int `json:"risk_guard_cooldown_seconds,omitzero"`
}

// runStatus probes the local gateway daemon and renders runtime or stopped-state diagnostics.
func runStatus(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	host := config.LoopbackHost(cfg.Host)
	port := cmp.Or(statusPort, cfg.Port, config.DefaultPort)
	baseURL := config.BaseURL(host, port)

	report := statusReport{
		URL:        baseURL,
		Version:    config.Version,
		Strategy:   cmp.Or(cfg.Strategy, config.DefaultStrategy),
		ConfigPath: activeConfigPath(),
	}

	var storedAccounts []*auth.Account
	if accPath, accErr := config.AccountsFilePath(); accErr == nil {
		if stored, loadErr := auth.NewStore(accPath).Load(); loadErr == nil {
			storedAccounts = stored
			report.TotalAccounts = len(stored)
			for _, a := range stored {
				if !a.Disabled && a.HasCredentials() {
					report.UsableAccounts++
				}
			}
			report.StoredAccounts = buildStoredAccountEntries(stored)
		}
	}

	if cachePath, cacheErr := config.ModelsCacheFilePath(); cacheErr == nil {
		cat := catalog.NewModelCatalog(cfg, nil, cachePath, nil)
		if loadErr := cat.LoadCache(); loadErr == nil {
			snap, fetchedAt := cat.Snapshot()
			report.CachedModels = len(snap.Models)
			if !fetchedAt.IsZero() {
				report.CacheFetchedAt = fetchedAt.UTC().Format(time.RFC3339)
			}
		}
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	healthCtx, cancel := context.WithTimeout(ctx, server.DefaultHealthTimeout)
	resolvedPID, health, resolveErr := server.ResolveRunningDaemon(healthCtx, host, port)
	cancel()

	if !errors.Is(resolveErr, server.ErrDaemonNotRunning) && health != nil && health.OK && health.Name == server.ServerName {
		report.Running = true
		report.Version = cmp.Or(health.Version, config.Version)
		report.UsableAccounts = health.Accounts
		report.StoredAccounts = nil
		if resolvedPID > 0 {
			report.PID = resolvedPID
		} else if statusPort == 0 || statusPort == cmp.Or(cfg.Port, config.DefaultPort) {
			// Fall back to the PID file only when inspecting the configured daemon port so a
			// custom --port probe never attributes the primary daemon's PID to another instance.
			if pidFile, pidErr := config.PIDFilePath(); pidErr == nil {
				if pid, readErr := server.ReadPID(pidFile); readErr == nil && server.IsProcessAlive(pid) {
					report.PID = pid
				}
			}
		}

		if accResp, diagErr := fetchRuntimeAccounts(ctx, baseURL, cfg.APIKey); diagErr != nil {
			report.DiagnosticsError = diagErr.Error()
		} else if accResp != nil {
			if accResp.Strategy != "" {
				report.Strategy = accResp.Strategy
			}
			report.RuntimeAccounts = accResp.Accounts
			report.TotalAccounts = len(accResp.Accounts)
		}
	}

	if statusRisk {
		captureRiskSnapshot(&report, cfg, storedAccounts, time.Now())
	}

	if statusJSON {
		return writeJSONOutput(cmd, report)
	}

	renderStatusReport(cmd, report, time.Now())
	return nil
}

// captureRiskSnapshot builds the local risk posture from stored accounts plus live runtime counters and
// persists it to risk.json. Failures are reported in the status output instead of aborting: telemetry is
// diagnostic, and a read-only home directory must not break incident triage.
func captureRiskSnapshot(report *statusReport, cfg *config.Config, stored []*auth.Account, now time.Time) {
	snapshot := buildRiskSnapshot(cfg, stored, report.RuntimeAccounts, now)
	report.Risk = &snapshot

	_, cooldown := cfg.PromptRejectionGuard()
	report.RiskGuardCooldownSeconds = int(cooldown / time.Second)

	riskPath, pathErr := config.RiskFilePath()
	if pathErr != nil {
		report.RiskError = pathErr.Error()
		return
	}
	if appendErr := risk.NewStore(riskPath).Append(snapshot); appendErr != nil {
		report.RiskError = appendErr.Error()
		return
	}
	report.RiskPath = riskPath
}

// buildRiskSnapshot merges stored account identities with live runtime counters. Identity comes from the
// credential store because it carries the opaque credential digest and suspension metadata; counters come
// from the daemon when one is running.
func buildRiskSnapshot(cfg *config.Config, stored []*auth.Account, runtime []rotator.AccountSnapshot, now time.Time) risk.Snapshot {
	limit, _ := cfg.PromptRejectionGuard()
	snapshot := risk.Snapshot{
		CapturedAt:           risk.TimestampNow(now),
		Profile:              cfg.EffectiveProfile(),
		RotationMode:         cfg.EffectiveRotationMode(),
		MaxInFlight:          cfg.MaxInFlightPerAccountLimit(),
		PromptRejectionLimit: limit,
		Accounts:             make([]risk.Account, 0, len(stored)),
	}

	counters := make(map[string]rotator.AccountSnapshot, len(runtime))
	for _, acc := range runtime {
		counters[strings.ToLower(acc.Email)] = acc
	}

	for _, acc := range stored {
		entry := risk.Account{
			Credential:  acc.CredentialKey(),
			Email:       risk.MaskEmail(acc.Email),
			Suspended:   acc.Suspended(),
			SuspendKind: acc.SuspendKind,
			LastError:   risk.RedactEmails(acc.LastError),
		}
		if live, ok := counters[strings.ToLower(acc.Email)]; ok {
			entry.Requests = live.Requests
			entry.RateLimits = live.RateLimits
			entry.PromptRejections = live.PromptRejections
			entry.LastUpstreamOK = live.LastUpstreamOK
			entry.LastError = risk.RedactEmails(cmp.Or(live.LastError, acc.LastError))
		}
		snapshot.Accounts = append(snapshot.Accounts, entry)
	}

	return snapshot
}

// fetchRuntimeAccounts retrieves redacted runtime account snapshots and strategy from /v1/accounts.
func fetchRuntimeAccounts(ctx context.Context, baseURL string, apiKey string) (*server.AccountsResponse, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/v1/accounts", nil)
	if err != nil {
		return nil, fmt.Errorf("building /v1/accounts request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	httpClient := client.NewNoRedirectClient(3 * time.Second)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying /v1/accounts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/v1/accounts returned HTTP %d", resp.StatusCode)
	}

	var accResp server.AccountsResponse
	if err := json.NewDecoder(resp.Body).Decode(&accResp); err != nil {
		return nil, fmt.Errorf("decoding /v1/accounts response: %w", err)
	}
	return &accResp, nil
}

// renderStatusReport writes a concise terminal summary of daemon health and runtime metrics.
func renderStatusReport(cmd *cobra.Command, report statusReport, now time.Time) {
	if report.Running {
		cmd.Printf("Daemon:    Running (v%s", report.Version)
		if report.PID > 0 {
			cmd.Printf(", PID %d", report.PID)
		}
		cmd.Println(")")
	} else {
		cmd.Println("Daemon:    Stopped")
	}

	cmd.Printf("Endpoint:  %s\n", report.URL)
	cmd.Printf("Strategy:  %s\n", report.Strategy)
	if report.ConfigPath != "" {
		cmd.Printf("Config:    %s\n", report.ConfigPath)
	}
	cmd.Printf("Accounts:  %d usable / %d total\n", report.UsableAccounts, report.TotalAccounts)
	if report.CachedModels > 0 {
		if report.CacheFetchedAt != "" {
			cmd.Printf("Catalog:   %d cached models (updated %s)\n", report.CachedModels, report.CacheFetchedAt)
		} else {
			cmd.Printf("Catalog:   %d cached models\n", report.CachedModels)
		}
	} else {
		cmd.Println("Catalog:   empty (run 'agw models list --refresh')")
	}

	if report.Risk != nil {
		renderRiskReport(cmd, report)
	}

	if !report.Running {
		cmd.Println("\nStart the daemon with 'agw start --detach'.")
		return
	}

	if report.DiagnosticsError != "" {
		cmd.Printf("\nRuntime metrics unavailable: %s\n", report.DiagnosticsError)
		return
	}

	if len(report.RuntimeAccounts) == 0 {
		return
	}

	cmd.Println()
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(w, "EMAIL\tTIER\tSTATUS\tHEALTH\tREQUESTS\t429S\tCOOLDOWNS\tLAST USED\tLAST ERROR")
	for _, acc := range report.RuntimeAccounts {
		tier := cmp.Or(acc.Tier, "-")
		status := "Active"
		if acc.Disabled {
			status = "Disabled"
		} else if len(acc.Cooldowns) > 0 {
			status = "Cooling"
		}

		healthStr := "-"
		if acc.Health > 0 || acc.RateLimits > 0 || acc.LastError != "" {
			healthStr = fmt.Sprintf("%.0f%%", acc.Health)
		}

		cooldowns := formatCooldownSummary(acc.Cooldowns, now)
		lastUsed := cmp.Or(formatRelativePast(acc.LastUsed, now), "-")
		lastErr := cmp.Or(acc.LastError, "-")

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
			acc.Email,
			tier,
			status,
			healthStr,
			acc.Requests,
			acc.RateLimits,
			cooldowns,
			lastUsed,
			lastErr,
		)
	}
	_ = w.Flush()
}

// formatCooldownSummary formats active model cooldowns and their remaining countdowns.
func formatCooldownSummary(cooldowns map[string]string, now time.Time) string {
	if len(cooldowns) == 0 {
		return "-"
	}
	models := slices.Sorted(maps.Keys(cooldowns))
	parts := make([]string, 0, len(models))
	for _, model := range models {
		parts = append(parts, fmt.Sprintf("%s (%s)", model, catalog.FormatResetCountdown(cooldowns[model], now)))
	}
	return strings.Join(parts, ", ")
}

// formatRelativePast formats an RFC3339 past timestamp concisely relative to now.
func formatRelativePast(ts string, now time.Time) string {
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	diff := now.Sub(t)
	if diff < 0 {
		return t.UTC().Format("15:04:05 UTC")
	}
	switch {
	case diff < time.Minute:
		return fmt.Sprintf("%ds ago", int(diff.Seconds()))
	case diff < time.Hour:
		return fmt.Sprintf("%dm ago", int(diff.Minutes()))
	case diff < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(diff.Hours()))
	default:
		return t.UTC().Format("Jan 02 15:04 UTC")
	}
}

// renderRiskReport prints the captured local risk posture together with per-account traffic counters.
func renderRiskReport(cmd *cobra.Command, report statusReport) {
	snapshot := report.Risk
	if snapshot == nil {
		return
	}

	cmd.Println("\n[Risk Telemetry]")
	cmd.Printf("  Captured:  %s\n", snapshot.CapturedAt)
	if report.RiskPath != "" {
		cmd.Printf("  Stored:    %s\n", report.RiskPath)
	} else if report.RiskError != "" {
		cmd.Printf("  Store:     unavailable (%s)\n", report.RiskError)
	}

	inFlight := "unbounded"
	if snapshot.MaxInFlight > 0 {
		inFlight = fmt.Sprintf("%d per account", snapshot.MaxInFlight)
	}
	guard := "off"
	if snapshot.PromptRejectionLimit > 0 {
		guard = fmt.Sprintf("%d bare 429s / %ds", snapshot.PromptRejectionLimit, report.RiskGuardCooldownSeconds)
	}
	cmd.Printf("  Posture:   %s profile, %s rotation, %s in flight, guard %s\n",
		snapshot.Profile, snapshot.RotationMode, inFlight, guard)

	if len(snapshot.Accounts) == 0 {
		return
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(w, "ACCOUNT\tSTATUS\tREQUESTS\t429S\tBARE 429S\tLAST OK\tLAST ERROR")
	for _, account := range snapshot.Accounts {
		status := "Active"
		if account.Suspended {
			status = fmt.Sprintf("Suspended (%s)", cmp.Or(account.SuspendKind, "unknown"))
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\t%s\n",
			cmp.Or(account.Email, "-"),
			status,
			account.Requests,
			account.RateLimits,
			account.PromptRejections,
			cmp.Or(account.LastUpstreamOK, "-"),
			cmp.Or(account.LastError, "-"),
		)
	}
	_ = w.Flush()
}
