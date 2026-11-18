// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"fmt"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/server"
	"github.com/spf13/cobra"
)

// stopCmd terminates the background daemon process recorded in ~/.agw/agw.pid.
var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the running background gateway daemon",
	Long:  `Stop reads the PID from ~/.agw/agw.pid and sends a SIGTERM signal to terminate the background daemon.`,
	RunE:  runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)
}

// runStop reads agw.pid and sends a SIGTERM signal to stop the background process.
func runStop(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	pid, err := server.StopDetached(cfg)
	if err != nil {
		return fmt.Errorf("stopping daemon: %w", err)
	}

	if pid == 0 {
		cmd.Println("No running antigravity-gateway daemon found.")
		return nil
	}

	cmd.Printf("Stopped antigravity-gateway daemon (PID: %d)\n", pid)
	return nil
}
