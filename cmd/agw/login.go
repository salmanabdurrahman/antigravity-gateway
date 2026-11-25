// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/spf13/cobra"
)

var (
	// noBrowser prevents automatic opening of the system browser during OAuth PKCE login.
	noBrowser bool

	// localImport triggers auto-import from local Antigravity desktop installation.
	localImport bool

	// loginCmd initiates the browser-based Google OAuth PKCE authentication flow.
	loginCmd = &cobra.Command{
		Use:   "login",
		Short: "Authenticate a Google Antigravity account via OAuth PKCE",
		Long: `Login opens Google's OAuth consent screen in your web browser.
Upon authorization, it exchanges the authorization code for tokens, discovers the
associated Cloud Code companion project, and saves the account in ~/.agw/accounts.json.`,
		RunE: runLogin,
	}
)

func init() {
	loginCmd.Flags().BoolVar(&noBrowser, "no-browser", false, "Do not attempt to open default browser automatically")
	loginCmd.Flags().BoolVar(&localImport, "local", false, "Auto-import credentials from local Antigravity desktop installation")
	rootCmd.AddCommand(loginCmd)
}

// runLogin executes the browser consent flow, callback listener, token exchange, and account storage.
func runLogin(cmd *cobra.Command, args []string) error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	if localImport {
		accPath, err := config.AccountsFilePath()
		if err != nil {
			return fmt.Errorf("resolving accounts file path: %w", err)
		}
		accountStore := auth.NewStore(accPath)
		acc, err := auth.ImportLocalSession(cmd.Context(), cfg, accountStore)
		if err != nil {
			return fmt.Errorf("importing local Antigravity session: %w", err)
		}
		if acc.RefreshToken != "" && !acc.IsLocalDesktopSession() {
			cmd.Printf("Account %s is already authenticated via OAuth; keeping refreshable credentials and skipping the desktop import.\n", acc.Email)
			return nil
		}
		cmd.Printf("✓ Successfully imported local Antigravity account: %s\n", acc.Email)
		cmd.Printf("  Companion Project ID: %s\n", acc.ProjectID)
		cmd.Println("  Credential: desktop session bearer token (no refresh token issued)")
		cmd.Println("  Re-run 'agw login --local' when the token expires and requests start returning 401.")
		cmd.Println("\nRun 'agw accounts' to view stored accounts.")
		return nil
	}

	pkce, err := auth.GeneratePKCE()
	if err != nil {
		return fmt.Errorf("generating PKCE pair: %w", err)
	}

	state, err := auth.GenerateState()
	if err != nil {
		return fmt.Errorf("generating OAuth state: %w", err)
	}

	authURL := auth.BuildAuthURL(cfg, state, pkce.Challenge)

	cmd.Println("Initiating Google OAuth authentication for Antigravity Gateway...")
	cmd.Println()
	cmd.Println("Open this URL in your browser to sign in:")
	cmd.Println(authURL)
	cmd.Println()

	if !noBrowser {
		openBrowserFn(authURL)
	}

	// Listen on loopback callback with 5-minute timeout
	ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
	defer cancel()

	codeChan := make(chan string, 1)

	redirectURI := config.DefaultRedirectURI
	if cfg != nil {
		redirectURI = cmp.Or(cfg.RedirectURI, config.DefaultRedirectURI)
	}
	listenAddr, callbackPath := auth.ResolveCallbackEndpoint(redirectURI)

	// Bind synchronously so a port conflict is reported from the command goroutine instead of racing
	// command output from a background goroutine.
	callbackListener, listenErr := net.Listen("tcp", listenAddr)
	if listenErr != nil {
		cmd.PrintErrf("Warning: callback listener could not bind to %s (%v); falling back to manual URL paste\n", listenAddr, listenErr)
	} else {
		go func() {
			res, err := auth.ServeCallbackListener(ctx, callbackListener, callbackPath, state)
			if err != nil {
				return
			}
			select {
			case codeChan <- res.Code:
			default:
			}
		}()
	}

	// Manual paste stays available for remote/SSH sessions. The reader never writes to the command
	// output (which would interleave with the prompt) and stops as soon as a code is selected, so the
	// only remaining goroutine is a single blocking read discarded at process exit.
	pasteCtx, stopPaste := context.WithCancel(ctx)
	defer stopPaste()
	pasteCodes := make(chan string, 1)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			select {
			case <-pasteCtx.Done():
				return
			default:
			}

			line = strings.TrimSpace(line)
			if !strings.Contains(line, "code=") {
				continue
			}
			parsed, parseErr := auth.ParseRedirectURL(line)
			if parseErr != nil || parsed.Code == "" {
				continue
			}
			// Verify state parameter to prevent OAuth CSRF in manually pasted redirect URLs.
			if subtle.ConstantTimeCompare([]byte(parsed.State), []byte(state)) != 1 {
				continue
			}
			select {
			case pasteCodes <- parsed.Code:
			default:
			}
			return
		}
	}()

	if listenErr == nil {
		cmd.Printf("Waiting for authentication callback on http://%s%s ...\n", listenAddr, callbackPath)
	}
	cmd.Println("(If browser doesn't redirect automatically, paste the full callback URL here and press Enter)")

	var code string
	select {
	case <-ctx.Done():
		return fmt.Errorf("login timed out after 5 minutes: %w", ctx.Err())
	case c := <-codeChan:
		code = c
	case c := <-pasteCodes:
		code = c
	}
	stopPaste()

	cmd.Println("Authorization code received. Exchanging for tokens...")
	tokens, err := auth.ExchangeCode(ctx, cfg, code, pkce.Verifier)
	if err != nil {
		return fmt.Errorf("exchanging code for tokens: %w", err)
	}

	cmd.Println("Retrieving account identity from Google...")
	identity, err := auth.GetUserIdentity(ctx, cfg, tokens.AccessToken)
	if err != nil {
		return fmt.Errorf("retrieving user identity: %w", err)
	}
	email := identity.Email

	cmd.Println("Discovering Cloud Code companion project...")
	projectID, tier, err := auth.DiscoverProjectAndTier(ctx, cfg, tokens.AccessToken)
	if err != nil {
		return fmt.Errorf("discovering companion project: %w", err)
	}

	accPath, err := config.AccountsFilePath()
	if err != nil {
		return fmt.Errorf("resolving accounts file path: %w", err)
	}
	accountStore := auth.NewStore(accPath)
	account := &auth.Account{
		Email:        identity.Email,
		UserID:       identity.ID,
		RefreshToken: tokens.RefreshToken,
		ProjectID:    projectID,
		Tier:         tier,
		AddedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	isNew, err := accountStore.Upsert(account)
	if err != nil {
		return fmt.Errorf("persisting account: %w", err)
	}

	cmd.Println()
	if isNew {
		cmd.Printf("Successfully added account: %s (Project: %s)\n", email, projectID)
	} else {
		cmd.Printf("Successfully refreshed account credentials: %s (Project: %s)\n", email, projectID)
	}

	return nil
}

// openBrowserFn launches the user's default web browser, swappable for isolated unit tests.
var openBrowserFn = openBrowser

// openBrowser launches the user's default web browser targeted at the authorization URL.
func openBrowser(url string) {
	var cmd string
	var args []string

	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "windows":
		cmd = "rundll32"
		args = []string{"url.dll,FileProtocolHandler", url}
	default:
		cmd = "xdg-open"
		args = []string{url}
	}

	// Safe to ignore start errors because URL is always rendered to stdout as fallback
	_ = exec.Command(cmd, args...).Start()
}
