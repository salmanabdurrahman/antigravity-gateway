// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"cmp"
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
	"github.com/spf13/cobra"
)

var (
	// refreshModels forces retrieval of live model metadata from upstream Google Cloud Code Assist.
	refreshModels bool

	// modelsCmd inspects available chat completion models and token boundaries.
	modelsCmd = &cobra.Command{
		Use:   "models",
		Short: "Inspect available chat models and token limits",
		Long: `Models displays Google Cloud Code Assist chat completion models,
context windows, max output token limits, and reasoning/multimodal capabilities.
By default, models are loaded from ~/.agw/models-cache.json for fast offline responses.`,
		RunE: runModelsList,
	}

	// modelsListCmd is the explicit 'list' subcommand under models.
	modelsListCmd = &cobra.Command{
		Use:   "list",
		Short: "List all chat completion models",
		Long:  `List displays all available chat completion models with their limits and capabilities in a formatted table.`,
		RunE:  runModelsList,
	}
)

func init() {
	modelsCmd.PersistentFlags().BoolVar(&refreshModels, "refresh", false, "Force refresh model list from live upstream accounts")
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
		// Ensure guarantees loading from disk or refreshing if empty
		if err := cat.Ensure(ctx); err != nil {
			// If ensure fails (e.g. offline and no cache), check if cache had anything
			_ = cat.LoadCache()
		}
	}

	snap, _ := cat.Snapshot()
	if len(snap.Models) == 0 {
		cmd.Println("No chat models available. Run 'agw login' to authenticate an account, or 'agw models list --refresh' to update.")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(w, "MODEL ID\tDISPLAY NAME\tCONTEXT\tMAX OUTPUT\tTHINKING\tIMAGES")

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

		_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n",
			m.ID,
			displayName,
			m.ContextWindow,
			m.MaxOutputTokens,
			thinking,
			images,
		)
	}

	return w.Flush()
}
