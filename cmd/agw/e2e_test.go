// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestEndToEndAuthWorkflow simulates the entire OAuth PKCE login workflow, token exchange, companion discovery, and storage.
func TestEndToEndAuthWorkflow(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	var (
		codeExchangeHit atomic.Bool
		userinfoHit     atomic.Bool
		loadAssistHit   atomic.Bool
	)

	// Mock Google OAuth & Cloud Code Assist upstream server
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/token":
			codeExchangeHit.Store(true)
			_ = r.ParseForm()
			if r.Form.Get("code") != "valid-mock-code" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token":  "mock-access-token-xyz",
				"refresh_token": "mock-refresh-token-123456",
				"expires_in":    3600,
			})

		case "/userinfo":
			userinfoHit.Store(true)
			if r.Header.Get("Authorization") != "Bearer mock-access-token-xyz" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"email": "developer-e2e@example.com",
			})

		case "/v1internal:loadCodeAssist":
			loadAssistHit.Store(true)
			if r.Header.Get("Authorization") != "Bearer mock-access-token-xyz" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cloudaicompanionProject": "mock-companion-project-999",
			})

		default:
			http.NotFound(w, r)
		}
	}))
	defer mockServer.Close()

	// Configure gateway to use mock server endpoints
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("ConfigFilePath failed: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.TokenURL = mockServer.URL + "/token"
	cfg.UserInfoURL = mockServer.URL + "/userinfo"
	cfg.Endpoints = []string{mockServer.URL}
	cfg.RedirectURI = "http://127.0.0.1:51128/oauth-callback"

	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("config.Save failed: %v", err)
	}

	// 1. Verify PKCE Generation & Auth URL Construction
	pkce, err := auth.GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE failed: %v", err)
	}
	state, err := auth.GenerateState()
	if err != nil {
		t.Fatalf("GenerateState failed: %v", err)
	}

	authURL := auth.BuildAuthURL(cfg, state, pkce.Challenge)
	parsedAuthURL, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parsing authURL failed: %v", err)
	}
	if parsedAuthURL.Query().Get("code_challenge") != pkce.Challenge {
		t.Errorf("code_challenge mismatch in generated auth URL")
	}

	// 2. Start loopback listener and simulate browser callback redirect
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	listenAddr, callbackPath := auth.ResolveCallbackEndpoint(cfg.RedirectURI)
	listenerResultChan := make(chan *auth.CallbackResult, 1)

	go func() {
		res, lErr := auth.StartCallbackListener(ctx, listenAddr, callbackPath, state)
		if lErr != nil {
			t.Errorf("StartCallbackListener error: %v", lErr)
			return
		}
		listenerResultChan <- res
	}()

	time.Sleep(50 * time.Millisecond)

	// Simulate browser callback hitting loopback server
	callbackURL := "http://" + listenAddr + callbackPath + "?code=valid-mock-code&state=" + state
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(callbackURL)
	if err != nil {
		t.Fatalf("simulating browser callback failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("browser callback answered HTTP %d, want 200", resp.StatusCode)
	}

	var code string
	select {
	case res := <-listenerResultChan:
		code = res.Code
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback listener")
	}

	if code != "valid-mock-code" {
		t.Fatalf("code mismatch: got %s, want valid-mock-code", code)
	}

	// 3. Exchange Code for Tokens against mock Google endpoint
	tokenRes, err := auth.ExchangeCode(ctx, cfg, code, pkce.Verifier)
	if err != nil {
		t.Fatalf("ExchangeCode failed: %v", err)
	}
	if !codeExchangeHit.Load() {
		t.Errorf("code exchange endpoint was not called")
	}
	if tokenRes.AccessToken != "mock-access-token-xyz" {
		t.Errorf("access token mismatch: got %s", tokenRes.AccessToken)
	}

	// 4. Retrieve User Email
	email, err := auth.GetUserEmail(ctx, cfg, tokenRes.AccessToken)
	if err != nil {
		t.Fatalf("GetUserEmail failed: %v", err)
	}
	if !userinfoHit.Load() {
		t.Errorf("userinfo endpoint was not called")
	}
	if email != "developer-e2e@example.com" {
		t.Errorf("email mismatch: got %s", email)
	}

	// 5. Discover Companion Project
	projectID, err := auth.DiscoverProject(ctx, cfg, tokenRes.AccessToken)
	if err != nil {
		t.Fatalf("DiscoverProject failed: %v", err)
	}
	if !loadAssistHit.Load() {
		t.Errorf("loadCodeAssist endpoint was not called")
	}
	if projectID != "mock-companion-project-999" {
		t.Errorf("projectID mismatch: got %s", projectID)
	}

	// 6. Save Account to Store with 0600 permissions
	accPath := filepath.Join(tempHome, "accounts.json")
	store := auth.NewStore(accPath)
	account := &auth.Account{
		Email:        email,
		RefreshToken: tokenRes.RefreshToken,
		ProjectID:    projectID,
		AddedAt:      time.Now().UTC().Format(time.RFC3339),
	}

	isNew, err := store.Upsert(account)
	if err != nil {
		t.Fatalf("store.Upsert failed: %v", err)
	}
	if !isNew {
		t.Errorf("expected isNew=true")
	}

	// 7. Verify CLI Accounts Output (No Token Leakage)
	buf := new(bytes.Buffer)
	accountsCmd.SetOut(buf)
	accountsCmd.SetErr(buf)
	if err := accountsCmd.RunE(accountsCmd, []string{}); err != nil {
		t.Fatalf("accountsCmd.RunE failed: %v", err)
	}

	accountsOutput := buf.String()
	if !strings.Contains(accountsOutput, "developer-e2e@example.com") {
		t.Errorf("accounts output missing email: %s", accountsOutput)
	}
	if !strings.Contains(accountsOutput, "mock-companion-project-999") {
		t.Errorf("accounts output missing project ID: %s", accountsOutput)
	}
	if strings.Contains(accountsOutput, "mock-refresh-token") {
		t.Errorf("SECURITY LEAK: output contains refresh token: %s", accountsOutput)
	}

	// 8. Verify CLI Logout
	logoutBuf := new(bytes.Buffer)
	logoutCmd.SetOut(logoutBuf)
	logoutCmd.SetErr(logoutBuf)
	if err := logoutCmd.RunE(logoutCmd, []string{"developer-e2e@example.com"}); err != nil {
		t.Fatalf("logoutCmd.RunE failed: %v", err)
	}

	remaining, err := store.Load()
	if err != nil {
		t.Fatalf("store.Load after logout failed: %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("expected 0 accounts remaining, got %d", len(remaining))
	}
}

// TestEndToEndCatalogAndQuotaWorkflow simulates live model catalog fetching, quota inspection, and agw accounts --check rendering.
func TestEndToEndCatalogAndQuotaWorkflow(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	var (
		tokenRefreshCount atomic.Int32
		catalogFetchCount atomic.Int32
	)

	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/token":
			tokenRefreshCount.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-refreshed-access-token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})

		case "/v1internal:fetchAvailableModels":
			catalogFetchCount.Add(1)
			geminiRemaining := 0.75
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": map[string]any{
					"gemini-3.8-flash-high": map[string]any{
						"maxTokens":        1048576,
						"maxOutputTokens":  65536,
						"supportsThinking": true,
						"supportsImages":   true,
						"displayName":      "Gemini 3.8 Flash High",
						"thinkingBudget":   10000,
						"quotaInfo": map[string]any{
							"remainingFraction": geminiRemaining,
							"resetTime":         "2026-11-14T18:00:00Z",
						},
					},
					"claude-sonnet-4-6": map[string]any{
						"maxTokens":        1000000,
						"maxOutputTokens":  64000,
						"supportsThinking": true,
						"supportsImages":   true,
						"displayName":      "Claude Sonnet 4.6",
						"quotaInfo": map[string]any{
							"remainingFraction": 1.0,
						},
					},
					"chat_internal_tool": map[string]any{
						"maxTokens": 10000,
					},
				},
				"defaultAgentModelId": "gemini-3.8-flash-high",
			})

		default:
			http.NotFound(w, r)
		}
	}))
	defer mockServer.Close()

	// Write gateway configuration
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("ConfigFilePath failed: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.TokenURL = mockServer.URL + "/token"
	cfg.Endpoints = []string{mockServer.URL}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("config.Save failed: %v", err)
	}

	// Upsert test account
	accPath := filepath.Join(tempHome, "accounts.json")
	store := auth.NewStore(accPath)
	_, err = store.Upsert(&auth.Account{
		Email:        "engineer-e2e@example.com",
		RefreshToken: "refresh-secret-e2e",
		ProjectID:    "companion-project-e2e",
		AddedAt:      time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("store.Upsert failed: %v", err)
	}

	// 1. Verify 'agw accounts --check'
	checkAccountsQuota = true
	defer func() { checkAccountsQuota = false }()

	bufCheck := new(bytes.Buffer)
	accountsCmd.SetOut(bufCheck)
	accountsCmd.SetErr(bufCheck)

	if err := accountsCmd.RunE(accountsCmd, []string{}); err != nil {
		t.Fatalf("accountsCmd.RunE (--check) failed: %v", err)
	}

	checkOutput := bufCheck.String()
	if !strings.Contains(checkOutput, "engineer-e2e@example.com") {
		t.Errorf("check output missing email: %s", checkOutput)
	}
	if !strings.Contains(checkOutput, "75%") {
		t.Errorf("check output missing gemini quota 75%%: %s", checkOutput)
	}
	if !strings.Contains(checkOutput, "100%") {
		t.Errorf("check output missing claude quota 100%%: %s", checkOutput)
	}
	if strings.Contains(checkOutput, "refresh-secret-e2e") {
		t.Errorf("SECURITY LEAK: check output contains refresh token: %s", checkOutput)
	}

	// Reset checkAccountsQuota flag for remaining tests
	checkAccountsQuota = false

	// 2. Verify 'agw models list' fetches and caches models
	bufModels := new(bytes.Buffer)
	modelsCmd.SetOut(bufModels)
	modelsCmd.SetErr(bufModels)

	if err := modelsCmd.RunE(modelsCmd, []string{}); err != nil {
		t.Fatalf("modelsCmd.RunE failed: %v", err)
	}

	modelsOutput := bufModels.String()
	if !strings.Contains(modelsOutput, "gemini-3.8-flash-high") || !strings.Contains(modelsOutput, "Gemini 3.8 Flash High") {
		t.Errorf("models output missing gemini model: %s", modelsOutput)
	}
	if !strings.Contains(modelsOutput, "claude-sonnet-4-6") {
		t.Errorf("models output missing claude model: %s", modelsOutput)
	}
	// Verify filtered out internal tool
	if strings.Contains(modelsOutput, "chat_internal_tool") {
		t.Errorf("models output should filter out chat_internal_tool: %s", modelsOutput)
	}

	// Verify disk cache permissions
	cachePath := filepath.Join(tempHome, "models-cache.json")
	cStat, err := os.Stat(cachePath)
	if err != nil {
		t.Fatalf("model cache file not found on disk: %v", err)
	}
	if cStat.Mode().Perm() != config.FilePerm {
		t.Errorf("expected cache file permissions %o, got %o", config.FilePerm, cStat.Mode().Perm())
	}

	// 3. Simulate offline mode by closing mock server
	mockServer.Close()
	catalogFetchesBefore := catalogFetchCount.Load()

	// 4. Run 'agw models list' offline: must serve immediately from disk cache
	bufOffline := new(bytes.Buffer)
	modelsCmd.SetOut(bufOffline)
	modelsCmd.SetErr(bufOffline)

	if err := modelsCmd.RunE(modelsCmd, []string{}); err != nil {
		t.Fatalf("offline modelsCmd.RunE failed: %v", err)
	}

	offlineOutput := bufOffline.String()
	if !strings.Contains(offlineOutput, "gemini-3.8-flash-high") {
		t.Errorf("offline output missing gemini from cache: %s", offlineOutput)
	}
	if catalogFetchCount.Load() != catalogFetchesBefore {
		t.Errorf("expected zero additional catalog fetches in offline mode, got %d", catalogFetchCount.Load()-catalogFetchesBefore)
	}
}
