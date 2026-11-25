// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// newOfflineAuthEndpoints serves deterministic userinfo and loadCodeAssist responses so local import
// tests never reach live Google endpoints.
func newOfflineAuthEndpoints(t *testing.T) *config.Config {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/userinfo":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sub":   "google-sub-offline",
				"email": "userinfo@example.com",
			})
		case "/v1internal:loadCodeAssist":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cloudaicompanionProject": "offline-companion-project",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := config.DefaultConfig()
	cfg.UserInfoURL = server.URL + "/userinfo"
	cfg.Endpoints = []string{server.URL}
	return cfg
}

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

	cfg := newOfflineAuthEndpoints(t)

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

	cfg := newOfflineAuthEndpoints(t)
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

// TestAgyTokenCandidatesHonorsOverride verifies that an explicit AGW_AGY_TOKEN_PATH is probed before
// the auto-discovered Antigravity CLI locations.
func TestAgyTokenCandidatesHonorsOverride(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "custom-acp_token.json")
	t.Setenv(AgyTokenPathEnv, custom)

	candidates := AgyTokenCandidates()
	if len(candidates) == 0 {
		t.Fatal("expected at least one candidate path")
	}
	if candidates[0] != custom {
		t.Errorf("expected override %q probed first, got %q", custom, candidates[0])
	}
}

// TestExtractSessionFromAgyToken verifies that Antigravity CLI token files are accepted in flat,
// nested, and bare-string shapes while malformed payloads are rejected without leaking the value.
func TestExtractSessionFromAgyToken(t *testing.T) {
	tests := []struct {
		name      string
		payload   string
		wantKey   string
		wantEmail string
		wantErr   bool
	}{
		{
			name:      "flat acp token",
			payload:   `{"apiKey":"agy-token-1","email":"agy@example.com","name":"Agy User"}`,
			wantKey:   "agy-token-1",
			wantEmail: "agy@example.com",
		},
		{
			name:      "nested oauth credential",
			payload:   `{"session":{"access_token":"agy-token-2","email":"nested@example.com"}}`,
			wantKey:   "agy-token-2",
			wantEmail: "nested@example.com",
		},
		{
			name:    "bare bearer string",
			payload: "agy-token-3\n",
			wantKey: "agy-token-3",
		},
		{
			name:    "missing token",
			payload: `{"email":"missing@example.com"}`,
			wantErr: true,
		},
		{
			name:    "whitespace inside token",
			payload: `{"apiKey":"bad token"}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "acp_token.json")
			if err := os.WriteFile(path, []byte(tc.payload), 0o600); err != nil {
				t.Fatalf("writing token fixture: %v", err)
			}

			sess, err := ExtractSessionFromAgyToken(path)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected validation error")
				}
				if tc.wantKey != "" && strings.Contains(err.Error(), tc.wantKey) {
					t.Errorf("error leaked the token value: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExtractSessionFromAgyToken failed: %v", err)
			}
			if sess.APIKey != tc.wantKey {
				t.Errorf("expected token %q, got %q", tc.wantKey, maskSecret(sess.APIKey))
			}
			if sess.Email != tc.wantEmail {
				t.Errorf("expected email %q, got %q", tc.wantEmail, sess.Email)
			}
		})
	}
}

// TestDetectLocalSessionFromAgyTokenFile verifies that a CLI token file is discovered when the IDE
// database is unavailable.
func TestDetectLocalSessionFromAgyTokenFile(t *testing.T) {
	t.Setenv("ANTIGRAVITY_DB_PATH", filepath.Join(t.TempDir(), "missing.vscdb"))
	tokenPath := filepath.Join(t.TempDir(), "acp_token.json")
	t.Setenv(AgyTokenPathEnv, tokenPath)
	t.Setenv(AgyKeyringEnv, "")

	if err := os.WriteFile(tokenPath, []byte(`{"apiKey":"detected-token","email":"detected@example.com"}`), 0o600); err != nil {
		t.Fatalf("writing token fixture: %v", err)
	}

	sess, err := DetectLocalSession(t.Context())
	if err != nil {
		t.Fatalf("DetectLocalSession failed: %v", err)
	}
	if sess.APIKey != "detected-token" || sess.Email != "detected@example.com" {
		t.Errorf("unexpected discovered session: %+v", sess)
	}
}

// TestExtractSessionFromKeyringRequiresOptIn verifies that the shared OS keyring is never read unless
// the operator explicitly opts in.
func TestExtractSessionFromKeyringRequiresOptIn(t *testing.T) {
	t.Setenv(AgyKeyringEnv, "")

	if _, err := ExtractSessionFromKeyring(t.Context()); err == nil {
		t.Fatal("expected keyring lookup to stay disabled without opt-in")
	}
}

// TestExtractSessionFromDBSQLiteFixture verifies the sqlite3 query path against a real database file,
// skipping when the environment has no trusted sqlite3 binary.
func TestExtractSessionFromDBSQLiteFixture(t *testing.T) {
	sqlitePath := ""
	for _, candidate := range sqlite3Candidates {
		if _, err := os.Stat(candidate); err == nil {
			sqlitePath = candidate
			break
		}
	}
	if sqlitePath == "" {
		t.Skip("sqlite3 CLI not installed; skipping real-database fixture test")
	}

	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	payload := `{"apiKey":"sqlite-token-321","email":"sqlite@example.com","name":"SQLite User"}`
	statement := fmt.Sprintf("CREATE TABLE ItemTable (key TEXT, value TEXT); INSERT INTO ItemTable VALUES ('antigravityAuthStatus', '%s');", payload)
	if out, err := exec.Command(sqlitePath, "--", dbPath, statement).CombinedOutput(); err != nil {
		t.Fatalf("creating sqlite fixture: %v (%s)", err, strings.TrimSpace(string(out)))
	}

	sess, err := ExtractSessionFromDB(dbPath)
	if err != nil {
		t.Fatalf("ExtractSessionFromDB failed: %v", err)
	}
	if sess.APIKey != "sqlite-token-321" {
		t.Errorf("expected sqlite token, got %q", maskSecret(sess.APIKey))
	}
	if sess.Email != "sqlite@example.com" {
		t.Errorf("expected sqlite@example.com, got %q", sess.Email)
	}
}
