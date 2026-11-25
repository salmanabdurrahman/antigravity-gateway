// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"context"
	"errors"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/spf13/cobra"
)

var (
	// checkAccountsQuota toggles live upstream quota inspection when listing accounts.
	checkAccountsQuota bool

	// checkAccountsReenable opts in to clearing the disabled flag for accounts whose refresh token
	// still exchanges successfully during a live --check inspection.
	checkAccountsReenable bool

	// accountsCmd lists configured Google Antigravity accounts and their statuses.
	accountsCmd = &cobra.Command{
		Use:   "accounts",
		Short: "List configured Google Antigravity accounts",
		Long: `Accounts displays all Google accounts stored in ~/.agw/accounts.json
along with their associated companion project IDs and active/disabled statuses.
Pass --check to inspect live upstream quota remaining per account.
Never reveals access or refresh tokens.`,
		RunE: runAccounts,
	}
)

func init() {
	accountsCmd.Flags().BoolVar(&checkAccountsQuota, "check", false, "Query live quota remaining per account")
	accountsCmd.Flags().BoolVar(&checkAccountsReenable, "re-enable", false, "Re-enable disabled accounts whose credentials still work (requires --check)")
	rootCmd.AddCommand(accountsCmd)
}

// runAccounts reads configured accounts from disk and renders a formatted status table.
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
		cmd.Println("No accounts configured yet. Run 'agw login' to authenticate an account.")
		return nil
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)

	if !checkAccountsQuota {
		_, _ = fmt.Fprintln(w, "EMAIL\tPROJECT ID\tSTATUS\tADDED AT")
		for _, acc := range accounts {
			status := "Active"
			if acc.Disabled {
				if acc.LastError != "" {
					status = fmt.Sprintf("Disabled (%s)", acc.LastError)
				} else {
					status = "Disabled"
				}
			}

			added := acc.AddedAt
			if added == "" {
				added = "-"
			}

			_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", acc.Email, acc.ProjectID, status, added)
		}
		return w.Flush()
	}

	// Live quota check mode
	_, _ = fmt.Fprintln(w, "EMAIL\tPROJECT ID\tSTATUS\tGEMINI QUOTA\tCLAUDE QUOTA\tRESET TIME")

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	cfg, cfgErr := loadConfig()
	now := time.Now()

	for _, acc := range accounts {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		status := "Active"
		if acc.Disabled {
			if acc.LastError != "" {
				status = fmt.Sprintf("Disabled (%s)", acc.LastError)
			} else {
				status = "Disabled"
			}
		}

		geminiQuota := "-"
		claudeQuota := "-"
		resetTime := "Ready"

		if cfgErr != nil {
			status = "Error (config)"
		} else {
			tokenRes, refreshErr := auth.AccessTokenFor(ctx, cfg, acc)
			if refreshErr != nil {
				var oauthErr *auth.OAuthError
				switch {
				case errors.Is(refreshErr, auth.ErrLocalSessionExpired):
					status = "Local session expired (run 'agw login --local')"
				case errors.As(refreshErr, &oauthErr) && oauthErr.InvalidGrant:
					_ = accountStore.SetDisabled(acc.Email, true, "refresh token revoked")
					status = "Disabled (refresh token revoked)"
				default:
					status = "Error (refresh failed)"
				}
			} else {
				if acc.Disabled {
					// A live inspection must not silently mutate stored account state: re-enabling a
					// disabled or ToS-flagged account requires the explicit --re-enable opt-in.
					if checkAccountsReenable {
						if reEnableErr := accountStore.SetDisabled(acc.Email, false, ""); reEnableErr != nil {
							return fmt.Errorf("re-enabling account %s: %w", acc.Email, reEnableErr)
						}
						status = "Active"
					} else {
						status = "Disabled (credentials valid; rerun with --re-enable to activate)"
					}
				}

				snap, fetchErr := catalog.FetchCatalogForTier(ctx, cfg, nil, tokenRes.AccessToken, acc.ProjectID, acc.Tier)
				if fetchErr != nil {
					geminiQuota = "Error"
					claudeQuota = "Error"
					resetTime = "-"
				} else {
					summary := catalog.SummarizeQuotas(snap.Quotas)
					geminiQuota = catalog.FormatQuota(summary.GeminiRemainingFraction)
					claudeQuota = catalog.FormatQuota(summary.ClaudeRemainingFraction)

					// If quota is exhausted (0%), status indicates Cooling
					isCooling := (summary.GeminiRemainingFraction != nil && *summary.GeminiRemainingFraction == 0) ||
						(summary.ClaudeRemainingFraction != nil && *summary.ClaudeRemainingFraction == 0)
					if isCooling && status == "Active" {
						status = "Cooling"
					}

					// Prioritize exhausted family (0% quota / cooling) first for reset time
					if summary.GeminiRemainingFraction != nil && *summary.GeminiRemainingFraction == 0 && summary.GeminiResetTime != "" {
						resetTime = catalog.FormatResetTime(summary.GeminiResetTime, now)
					} else if summary.ClaudeRemainingFraction != nil && *summary.ClaudeRemainingFraction == 0 && summary.ClaudeResetTime != "" {
						resetTime = catalog.FormatResetTime(summary.ClaudeResetTime, now)
					} else if summary.GeminiRemainingFraction != nil && *summary.GeminiRemainingFraction < 1.0 && summary.GeminiResetTime != "" {
						resetTime = catalog.FormatResetTime(summary.GeminiResetTime, now)
					} else if summary.ClaudeRemainingFraction != nil && *summary.ClaudeRemainingFraction < 1.0 && summary.ClaudeResetTime != "" {
						resetTime = catalog.FormatResetTime(summary.ClaudeResetTime, now)
					} else if summary.GeminiResetTime != "" {
						resetTime = catalog.FormatResetTime(summary.GeminiResetTime, now)
					} else if summary.ClaudeResetTime != "" {
						resetTime = catalog.FormatResetTime(summary.ClaudeResetTime, now)
					}
				}
			}
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", acc.Email, acc.ProjectID, status, geminiQuota, claudeQuota, resetTime)
	}

	return w.Flush()
}
