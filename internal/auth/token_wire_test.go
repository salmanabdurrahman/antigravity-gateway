// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestRefreshAccessTokenUsesHTTP11Wire verifies that a token refresh is dispatched over HTTP/1.1, matching
// the inference path's wire shape instead of the legacy shared HTTP/2 pool.
func TestRefreshAccessTokenUsesHTTP11Wire(t *testing.T) {
	t.Setenv(config.WireModeEnv, config.WireModeHTTP1)

	var protoMajor int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protoMajor = r.ProtoMajor
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "refreshed-token",
			"expires_in":   3600,
			"token_type":   "Bearer",
		})
	}))
	defer server.Close()

	result, err := RefreshAccessToken(t.Context(), &config.Config{TokenURL: server.URL}, "refresh-wire-test")
	if err != nil {
		t.Fatalf("RefreshAccessToken failed: %v", err)
	}
	if result.AccessToken != "refreshed-token" {
		t.Errorf("AccessToken = %q; want refreshed-token", result.AccessToken)
	}
	if protoMajor != 1 {
		t.Errorf("token refresh used HTTP/%d; want HTTP/1.1", protoMajor)
	}
}
