// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"fmt"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/spf13/cobra"
)

// versionCmd prints the compiled semantic version string.
var versionCmd = &cobra.Command{
	Use:     "version",
	Short:   "Print the semantic version of agw",
	Long:    "Displays the compiled version string for the Antigravity Gateway binary.",
	Example: `  agw version`,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "agw version %s\n", config.Version)
	},
}
