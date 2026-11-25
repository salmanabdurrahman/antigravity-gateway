// Package client provides automated configuration injectors and diagnostic tools
// for local AI coding agents such as Pi and OpenCode.
package client

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestTestClientsBlockCredentialRedirects verifies the completion probes never follow HTTP redirects,
// so the gateway API key cannot be forwarded to a foreign origin through a 3xx response.
func TestTestClientsBlockCredentialRedirects(t *testing.T) {
	var leakedCredentials atomic.Bool

	leakTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "" || r.Header.Get("Authorization") != "" {
			leakedCredentials.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"leaked"}],"choices":[{"message":{"content":"leaked"}}]}`))
	}))
	defer leakTarget.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, leakTarget.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	t.Run("anthropic probe", func(t *testing.T) {
		if _, err := TestAnthropicCompletion(t.Context(), redirector.URL, "test-secret-key", "claude-sonnet-4-6"); err == nil {
			t.Fatal("expected the redirect status to surface as an error instead of being followed")
		}
	})

	t.Run("openai probe", func(t *testing.T) {
		if _, err := TestChatCompletion(t.Context(), redirector.URL, "test-secret-key", "gemini-2.5-flash"); err == nil {
			t.Fatal("expected the redirect status to surface as an error instead of being followed")
		}
	})

	if leakedCredentials.Load() {
		t.Error("probe clients forwarded credentials to the redirect target")
	}
}
