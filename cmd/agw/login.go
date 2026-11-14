// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
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
	rootCmd.AddCommand(loginCmd)
}

// runLogin executes the browser consent flow, callback listener, token exchange, and account storage.
func runLogin(cmd *cobra.Command, args []string) error {
	var (
		cfg *config.Config
		err error
	)
	if cfgFile != "" {
		cfg, err = config.Load(cfgFile)
	} else {
		cfg, err = config.LoadOrCreate()
	}
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
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
		openBrowser(authURL)
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

	go func() {
		res, err := auth.StartCallbackListener(ctx, listenAddr, callbackPath, state)
		if err != nil {
			cmd.PrintErrf("Warning: callback listener could not bind to %s (%v); falling back to manual URL paste\n", listenAddr, err)
			return
		}
		select {
		case codeChan <- res.Code:
		default:
		}
	}()

	// Also support pasting the callback URL in terminal if browser redirect did not hit server
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimSpace(line)
			if strings.Contains(line, "code=") {
				parsed, parseErr := auth.ParseRedirectURL(line)
				if parseErr == nil && parsed.Code != "" {
					select {
					case codeChan <- parsed.Code:
					default:
					}
					return
				}
			}
		}
	}()

	cmd.Printf("Waiting for authentication callback on http://%s%s ...\n", listenAddr, callbackPath)
	cmd.Println("(If browser doesn't redirect automatically, paste the full callback URL here and press Enter)")

	var code string
	select {
	case <-ctx.Done():
		return fmt.Errorf("login timed out after 5 minutes: %w", ctx.Err())
	case c := <-codeChan:
		code = c
	}

	cmd.Println("Authorization code received. Exchanging for tokens...")
	tokens, err := auth.ExchangeCode(ctx, cfg, code, pkce.Verifier)
	if err != nil {
		return fmt.Errorf("exchanging code for tokens: %w", err)
	}

	cmd.Println("Retrieving account email from Google...")
	email, err := auth.GetUserEmail(ctx, cfg, tokens.AccessToken)
	if err != nil {
		return fmt.Errorf("retrieving user email: %w", err)
	}

	cmd.Println("Discovering Cloud Code companion project...")
	projectID, err := auth.DiscoverProject(ctx, cfg, tokens.AccessToken)
	if err != nil {
		return fmt.Errorf("discovering companion project: %w", err)
	}

	accPath, err := config.AccountsFilePath()
	if err != nil {
		return fmt.Errorf("resolving accounts file path: %w", err)
	}
	accountStore := auth.NewStore(accPath)
	account := &auth.Account{
		Email:        email,
		RefreshToken: tokens.RefreshToken,
		ProjectID:    projectID,
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
