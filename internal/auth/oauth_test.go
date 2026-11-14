// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestGeneratePKCE verifies cryptographic RFC 7636 PKCE verifier and challenge generation.
func TestGeneratePKCE(t *testing.T) {
	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE() failed: %v", err)
	}

	if pkce.Verifier == "" {
		t.Fatal("GeneratePKCE() returned empty verifier")
	}
	if pkce.Challenge == "" {
		t.Fatal("GeneratePKCE() returned empty challenge")
	}

	// Verify URL safe without padding (RFC 7636)
	if strings.ContainsAny(pkce.Verifier, "+/=") {
		t.Errorf("verifier contains invalid URL-safe base64 characters: %s", pkce.Verifier)
	}
	if strings.ContainsAny(pkce.Challenge, "+/=") {
		t.Errorf("challenge contains invalid URL-safe base64 characters: %s", pkce.Challenge)
	}

	// Verify SHA-256 calculation
	hash := sha256.Sum256([]byte(pkce.Verifier))
	expectedChallenge := base64.RawURLEncoding.EncodeToString(hash[:])
	if pkce.Challenge != expectedChallenge {
		t.Errorf("challenge mismatch: got %s, want %s", pkce.Challenge, expectedChallenge)
	}
}

// TestGenerateState verifies cryptographic randomness and URL-safety of OAuth state tokens.
func TestGenerateState(t *testing.T) {
	state, err := GenerateState()
	if err != nil {
		t.Fatalf("GenerateState() failed: %v", err)
	}
	if state == "" {
		t.Fatal("GenerateState() returned empty state")
	}
	if strings.ContainsAny(state, "+/=") {
		t.Errorf("state contains invalid URL-safe characters: %s", state)
	}
}

// TestBuildAuthURL verifies construction of the Google OAuth 2.0 consent URL with required parameters and scopes.
func TestBuildAuthURL(t *testing.T) {
	cfg := config.DefaultConfig()
	state := "test-state-123"
	challenge := "test-challenge-abc"

	authURL := BuildAuthURL(cfg, state, challenge)
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("failed to parse generated auth URL: %v", err)
	}

	if parsed.Scheme != "https" || parsed.Host != "accounts.google.com" || parsed.Path != "/o/oauth2/v2/auth" {
		t.Errorf("unexpected base URL: %s", parsed.String())
	}

	q := parsed.Query()
	if q.Get("client_id") != cfg.ClientID {
		t.Errorf("client_id mismatch: got %s, want %s", q.Get("client_id"), cfg.ClientID)
	}
	if q.Get("response_type") != "code" {
		t.Errorf("response_type mismatch: got %s, want code", q.Get("response_type"))
	}
	if q.Get("redirect_uri") != cfg.RedirectURI {
		t.Errorf("redirect_uri mismatch: got %s, want %s", q.Get("redirect_uri"), cfg.RedirectURI)
	}
	if q.Get("code_challenge") != challenge {
		t.Errorf("code_challenge mismatch: got %s, want %s", q.Get("code_challenge"), challenge)
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method mismatch: got %s, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("state") != state {
		t.Errorf("state mismatch: got %s, want %s", q.Get("state"), state)
	}
	if q.Get("access_type") != "offline" {
		t.Errorf("access_type mismatch: got %s, want offline", q.Get("access_type"))
	}
	if q.Get("prompt") != "consent" {
		t.Errorf("prompt mismatch: got %s, want consent", q.Get("prompt"))
	}

	scope := q.Get("scope")
	for _, expectedScope := range DefaultOAuthScopes {
		if !strings.Contains(scope, expectedScope) {
			t.Errorf("missing scope in auth URL: %s", expectedScope)
		}
	}
}

// TestBuildAuthURLNilConfig verifies safe fallback to built-in defaults when provided a nil configuration pointer.
func TestBuildAuthURLNilConfig(t *testing.T) {
	authURL := BuildAuthURL(nil, "state", "challenge")
	if !strings.HasPrefix(authURL, config.DefaultAuthURL) {
		t.Errorf("expected default auth URL prefix, got %s", authURL)
	}
}
