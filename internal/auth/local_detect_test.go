// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestDetectAntigravityDBPath verifies path resolution respecting ANTIGRAVITY_DB_PATH override.
func TestDetectAntigravityDBPath(t *testing.T) {
	customPath := filepath.Join(t.TempDir(), "custom.vscdb")
	t.Setenv("ANTIGRAVITY_DB_PATH", customPath)

	path, err := DetectAntigravityDBPath()
	if err != nil {
		t.Fatalf("DetectAntigravityDBPath returned error: %v", err)
	}
	if path != customPath {
		t.Errorf("expected %s, got %s", customPath, path)
	}
}

// TestExtractSessionFromBytes verifies binary byte-scanning for antigravityAuthStatus JSON payload.
func TestExtractSessionFromBytes(t *testing.T) {
	jsonPayload := `{"apiKey":"mock-api-key-123","email":"dev@example.com","name":"Test Dev"}`
	var buf []byte
	buf = append(buf, []byte("random_sqlite_header_bytes")...)
	buf = append(buf, []byte("antigravityAuthStatus")...)
	buf = append(buf, []byte(":extra_padding:")...)
	buf = append(buf, []byte(jsonPayload)...)
	buf = append(buf, []byte("trailing_binary_data")...)

	sess, err := ExtractSessionFromBytes(buf)
	if err != nil {
		t.Fatalf("ExtractSessionFromBytes failed: %v", err)
	}

	if sess.APIKey != "mock-api-key-123" {
		t.Errorf("expected apiKey mock-api-key-123, got %s", sess.APIKey)
	}
	if sess.Email != "dev@example.com" {
		t.Errorf("expected email dev@example.com, got %s", sess.Email)
	}
	if sess.Name != "Test Dev" {
		t.Errorf("expected name Test Dev, got %s", sess.Name)
	}
}

// TestExtractSessionFromHTTP verifies extracting base64-encoded chatParams from Antigravity local HTTP server.
func TestExtractSessionFromHTTP(t *testing.T) {
	jsonPayload := `{"apiKey":"http-token-456","email":"http-user@example.com"}`
	encoded := base64.StdEncoding.EncodeToString([]byte(jsonPayload))
	html := fmt.Sprintf(`<html><body><script>window.chatParams = '%s';</script></body></html>`, encoded)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(html))
	}))
	defer server.Close()

	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())

	sess, err := ExtractSessionFromHTTP(t.Context(), port)
	if err != nil {
		t.Fatalf("ExtractSessionFromHTTP failed: %v", err)
	}

	if sess.APIKey != "http-token-456" {
		t.Errorf("expected apiKey http-token-456, got %s", sess.APIKey)
	}
	if sess.Email != "http-user@example.com" {
		t.Errorf("expected email http-user@example.com, got %s", sess.Email)
	}
}

// TestImportLocalSession verifies importing credentials and updating the Account store.
func TestImportLocalSession(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "state.vscdb")
	t.Setenv("ANTIGRAVITY_DB_PATH", dbPath)

	jsonPayload := `{"apiKey":"imported-token-789","email":"imported@example.com","name":"Imported User"}`
	data := append([]byte("prefix_antigravityAuthStatus_padding"), []byte(jsonPayload)...)
	if err := os.WriteFile(dbPath, data, 0o600); err != nil {
		t.Fatalf("writing mock db file: %v", err)
	}

	storePath := filepath.Join(tempDir, "accounts.json")
	store := NewStore(storePath)

	cfg := config.DefaultConfig()

	acc, err := ImportLocalSession(t.Context(), cfg, store)
	if err != nil {
		t.Fatalf("ImportLocalSession failed: %v", err)
	}

	if acc.Email != "imported@example.com" {
		t.Errorf("expected imported@example.com, got %s", acc.Email)
	}
	if acc.AccessToken != "imported-token-789" {
		t.Errorf("expected token stored in AccessToken, got %q", maskSecret(acc.AccessToken))
	}
	if acc.RefreshToken != "" {
		t.Errorf("desktop session token must not be stored as a refresh token, got %q", maskSecret(acc.RefreshToken))
	}
	if acc.CredentialSource != CredentialSourceLocalDesktop {
		t.Errorf("expected credential source %q, got %q", CredentialSourceLocalDesktop, acc.CredentialSource)
	}

	loaded, err := store.Load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("expected 1 account in store, got %d (err: %v)", len(loaded), err)
	}
	if !loaded[0].HasCredentials() {
		t.Error("imported account must survive reload with credentials intact")
	}
}

// TestValidateLocalSession verifies that imported desktop payloads are normalized and rejected when
// the bearer token is missing, oversized, or carries whitespace or control characters that would
// corrupt the Authorization header on replay.
func TestValidateLocalSession(t *testing.T) {
	cases := []struct {
		name    string
		session LocalSession
		wantErr bool
	}{
		{name: "valid token", session: LocalSession{APIKey: "token-abc"}},
		{name: "trimmed values", session: LocalSession{APIKey: "  token-abc  ", Email: " user@example.com "}},
		{name: "empty token", session: LocalSession{APIKey: "   "}, wantErr: true},
		{name: "oversized token", session: LocalSession{APIKey: strings.Repeat("a", maxLocalTokenLength+1)}, wantErr: true},
		{name: "embedded space", session: LocalSession{APIKey: "token abc"}, wantErr: true},
		{name: "control character", session: LocalSession{APIKey: "token\x00abc"}, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := tc.session
			err := validateLocalSession(&session)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected validation error for %s", tc.name)
				}
				if session.APIKey != "" && strings.Contains(err.Error(), session.APIKey) {
					t.Errorf("validation error leaked the token value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
			if session.APIKey != strings.TrimSpace(tc.session.APIKey) {
				t.Errorf("expected trimmed token %q, got %q", tc.session.APIKey, session.APIKey)
			}
		})
	}
}

// TestSQLiteCandidatesUseAbsolutePaths verifies that the sqlite3 probe list never relies on PATH
// resolution, so a shadowed binary cannot be handed the credential database.
func TestSQLiteCandidatesUseAbsolutePaths(t *testing.T) {
	if len(sqlite3Candidates) == 0 {
		t.Fatal("expected at least one sqlite3 candidate")
	}
	for _, candidate := range sqlite3Candidates {
		if !filepath.IsAbs(candidate) {
			t.Errorf("sqlite3 candidate %q must be an absolute path", candidate)
		}
	}
}

// TestImportLocalSessionKeepsRefreshableAccount verifies that importing a desktop session never
// replaces an existing OAuth account holding a refresh token, since the refresh token cannot be
// recovered once overwritten by a short-lived bearer token.
func TestImportLocalSessionKeepsRefreshableAccount(t *testing.T) {
	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "state.vscdb")
	t.Setenv("ANTIGRAVITY_DB_PATH", dbPath)

	jsonPayload := `{"apiKey":"desktop-token-456","email":"shared@example.com"}`
	data := append([]byte("prefix_antigravityAuthStatus_"), []byte(jsonPayload)...)
	if err := os.WriteFile(dbPath, data, 0o600); err != nil {
		t.Fatalf("writing mock db file: %v", err)
	}

	store := NewStore(filepath.Join(tempDir, "accounts.json"))
	existing := &Account{
		Email:        "shared@example.com",
		RefreshToken: "oauth-refresh-token",
		ProjectID:    "proj-oauth",
		AddedAt:      time.Now().UTC().Format(time.RFC3339),
	}
	if _, err := store.Upsert(existing); err != nil {
		t.Fatalf("seeding OAuth account: %v", err)
	}

	cfg := config.DefaultConfig()
	acc, err := ImportLocalSession(t.Context(), cfg, store)
	if err != nil {
		t.Fatalf("ImportLocalSession failed: %v", err)
	}
	if acc.RefreshToken != "oauth-refresh-token" {
		t.Errorf("expected existing refresh token to be preserved, got %q", maskSecret(acc.RefreshToken))
	}
	if acc.AccessToken != "" {
		t.Errorf("expected no desktop access token to be stored, got %q", maskSecret(acc.AccessToken))
	}

	loaded, err := store.Load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("expected 1 account after import, got %d (err: %v)", len(loaded), err)
	}
	if loaded[0].RefreshToken != "oauth-refresh-token" {
		t.Errorf("stored account lost its refresh token: %q", maskSecret(loaded[0].RefreshToken))
	}
}
