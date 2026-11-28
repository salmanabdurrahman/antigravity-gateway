// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// suspensionUpstreamBody is the Cloud Code Assist suspension envelope used by the upstream mock. The
// appeal host and path stay inside the allowed metadata allowlist so the handler surfaces them.
const suspensionUpstreamBody = `{
  "error": {
    "code": 403,
    "message": "This service has been disabled in this account for violation of Terms of Service. Please submit an appeal to continue using this product.",
    "status": "PERMISSION_DENIED",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        "reason": "TOS_VIOLATION",
        "domain": "cloudcode-pa.googleapis.com",
        "metadata": {
          "uiMessage": "true",
          "appeal_url": "https://forms.gle/SERVERCASE",
          "appeal_url_link_text": "Submit Appeal"
        }
      }
    ]
  }
}`

// newSuspendingUpstream builds an upstream handler that mints tokens normally and refuses every
// content-generation call with a terminal suspension verdict.
func newSuspendingUpstream() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(suspensionUpstreamBody))
	}
}

// TestChatCompletionsSuspensionSurfacesAppealURL verifies that a terminal suspension verdict reaches the
// OpenAI-compatible client as a permission error carrying the upstream appeal link, and that the armed
// protective pause answers the next request with a Retry-After header instead of contacting upstream.
func TestChatCompletionsSuspensionSurfacesAppealURL(t *testing.T) {
	harness := setupTestHarness(t, newSuspendingUpstream())
	handler := harness.server.buildHandler()

	payload := `{
		"model": "gemini-2.5-flash",
		"messages": [{"role": "user", "content": "Hi"}],
		"stream": false
	}`

	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(payload))
		req.Header.Set("Authorization", "Bearer "+harness.apiKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	rec := post()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d: %s", rec.Code, rec.Body.String())
	}

	var suspErr APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &suspErr); err != nil {
		t.Fatalf("decoding suspension error body: %v\n%s", err, rec.Body.String())
	}
	if suspErr.Error.Type != "permission_error" {
		t.Errorf("error.type = %q; want permission_error", suspErr.Error.Type)
	}
	if suspErr.Error.Code != http.StatusForbidden {
		t.Errorf("error.code = %d; want %d", suspErr.Error.Code, http.StatusForbidden)
	}
	if suspErr.Error.AppealURL != "https://forms.gle/SERVERCASE" {
		t.Errorf("error.appeal_url = %q; want the upstream appeal link", suspErr.Error.AppealURL)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("violation of Terms of Service")) {
		t.Errorf("error message lost the upstream explanation: %s", rec.Body.String())
	}

	// The suspension arms the protective pool pause, so the following request must be refused locally.
	paused := post()
	if paused.Code != http.StatusForbidden {
		t.Fatalf("expected status 403 during the protective pause, got %d: %s", paused.Code, paused.Body.String())
	}
	if paused.Header().Get("Retry-After") == "" {
		t.Errorf("expected a Retry-After header while the protective pause is armed")
	}
	var pausedErr APIErrorResponse
	if err := json.Unmarshal(paused.Body.Bytes(), &pausedErr); err != nil {
		t.Fatalf("decoding paused error body: %v\n%s", err, paused.Body.String())
	}
	if pausedErr.Error.AppealURL != "https://forms.gle/SERVERCASE" {
		t.Errorf("paused error.appeal_url = %q; want the upstream appeal link", pausedErr.Error.AppealURL)
	}
}

// TestAnthropicMessagesSuspensionSurfacesAppealURL verifies that the Anthropic-compatible endpoint reports
// the same terminal verdict with the validated appeal link in its own error envelope.
func TestAnthropicMessagesSuspensionSurfacesAppealURL(t *testing.T) {
	harness := setupTestHarness(t, newSuspendingUpstream())
	handler := harness.server.buildHandler()

	payload := `{"model": "claude-sonnet-4-5", "max_tokens": 64, "messages": [{"role": "user", "content": "Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d: %s", rec.Code, rec.Body.String())
	}

	var suspErr AnthropicErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &suspErr); err != nil {
		t.Fatalf("decoding anthropic suspension error body: %v\n%s", err, rec.Body.String())
	}
	if suspErr.Error.Type != "permission_error" {
		t.Errorf("error.type = %q; want permission_error", suspErr.Error.Type)
	}
	if suspErr.Error.AppealURL != "https://forms.gle/SERVERCASE" {
		t.Errorf("error.appeal_url = %q; want the upstream appeal link", suspErr.Error.AppealURL)
	}
}
