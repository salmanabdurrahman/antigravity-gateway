// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAuthMiddleware verifies Bearer and X-API-Key token validation and health probe bypasses.
func TestAuthMiddleware(t *testing.T) {
	apiKey := "test_secret_key_12345"
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list"}`))
	})

	handler := AuthMiddleware(apiKey, logger)(mux)

	tests := []struct {
		name       string
		method     string
		path       string
		headers    map[string]string
		wantStatus int
	}{
		{
			name:       "health probe bypasses auth",
			method:     http.MethodGet,
			path:       "/health",
			wantStatus: http.StatusOK,
		},
		{
			name:       "root probe bypasses auth",
			method:     http.MethodGet,
			path:       "/",
			wantStatus: http.StatusOK,
		},
		{
			name:       "protected route missing auth returns 401",
			method:     http.MethodGet,
			path:       "/v1/models",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "protected route invalid bearer returns 401",
			method:     http.MethodGet,
			path:       "/v1/models",
			headers:    map[string]string{"Authorization": "Bearer wrong_key"},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "protected route valid bearer returns 200",
			method:     http.MethodGet,
			path:       "/v1/models",
			headers:    map[string]string{"Authorization": "Bearer " + apiKey},
			wantStatus: http.StatusOK,
		},
		{
			name:       "protected route valid x-api-key returns 200",
			method:     http.MethodGet,
			path:       "/v1/models",
			headers:    map[string]string{"X-API-Key": apiKey},
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("expected status %d, got %d. Body: %s", tc.wantStatus, rec.Code, rec.Body.String())
			}

			if tc.wantStatus == http.StatusUnauthorized {
				var errResp APIErrorResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
					t.Fatalf("failed to decode error json: %v", err)
				}
				if errResp.Error.Type != "authentication_error" {
					t.Errorf("expected error type 'authentication_error', got %q", errResp.Error.Type)
				}
				if errResp.Error.Code != http.StatusUnauthorized {
					t.Errorf("expected error code 401, got %d", errResp.Error.Code)
				}
			}
		})
	}
}

// TestRecoveryMiddleware verifies recovering from unhandled panics and returning standard HTTP 500 JSON errors.
func TestRecoveryMiddleware(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	panickingHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("simulated fatal panic in handler")
	})

	handler := RecoveryMiddleware(logger)(panickingHandler)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status %d after panic recovery, got %d", http.StatusInternalServerError, rec.Code)
	}

	var errResp APIErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to unmarshal panic response: %v", err)
	}

	if errResp.Error.Type != "server_error" {
		t.Errorf("expected error type 'server_error', got %q", errResp.Error.Type)
	}
}

// TestLoggingMiddleware verifies that structured HTTP request access logs are recorded with status and duration.
func TestLoggingMiddleware(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	handler := LoggingMiddleware(logger)(nextHandler)

	req := httptest.NewRequest(http.MethodGet, "/logging-test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Fatalf("expected status %d, got %d", http.StatusTeapot, rec.Code)
	}
}
