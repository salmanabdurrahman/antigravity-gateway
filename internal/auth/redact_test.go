// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
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
