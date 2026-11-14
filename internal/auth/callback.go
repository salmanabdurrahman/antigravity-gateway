// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CallbackResult contains the authorization code or error retrieved from the loopback OAuth listener.
type CallbackResult struct {
	// Code is the temporary authorization code returned by Google upon successful consent.
	Code string

	// State is the CSRF state token echoed back by Google to match the initial request.
	State string

	// Error is any error code or description returned by Google if consent was denied.
	Error string
}

// successHTML returns a user-friendly HTML confirmation page rendered in the user's browser upon successful authentication.
func successHTML() string {
	return `<!doctype html>
<html>
<head><meta charset="utf-8"><title>Antigravity Gateway - Signed In</title></head>
<body style="font-family:system-ui,-apple-system,sans-serif;margin:4rem;background:#0d1117;color:#c9d1d9;">
  <h1 style="color:#58a6ff;">Signed In Successfully</h1>
  <p>You can close this tab and return to your terminal.</p>
</body>
</html>`
}

// failureHTML returns an HTML error notification page rendered in the user's browser when authentication fails.
func failureHTML(errDesc string) string {
	safeErr := html.EscapeString(errDesc)
	return fmt.Sprintf(`<!doctype html>
<html>
<head><meta charset="utf-8"><title>Antigravity Gateway - Sign In Failed</title></head>
<body style="font-family:system-ui,-apple-system,sans-serif;margin:4rem;background:#0d1117;color:#c9d1d9;">
  <h1 style="color:#f85149;">Sign In Failed</h1>
  <p>%s</p>
  <p>Please return to the terminal to retry.</p>
</body>
</html>`, safeErr)
}

// ResolveCallbackEndpoint parses a configured redirect URI string and extracts the host:port and path.
// Defaults to 127.0.0.1:51121 and /oauth-callback if parsing fails or components are missing.
func ResolveCallbackEndpoint(rawURI string) (listenAddr string, callbackPath string) {
	listenAddr = "127.0.0.1:51121"
	callbackPath = "/oauth-callback"

	if strings.TrimSpace(rawURI) == "" {
		return listenAddr, callbackPath
	}

	u, err := url.Parse(rawURI)
	if err != nil {
		return listenAddr, callbackPath
	}

	host := u.Hostname()
	if host == "localhost" || host == "" {
		host = "127.0.0.1"
	}

	port := u.Port()
	if port == "" {
		port = "51121"
	}

	listenAddr = net.JoinHostPort(host, port)
	if u.Path != "" {
		callbackPath = u.Path
	}

	return listenAddr, callbackPath
}

// ParseRedirectURL parses a raw redirect URL string and extracts OAuth callback parameters (code, state, error).
func ParseRedirectURL(rawURL string) (*CallbackResult, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing redirect url: %w", err)
	}

	q := parsed.Query()
	return &CallbackResult{
		Code:  q.Get("code"),
		State: q.Get("state"),
		Error: q.Get("error"),
	}, nil
}

// StartCallbackListener starts an ephemeral HTTP server on 127.0.0.1:51121 waiting for the OAuth callback.
// It stops listening as soon as a valid code or error is handled, or when ctx is cancelled.
func StartCallbackListener(ctx context.Context, listenAddr string, callbackPath string, expectedState string) (*CallbackResult, error) {
	if listenAddr == "" {
		listenAddr = "127.0.0.1:51121"
	}
	if callbackPath == "" {
		callbackPath = "/oauth-callback"
	}

	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("binding local oauth callback listener on %s: %w", listenAddr, err)
	}

	resultChan := make(chan *CallbackResult, 1)
	var once sync.Once

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+callbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := q.Get("code")
		state := q.Get("state")
		errParam := q.Get("error")

		if errParam != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(failureHTML(errParam)))
			once.Do(func() {
				resultChan <- &CallbackResult{Error: errParam, State: state}
			})
			return
		}

		if code == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(failureHTML("Missing authorization code in redirect callback.")))
			return
		}

		if expectedState != "" && state != expectedState {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(failureHTML("State mismatch parameter detected. Possible CSRF attempt.")))
			// Do not abort the server on foreign or scanner requests; keep listening for legitimate redirect
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(successHTML()))

		once.Do(func() {
			resultChan <- &CallbackResult{Code: code, State: state}
		})
	})

	server := &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	serverErrChan := make(chan error, 1)
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			serverErrChan <- serveErr
		}
	}()

	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case sErr := <-serverErrChan:
		return nil, fmt.Errorf("oauth callback server error: %w", sErr)
	case res := <-resultChan:
		if res.Error != "" {
			return nil, fmt.Errorf("oauth callback received error: %s", res.Error)
		}
		return res, nil
	}
}
