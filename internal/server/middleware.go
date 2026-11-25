// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"runtime/debug"
	"strings"
	"time"
)

// APIError details a structured error payload formatted for OpenAI client compatibility.
type APIError struct {
	// Message provides a human-readable explanation of why the request failed.
	Message string `json:"message"`

	// Type categorizes the error (e.g. "invalid_request_error", "rate_limit_error", "authentication_error").
	Type string `json:"type"`

	// Code holds the numerical HTTP status code.
	Code int `json:"code"`
}

// APIErrorResponse wraps APIError in the standard OpenAI error envelope.
type APIErrorResponse struct {
	// Error contains the structured error payload.
	Error APIError `json:"error"`
}

// statusWriter captures the HTTP status code written by downstream handlers for structured logging.
type statusWriter struct {
	// ResponseWriter is the wrapped underlying response writer.
	http.ResponseWriter

	// status captures the HTTP status code passed to WriteHeader.
	status int
}

// WriteHeader records the status code and delegates to the underlying ResponseWriter.
func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Flush implements http.Flusher to support streaming SSE chunks through wrapped response writers.
func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap provides access to the underlying http.ResponseWriter for http.ResponseController inspection.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// RecoveryMiddleware catches unhandled panics in HTTP request handlers, logs stack traces,
// and returns a standard HTTP 500 JSON error response if headers have not yet been written.
func RecoveryMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sw := &statusWriter{ResponseWriter: w, status: 0}
			defer func() {
				if rec := recover(); rec != nil {
					stack := string(debug.Stack())
					logger.Error("unhandled panic recovered in HTTP handler",
						"error", fmt.Sprint(rec),
						"method", r.Method,
						"path", r.URL.Path,
						"stack", stack,
					)
					if sw.status == 0 {
						if strings.HasPrefix(r.URL.Path, "/v1/messages") {
							sendAnthropicJSONError(sw, http.StatusInternalServerError, "Internal server error", 0)
						} else {
							sendJSONError(sw, http.StatusInternalServerError, "Internal server error", 0)
						}
					}
				}
			}()
			next.ServeHTTP(sw, r)
		})
	}
}

// AuthMiddleware validates client requests against the configured gateway API key.
// It bypasses authentication for public liveness probes (/health and /).
func AuthMiddleware(apiKey string, logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Normalize the request path before the public-endpoint decision: an uncleaned
			// "/health/../v1/accounts" must never inherit an unauthenticated prefix, and any
			// leftover traversal segment is rejected outright instead of relying on ServeMux
			// redirect behavior.
			if hasTraversalSegment(r.URL.Path) {
				logger.Warn("rejected request path traversal attempt",
					"method", r.Method,
					"path", r.URL.Path,
					"remote", r.RemoteAddr,
				)
				sendJSONError(w, http.StatusBadRequest, "Invalid request path.", 0)
				return
			}

			cleanPath := path.Clean("/" + r.URL.Path)
			if cleanPath == "/" || cleanPath == "/health" {
				next.ServeHTTP(w, r)
				return
			}

			if !isAuthorized(r, apiKey) {
				logger.Warn("unauthorized request rejected",
					"method", r.Method,
					"path", r.URL.Path,
					"remote", r.RemoteAddr,
				)
				if strings.HasPrefix(r.URL.Path, "/v1/messages") {
					sendAnthropicJSONError(w, http.StatusUnauthorized, "Invalid API key. Provide Bearer or x-api-key matching agw config.", 0)
					return
				}
				sendJSONError(w, http.StatusUnauthorized, "Invalid API key. Provide Bearer token matching agw config.", 0)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// hasTraversalSegment reports whether any decoded path segment is a parent-directory reference.
// `path.Clean` resolves such segments silently, so the raw request path is inspected instead.
func hasTraversalSegment(rawPath string) bool {
	for segment := range strings.SplitSeq(rawPath, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// LoggingMiddleware logs HTTP requests, response status codes, and execution latency with log/slog.
func LoggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

			next.ServeHTTP(sw, r)

			duration := time.Since(start)
			logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"duration", duration.String(),
				"remote", r.RemoteAddr,
			)
		})
	}
}

// isAuthorized performs timing-safe comparison of provided Authorization or X-API-Key headers against the configured key.
func isAuthorized(r *http.Request, expectedKey string) bool {
	if expectedKey == "" {
		return false
	}

	authHeader := r.Header.Get("Authorization")
	givenKey := ""
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		givenKey = strings.TrimSpace(authHeader[7:])
	} else if xKey := r.Header.Get("X-API-Key"); xKey != "" {
		givenKey = strings.TrimSpace(xKey)
	}

	if givenKey == "" {
		return false
	}

	return subtle.ConstantTimeCompare([]byte(givenKey), []byte(expectedKey)) == 1
}

// sendJSON writes a JSON payload with appropriate Content-Type header.
func sendJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// sendJSONError formats and sends an OpenAI-compatible error JSON response.
func sendJSONError(w http.ResponseWriter, status int, message string, retryAfter time.Duration) {
	errType := "invalid_request_error"
	switch {
	case status == http.StatusTooManyRequests:
		errType = "rate_limit_error"
	case status == http.StatusUnauthorized:
		errType = "authentication_error"
	case status == http.StatusForbidden:
		errType = "permission_error"
	case status >= http.StatusInternalServerError:
		errType = "server_error"
	}

	if retryAfter > 0 {
		seconds := int(retryAfter.Seconds())
		if seconds <= 0 {
			seconds = 1
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	}

	sendJSON(w, status, APIErrorResponse{
		Error: APIError{
			Message: message,
			Type:    errType,
			Code:    status,
		},
	})
}

// AnthropicError defines the nested error structure for Anthropic API error responses.
type AnthropicError struct {
	// Type indicates the error category (e.g. "invalid_request_error", "rate_limit_error").
	Type string `json:"type"`

	// Message is the descriptive error explanation.
	Message string `json:"message"`
}

// AnthropicErrorResponse wraps the Anthropic error envelope.
type AnthropicErrorResponse struct {
	// Type is always "error".
	Type string `json:"type"`

	// Error contains the nested error details.
	Error AnthropicError `json:"error"`
}

// sendAnthropicJSONError formats and sends an Anthropic-compatible error JSON response.
func sendAnthropicJSONError(w http.ResponseWriter, status int, message string, retryAfter time.Duration) {
	errType := "invalid_request_error"
	switch {
	case status == http.StatusTooManyRequests:
		errType = "rate_limit_error"
	case status == http.StatusUnauthorized:
		errType = "authentication_error"
	case status == http.StatusForbidden:
		errType = "permission_error"
	case status == http.StatusNotFound:
		errType = "not_found_error"
	case status >= http.StatusInternalServerError:
		errType = "api_error"
	}

	if retryAfter > 0 {
		seconds := int(retryAfter.Seconds())
		if seconds <= 0 {
			seconds = 1
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", seconds))
	}

	sendJSON(w, status, AnthropicErrorResponse{
		Type: "error",
		Error: AnthropicError{
			Type:    errType,
			Message: message,
		},
	})
}
