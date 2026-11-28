// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"fmt"
	"slices"
	"strings"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/spf13/cobra"
)

var (
	// logoutCmd removes a Google account from the local credential store.
	logoutCmd = &cobra.Command{
		Use:   "logout <email>",
		Short: "Remove a Google account from the local account store",
		Long: `Logout removes the specified Google account from ~/.agw/accounts.json.
Future requests will no longer rotate to or authenticate with this account.`,
		Example: `  agw logout user@example.com`,
		Args:    cobra.ExactArgs(1),
		RunE:    runLogout,
	}
)

func init() {
	rootCmd.AddCommand(logoutCmd)
}

// runLogout deletes the specified account by email address from accounts.json.
func runLogout(cmd *cobra.Command, args []string) error {
	email := strings.TrimSpace(args[0])
	accPath, err := config.AccountsFilePath()
	if err != nil {
		return fmt.Errorf("resolving accounts file path: %w", err)
	}
	accountStore := auth.NewStore(accPath)

	// Capture the stored verdict before removal: a suspended account needs remediation guidance the
	// operator can still act on, and the record is gone once the account is deleted.
	var removed *auth.Account
	if accounts, loadErr := accountStore.Load(); loadErr == nil {
		if idx := slices.IndexFunc(accounts, func(a *auth.Account) bool {
			return strings.EqualFold(strings.TrimSpace(a.Email), email)
		}); idx != -1 {
			removed = accounts[idx]
		}
	}

	wasRemoved, err := accountStore.Remove(email)
	if err != nil {
		return fmt.Errorf("removing account %s: %w", email, err)
	}

	if !wasRemoved {
		cmd.Printf("Account not found: %s\n", email)
		return nil
	}

	cmd.Printf("Successfully removed account: %s\n", email)
	printSuspensionGuidance(cmd, removed)
	return nil
}

// printSuspensionGuidance explains the remediation path for an account Google suspended before its
// credentials were deleted. It exists so the operator revokes the third-party integration before
// signing in again, instead of triggering a second and permanent suspension with background traffic.
func printSuspensionGuidance(cmd *cobra.Command, acc *auth.Account) {
	if acc == nil || !acc.Suspended() {
		return
	}

	cmd.Printf("\nThis account was suspended upstream (%s).\n", acc.SuspendKind)
	cmd.Println("Revoke third-party access at https://myaccount.google.com/permissions before signing in again.")
	if acc.AppealURL != "" {
		cmd.Printf("Appeal once: %s\n", acc.AppealURL)
	}
}
