// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/server"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/updater"
	"github.com/spf13/cobra"
)

var (
	// updateCheckOnly flags checking for available releases without downloading or replacing the binary.
	updateCheckOnly bool

	// updateTargetVersion pins a specific release version (e.g. "0.1.1" or "v0.1.1") for upgrade or downgrade.
	updateTargetVersion string

	// updateForce forces download and reinstallation even when already on the latest version.
	updateForce bool

	// updateNoRestart prevents automatically restarting a running background daemon after a binary update.
	updateNoRestart bool

	// updateCmd implements the in-place self-upgrade command for Antigravity Gateway.
	updateCmd = &cobra.Command{
		Use:     "update",
		Aliases: []string{"upgrade"},
		Short:   "Update Antigravity Gateway to the latest release",
		Long: `Update checks GitHub Releases for newer versions of Antigravity Gateway (agw),
downloads the platform-specific release archive, verifies its cryptographic SHA-256 digest,
and replaces the current executable in-place atomically.

If a background daemon is running, it will be automatically restarted with the new binary
unless --no-restart is supplied.`,
		RunE: runUpdate,
	}

	// updateHTTPClient allows injecting mock HTTP clients for isolated testing.
	updateHTTPClient *http.Client
	// updateRepoURL overrides the default GitHub repository endpoint, useful for isolated mock testing.
	updateRepoURL string
)

func init() {
	updateCmd.Flags().BoolVar(&updateCheckOnly, "check", false, "Check for available updates without downloading or installing")
	updateCmd.Flags().StringVar(&updateTargetVersion, "version", "", "Target a specific release version (e.g. '0.1.1' or 'v0.1.1')")
	updateCmd.Flags().BoolVar(&updateForce, "force", false, "Force download and reinstall even if already up to date")
	updateCmd.Flags().BoolVar(&updateNoRestart, "no-restart", false, "Do not restart running background daemon after update")
	rootCmd.AddCommand(updateCmd)
}

// runUpdate executes the update command flow: release query, verification, atomic replacement, and daemon restart.
func runUpdate(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	currentVersion := config.Version
	cmd.Println("Checking for updates from GitHub Releases...")

	targetRepo := updater.DefaultGitHubRepo
	if updateRepoURL != "" {
		targetRepo = updateRepoURL
	}

	rel, err := updater.CheckUpdate(ctx, targetRepo, currentVersion, updateTargetVersion, updateHTTPClient)
	if err != nil {
		return fmt.Errorf("checking for updates: %w", err)
	}

	if updateCheckOnly {
		cmd.Printf("Current version: v%s\n", rel.CurrentVersion)
		cmd.Printf("Latest version:  v%s", rel.TargetVersion)
		if rel.PublishedAt != "" {
			cmd.Printf(" (published %s)", rel.PublishedAt)
		}
		cmd.Println()
		if rel.UpdateAvailable {
			cmd.Println("\nA new version is available! Run 'agw update' to upgrade.")
		} else {
			cmd.Println("You are running the latest version.")
		}
		return nil
	}

	if !rel.UpdateAvailable && !updateForce && updateTargetVersion == "" {
		cmd.Printf("✓ You are already running the latest version of Antigravity Gateway (v%s).\n", currentVersion)
		cmd.Println("Pass --force to reinstall the current version.")
		return nil
	}

	cmd.Printf("Target update: v%s -> v%s\n", rel.CurrentVersion, rel.TargetVersion)
	cmd.Printf("--> Target archive: %s\n", rel.ArchiveName)

	cmd.Println("--> Downloading archive and verifying SHA-256 cryptographic digest...")
	newBinaryBytes, err := updater.DownloadAndVerify(ctx, rel, updateHTTPClient)
	if err != nil {
		return fmt.Errorf("downloading and verifying release archive: %w", err)
	}
	cmd.Println("✓ Cryptographic checksum verified successfully.")

	// Determine active executable location
	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving path to current executable: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolving symlink for current executable: %w", err)
	}

	// Check if daemon is currently running so we can coordinate restart
	cfg, cfgErr := loadConfig()
	daemonWasRunning := false
	var runningDaemonPID int
	if cfgErr == nil && cfg != nil && !updateNoRestart {
		host := cmp.Or(cfg.Host, config.DefaultHost)
		port := cmp.Or(cfg.Port, config.DefaultPort)
		healthURL := fmt.Sprintf("http://%s:%d/health", host, port)
		healthCtx, cancel := context.WithTimeout(ctx, server.DefaultHealthTimeout)
		health, healthErr := server.CheckHealth(healthCtx, healthURL)
		cancel()
		if healthErr == nil && health != nil && health.OK && health.Name == server.ServerName {
			daemonWasRunning = true
			if pidFile, err := config.PIDFilePath(); err == nil {
				runningDaemonPID, _ = server.ReadPID(pidFile)
			}
		}
	}

	// Stop running daemon before replacing executable
	if daemonWasRunning {
		cmd.Printf("--> Stopping active daemon (PID: %d) before binary replacement...\n", runningDaemonPID)
		if _, stopErr := server.StopDetached(cfg); stopErr != nil {
			cmd.PrintErrf("Warning: graceful daemon stop returned: %v\n", stopErr)
		}
	}

	cmd.Printf("--> Applying binary replacement to %s...\n", execPath)
	if err := updater.ApplyBinary(execPath, newBinaryBytes); err != nil {
		return fmt.Errorf("applying replacement binary: %w", err)
	}
	cmd.Printf("✓ Successfully updated Antigravity Gateway to v%s!\n", rel.TargetVersion)

	// Restart daemon if it was running previously
	if daemonWasRunning {
		cmd.Println("--> Restarting Antigravity Gateway daemon in background...")
		newPID, logFile, startErr := server.StartDetached(cfg, nil)
		if startErr != nil {
			cmd.PrintErrf("Warning: failed restarting daemon automatically: %v\n", startErr)
			cmd.Println("Start the daemon manually with: agw start -d")
		} else {
			cmd.Printf("✓ Antigravity Gateway daemon restarted successfully (PID: %d).\n", newPID)
			cmd.Printf("  Logs appended to: %s\n", logFile)
		}
	}

	cmd.Println("\nRun 'agw doctor' to verify system health and client configurations.")
	return nil
}
