// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"fmt"
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
		Args: cobra.ExactArgs(1),
		RunE: runLogout,
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

	removed, err := accountStore.Remove(email)
	if err != nil {
		return fmt.Errorf("removing account %s: %w", email, err)
	}

	if !removed {
		cmd.Printf("Account not found: %s\n", email)
		return nil
	}

	cmd.Printf("Successfully removed account: %s\n", email)
	return nil
}
