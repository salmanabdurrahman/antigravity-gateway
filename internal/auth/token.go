// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// OAuthError represents an explicit OAuth failure returned by Google's token endpoint.
type OAuthError struct {
	// StatusCode is the HTTP response code returned by Google's token endpoint.
	StatusCode int

	// ErrorCode is the machine-readable error token (e.g. "invalid_grant").
	ErrorCode string

	// Description is the human-readable explanation of why the grant failed.
	Description string

	// InvalidGrant is true when Google explicitly rejected the refresh token or authorization code.
	InvalidGrant bool
}

// Error formats the OAuthError as a human-readable diagnostic message.
func (e *OAuthError) Error() string {
	if e.ErrorCode != "" {
		return fmt.Sprintf("google oauth error (HTTP %d, %s): %s", e.StatusCode, e.ErrorCode, e.Description)
	}
	return fmt.Sprintf("google oauth error (HTTP %d): %s", e.StatusCode, e.Description)
}

// TokenResult represents the outcome of an OAuth token exchange or refresh operation.
type TokenResult struct {
	// AccessToken is the short-lived Google OAuth 2.0 Bearer token used for API requests.
	AccessToken string

	// RefreshToken is the long-lived token used to obtain new access tokens.
	RefreshToken string

	// ExpiresIn indicates token lifetime in seconds as reported by Google.
	ExpiresIn int

	// Expiry calculates the absolute local time when the access token ceases to be valid.
	Expiry time.Time
}

// String returns a redacted representation of TokenResult to prevent credential leakage in logs.
func (t *TokenResult) String() string {
	if t == nil {
		return "<nil>"
	}
	accMasked := "<empty>"
	if len(t.AccessToken) > 8 {
		accMasked = t.AccessToken[:4] + "..." + t.AccessToken[len(t.AccessToken)-4:]
	} else if t.AccessToken != "" {
		accMasked = "***"
	}
	refMasked := "<empty>"
	if len(t.RefreshToken) > 8 {
		refMasked = t.RefreshToken[:4] + "..." + t.RefreshToken[len(t.RefreshToken)-4:]
	} else if t.RefreshToken != "" {
		refMasked = "***"
	}
	return fmt.Sprintf("TokenResult{AccessToken:%s, RefreshToken:%s, ExpiresIn:%d, Expiry:%s}",
		accMasked, refMasked, t.ExpiresIn, t.Expiry.Format(time.RFC3339))
}

// IsExpiredOrExpiringSoon returns true if the token is expired or expires within the given margin.
// Default margin should be at least 120s to ensure proactive refresh before token expiration.
func (t *TokenResult) IsExpiredOrExpiringSoon(margin time.Duration) bool {
	if t.Expiry.IsZero() {
		return true
	}
	return time.Until(t.Expiry) <= margin
}

// googleTokenResponse maps the JSON payload structure returned by Google's OAuth 2.0 token endpoint.
type googleTokenResponse struct {
	// AccessToken holds the newly minted access token string.
	AccessToken string `json:"access_token"`

	// RefreshToken holds the refresh token string (present on initial authorization_code exchange).
	RefreshToken string `json:"refresh_token"`

	// ExpiresIn specifies the token lifetime in seconds.
	ExpiresIn int `json:"expires_in"`

	// TokenType is typically "Bearer".
	TokenType string `json:"token_type"`

	// Error holds the error code string if the request failed.
	Error string `json:"error"`

	// ErrorDesc provides detailed diagnostic text from Google.
	ErrorDesc string `json:"error_description"`
}

// ExchangeCode exchanges a temporary OAuth authorization code and PKCE verifier for an access and refresh token.
func ExchangeCode(ctx context.Context, cfg *config.Config, code string, verifier string) (*TokenResult, error) {
	data := url.Values{}
	data.Set("code", code)
	data.Set("grant_type", "authorization_code")
	data.Set("code_verifier", verifier)

	redirectURI := config.DefaultRedirectURI
	if cfg != nil {
		redirectURI = cmp.Or(cfg.RedirectURI, config.DefaultRedirectURI)
	}
	data.Set("redirect_uri", redirectURI)

	return executeTokenRequest(ctx, cfg, data)
}

// RefreshAccessToken requests a new access token from Google using the given refresh token.
func RefreshAccessToken(ctx context.Context, cfg *config.Config, refreshToken string) (*TokenResult, error) {
	if refreshToken == "" {
		return nil, errors.New("refresh token cannot be empty")
	}

	data := url.Values{}
	data.Set("refresh_token", refreshToken)
	data.Set("grant_type", "refresh_token")

	result, err := executeTokenRequest(ctx, cfg, data)
	if err != nil {
		return nil, err
	}
	// Google does not always re-issue the refresh_token on refresh grants; preserve original
	if result.RefreshToken == "" {
		result.RefreshToken = refreshToken
	}
	return result, nil
}

// executeTokenRequest performs the actual HTTP POST against Google's OAuth 2.0 token endpoint,
// validating the response code and parsing tokens or detailed error objects.
func executeTokenRequest(ctx context.Context, cfg *config.Config, form url.Values) (*TokenResult, error) {
	tokenURL := config.DefaultTokenURL
	clientID := config.DefaultClientID
	clientSecret := config.DefaultClientSecret

	if cfg != nil {
		tokenURL = cmp.Or(cfg.TokenURL, config.DefaultTokenURL)
		clientID = cmp.Or(cfg.ClientID, config.DefaultClientID)
		clientSecret = cmp.Or(cfg.ClientSecret, config.DefaultClientSecret)
	}

	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dispatching token request to %s: %w", tokenURL, err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB ceiling
	if err != nil {
		return nil, fmt.Errorf("reading token response body: %w", err)
	}

	var parsed googleTokenResponse
	// Safe to ignore decode error on raw body since parsed fields are checked for non-empty below
	_ = json.Unmarshal(bodyBytes, &parsed)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 || parsed.AccessToken == "" {
		desc := parsed.ErrorDesc
		if desc == "" {
			desc = strings.TrimSpace(string(bodyBytes))
		}
		return nil, &OAuthError{
			StatusCode:   resp.StatusCode,
			ErrorCode:    parsed.Error,
			Description:  desc,
			InvalidGrant: parsed.Error == "invalid_grant",
		}
	}

	expiresIn := parsed.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}

	return &TokenResult{
		AccessToken:  parsed.AccessToken,
		RefreshToken: parsed.RefreshToken,
		ExpiresIn:    expiresIn,
		Expiry:       time.Now().Add(time.Duration(expiresIn) * time.Second),
	}, nil
}
