// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/spf13/cobra"
)

// maxConcurrentQuotaChecks bounds parallel upstream account inspections so multi-account pools
// stay responsive without bursting Google OAuth and Cloud Code endpoints simultaneously.
const maxConcurrentQuotaChecks = 4

var (
	// checkAccountsQuota toggles live upstream quota inspection when listing accounts.
	checkAccountsQuota bool

	// checkAccountsReenable opts in to clearing the disabled flag for accounts whose refresh token
	// still exchanges successfully during a live --check inspection.
	checkAccountsReenable bool

	// accountsJSON toggles machine-readable JSON output for account listings and live checks.
	accountsJSON bool

	// accountsCmd lists configured Google Antigravity accounts and their statuses.
	accountsCmd = &cobra.Command{
		Use:   "accounts",
		Short: "List configured Google Antigravity accounts",
		Long: `Accounts displays all Google accounts stored in ~/.agw/accounts.json
along with their companion project IDs, tiers, credential sources, and statuses.
Pass --check to inspect live upstream rolling (5h), weekly, and family quotas.
Never reveals access or refresh tokens.`,
		Example: `  agw accounts
  agw accounts --check
  agw accounts --check --re-enable
  agw accounts --check --json`,
		RunE: runAccounts,
	}
)

func init() {
	accountsCmd.Flags().BoolVar(&checkAccountsQuota, "check", false, "Query live quota remaining per account")
	accountsCmd.Flags().BoolVar(&checkAccountsReenable, "re-enable", false, "Re-enable disabled accounts whose credentials still work (requires --check)")
	accountsCmd.Flags().BoolVar(&accountsJSON, "json", false, "Output account details as JSON")
	rootCmd.AddCommand(accountsCmd)
}

// accountReportEntry holds redacted account metadata and optional live quota details for CLI and
// JSON output. Credentials are never stored or serialized in this structure.
type accountReportEntry struct {
	// Email is the Google account email address.
	Email string `json:"email"`

	// ProjectID is the associated Google Cloud Code companion project identifier.
	ProjectID string `json:"project_id"`

	// Tier is the normalized subscription tier ("personal", "business", "enterprise", or empty).
	Tier string `json:"tier,omitempty"`

	// Source identifies how credentials were stored ("oauth" or "local_desktop").
	Source string `json:"source"`

	// Status describes the current operational or cooldown state.
	Status string `json:"status"`

	// Disabled reports whether the account is currently disabled in accounts.json.
	Disabled bool `json:"disabled,omitzero"`

	// AppealURL carries the validated upstream appeal link for a suspended account, omitted otherwise.
	AppealURL string `json:"appeal_url,omitempty"`

	// AddedAt records when the account was first authenticated.
	AddedAt string `json:"added_at,omitempty"`

	// FiveHourQuota formats the rolling five-hour quota percentage when checked.
	FiveHourQuota string `json:"five_hour_quota,omitempty"`

	// WeeklyQuota formats the weekly allowance percentage when checked.
	WeeklyQuota string `json:"weekly_quota,omitempty"`

	// GeminiQuota formats the minimum remaining Gemini family quota percentage when checked.
	GeminiQuota string `json:"gemini_quota,omitempty"`

	// ClaudeQuota formats the minimum remaining Claude family quota percentage when checked.
	ClaudeQuota string `json:"claude_quota,omitempty"`

	// ResetTime formats the earliest relevant reset countdown when checked.
	ResetTime string `json:"reset_time,omitempty"`

	// FiveHour holds the raw rolling five-hour window metrics when available.
	FiveHour *catalog.QuotaWindow `json:"five_hour,omitzero"`

	// Weekly holds the raw weekly allowance window metrics when available.
	Weekly *catalog.QuotaWindow `json:"weekly,omitzero"`

	// Summary holds the per-family remaining fractions and reset timestamps when available.
	Summary *catalog.QuotaSummary `json:"family_summary,omitzero"`

	// Models holds per-model quota metrics keyed by lowercase model ID when live-checked.
	Models map[string]catalog.ModelQuota `json:"models,omitempty"`

	// Error holds a sanitized fetch error message if upstream inspection failed for this account.
	Error string `json:"error,omitempty"`
}

// accountSourceLabel normalizes the credential source label shown in tables and JSON reports.
func accountSourceLabel(acc *auth.Account) string {
	if acc != nil && acc.IsLocalDesktopSession() {
		return auth.CredentialSourceLocalDesktop
	}
	return "oauth"
}

// accountBaseStatus formats the persisted active, suspended, or disabled status string from accounts.json.
func accountBaseStatus(acc *auth.Account) string {
	if acc == nil || !acc.Disabled {
		return "Active"
	}
	if acc.Suspended() {
		return fmt.Sprintf("Suspended (%s)", acc.SuspensionKind())
	}
	if acc.LastError != "" {
		return fmt.Sprintf("Disabled (%s)", acc.LastError)
	}
	return "Disabled"
}

// buildStoredAccountEntries builds redacted account entries purely from stored accounts.json data.
func buildStoredAccountEntries(accounts []*auth.Account) []accountReportEntry {
	entries := make([]accountReportEntry, len(accounts))
	for i, acc := range accounts {
		entries[i] = accountReportEntry{
			Email:     acc.Email,
			ProjectID: acc.ProjectID,
			Tier:      acc.Tier,
			Source:    accountSourceLabel(acc),
			Status:    accountBaseStatus(acc),
			Disabled:  acc.Disabled,
			AppealURL: acc.AppealURL,
			AddedAt:   acc.AddedAt,
		}
	}
	return entries
}

// inspectAccountQuotas queries live upstream model catalogs and account quota summaries across all
// configured accounts in parallel using a bounded worker pool, preserving account slice order.
func inspectAccountQuotas(
	ctx context.Context,
	cfg *config.Config,
	accountStore *auth.Store,
	accounts []*auth.Account,
	reEnable bool,
	now time.Time,
	httpClient *http.Client,
) []accountReportEntry {
	entries := make([]accountReportEntry, len(accounts))
	if len(accounts) == 0 {
		return entries
	}
	if httpClient == nil {
		httpClient = config.DefaultHTTPClient(catalog.DefaultFetchTimeout)
	}

	sem := make(chan struct{}, min(maxConcurrentQuotaChecks, len(accounts)))
	var wg sync.WaitGroup

	for i, acc := range accounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ctx.Err() != nil {
				entries[i] = cancelledAccountReportEntry(acc, ctx.Err())
				return
			}
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				entries[i] = cancelledAccountReportEntry(acc, ctx.Err())
				return
			}
			if ctx.Err() != nil {
				entries[i] = cancelledAccountReportEntry(acc, ctx.Err())
				return
			}

			entries[i] = inspectSingleAccountQuota(ctx, cfg, accountStore, acc, reEnable, now, httpClient)
		}()
	}

	wg.Wait()
	return entries
}

// cancelledAccountReportEntry builds a redacted entry when quota inspection is aborted by context cancellation.
func cancelledAccountReportEntry(acc *auth.Account, err error) accountReportEntry {
	return accountReportEntry{
		Email:     acc.Email,
		ProjectID: acc.ProjectID,
		Tier:      acc.Tier,
		Source:    accountSourceLabel(acc),
		Status:    "Cancelled",
		Disabled:  acc.Disabled,
		AddedAt:   acc.AddedAt,
		Error:     err.Error(),
	}
}

// inspectSingleAccountQuota retrieves live quota windows and per-model quotas for one account.
func inspectSingleAccountQuota(
	ctx context.Context,
	cfg *config.Config,
	accountStore *auth.Store,
	acc *auth.Account,
	reEnable bool,
	now time.Time,
	httpClient *http.Client,
) accountReportEntry {
	entry := accountReportEntry{
		Email:         acc.Email,
		ProjectID:     acc.ProjectID,
		Tier:          acc.Tier,
		Source:        accountSourceLabel(acc),
		Status:        accountBaseStatus(acc),
		Disabled:      acc.Disabled,
		AppealURL:     acc.AppealURL,
		AddedAt:       acc.AddedAt,
		FiveHourQuota: "-",
		WeeklyQuota:   "-",
		GeminiQuota:   "-",
		ClaudeQuota:   "-",
		ResetTime:     "Ready",
	}

	// A suspended account must not receive any upstream traffic: Google refused the account itself, and
	// probing its quota afterwards is exactly what turns a first suspension into a permanent one.
	// Re-enabling stays an explicit operator action, so --re-enable clears the local verdict without a
	// probe instead of sending a token refresh and catalog fetch to a suspended account.
	if acc.Suspended() {
		if reEnable {
			if clearErr := accountStore.SetDisabled(acc.Email, false, ""); clearErr != nil {
				entry.Status = fmt.Sprintf("Error (re-enable failed: %v)", clearErr)
				entry.Error = clearErr.Error()
				return entry
			}
			entry.Status = "Active (suspension cleared; appeal verified)"
			entry.Disabled = false
			entry.AppealURL = ""
			return entry
		}
		entry.Status = accountBaseStatus(acc)
		return entry
	}

	tokenRes, refreshErr := auth.AccessTokenFor(ctx, cfg, acc)
	if refreshErr != nil {
		var oauthErr *auth.OAuthError
		switch {
		case errors.Is(refreshErr, auth.ErrLocalSessionExpired):
			entry.Status = "Local session expired (run 'agw login --local')"
		case errors.As(refreshErr, &oauthErr) && oauthErr.InvalidGrant:
			_ = accountStore.SetDisabled(acc.Email, true, "refresh token revoked")
			entry.Status = "Disabled (refresh token revoked)"
			entry.Disabled = true
		default:
			entry.Status = "Error (refresh failed)"
		}
		entry.Error = refreshErr.Error()
		return entry
	}

	if acc.Disabled {
		// A live inspection must not silently mutate stored account state: re-enabling a
		// disabled or ToS-flagged account requires the explicit --re-enable opt-in.
		if reEnable {
			if reEnableErr := accountStore.SetDisabled(acc.Email, false, ""); reEnableErr != nil {
				entry.Status = fmt.Sprintf("Error (re-enable failed: %v)", reEnableErr)
				entry.Error = reEnableErr.Error()
				return entry
			}
			entry.Status = "Active"
			entry.Disabled = false
		} else {
			entry.Status = "Disabled (credentials valid; rerun with --re-enable to activate)"
		}
	}

	snap, fetchErr := catalog.FetchCatalogForTier(ctx, cfg, httpClient, tokenRes.AccessToken, acc.ProjectID, acc.Tier)
	if fetchErr != nil {
		entry.GeminiQuota = "Error"
		entry.ClaudeQuota = "Error"
		entry.ResetTime = "-"
		entry.Error = fetchErr.Error()
	} else {
		summary := catalog.SummarizeQuotas(snap.Quotas)
		entry.Summary = &summary
		entry.Models = snap.Quotas
		entry.GeminiQuota = catalog.FormatQuota(summary.GeminiRemainingFraction)
		entry.ClaudeQuota = catalog.FormatQuota(summary.ClaudeRemainingFraction)

		isCooling := (summary.GeminiRemainingFraction != nil && *summary.GeminiRemainingFraction == 0) ||
			(summary.ClaudeRemainingFraction != nil && *summary.ClaudeRemainingFraction == 0)
		if isCooling && entry.Status == "Active" {
			entry.Status = "Cooling"
		}

		// Fold weekly window from per-model quota summaries if present.
		if weeklyFrac, weeklyReset := minWeeklyWindow(summary); weeklyFrac != nil || weeklyReset != "" {
			entry.Weekly = &catalog.QuotaWindow{
				RemainingFraction: weeklyFrac,
				ResetTime:         weeklyReset,
			}
		}
	}

	// Query v1internal:retrieveUserQuotaSummary best-effort for dedicated 5-hour and weekly windows.
	// Unavailable summary endpoints (e.g. 404 on unsupported surfaces) degrade cleanly to "-".
	if accSummary, summaryErr := catalog.FetchAccountQuotaSummary(ctx, cfg, httpClient, tokenRes.AccessToken, acc.ProjectID, acc.Tier); summaryErr == nil && accSummary != nil {
		if accSummary.FiveHour != nil {
			entry.FiveHour = accSummary.FiveHour
			entry.FiveHourQuota = catalog.FormatQuota(accSummary.FiveHour.RemainingFraction)
			if accSummary.FiveHour.RemainingFraction != nil && *accSummary.FiveHour.RemainingFraction == 0 && entry.Status == "Active" {
				entry.Status = "Cooling"
			}
		}
		if accSummary.Weekly != nil {
			entry.Weekly = accSummary.Weekly
			if accSummary.Weekly.RemainingFraction != nil && *accSummary.Weekly.RemainingFraction == 0 && entry.Status == "Active" {
				entry.Status = "Cooling"
			}
		}
	}

	if entry.Weekly != nil && (entry.Weekly.RemainingFraction != nil || entry.Weekly.ResetTime != "") {
		entry.WeeklyQuota = catalog.FormatQuota(entry.Weekly.RemainingFraction)
	}

	entry.ResetTime = selectPrimaryResetTime(entry, now)
	return entry
}

// minWeeklyWindow returns the most constrained weekly fraction and reset timestamp across model families.
func minWeeklyWindow(summary catalog.QuotaSummary) (*float64, string) {
	var frac *float64
	var reset string

	if summary.GeminiWeeklyRemainingFraction != nil || summary.GeminiWeeklyResetTime != "" {
		frac = summary.GeminiWeeklyRemainingFraction
		reset = summary.GeminiWeeklyResetTime
	}
	if summary.ClaudeWeeklyRemainingFraction != nil || summary.ClaudeWeeklyResetTime != "" {
		if frac == nil || (summary.ClaudeWeeklyRemainingFraction != nil && *summary.ClaudeWeeklyRemainingFraction < *frac) {
			frac = summary.ClaudeWeeklyRemainingFraction
			if summary.ClaudeWeeklyResetTime != "" {
				reset = summary.ClaudeWeeklyResetTime
			}
		} else if reset == "" && summary.ClaudeWeeklyResetTime != "" {
			reset = summary.ClaudeWeeklyResetTime
		}
	}
	return frac, reset
}

// selectPrimaryResetTime picks the highest-priority reset timestamp for tabular display, favoring
// exhausted windows (0% remaining), then partially consumed windows, then remaining reset metadata.
func selectPrimaryResetTime(entry accountReportEntry, now time.Time) string {
	type candidate struct {
		// fraction is the remaining capacity fraction for the window, if reported.
		fraction *float64
		// reset is the RFC 3339 reset timestamp for the window.
		reset string
	}

	var candidates []candidate
	if entry.FiveHour != nil {
		candidates = append(candidates, candidate{entry.FiveHour.RemainingFraction, entry.FiveHour.ResetTime})
	}
	if entry.Summary != nil {
		candidates = append(candidates,
			candidate{entry.Summary.GeminiRemainingFraction, entry.Summary.GeminiResetTime},
			candidate{entry.Summary.ClaudeRemainingFraction, entry.Summary.ClaudeResetTime},
		)
	}
	if entry.Weekly != nil {
		candidates = append(candidates, candidate{entry.Weekly.RemainingFraction, entry.Weekly.ResetTime})
	}

	for _, c := range candidates {
		if c.fraction != nil && *c.fraction == 0 && c.reset != "" {
			return catalog.FormatResetCountdown(c.reset, now)
		}
	}
	for _, c := range candidates {
		if c.fraction != nil && *c.fraction < 1.0 && c.reset != "" {
			return catalog.FormatResetCountdown(c.reset, now)
		}
	}
	for _, c := range candidates {
		if c.reset != "" {
			return catalog.FormatResetCountdown(c.reset, now)
		}
	}

	if entry.Error != "" && entry.GeminiQuota == "Error" {
		return "-"
	}
	return "Ready"
}

// runAccounts reads configured accounts from disk and renders a formatted status table or JSON report.
func runAccounts(cmd *cobra.Command, args []string) error {
	if checkAccountsReenable && !checkAccountsQuota {
		return errors.New("--re-enable requires --check: it only clears the disabled flag for accounts verified during a live quota inspection")
	}

	accPath, err := config.AccountsFilePath()
	if err != nil {
		return fmt.Errorf("resolving accounts file path: %w", err)
	}
	accountStore := auth.NewStore(accPath)
	accounts, err := accountStore.Load()
	if err != nil {
		return fmt.Errorf("loading accounts: %w", err)
	}

	if len(accounts) == 0 {
		if accountsJSON {
			return writeJSONOutput(cmd, map[string]any{"accounts": []accountReportEntry{}})
		}
		cmd.Println("No accounts configured yet. Run 'agw login' to authenticate an account.")
		return nil
	}

	if !checkAccountsQuota {
		entries := buildStoredAccountEntries(accounts)
		if accountsJSON {
			return writeJSONOutput(cmd, map[string]any{"accounts": entries})
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
		_, _ = fmt.Fprintln(w, "EMAIL\tPROJECT ID\tTIER\tSOURCE\tSTATUS\tADDED AT")
		hasDisabled := false
		hasSuspended := false
		for _, entry := range entries {
			if entry.Disabled && entry.AppealURL == "" {
				hasDisabled = true
			}
			if entry.AppealURL != "" {
				hasSuspended = true
			}
			tier := cmp.Or(entry.Tier, "-")
			added := cmp.Or(entry.AddedAt, "-")
			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				entry.Email, entry.ProjectID, tier, entry.Source, entry.Status, added)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		cmd.Println("\nRun 'agw accounts --check' for live quota, or 'agw quota --models' for per-model breakdown.")
		if hasSuspended {
			cmd.Println("Suspended accounts stay out of rotation until Google lifts the suspension. Appeal once, then revoke third-party access before signing in again:")
			for _, entry := range entries {
				if entry.AppealURL != "" {
					cmd.Printf("  %s  %s\n", entry.Email, entry.AppealURL)
				}
			}
		}
		if hasDisabled {
			cmd.Println("Tip: verify and reactivate working disabled accounts with 'agw accounts --check --re-enable'.")
		}
		return nil
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	// A live quota inspection cannot report anything meaningful without configuration, so a config
	// failure aborts the command instead of printing an "Error (config)" status and exiting 0.
	cfg, cfgErr := loadConfig()
	if cfgErr != nil {
		return fmt.Errorf("loading configuration for live quota check: %w", cfgErr)
	}

	now := time.Now()
	httpClient := config.DefaultHTTPClient(catalog.DefaultFetchTimeout)
	entries := inspectAccountQuotas(ctx, cfg, accountStore, accounts, checkAccountsReenable, now, httpClient)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if accountsJSON {
		return writeJSONOutput(cmd, map[string]any{
			"checked_at": now.UTC().Format(time.RFC3339),
			"accounts":   entries,
		})
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(w, "EMAIL\tPROJECT ID\tTIER\tSTATUS\t5H QUOTA\tWEEKLY QUOTA\tGEMINI QUOTA\tCLAUDE QUOTA\tRESET TIME")
	for _, entry := range entries {
		tier := cmp.Or(entry.Tier, "-")
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			entry.Email,
			entry.ProjectID,
			tier,
			entry.Status,
			entry.FiveHourQuota,
			entry.WeeklyQuota,
			entry.GeminiQuota,
			entry.ClaudeQuota,
			entry.ResetTime,
		)
	}
	return w.Flush()
}

// writeJSONOutput encodes v as indented JSON onto cmd's standard output stream.
func writeJSONOutput(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
