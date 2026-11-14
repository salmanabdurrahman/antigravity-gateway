// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"cmp"
	"net/url"
	"strings"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// DefaultOAuthScopes enumerates the exact Google Cloud and Antigravity authorization scopes
// required to interact with Cloud Code Assist v1internal endpoints.
var DefaultOAuthScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// BuildAuthURL constructs the Google OAuth 2.0 authorization URL with PKCE parameters,
// offline access type, forced consent prompt, and configured Google Cloud scopes.
func BuildAuthURL(cfg *config.Config, state string, challenge string) string {
	authBase := config.DefaultAuthURL
	clientID := config.DefaultClientID
	redirectURI := config.DefaultRedirectURI

	if cfg != nil {
		authBase = cmp.Or(cfg.AuthURL, config.DefaultAuthURL)
		clientID = cmp.Or(cfg.ClientID, config.DefaultClientID)
		redirectURI = cmp.Or(cfg.RedirectURI, config.DefaultRedirectURI)
	}

	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", strings.Join(DefaultOAuthScopes, " "))
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)
	params.Set("access_type", "offline")
	params.Set("prompt", "consent")

	return authBase + "?" + params.Encode()
}
