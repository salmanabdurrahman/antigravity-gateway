// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"fmt"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/server"
	"github.com/spf13/cobra"
)

var (
	// stopPort overrides the gateway HTTP port used when rediscovering and stopping the daemon.
	stopPort int

	// stopCmd terminates the background daemon process recorded in ~/.agw/agw.pid.
	stopCmd = &cobra.Command{
		Use:   "stop",
		Short: "Stop the running background gateway daemon",
		Long:  `Stop reads the PID from ~/.agw/agw.pid (or probes /health on the configured port) and sends a graceful termination signal.`,
		Example: `  agw stop
  agw stop --port 51401`,
		RunE: runStop,
	}
)

func init() {
	stopCmd.Flags().IntVarP(&stopPort, "port", "p", 0, "Gateway HTTP port to stop (default from config or 51400)")
	rootCmd.AddCommand(stopCmd)
}

// runStop reads agw.pid (or probes the target port) and sends a graceful termination signal to the daemon.
func runStop(cmd *cobra.Command, args []string) error {
	if cmd.Flags().Changed("port") && (stopPort <= 0 || stopPort > 65535) {
		return fmt.Errorf("invalid --port %d: must be between 1 and 65535", stopPort)
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	var pid int
	if cmd.Flags().Changed("port") {
		pid, err = server.StopOnPort(cfg.Host, stopPort)
	} else {
		pid, err = server.StopDetached(cfg)
	}
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
