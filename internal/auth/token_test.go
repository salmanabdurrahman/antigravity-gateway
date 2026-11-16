// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestTokenResultExpiry verifies token expiration calculations under varying skew margins.
func TestTokenResultExpiry(t *testing.T) {
	tr := &TokenResult{
		AccessToken: "test-token",
		Expiry:      time.Now().Add(60 * time.Second),
	}

	// With 120s margin, 60s remaining is expiring soon
	if !tr.IsExpiredOrExpiringSoon(120 * time.Second) {
		t.Errorf("expected token with 60s remaining to be expiring soon with 120s margin")
	}

	// With 30s margin, 60s remaining is NOT expiring soon
	if tr.IsExpiredOrExpiringSoon(30 * time.Second) {
		t.Errorf("expected token with 60s remaining not to be expiring soon with 30s margin")
	}

	// Zero expiry should be treated as expired
	zeroTR := &TokenResult{}
	if !zeroTR.IsExpiredOrExpiringSoon(120 * time.Second) {
		t.Errorf("expected zero-expiry token to be treated as expired")
	}
}

// TestExchangeCodeSuccess verifies exchanging an authorization code and PKCE verifier for tokens.
func TestExchangeCodeSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST method, got %s", r.Method)
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "authorization_code" {
			t.Errorf("expected grant_type authorization_code, got %s", r.Form.Get("grant_type"))
		}
		if r.Form.Get("code") != "auth-code-123" {
			t.Errorf("expected code auth-code-123, got %s", r.Form.Get("code"))
		}
		if r.Form.Get("code_verifier") != "verifier-xyz" {
			t.Errorf("expected code_verifier verifier-xyz, got %s", r.Form.Get("code_verifier"))
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-token-123",
			"refresh_token": "refresh-token-456",
			"expires_in":    3600,
			"token_type":    "Bearer",
		})
	}))
	defer ts.Close()

	cfg := config.DefaultConfig()
	cfg.TokenURL = ts.URL

	ctx := t.Context()
	result, err := ExchangeCode(ctx, cfg, "auth-code-123", "verifier-xyz")
	if err != nil {
		t.Fatalf("ExchangeCode failed: %v", err)
	}

	if result.AccessToken != "access-token-123" {
		t.Errorf("access token mismatch: got %s", result.AccessToken)
	}
	if result.RefreshToken != "refresh-token-456" {
		t.Errorf("refresh token mismatch: got %s", result.RefreshToken)
	}
	if result.ExpiresIn != 3600 {
		t.Errorf("expires_in mismatch: got %d", result.ExpiresIn)
	}
}

// TestExchangeCodeErrorInvalidGrant verifies classification of invalid_grant OAuth rejections.
func TestExchangeCodeErrorInvalidGrant(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":             "invalid_grant",
			"error_description": "Bad Request code expired",
		})
	}))
	defer ts.Close()

	cfg := config.DefaultConfig()
	cfg.TokenURL = ts.URL

	ctx := t.Context()
	_, err := ExchangeCode(ctx, cfg, "expired-code", "verifier")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var oauthErr *OAuthError
	if !errors.As(err, &oauthErr) {
		t.Fatalf("expected *OAuthError, got %T: %v", err, err)
	}
	if !oauthErr.InvalidGrant {
		t.Errorf("expected InvalidGrant to be true")
	}
	if oauthErr.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status code 400, got %d", oauthErr.StatusCode)
	}
}

// TestRefreshAccessTokenSuccess verifies refreshing an access token using a valid refresh token.
func TestRefreshAccessTokenSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("expected grant_type refresh_token, got %s", r.Form.Get("grant_type"))
		}
		if r.Form.Get("refresh_token") != "existing-refresh-token" {
			t.Errorf("expected existing-refresh-token, got %s", r.Form.Get("refresh_token"))
		}

		w.Header().Set("Content-Type", "application/json")
		// Google often omits refresh_token in refresh response
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-access-token",
			"expires_in":   1800,
		})
	}))
	defer ts.Close()

	cfg := config.DefaultConfig()
	cfg.TokenURL = ts.URL

	ctx := t.Context()
	result, err := RefreshAccessToken(ctx, cfg, "existing-refresh-token")
	if err != nil {
		t.Fatalf("RefreshAccessToken failed: %v", err)
	}

	if result.AccessToken != "new-access-token" {
		t.Errorf("expected new access token, got %s", result.AccessToken)
	}
	// Preserves original refresh token if omitted by upstream
	if result.RefreshToken != "existing-refresh-token" {
		t.Errorf("expected original refresh token to be preserved, got %s", result.RefreshToken)
	}
}

// TestParseRedirectURL verifies parsing query parameters from OAuth redirect callbacks.
func TestParseRedirectURL(t *testing.T) {
	raw := "http://localhost:51121/oauth-callback?code=abc12345&state=xyz987"
	res, err := ParseRedirectURL(raw)
	if err != nil {
		t.Fatalf("ParseRedirectURL failed: %v", err)
	}
	if res.Code != "abc12345" || res.State != "xyz987" {
		t.Errorf("unexpected parsed result: %+v", res)
	}

	rawErr := "http://localhost:51121/oauth-callback?error=access_denied"
	resErr, err := ParseRedirectURL(rawErr)
	if err != nil {
		t.Fatalf("ParseRedirectURL failed: %v", err)
	}
	if resErr.Error != "access_denied" {
		t.Errorf("unexpected error param: %s", resErr.Error)
	}
}

// TestStartCallbackListener verifies loopback HTTP callback listener mechanics and CSRF state validation.
func TestStartCallbackListener(t *testing.T) {
	ctx := t.Context()
	listenAddr := "127.0.0.1:51129"
	callbackPath := "/test-callback"
	expectedState := "expected-state-value"

	resChan := make(chan *CallbackResult, 1)
	errChan := make(chan error, 1)

	go func() {
		res, err := StartCallbackListener(ctx, listenAddr, callbackPath, expectedState)
		if err != nil {
			errChan <- err
			return
		}
		resChan <- res
	}()

	// Allow listener to bind
	time.Sleep(50 * time.Millisecond)

	// Send callback request
	client := &http.Client{Timeout: 2 * time.Second}
	reqURL := fmt.Sprintf("http://%s%s?code=returned-code&state=%s", listenAddr, callbackPath, expectedState)
	resp, err := client.Get(reqURL)
	if err != nil {
		t.Fatalf("failed to invoke callback endpoint: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK from callback, got %d", resp.StatusCode)
	}

	select {
	case res := <-resChan:
		if res.Code != "returned-code" {
			t.Errorf("expected code returned-code, got %s", res.Code)
		}
		if res.State != expectedState {
			t.Errorf("expected state %s, got %s", expectedState, res.State)
		}
	case err := <-errChan:
		t.Fatalf("StartCallbackListener returned error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback listener result")
	}
}

// TestAccessTokenForLocalDesktopSession verifies that an imported desktop account returns its stored
// bearer token without contacting the OAuth token endpoint, and that an account without any
// credential reports the actionable re-import error instead of a misleading OAuth failure.
func TestAccessTokenForLocalDesktopSession(t *testing.T) {
	cfg := config.DefaultConfig()
	// Point the token endpoint at an unroutable URL: any refresh attempt would fail the test.
	cfg.TokenURL = "http://127.0.0.1:1/token"

	local := &Account{
		Email:            "desktop@example.com",
		AccessToken:      "desktop-token-123",
		CredentialSource: CredentialSourceLocalDesktop,
	}

	res, err := AccessTokenFor(t.Context(), cfg, local)
	if err != nil {
		t.Fatalf("AccessTokenFor(local session) returned error: %v", err)
	}
	if res.AccessToken != "desktop-token-123" {
		t.Errorf("expected imported bearer token, got %q", maskSecret(res.AccessToken))
	}
	if res.RefreshToken != "" {
		t.Errorf("expected no refresh token for a desktop session, got %q", maskSecret(res.RefreshToken))
	}

	if _, err := AccessTokenFor(t.Context(), cfg, &Account{Email: "empty@example.com"}); !errors.Is(err, ErrLocalSessionExpired) {
		t.Errorf("expected ErrLocalSessionExpired for credential-less account, got %v", err)
	}

	if _, err := AccessTokenFor(t.Context(), cfg, nil); err == nil {
		t.Error("expected error for nil account")
	}
}
