// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// AntigravityAuthPort defines the local HTTP port exposed by the running Antigravity IDE.
const AntigravityAuthPort = 9092

// AgyTokenPathEnv names an explicit Antigravity CLI (`agy`) token file, mirroring
// ANTIGRAVITY_DB_PATH for the IDE database. It takes precedence over the auto-discovered locations.
const AgyTokenPathEnv = "AGW_AGY_TOKEN_PATH"

// AgyKeyringEnv opts in to a read-only OS keyring lookup for the Antigravity CLI token. The lookup is
// disabled by default because it shells out to platform credential tools and reads a store shared with
// other applications.
const AgyKeyringEnv = "AGW_AGY_KEYRING"

const (
	// maxLocalTokenLength bounds an accepted imported bearer token so a malformed database page
	// cannot smuggle an arbitrarily large payload into the credential store.
	maxLocalTokenLength = 4096

	// maxLocalResponseBytes bounds how much of the Antigravity local listener page is read while
	// looking for the base64 chatParams payload.
	maxLocalResponseBytes = 2 << 20

	// authStatusWindowBytes bounds how far past the `antigravityAuthStatus` key the binary scanner looks
	// for the opening brace of the JSON payload.
	authStatusWindowBytes = 8192

	// maxAgyTokenBytes bounds an Antigravity CLI token file read.
	maxAgyTokenBytes = 64 << 10
)

// sqlite3Candidates lists the absolute sqlite3 locations probed on POSIX systems. Absolute paths
// are used deliberately: resolving the executable through PATH would let any earlier directory
// shadow the binary that is handed the credential database.
var sqlite3Candidates = []string{"/usr/bin/sqlite3", "/bin/sqlite3", "/usr/local/bin/sqlite3", "/opt/homebrew/bin/sqlite3"}

// keyringTokenArgs lists the read-only OS keyring lookups that can expose the Antigravity CLI secret,
// ordered per platform. Both tools are invoked through an absolute path so PATH shadowing cannot
// intercept the lookup, and neither can mutate the keyring.
var keyringTokenArgs = []struct {
	// tool is the absolute path of the platform credential helper.
	tool string

	// args are the read-only lookup arguments passed to the helper.
	args []string
}{
	{tool: "/usr/bin/security", args: []string{"find-generic-password", "-s", "gemini:antigravity", "-w"}},
	{tool: "/usr/bin/secret-tool", args: []string{"lookup", "service", "gemini:antigravity"}},
}

// fallbackCompanionProject is the generic Cloud Code companion project used when upstream refuses to
// allocate or report one for an account.
const fallbackCompanionProject = "rising-fact-p41fc"

// LocalSession represents authentication credentials extracted from a local Antigravity installation.
type LocalSession struct {
	// APIKey is the active Bearer token or OAuth access token.
	APIKey string `json:"apiKey"`

	// Email is the Google account email address.
	Email string `json:"email"`

	// Name is the display name of the authenticated user.
	Name string `json:"name,omitempty"`
}

// String returns a redacted representation of LocalSession to prevent credential leakage in logs.
func (s *LocalSession) String() string {
	if s == nil {
		return "<nil>"
	}
	return fmt.Sprintf("LocalSession{Email:%s, Name:%s, APIKey:%s}",
		s.Email, s.Name, maskSecret(s.APIKey))
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
	fi, err := os.Stat(dbPath)
	if err != nil {
		return nil, fmt.Errorf("antigravity database not accessible: %w", err)
	}

	// Method 1: query the database with a trusted absolute sqlite3 binary when one is installed.
	for _, sqlitePath := range sqlite3Candidates {
		if _, err := os.Stat(sqlitePath); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		cmd := exec.CommandContext(ctx, sqlitePath, "--", dbPath, "SELECT value FROM ItemTable WHERE key = 'antigravityAuthStatus' LIMIT 1;")
		out, cmdErr := cmd.Output()
		cancel()
		if cmdErr != nil || len(out) == 0 {
			continue
		}

		var session LocalSession
		if jsonErr := json.Unmarshal(bytes.TrimSpace(out), &session); jsonErr != nil {
			continue
		}
		if validateErr := validateLocalSession(&session); validateErr == nil {
			return &session, nil
		}
	}

	// Method 2: Binary byte-scanning fallback (zero-dependency static binary compatibility)
	// Bound fallback reading to 50 MB to prevent excessive memory usage on large databases.
	const maxDBFallbackBytes = 50 << 20
	if fi.Size() > maxDBFallbackBytes {
		return nil, fmt.Errorf("database file %s exceeds fallback scanning size limit (%d bytes)", dbPath, fi.Size())
	}

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

	// Search for the opening bracket '{' following the key within the bounded scanner window.
	windowStart := idx + len(target)
	windowEnd := min(len(data), windowStart+authStatusWindowBytes)
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
	if err := validateLocalSession(&sess); err != nil {
		return nil, err
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

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxLocalResponseBytes))
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

	if err := validateLocalSession(&sess); err != nil {
		return nil, err
	}

	return &sess, nil
}

// validateLocalSession rejects payloads that cannot be a genuine desktop bearer token: the token is
// trimmed, must be present, must stay within a sane length, and must not contain whitespace or
// control characters that would corrupt the Authorization header on replay. Errors never echo the
// credential value, only the reason.
func validateLocalSession(sess *LocalSession) error {
	if sess == nil {
		return errors.New("local session payload is empty")
	}

	sess.APIKey = strings.TrimSpace(sess.APIKey)
	sess.Email = strings.TrimSpace(sess.Email)
	sess.Name = strings.TrimSpace(sess.Name)

	if sess.APIKey == "" {
		return errors.New("local session payload is missing apiKey")
	}
	if len(sess.APIKey) > maxLocalTokenLength {
		return fmt.Errorf("local session apiKey exceeds the %d byte limit", maxLocalTokenLength)
	}

	for _, r := range sess.APIKey {
		if r <= ' ' || r == 0x7f {
			return errors.New("local session apiKey contains whitespace or control characters")
		}
	}

	return nil
}

// AgyTokenCandidates returns the well-known Antigravity CLI (`agy`) token file locations for the
// current platform, ordered from most to least specific. An explicit AGW_AGY_TOKEN_PATH override is
// always tried first so operators and tests can point at a non-standard installation.
func AgyTokenCandidates() []string {
	var candidates []string
	if custom := strings.TrimSpace(os.Getenv(AgyTokenPathEnv)); custom != "" {
		candidates = append(candidates, custom)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return candidates
	}

	candidates = append(candidates,
		filepath.Join(home, ".config", "antigravity", "acp_token.json"),
		filepath.Join(home, ".antigravity", "acp_token.json"),
		filepath.Join(home, ".gemini", "acp_token.json"),
		filepath.Join(home, ".gemini", "antigravity-oauth-token"),
	)

	switch runtime.GOOS {
	case "darwin":
		candidates = append(candidates, filepath.Join(home, "Library", "Application Support", "Antigravity", "acp_token.json"))
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		candidates = append(candidates, filepath.Join(appData, "Antigravity", "acp_token.json"))
	}

	return candidates
}

// ExtractSessionFromAgyToken reads an Antigravity CLI (`agy`) token file and normalizes it into a
// LocalSession. Both the flat shape used by `acp_token.json` and the nested OAuth credential shape
// used by `antigravity-oauth-token` are accepted. Errors name the file and the reason only; the token
// value is never echoed.
func ExtractSessionFromAgyToken(path string) (*LocalSession, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("antigravity cli token not accessible: %w", err)
	}
	if fi.IsDir() {
		return nil, fmt.Errorf("antigravity cli token path %s is a directory", path)
	}
	if fi.Size() > maxAgyTokenBytes {
		return nil, fmt.Errorf("antigravity cli token file %s exceeds the %d byte limit", path, maxAgyTokenBytes)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading antigravity cli token %s: %w", path, err)
	}

	sess := &LocalSession{}
	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &payload); err != nil {
		// Some CLI builds persist the bearer token as a bare string instead of a JSON document.
		sess.APIKey = strings.TrimSpace(string(data))
	} else {
		sess.APIKey = firstStringValue(payload, "apiKey", "access_token", "accessToken", "token")
		sess.Email = firstStringValue(payload, "email", "userEmail")
		sess.Name = firstStringValue(payload, "name", "displayName")

		if sess.APIKey == "" {
			// Nested credential documents keep the bearer token inside a session/auth object.
			for _, key := range []string{"session", "auth", "credentials", "token"} {
				nested, ok := payload[key].(map[string]any)
				if !ok {
					continue
				}
				sess.APIKey = firstStringValue(nested, "apiKey", "access_token", "accessToken", "token")
				sess.Email = cmp.Or(sess.Email, firstStringValue(nested, "email", "userEmail"))
				sess.Name = cmp.Or(sess.Name, firstStringValue(nested, "name", "displayName"))
				if sess.APIKey != "" {
					break
				}
			}
		}
	}

	if err := validateLocalSession(sess); err != nil {
		return nil, fmt.Errorf("validating antigravity cli token %s: %w", path, err)
	}

	return sess, nil
}

// firstStringValue returns the first non-empty trimmed string value among the candidate keys.
func firstStringValue(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		val, ok := payload[key].(string)
		if !ok {
			continue
		}
		if trimmed := strings.TrimSpace(val); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// ExtractSessionFromKeyring reads the Antigravity CLI bearer token from the OS keyring through a
// read-only platform credential helper. The lookup is opt-in via AGW_AGY_KEYRING=1 because the
// keyring is shared with other applications and is not part of the standard detection surface.
func ExtractSessionFromKeyring(ctx context.Context) (*LocalSession, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(AgyKeyringEnv))) {
	case "1", "true", "yes":
	default:
		return nil, fmt.Errorf("keyring lookup disabled; set %s=1 to opt in", AgyKeyringEnv)
	}

	for _, lookup := range keyringTokenArgs {
		if _, err := os.Stat(lookup.tool); err != nil {
			continue
		}

		lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		out, err := exec.CommandContext(lookupCtx, lookup.tool, lookup.args...).Output()
		cancel()
		if err != nil || len(out) == 0 {
			continue
		}

		sess := &LocalSession{APIKey: strings.TrimSpace(string(out))}
		if err := validateLocalSession(sess); err != nil {
			continue
		}
		return sess, nil
	}

	return nil, errors.New("no antigravity cli token found in the os keyring")
}

// DetectLocalSession attempts all detection vectors (IDE database first, then Antigravity CLI token
// files, the opt-in keyring, and finally the IDE local HTTP port) to discover active credentials.
func DetectLocalSession(ctx context.Context) (*LocalSession, error) {
	dbPath, err := DetectAntigravityDBPath()
	if err == nil {
		if sess, dbErr := ExtractSessionFromDB(dbPath); dbErr == nil && sess != nil {
			return sess, nil
		}
	}

	for _, tokenPath := range AgyTokenCandidates() {
		if sess, tokenErr := ExtractSessionFromAgyToken(tokenPath); tokenErr == nil && sess != nil {
			return sess, nil
		}
	}

	if sess, keyringErr := ExtractSessionFromKeyring(ctx); keyringErr == nil && sess != nil {
		return sess, nil
	}

	if sess, httpErr := ExtractSessionFromHTTP(ctx, AntigravityAuthPort); httpErr == nil && sess != nil {
		return sess, nil
	}

	return nil, errors.New("no active Antigravity session found via database, CLI token, keyring, or local HTTP port")
}

// ImportLocalSession auto-discovers companion project ID and registers the local session into the account store.
func ImportLocalSession(ctx context.Context, cfg *config.Config, store *Store) (*Account, error) {
	sess, err := DetectLocalSession(ctx)
	if err != nil {
		return nil, err
	}

	email := sess.Email
	userID := ""
	// Resolve the immutable Google identity so re-imports after an email change keep updating the same
	// stored account instead of appending a duplicate. The session payload stays authoritative for the
	// email when userinfo is unreachable, because the address is display metadata only.
	if identity, idErr := GetUserIdentity(ctx, cfg, sess.APIKey); idErr == nil {
		userID = identity.ID
		if email == "" {
			email = identity.Email
		}
	}
	if email == "" {
		// Email is cosmetic for diagnostics; fall back to a placeholder when userinfo is unreachable.
		email = "antigravity-local@google.com"
	}

	projectID, tier, discErr := DiscoverProjectAndTier(ctx, cfg, sess.APIKey)
	if discErr != nil || projectID == "" {
		// Silent fallbacks masked upstream loadCodeAssist failures: an operator could not tell an attached
		// companion project from the generic placeholder, so the degradation is logged explicitly.
		slog.Warn("companion project discovery failed; importing desktop session with fallback project",
			"error", discErr,
			"project", fallbackCompanionProject,
			"email", email,
		)
		projectID = fallbackCompanionProject
	}

	now := time.Now().UTC().Format(time.RFC3339)
	account := &Account{
		Email:            email,
		UserID:           userID,
		AccessToken:      sess.APIKey,
		CredentialSource: CredentialSourceLocalDesktop,
		ProjectID:        projectID,
		Tier:             tier,
		AddedAt:          now,
	}

	if store != nil {
		// Never downgrade a refreshable OAuth account to a short-lived desktop token: the refresh
		// token cannot be recovered once overwritten, while the desktop token expires within hours.
		if existing, loadErr := store.Load(); loadErr == nil {
			if idx := slices.IndexFunc(existing, func(a *Account) bool {
				return a.Matches(account)
			}); idx != -1 && existing[idx].RefreshToken != "" {
				return existing[idx], nil
			}
		}

		if _, saveErr := store.Upsert(account); saveErr != nil {
			return nil, fmt.Errorf("saving imported account to store: %w", saveErr)
		}
	}

	return account, nil
}
