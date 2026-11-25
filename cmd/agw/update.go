// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
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

// daemonHealth probes the local gateway /health endpoint and reports the daemon PID together with
// whether a gateway daemon is actually answering. The health identity probe is authoritative: a
// foreign listener on the same port is never reported as a gateway daemon.
func daemonHealth(ctx context.Context, cfg *config.Config) (int, bool) {
	if cfg == nil {
		return 0, false
	}
	host := config.LoopbackHost(cfg.Host)
	port := cfg.Port
	if port <= 0 {
		port = config.DefaultPort
	}

	healthCtx, cancel := context.WithTimeout(ctx, server.DefaultHealthTimeout)
	health, err := server.CheckHealth(healthCtx, config.URL(host, port, "/health"))
	cancel()
	if err != nil || health == nil || !health.OK || health.Name != server.ServerName {
		return 0, false
	}

	pid := 0
	if pidFile, pidErr := config.PIDFilePath(); pidErr == nil {
		// A missing or unreadable PID file only degrades the reported PID; the health probe already
		// proved a gateway daemon is serving the loopback port.
		pid, _ = server.ReadPID(pidFile)
	}
	return pid, true
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

	// Check if daemon is currently running so we can stop it before the swap and restart it afterwards.
	// The probe runs even with --no-restart: the binary still must not be replaced under a live process.
	cfg, cfgErr := loadConfig()
	daemonWasRunning := false
	var runningDaemonPID int
	if cfgErr == nil && cfg != nil {
		runningDaemonPID, daemonWasRunning = daemonHealth(ctx, cfg)
	}

	// Stop running daemon before replacing executable. A daemon that keeps running while its binary
	// is swapped can serve a mixture of old and new code and will silently re-create the PID file, so
	// replacement is aborted unless --force is supplied.
	if daemonWasRunning {
		cmd.Printf("--> Stopping active daemon (PID: %d) before binary replacement...\n", runningDaemonPID)
		if _, stopErr := server.StopDetached(cfg); stopErr != nil {
			if !updateForce {
				return fmt.Errorf("stopping the running daemon before binary replacement: %w (rerun with --force to replace the binary anyway)", stopErr)
			}
			cmd.PrintErrf("Warning: graceful daemon stop returned: %v; continuing because --force was supplied\n", stopErr)
		}
		if pid, stillRunning := daemonHealth(ctx, cfg); stillRunning && !updateForce {
			return fmt.Errorf("daemon (PID: %d) is still answering /health after the stop request; refusing to replace the running binary (rerun with --force to override)", pid)
		}
	}

	cmd.Printf("--> Applying binary replacement to %s...\n", execPath)
	if err := updater.ApplyBinary(execPath, newBinaryBytes); err != nil {
		return fmt.Errorf("applying replacement binary: %w", err)
	}
	cmd.Printf("✓ Successfully updated Antigravity Gateway to v%s!\n", rel.TargetVersion)

	// Restart daemon if it was running previously, forwarding the same --config path so the new
	// process reads the identical configuration the operator launched it with. With --no-restart the
	// daemon is left stopped and the operator is told how to bring it back.
	if daemonWasRunning {
		if updateNoRestart {
			cmd.Println("--> Daemon stopped for the binary replacement; --no-restart was supplied, so it was not restarted.")
			cmd.Println("    Start it again with: agw start -d")
		} else {
			cmd.Println("--> Restarting Antigravity Gateway daemon in background...")
			var restartArgs []string
			if cfgFile != "" {
				restartArgs = append(restartArgs, "--config", cfgFile)
			}
			newPID, logFile, startErr := server.StartDetached(cfg, restartArgs)
			if startErr != nil {
				cmd.PrintErrf("Warning: failed restarting daemon automatically: %v\n", startErr)
				cmd.Println("Start the daemon manually with: agw start -d")
			} else {
				cmd.Printf("✓ Antigravity Gateway daemon restarted successfully (PID: %d).\n", newPID)
				cmd.Printf("  Logs appended to: %s\n", logFile)
			}
		}
	}

	cmd.Println("\nRun 'agw doctor' to verify system health and client configurations.")
	return nil
}
