// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"github.com/spf13/cobra"
)

var (
	// cfgFile stores the user-specified custom configuration file path.
	cfgFile string

	// rootCmd represents the base command executed when agw is invoked without subcommands.
	rootCmd = &cobra.Command{
		Use:   "agw",
		Short: "Antigravity Gateway local reverse proxy CLI and daemon",
		Long: `Antigravity Gateway (agw) is a lightweight, zero-dependency local OpenAI-compatible
reverse proxy connecting AI coding agents directly to Google Antigravity subscriptions.`,
	}
)

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file path (default is ~/.agw/config.json or $AGW_HOME/config.json)")
	rootCmd.AddCommand(versionCmd)
}

// Execute runs the root CLI command and handles error returns.
func Execute() error {
	return rootCmd.Execute()
}
