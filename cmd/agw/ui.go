// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"cmp"
	"fmt"
	"net/http"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/spf13/cobra"
)

// uiCmd opens the local web management dashboard in the default system browser.
var uiCmd = &cobra.Command{
	Use:   "ui",
	Short: "Open the local Antigravity Gateway web management dashboard",
	Long: `UI launches your default web browser and navigates to the local
dashboard hosted by the running Antigravity Gateway daemon (http://127.0.0.1:51400/ui).`,
	RunE: runUI,
}

func init() {
	rootCmd.AddCommand(uiCmd)
}

// runUI checks daemon status and opens the dashboard URL in the user's browser.
func runUI(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadOrCreate()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	host := cmp.Or(cfg.Host, config.DefaultHost)
	port := cfg.Port
	if port <= 0 {
		port = config.DefaultPort
	}

	dashboardURL := fmt.Sprintf("http://%s:%d/ui", host, port)

	// Check if daemon is running
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://%s:%d/health", host, port))
	if err != nil || resp.StatusCode != http.StatusOK {
		cmd.Println("Warning: Antigravity Gateway daemon does not appear to be running.")
		cmd.Println("Start the daemon in the background first:")
		cmd.Println("  agw start -d")
		cmd.Println()
	} else {
		_ = resp.Body.Close()
	}

	cmd.Printf("Opening dashboard at %s ...\n", dashboardURL)
	openBrowser(dashboardURL)
	return nil
}
