// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
	"github.com/spf13/cobra"
)

var (
	// refreshModels forces retrieval of live model metadata from upstream Google Cloud Code Assist.
	refreshModels bool

	// modelsShowQuota includes cached remaining quota and reset countdowns in the model listing.
	modelsShowQuota bool

	// modelsJSON outputs the model catalog (and optional aliases/cached quotas) as JSON.
	modelsJSON bool

	// modelsCmd inspects available chat completion models and token boundaries.
	modelsCmd = &cobra.Command{
		Use:   "models",
		Short: "Inspect available chat models and token limits",
		Long: `Models displays Google Cloud Code Assist chat completion models,
context windows, max output token limits, and reasoning/multimodal capabilities.
By default, models are loaded from ~/.agw/models-cache.json for fast offline responses.`,
		Example: `  agw models list
  agw models list --quota
  agw models list --refresh
  agw models list --json`,
		RunE: runModelsList,
	}

	// modelsListCmd is the explicit 'list' subcommand under models.
	modelsListCmd = &cobra.Command{
		Use:     "list",
		Short:   "List all chat completion models",
		Long:    `List displays all available chat completion models with their limits and capabilities in a formatted table.`,
		Example: `  agw models list --refresh --quota`,
		RunE:    runModelsList,
	}
)

func init() {
	modelsCmd.PersistentFlags().BoolVar(&refreshModels, "refresh", false, "Force refresh model list from live upstream accounts")
	modelsCmd.PersistentFlags().BoolVarP(&modelsShowQuota, "quota", "q", false, "Include cached remaining quota per model from models-cache.json")
	modelsCmd.PersistentFlags().BoolVar(&modelsJSON, "json", false, "Output model catalog as JSON")
	modelsCmd.AddCommand(modelsListCmd)
	rootCmd.AddCommand(modelsCmd)
}

// runModelsList queries the model cache or upstream endpoints and renders a formatted table.
func runModelsList(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	cachePath, err := config.ModelsCacheFilePath()
	if err != nil {
		return fmt.Errorf("resolving model cache path: %w", err)
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

	var rot *rotator.Rotator
	if len(accounts) > 0 {
		pool, poolErr := rotator.NewPool(accountStore, cfg, nil)
		if poolErr == nil {
			rot = rotator.NewRotator(pool, cfg, nil, nil)
		}
	}

	var runner catalog.AccountRunner
	if rot != nil {
		runner = rot
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	cat := catalog.NewModelCatalog(cfg, runner, cachePath, nil)

	// If explicit refresh requested, refresh live
	if refreshModels {
		if rot == nil {
			return fmt.Errorf("cannot refresh models: no accounts configured. Run 'agw login' first")
		}
		if err := cat.Refresh(ctx); err != nil {
			return fmt.Errorf("refreshing model catalog: %w", err)
		}
	} else {
		// Ensure loads from disk or refreshes when the cache is empty. An upstream failure is only
		// recoverable when the cache itself is readable, so both failures are surfaced instead of
		// silently rendering an empty table.
		if ensureErr := cat.Ensure(ctx); ensureErr != nil {
			if loadErr := cat.LoadCache(); loadErr != nil {
				return fmt.Errorf("loading model catalog: %w", errors.Join(ensureErr, loadErr))
			}
		}
	}

	snap, fetchedAt := cat.Snapshot()
	if modelsJSON {
		payload := map[string]any{
			"models":        snap.Models,
			"default_model": snap.DefaultModel,
			"model_mapping": cfg.ModelMapping,
		}
		if !fetchedAt.IsZero() {
			payload["fetched_at"] = fetchedAt.UTC().Format(time.RFC3339)
		}
		if modelsShowQuota {
			quotas := snap.Quotas
			if quotas == nil {
				quotas = map[string]catalog.ModelQuota{}
			}
			payload["quotas"] = quotas
		}
		return writeJSONOutput(cmd, payload)
	}

	if len(snap.Models) == 0 {
		cmd.Println("No chat models available. Run 'agw login' to authenticate an account, or 'agw models list --refresh' to update.")
		return nil
	}

	now := time.Now()
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
	if modelsShowQuota {
		_, _ = fmt.Fprintln(w, "MODEL ID\tDISPLAY NAME\tCONTEXT\tMAX OUTPUT\tTHINKING\tIMAGES\tCACHED QUOTA")
	} else {
		_, _ = fmt.Fprintln(w, "MODEL ID\tDISPLAY NAME\tCONTEXT\tMAX OUTPUT\tTHINKING\tIMAGES")
	}

	for _, m := range snap.Models {
		displayName := cmp.Or(m.DisplayName, "-")

		thinking := "No"
		if m.Thinking {
			if m.ThinkingBudget != nil && *m.ThinkingBudget == -1 {
				thinking = "Yes (Adaptive)"
			} else if m.ThinkingBudget != nil && *m.ThinkingBudget > 0 {
				thinking = fmt.Sprintf("Yes (%d)", *m.ThinkingBudget)
			} else {
				thinking = "Yes"
			}
		}

		images := "No"
		if m.Images {
			images = "Yes"
		}

		if modelsShowQuota {
			quotaStr := formatCachedModelQuota(snap.Quotas, m.ID, now)
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
				m.ID,
				displayName,
				m.ContextWindow,
				m.MaxOutputTokens,
				thinking,
				images,
				quotaStr,
			)
		} else {
			_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n",
				m.ID,
				displayName,
				m.ContextWindow,
				m.MaxOutputTokens,
				thinking,
				images,
			)
		}
	}

	if err := w.Flush(); err != nil {
		return err
	}

	if len(cfg.ModelMapping) > 0 {
		cmd.Println("\nConfigured model aliases (config.json):")
		for _, alias := range slices.Sorted(maps.Keys(cfg.ModelMapping)) {
			cmd.Printf("  %s -> %s\n", alias, cfg.ModelMapping[alias])
		}
	}

	if !refreshModels {
		cmd.Printf("\n%s\n", formatCatalogCacheHint(fetchedAt, now))
	}
	return nil
}

// formatCachedModelQuota formats cached quota percentage and reset countdown for a single model.
func formatCachedModelQuota(quotas map[string]catalog.ModelQuota, modelID string, now time.Time) string {
	if len(quotas) == 0 {
		return "-"
	}
	q, ok := quotas[strings.ToLower(modelID)]
	if !ok {
		return "-"
	}
	pct := catalog.FormatQuota(q.RemainingFraction)
	if q.ResetTime == "" {
		return pct
	}
	countdown := catalog.FormatResetCountdown(q.ResetTime, now)
	if countdown == "Ready" {
		return pct
	}
	return fmt.Sprintf("%s (reset %s)", pct, countdown)
}

// formatCatalogCacheHint builds a concise cache age and refresh hint for cached model listings.
func formatCatalogCacheHint(fetchedAt, now time.Time) string {
	if fetchedAt.IsZero() {
		return "Served from local cache · run 'agw models list --refresh' to fetch live upstream models."
	}
	return fmt.Sprintf("Cached %s · run 'agw models list --refresh' to fetch live upstream models.", formatRelativePast(fetchedAt.UTC().Format(time.RFC3339), now))
}
