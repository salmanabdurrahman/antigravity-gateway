// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/server"
	"github.com/spf13/cobra"
)

var (
	// startStrategy overrides the account rotation algorithm ("sticky" or "round-robin").
	startStrategy string

	// startDetach runs the reverse proxy daemon in the background as a detached process.
	startDetach bool

	// startPort overrides the TCP listening port.
	startPort int

	// startCmd launches the local reverse proxy daemon in foreground or detached mode.
	startCmd = &cobra.Command{
		Use:   "start",
		Short: "Start the local OpenAI-compatible gateway reverse proxy",
		Long: `Start launches the local HTTP reverse proxy server at 127.0.0.1:51400.
It translates OpenAI chat completion requests to Google Cloud Code Assist format,
coordinates multi-account rotation with transparent 429 failover, and preserves
thinking signatures for Gemini 3 and Claude models.

To run the daemon in the background, supply the --detach flag.`,
		RunE: runStart,
	}
)

func init() {
	startCmd.Flags().StringVar(&startStrategy, "strategy", "", "Account rotation strategy ('sticky', 'round-robin', or 'hybrid')")
	startCmd.Flags().BoolVarP(&startDetach, "detach", "d", false, "Run gateway daemon in the background")
	startCmd.Flags().IntVarP(&startPort, "port", "p", 0, "HTTP port to bind (default 51400 or from config)")
	rootCmd.AddCommand(startCmd)
}

// runStart validates flags, ensures single-instance execution, and launches the HTTP server.
func runStart(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadOrCreate()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	if startStrategy != "" {
		if startStrategy != string(rotator.StrategySticky) &&
			startStrategy != string(rotator.StrategyRoundRobin) &&
			startStrategy != string(rotator.StrategyHybrid) {
			return fmt.Errorf("invalid strategy %q: must be 'sticky', 'round-robin', or 'hybrid'", startStrategy)
		}
		cfg.Strategy = startStrategy
	}

	if startPort > 0 {
		cfg.Port = startPort
	}

	if startDetach {
		var extraArgs []string
		if startStrategy != "" {
			extraArgs = append(extraArgs, "--strategy", startStrategy)
		}
		if startPort > 0 {
			extraArgs = append(extraArgs, "--port", fmt.Sprintf("%d", startPort))
		}

		pid, logFile, err := server.StartDetached(cfg, extraArgs)
		if err != nil {
			return fmt.Errorf("starting detached daemon: %w", err)
		}

		host := cfg.Host
		if host == "" {
			host = config.DefaultHost
		}
		port := cfg.Port
		if port <= 0 {
			port = config.DefaultPort
		}

		cmd.Printf("Started antigravity-gateway daemon (PID: %d)\n", pid)
		cmd.Printf("Listening on http://%s:%d\n", host, port)
		cmd.Printf("Logs written to %s\n", logFile)
		return nil
	}

	// Foreground mode
	pidFile, err := config.PIDFilePath()
	if err != nil {
		return fmt.Errorf("resolving pid file path: %w", err)
	}

	if existingPID, readErr := server.ReadPID(pidFile); readErr == nil && server.IsProcessAlive(existingPID) {
		if existingPID != os.Getpid() {
			host := cmp.Or(cfg.Host, config.DefaultHost)
			port := cmp.Or(cfg.Port, config.DefaultPort)
			healthURL := fmt.Sprintf("http://%s:%d/health", host, port)
			ctx, cancel := context.WithTimeout(cmd.Context(), server.DefaultHealthTimeout)
			health, healthErr := server.CheckHealth(ctx, healthURL)
			cancel()
			if healthErr == nil && health != nil && health.OK && health.Name == server.ServerName {
				return fmt.Errorf("antigravity-gateway daemon is already running (PID: %d); run 'agw stop' to restart it", existingPID)
			}
			// Stale PID from dead daemon or recycled PID; clean up and continue
			_ = server.ClearPID(pidFile)
		}
	}

	accPath, err := config.AccountsFilePath()
	if err != nil {
		return fmt.Errorf("resolving accounts file path: %w", err)
	}

	accountStore := auth.NewStore(accPath)
	pool, err := rotator.NewPool(accountStore, cfg, time.Now)
	if err != nil {
		return fmt.Errorf("initializing account pool: %w", err)
	}

	logger := slog.Default()
	rot := rotator.NewRotator(pool, cfg, logger, time.Now)

	cachePath, _ := config.ModelsCacheFilePath()
	cat := catalog.NewModelCatalog(cfg, rot, cachePath, logger)
	_ = cat.LoadCache()

	srv, err := server.NewServer(server.Options{
		Config:  cfg,
		Rotator: rot,
		Catalog: cat,
		Logger:  logger,
		PIDFile: pidFile,
	})
	if err != nil {
		return fmt.Errorf("initializing server: %w", err)
	}

	host := cfg.Host
	if host == "" {
		host = config.DefaultHost
	}
	port := cfg.Port
	if port <= 0 {
		port = config.DefaultPort
	}
	cmd.Printf("Antigravity Gateway running on http://%s:%d\n", host, port)
	cmd.Println("Press Ctrl+C to terminate.")

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- srv.Start()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		cmd.Printf("\nReceived signal %s; shutting down gracefully...\n", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			cmd.PrintErrf("Error during shutdown: %v\n", err)
		}
		return nil
	case err := <-serverErrCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("gateway server failure: %w", err)
		}
		return nil
	}
}
