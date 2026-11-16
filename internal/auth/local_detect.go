// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// AntigravityAuthPort defines the local HTTP port exposed by the running Antigravity IDE.
const AntigravityAuthPort = 9092

// LocalSession represents authentication credentials extracted from a local Antigravity installation.
type LocalSession struct {
	// APIKey is the active Bearer token or OAuth access token.
	APIKey string `json:"apiKey"`

	// Email is the Google account email address.
	Email string `json:"email"`

	// Name is the display name of the authenticated user.
	Name string `json:"name,omitempty"`
}

var chatParamsRegex = regexp.MustCompile(`window\.chatParams\s*=\s*'([^']+)'`)

// DetectAntigravityDBPath returns the platform-specific path to Antigravity's state.vscdb file.
func DetectAntigravityDBPath() (string, error) {
	if custom := os.Getenv("ANTIGRAVITY_DB_PATH"); custom != "" {
		return custom, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving user home directory: %w", err)
	}

	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Antigravity", "User", "globalStorage", "state.vscdb"), nil
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "Antigravity", "User", "globalStorage", "state.vscdb"), nil
	default:
		// Linux, FreeBSD, etc.
		configDir := os.Getenv("XDG_CONFIG_HOME")
		if configDir == "" {
			configDir = filepath.Join(home, ".config")
		}
		return filepath.Join(configDir, "Antigravity", "User", "globalStorage", "state.vscdb"), nil
	}
}

// ExtractSessionFromDB queries or parses Antigravity's SQLite database to retrieve the active session.
func ExtractSessionFromDB(dbPath string) (*LocalSession, error) {
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("antigravity database not accessible: %w", err)
	}

	// Method 1: Try system sqlite3 CLI if available in PATH
	if sqlitePath, err := exec.LookPath("sqlite3"); err == nil && sqlitePath != "" {
		cmd := exec.Command(sqlitePath, dbPath, "SELECT value FROM ItemTable WHERE key = 'antigravityAuthStatus' LIMIT 1;")
		out, cmdErr := cmd.Output()
		if cmdErr == nil && len(out) > 0 {
			var session LocalSession
			if jsonErr := json.Unmarshal(bytes.TrimSpace(out), &session); jsonErr == nil && session.APIKey != "" {
				return &session, nil
			}
		}
	}

	// Method 2: Binary byte-scanning fallback (zero-dependency static binary compatibility)
	fileBytes, err := os.ReadFile(dbPath)
	if err != nil {
		return nil, fmt.Errorf("reading database file %s: %w", dbPath, err)
	}

	session, err := ExtractSessionFromBytes(fileBytes)
	if err != nil {
		return nil, fmt.Errorf("scanning database file %s: %w", dbPath, err)
	}
	return session, nil
}

// ExtractSessionFromBytes scans raw binary bytes of state.vscdb for the antigravityAuthStatus JSON payload.
func ExtractSessionFromBytes(data []byte) (*LocalSession, error) {
	target := []byte("antigravityAuthStatus")
	idx := bytes.Index(data, target)
	if idx == -1 {
		return nil, errors.New("antigravityAuthStatus key not found in database bytes")
	}

	// Search for opening bracket '{' following the key within 512 bytes
	windowStart := idx + len(target)
	windowEnd := min(len(data), windowStart+8192)
	window := data[windowStart:windowEnd]

	openIdx := bytes.IndexByte(window, '{')
	if openIdx == -1 {
		return nil, errors.New("malformed auth status entry: opening brace not found")
	}

	// Decode exactly one JSON object starting at the opening brace: trailing SQLite
	// page bytes after the payload are ignored by the streaming decoder.
	slice := window[openIdx:]
	var sess LocalSession
	if err := json.NewDecoder(bytes.NewReader(slice)).Decode(&sess); err != nil {
		return nil, fmt.Errorf("decoding auth status JSON payload: %w", err)
	}
	if sess.APIKey == "" {
		return nil, errors.New("auth status JSON payload is missing apiKey")
	}

	return &sess, nil
}

// ExtractSessionFromHTTP attempts to connect to Antigravity's local HTTP listener on port 9092.
func ExtractSessionFromHTTP(ctx context.Context, port int) (*LocalSession, error) {
	if port <= 0 {
		port = AntigravityAuthPort
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating http request: %w", err)
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connecting to antigravity local listener on port %d: %w", port, err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response from port %d: %w", port, err)
	}

	match := chatParamsRegex.FindSubmatch(bodyBytes)
	if len(match) < 2 {
		return nil, errors.New("chatParams not found in antigravity page HTML")
	}

	decoded, err := base64.StdEncoding.DecodeString(string(match[1]))
	if err != nil {
		return nil, fmt.Errorf("decoding base64 chatParams: %w", err)
	}

	var sess LocalSession
	if err := json.Unmarshal(decoded, &sess); err != nil {
		return nil, fmt.Errorf("parsing chatParams JSON: %w", err)
	}

	if sess.APIKey == "" {
		return nil, errors.New("chatParams missing apiKey")
	}

	return &sess, nil
}

// DetectLocalSession attempts all detection vectors (DB first, HTTP port second) to discover active credentials.
func DetectLocalSession(ctx context.Context) (*LocalSession, error) {
	dbPath, err := DetectAntigravityDBPath()
	if err == nil {
		if sess, dbErr := ExtractSessionFromDB(dbPath); dbErr == nil && sess != nil {
			return sess, nil
		}
	}

	if sess, httpErr := ExtractSessionFromHTTP(ctx, AntigravityAuthPort); httpErr == nil && sess != nil {
		return sess, nil
	}

	return nil, errors.New("no active Antigravity session found via database or local HTTP port")
}

// ImportLocalSession auto-discovers companion project ID and registers the local session into the account store.
func ImportLocalSession(ctx context.Context, cfg *config.Config, store *Store) (*Account, error) {
	sess, err := DetectLocalSession(ctx)
	if err != nil {
		return nil, err
	}

	email := sess.Email
	if email == "" {
		// Email is cosmetic for diagnostics; fall back to a placeholder when userinfo is unreachable.
		email, _ = GetUserEmail(ctx, cfg, sess.APIKey)
		if email == "" {
			email = "antigravity-local@google.com"
		}
	}

	projectID, discErr := DiscoverProject(ctx, cfg, sess.APIKey)
	if discErr != nil || projectID == "" {
		projectID = "rising-fact-p41fc" // Default fallback project
	}

	now := time.Now().UTC().Format(time.RFC3339)
	account := &Account{
		Email:        email,
		RefreshToken: sess.APIKey, // Store token for local bearer access
		ProjectID:    projectID,
		AddedAt:      now,
	}

	if store != nil {
		if _, saveErr := store.Upsert(account); saveErr != nil {
			return nil, fmt.Errorf("saving imported account to store: %w", saveErr)
		}
	}

	return account, nil
}
