// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAccountStringRedaction verifies that Account.String() masks refresh tokens to prevent credential leaks in logs.
func TestAccountStringRedaction(t *testing.T) {
	acc := &Account{
		Email:        "user@example.com",
		RefreshToken: "1//04supersecretrefreshtoken123456789",
		ProjectID:    "companion-123",
		AddedAt:      "2026-01-01T00:00:00Z",
	}

	str := acc.String()
	if strings.Contains(str, "supersecret") {
		t.Errorf("SECURITY LEAK: Account.String() contains raw secret token: %s", str)
	}
	if !strings.Contains(str, "user@example.com") || !strings.Contains(str, "companion-123") {
		t.Errorf("Account.String() missing non-sensitive fields: %s", str)
	}
}

// TestTokenResultStringRedaction verifies that TokenResult.String() masks access and refresh tokens.
func TestTokenResultStringRedaction(t *testing.T) {
	tr := &TokenResult{
		AccessToken:  "ya29.a0AfH6SMD_very_secret_access_token_xyz",
		RefreshToken: "1//04supersecretrefreshtoken",
		ExpiresIn:    3600,
		Expiry:       time.Now().Add(1 * time.Hour),
	}

	str := tr.String()
	if strings.Contains(str, "very_secret") || strings.Contains(str, "supersecret") {
		t.Errorf("SECURITY LEAK: TokenResult.String() contains raw secret tokens: %s", str)
	}
}

// TestResolveCallbackEndpoint tests parsing host:port and route paths from varied redirect URI formats.
func TestResolveCallbackEndpoint(t *testing.T) {
	tests := []struct {
		input        string
		expectedAddr string
		expectedPath string
	}{
		{
			input:        "http://localhost:51121/oauth-callback",
			expectedAddr: "127.0.0.1:51121",
			expectedPath: "/oauth-callback",
		},
		{
			input:        "http://127.0.0.1:52000/custom/callback",
			expectedAddr: "127.0.0.1:52000",
			expectedPath: "/custom/callback",
		},
		{
			input:        "",
			expectedAddr: "127.0.0.1:51121",
			expectedPath: "/oauth-callback",
		},
	}

	for _, tc := range tests {
		addr, path := ResolveCallbackEndpoint(tc.input)
		if addr != tc.expectedAddr || path != tc.expectedPath {
			t.Errorf("ResolveCallbackEndpoint(%q) = (%q, %q), want (%q, %q)",
				tc.input, addr, path, tc.expectedAddr, tc.expectedPath)
		}
	}
}

// TestLocalSessionStringRedaction verifies that LocalSession.String() redacts the active bearer
// APIKey to prevent credential leakage in logs and diagnostics.
func TestLocalSessionStringRedaction(t *testing.T) {
	session := &LocalSession{
		Email:  "developer@example.com",
		Name:   "Test Developer",
		APIKey: "ya29.a0AfH6SMD_very_secret_imported_token_12345678",
	}

	rendered := session.String()
	if strings.Contains(rendered, "very_secret_imported") {
		t.Errorf("SECURITY LEAK: LocalSession.String() exposed APIKey: %s", rendered)
	}
	if !strings.Contains(rendered, "developer@example.com") {
		t.Errorf("LocalSession.String() missing email: %s", rendered)
	}
}

// TestStartCallbackListenerStateValidation verifies that StartCallbackListener ignores mismatched
// state parameters on error callbacks instead of aborting legitimate active authentication flows.
func TestStartCallbackListenerStateValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	listenAddr := "127.0.0.1:51199"
	callbackPath := "/oauth-callback"
	expectedState := "valid-session-state-12345"

	resultChan := make(chan *CallbackResult, 1)
	errChan := make(chan error, 1)

	go func() {
		res, err := StartCallbackListener(ctx, listenAddr, callbackPath, expectedState)
		if err != nil {
			errChan <- err
			return
		}
		resultChan <- res
	}()

	// Give ephemeral listener time to bind
	time.Sleep(50 * time.Millisecond)

	// Step 1: Send forged error callback with wrong state (should be rejected with 400 and ignored)
	badURL := fmt.Sprintf("http://%s%s?error=access_denied&state=forged-state", listenAddr, callbackPath)
	respBad, err := http.Get(badURL)
	if err != nil {
		t.Fatalf("sending forged callback request: %v", err)
	}
	_ = respBad.Body.Close()
	if respBad.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400 for mismatched state on error param, got %d", respBad.StatusCode)
	}

	// Step 2: Send legitimate success callback with valid state
	goodURL := fmt.Sprintf("http://%s%s?code=valid-auth-code-777&state=%s", listenAddr, callbackPath, expectedState)
	respGood, err := http.Get(goodURL)
	if err != nil {
		t.Fatalf("sending legitimate callback request: %v", err)
	}
	_ = respGood.Body.Close()
	if respGood.StatusCode != http.StatusOK {
		t.Errorf("expected status 200 for legitimate callback, got %d", respGood.StatusCode)
	}

	select {
	case err := <-errChan:
		t.Fatalf("listener failed unexpectedly: %v", err)
	case res := <-resultChan:
		if res.Code != "valid-auth-code-777" {
			t.Errorf("expected code valid-auth-code-777, got %s", res.Code)
		}
		if res.State != expectedState {
			t.Errorf("expected state %s, got %s", expectedState, res.State)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for callback result")
	}
}
