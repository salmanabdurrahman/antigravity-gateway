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
	"testing"

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
	if acc.RefreshToken != "imported-token-789" {
		t.Errorf("expected token stored in RefreshToken, got %s", acc.RefreshToken)
	}

	loaded, err := store.Load()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("expected 1 account in store, got %d (err: %v)", len(loaded), err)
	}
}
