// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/spf13/cobra"
)

// minQuotaWatchInterval prevents aggressive polling loops against Google Cloud Code endpoints.
const minQuotaWatchInterval = 5 * time.Second

var (
	// quotaShowModels toggles printing full per-model quota breakdowns under each account.
	quotaShowModels bool

	// quotaJSON outputs the live quota report as structured JSON.
	quotaJSON bool

	// quotaWatch enables periodic terminal polling of live account quotas.
	quotaWatch bool

	// quotaWatchInterval defines the polling interval when --watch is enabled.
	quotaWatchInterval time.Duration

	// quotaReenable opts in to clearing the disabled flag for accounts whose credentials work.
	quotaReenable bool

	// quotaCmd inspects live upstream rolling (5h), weekly, and per-model quotas across accounts.
	quotaCmd = &cobra.Command{
		Use:   "quota",
		Short: "Inspect live rolling (5h), weekly, and per-model account quotas",
		Long: `Quota queries live upstream Cloud Code Assist usage across all configured accounts.
It separates rolling 5-hour and weekly allowance windows, reports family minimums,
and highlights constrained or cooling models. Pass --models for the complete per-model
table or --watch for continuous terminal monitoring.`,
		Example: `  agw quota
  agw quota --models
  agw quota --watch --interval 30s
  agw quota --json`,
		RunE: runQuota,
	}
)

func init() {
	quotaCmd.Flags().BoolVarP(&quotaShowModels, "models", "m", false, "Show full per-model quota breakdown for every account")
	quotaCmd.Flags().BoolVar(&quotaJSON, "json", false, "Output quota report as JSON")
	quotaCmd.Flags().BoolVarP(&quotaWatch, "watch", "w", false, "Continuously refresh quota status in the terminal")
	quotaCmd.Flags().DurationVar(&quotaWatchInterval, "interval", 30*time.Second, "Refresh interval when --watch is enabled (minimum 5s)")
	quotaCmd.Flags().BoolVar(&quotaReenable, "re-enable", false, "Re-enable disabled accounts whose credentials still work")
	rootCmd.AddCommand(quotaCmd)
}

// runQuota executes one live quota snapshot or runs a periodic watch loop until cancelled.
func runQuota(cmd *cobra.Command, args []string) error {
	if quotaWatch && quotaWatchInterval < minQuotaWatchInterval {
		return fmt.Errorf("invalid --interval %s: must be at least %s", quotaWatchInterval, minQuotaWatchInterval)
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	if quotaWatch {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	accPath, err := config.AccountsFilePath()
	if err != nil {
		return fmt.Errorf("resolving accounts file path: %w", err)
	}
	accountStore := auth.NewStore(accPath)
	httpClient := config.DefaultHTTPClient(catalog.DefaultFetchTimeout)

	renderOnce := func() error {
		accounts, loadErr := accountStore.Load()
		if loadErr != nil {
			return fmt.Errorf("loading accounts: %w", loadErr)
		}

		if len(accounts) == 0 {
			if quotaJSON {
				return writeJSONOutput(cmd, map[string]any{"accounts": []accountReportEntry{}})
			}
			cmd.Println("No accounts configured yet. Run 'agw login' to authenticate an account.")
			return nil
		}

		now := time.Now()
		entries := inspectAccountQuotas(ctx, cfg, accountStore, accounts, quotaReenable, now, httpClient)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if quotaJSON {
			return writeJSONOutput(cmd, map[string]any{
				"checked_at": now.UTC().Format(time.RFC3339),
				"accounts":   entries,
			})
		}

		renderQuotaReport(cmd, entries, quotaShowModels, now)
		return nil
	}

	if err := renderOnce(); err != nil {
		return err
	}
	if !quotaWatch {
		return nil
	}

	ticker := time.NewTicker(quotaWatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if !quotaJSON {
				cmd.Println()
				cmd.Printf("--- Refreshed at %s (every %s, Ctrl+C to stop) ---\n", time.Now().UTC().Format("15:04:05 UTC"), quotaWatchInterval)
			}
			if err := renderOnce(); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil
				}
				return err
			}
		}
	}
}

// renderQuotaReport prints human-readable per-account quota windows and optional per-model tables.
func renderQuotaReport(cmd *cobra.Command, entries []accountReportEntry, showAllModels bool, now time.Time) {
	for i, entry := range entries {
		if i > 0 {
			cmd.Println()
		}

		tier := cmp.Or(entry.Tier, "unknown")
		cmd.Printf("%s  [%s · %s · %s]\n", entry.Email, tier, entry.Source, entry.Status)
		if entry.ProjectID != "" {
			cmd.Printf("  Project:       %s\n", entry.ProjectID)
		}
		if entry.Error != "" && entry.GeminiQuota == "-" {
			cmd.Printf("  Error:         %s\n", entry.Error)
			continue
		}

		cmd.Printf("  5h Window:     %s\n", formatWindowLine(entry.FiveHour, now))
		cmd.Printf("  Weekly Window: %s\n", formatWindowLine(entry.Weekly, now))
		cmd.Printf("  Gemini Family: %s\n", formatFamilyLine(entry.GeminiQuota, summaryGeminiReset(entry.Summary), now))
		cmd.Printf("  Claude Family: %s\n", formatFamilyLine(entry.ClaudeQuota, summaryClaudeReset(entry.Summary), now))
		if entry.Error != "" {
			cmd.Printf("  Error:         %s\n", entry.Error)
		}

		modelRows := collectModelQuotaRows(entry.Models, showAllModels)
		if len(modelRows) == 0 {
			continue
		}

		if showAllModels {
			cmd.Println("  Models:")
		} else {
			cmd.Println("  Constrained Models (<100%):")
		}

		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
		_, _ = fmt.Fprintln(w, "    MODEL\tREMAINING\tRESET\tWEEKLY")
		for _, row := range modelRows {
			weeklyStr := "-"
			if row.quota.Weekly != nil && !row.quota.Weekly.Untouched() {
				weeklyStr = formatWindowLine(row.quota.Weekly, now)
			}
			_, _ = fmt.Fprintf(w, "    %s\t%s\t%s\t%s\n",
				row.id,
				catalog.FormatQuota(row.quota.RemainingFraction),
				catalog.FormatResetCountdown(row.quota.ResetTime, now),
				weeklyStr,
			)
		}
		_ = w.Flush()
	}
}

// modelQuotaRow pairs a sorted model identifier with its quota descriptor for table rendering.
type modelQuotaRow struct {
	// id is the lowercase model identifier.
	id string

	// quota holds the upstream quota metrics for this model.
	quota catalog.ModelQuota
}

// collectModelQuotaRows extracts sorted model quota rows. When includeAll is false, only models
// with less than 100% capacity remaining or a non-untouched weekly window are included.
func collectModelQuotaRows(quotas map[string]catalog.ModelQuota, includeAll bool) []modelQuotaRow {
	if len(quotas) == 0 {
		return nil
	}

	keys := slices.Sorted(maps.Keys(quotas))
	rows := make([]modelQuotaRow, 0, len(keys))
	for _, id := range keys {
		q := quotas[id]
		constrained := (q.RemainingFraction != nil && *q.RemainingFraction < 1.0) ||
			(q.Weekly != nil && !q.Weekly.Untouched())
		if !includeAll && !constrained {
			continue
		}
		rows = append(rows, modelQuotaRow{id: id, quota: q})
	}
	return rows
}

// formatWindowLine formats a rolling or weekly QuotaWindow for terminal display.
func formatWindowLine(w *catalog.QuotaWindow, now time.Time) string {
	if w == nil || (w.RemainingFraction == nil && strings.TrimSpace(w.ResetTime) == "") {
		return "- (not reported)"
	}
	pct := catalog.FormatQuota(w.RemainingFraction)
	if w.ResetTime == "" {
		return pct
	}
	countdown := catalog.FormatResetCountdown(w.ResetTime, now)
	if countdown == "Ready" {
		return pct
	}
	return fmt.Sprintf("%s (reset %s)", pct, countdown)
}

// formatFamilyLine formats a family quota percentage alongside its earliest reset countdown.
func formatFamilyLine(pct string, reset string, now time.Time) string {
	if pct == "-" || pct == "Error" {
		return pct
	}
	if reset == "" {
		return pct
	}
	countdown := catalog.FormatResetCountdown(reset, now)
	if countdown == "Ready" {
		return pct
	}
	return fmt.Sprintf("%s (reset %s)", pct, countdown)
}

// summaryGeminiReset returns the Gemini reset timestamp from an optional QuotaSummary.
func summaryGeminiReset(summary *catalog.QuotaSummary) string {
	if summary == nil {
		return ""
	}
	return summary.GeminiResetTime
}

// summaryClaudeReset returns the Claude reset timestamp from an optional QuotaSummary.
func summaryClaudeReset(summary *catalog.QuotaSummary) string {
	if summary == nil {
		return ""
	}
	return summary.ClaudeResetTime
}
