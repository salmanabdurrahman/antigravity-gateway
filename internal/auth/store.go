// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// CredentialSourceLocalDesktop marks accounts whose credential was imported from a running
// Antigravity desktop session instead of an OAuth PKCE login. Such accounts hold a short-lived
// bearer access token that Google's token endpoint cannot exchange for a new one.
const CredentialSourceLocalDesktop = "local_desktop"

// Account represents a configured Google Antigravity account with associated credentials and companion project.
type Account struct {
	// Email is the primary Google user email address associated with this account. It is display and
	// lookup metadata only: Google users can change or alias it, so it is never the identity key.
	Email string `json:"email"`

	// UserID is Google's immutable account subject identifier (`sub` from the userinfo endpoint). It
	// stays stable across email changes and aliases, so stored accounts, duplicate detection, and
	// cooldown continuity key on it whenever it is known. It is empty for legacy files written before
	// the identifier was captured and is backfilled on the next login or local import.
	UserID string `json:"user_id,omitempty"`

	// RefreshToken is the long-lived OAuth refresh token used to mint fresh access tokens.
	RefreshToken string `json:"refresh_token,omitempty"`

	// AccessToken holds a short-lived Bearer token imported from a desktop session. It is kept
	// separate from RefreshToken because it cannot be exchanged at the OAuth token endpoint.
	AccessToken string `json:"access_token,omitempty"`

	// CredentialSource records how the credentials were obtained (empty for OAuth PKCE logins,
	// CredentialSourceLocalDesktop for imported desktop sessions).
	CredentialSource string `json:"credential_source,omitempty"`

	// ProjectID is the Google Cloud Code companion project identifier hosting quota resources.
	ProjectID string `json:"project_id"`

	// Tier records the normalized subscription tier (personal, business, enterprise) resolved during
	// login. It drives upstream endpoint ordering because paid tiers are served by the production
	// Cloud Code endpoint while consumer tiers use the daily endpoint first.
	Tier string `json:"tier,omitempty"`

	// AddedAt records the RFC 3339 timestamp when this account was authenticated.
	AddedAt string `json:"added_at"`

	// Disabled indicates whether this account has been manually or automatically removed from rotation.
	Disabled bool `json:"disabled,omitzero"`

	// LastError records the most recent fatal authentication or authorization error message.
	LastError string `json:"last_error,omitempty"`

	// SuspendedAt records the RFC 3339 timestamp of the terminal upstream suspension verdict. It is
	// empty for accounts that were disabled for other reasons.
	SuspendedAt string `json:"suspended_at,omitempty"`

	// SuspendKind classifies the terminal suspension (for example tos_violation or failed_precondition)
	// so operators and clients can distinguish it from an ordinary disabled account.
	SuspendKind string `json:"suspend_kind,omitempty"`

	// SuspendReason stores the upstream explanation for the suspension, kept separate from LastError
	// so a later transient error cannot erase the verdict Google returned.
	SuspendReason string `json:"suspend_reason,omitempty"`

	// AppealURL stores the validated operator appeal link upstream returned with the verdict. It is
	// empty when upstream supplied none or the link failed validation.
	AppealURL string `json:"appeal_url,omitempty"`
}

// Suspended reports whether upstream returned a terminal suspension verdict for this account.
func (a *Account) Suspended() bool {
	return a != nil && strings.TrimSpace(a.SuspendKind) != ""
}

// CredentialKey returns the opaque connection-pool key for this account's Google credential. The key
// is a truncated digest, so callers may use it in maps and logs without exposing credential material.
func (a *Account) CredentialKey() string {
	if a == nil {
		return ""
	}
	return config.CredentialKey(a.RefreshToken, a.AccessToken, a.Email)
}

// HasCredentials reports whether the account holds at least one usable credential: an OAuth refresh
// token or an imported desktop access token.
func (a *Account) HasCredentials() bool {
	if a == nil {
		return false
	}
	return strings.TrimSpace(a.RefreshToken) != "" || strings.TrimSpace(a.AccessToken) != ""
}

// Key returns the account's stable identity key: the immutable Google user ID when known, otherwise
// the lowercased email address of a legacy entry that predates identity capture.
func (a *Account) Key() string {
	if a == nil {
		return ""
	}
	if id := strings.TrimSpace(a.UserID); id != "" {
		return id
	}
	return strings.ToLower(strings.TrimSpace(a.Email))
}

// Matches reports whether the account represents the same Google identity as another account.
// The immutable UserID is authoritative whenever both sides carry one: an email change or alias updates
// the existing entry instead of creating a duplicate, while two distinct subjects that happen to share
// an address stay separate. Email is only the fallback for legacy entries without an identifier.
func (a *Account) Matches(other *Account) bool {
	if a == nil || other == nil {
		return false
	}
	if a.UserID != "" && other.UserID != "" {
		return a.UserID == other.UserID
	}
	return a.Email != "" && strings.EqualFold(a.Email, other.Email)
}

// IsLocalDesktopSession reports whether the account credentials came from a local desktop import,
// which can never be refreshed and must be re-imported once the bearer token expires.
func (a *Account) IsLocalDesktopSession() bool {
	return a != nil && a.CredentialSource == CredentialSourceLocalDesktop
}

// String returns a redacted representation of Account to prevent token leakage in logs.
func (a *Account) String() string {
	if a == nil {
		return "<nil>"
	}
	return fmt.Sprintf("Account{Email:%s, ProjectID:%s, RefreshToken:%s, AccessToken:%s, Source:%s, Disabled:%t, AddedAt:%s}",
		a.Email, a.ProjectID, maskSecret(a.RefreshToken), maskSecret(a.AccessToken), a.CredentialSource, a.Disabled, a.AddedAt)
}

// maskSecret renders a credential with only its first and last four characters, so logs and error
// messages can identify a token without leaking it.
func maskSecret(secret string) string {
	switch {
	case secret == "":
		return "<empty>"
	case len(secret) > 8:
		return secret[:4] + "..." + secret[len(secret)-4:]
	default:
		return "***"
	}
}

// accountsFilePayload defines the JSON file structure used to persist accounts to disk.
type accountsFilePayload struct {
	// Version tracks the schema version of the stored accounts file format.
	Version int `json:"version"`

	// Accounts lists all registered Google accounts.
	Accounts []*Account `json:"accounts"`
}

// Store provides thread-safe management and atomic persistence for Antigravity accounts.
type Store struct {
	// mu guards concurrent access to the account store file on disk.
	mu sync.RWMutex

	// path is the target filesystem location for the accounts JSON file.
	path string
}

// NewStore initializes an account store targeted at the given file path.
func NewStore(path string) *Store {
	return &Store{
		path: path,
	}
}

// Path returns the destination file path managed by the store.
func (s *Store) Path() string {
	return s.path
}

// Load reads accounts from disk without modifying state. Returns an empty slice if the file does not exist.
func (s *Store) Load() ([]*Account, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.loadUnlocked()
}

// loadUnlocked reads and decodes the accounts file without acquiring the internal mutex.
// Callers must hold either a read or write lock prior to invocation.
func (s *Store) loadUnlocked() ([]*Account, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []*Account{}, nil
		}
		return nil, fmt.Errorf("reading accounts file %s: %w", s.path, err)
	}

	var payload accountsFilePayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, fmt.Errorf("parsing accounts json from %s: %w", s.path, err)
	}

	// Filter valid accounts
	var valid []*Account
	for _, acc := range payload.Accounts {
		if acc == nil || strings.TrimSpace(acc.Email) == "" || !acc.HasCredentials() {
			continue
		}
		valid = append(valid, acc)
	}

	return dedupeAccountsByUserID(valid), nil
}

// dedupeAccountsByUserID collapses legacy duplicates that describe the same immutable Google identity,
// for example an account stored once per pre-change email alias. Entries without a UserID are kept
// verbatim because email remains the only available identity for them.
func dedupeAccountsByUserID(accounts []*Account) []*Account {
	positions := make(map[string]int, len(accounts))
	out := make([]*Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc.UserID == "" {
			out = append(out, acc)
			continue
		}
		pos, exists := positions[acc.UserID]
		if !exists {
			positions[acc.UserID] = len(out)
			out = append(out, acc)
			continue
		}
		if preferAccount(acc, out[pos]) {
			out[pos] = acc
		}
	}
	return out
}

// preferAccount reports whether candidate should replace existing when both describe the same identity.
// A refreshable credential wins over a short-lived desktop token, and the most recently added entry wins
// otherwise, so a deduplicated account keeps the credentials that can still be refreshed.
func preferAccount(candidate, existing *Account) bool {
	if candidate == nil {
		return false
	}
	if existing == nil {
		return true
	}
	candidateRefreshable := strings.TrimSpace(candidate.RefreshToken) != ""
	existingRefreshable := strings.TrimSpace(existing.RefreshToken) != ""
	if candidateRefreshable != existingRefreshable {
		return candidateRefreshable
	}
	return candidate.AddedAt > existing.AddedAt
}

// Save atomically writes the account slice to disk with POSIX 0600 permissions.
func (s *Store) Save(accounts []*Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.saveUnlocked(accounts)
}

// saveUnlocked persists the account list to disk atomically with POSIX 0600 permissions
// without acquiring the internal mutex. Callers must hold a write lock prior to invocation.
func (s *Store) saveUnlocked(accounts []*Account) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, config.DirPerm); err != nil {
		return fmt.Errorf("ensuring accounts directory %s: %w", dir, err)
	}

	payload := accountsFilePayload{
		Version:  1,
		Accounts: accounts,
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling accounts payload: %w", err)
	}
	data = append(data, '\n')

	tempFile, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temporary accounts file in %s: %w", dir, err)
	}
	tempPath := tempFile.Name()

	defer func() {
		_ = os.Remove(tempPath)
	}()

	if err := tempFile.Chmod(config.FilePerm); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("setting file permissions 0600 on %s: %w", tempPath, err)
	}

	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("writing accounts data to %s: %w", tempPath, err)
	}

	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return fmt.Errorf("syncing accounts data on %s: %w", tempPath, err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("closing temporary file %s: %w", tempPath, err)
	}

	if err := os.Rename(tempPath, s.path); err != nil {
		return fmt.Errorf("atomic rename from %s to %s: %w", tempPath, s.path, err)
	}

	return nil
}

// Upsert adds a new account or updates an existing account describing the same Google identity.
// Identity is the immutable user ID when both sides carry one, falling back to the case-insensitive
// email address for legacy entries. Returns true if a new account was inserted, or false if an
// existing account was updated.
func (s *Store) Upsert(account *Account) (bool, error) {
	if account == nil {
		return false, errors.New("cannot upsert nil account")
	}
	if strings.TrimSpace(account.Email) == "" {
		return false, errors.New("cannot upsert account with empty email")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	accounts, err := s.loadUnlocked()
	if err != nil {
		return false, err
	}

	index := slices.IndexFunc(accounts, func(a *Account) bool {
		return a.Matches(account)
	})

	isNew := index == -1
	if isNew {
		if account.AddedAt == "" {
			account.AddedAt = time.Now().UTC().Format(time.RFC3339)
		}
		accounts = append(accounts, account)
	} else {
		// Preserve original AddedAt date when updating credentials, and backfill the immutable user ID
		// onto a legacy email-keyed entry so later logins match by identity instead of address.
		if account.AddedAt == "" {
			account.AddedAt = accounts[index].AddedAt
		}
		if account.UserID == "" {
			account.UserID = accounts[index].UserID
		}
		accounts[index] = account
	}

	if err := s.saveUnlocked(accounts); err != nil {
		return false, err
	}

	return isNew, nil
}

// Remove deletes an account matching the given email address (case-insensitive).
// Returns true if an account was removed, or false if not found.
func (s *Store) Remove(email string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	accounts, err := s.loadUnlocked()
	if err != nil {
		return false, err
	}

	initialLen := len(accounts)
	filtered := slices.DeleteFunc(accounts, func(a *Account) bool {
		return strings.EqualFold(a.Email, email)
	})

	if len(filtered) == initialLen {
		return false, nil
	}

	if err := s.saveUnlocked(filtered); err != nil {
		return false, err
	}

	return true, nil
}

// SetDisabled sets the disabled state and optional last error message on an account.
// Used when Google invalidates a refresh token or revokes access, and by the operator re-enable path.
// Re-enabling clears any suspension metadata so a stale verdict cannot outlive an appeal.
func (s *Store) SetDisabled(email string, disabled bool, lastError string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	accounts, err := s.loadUnlocked()
	if err != nil {
		return err
	}

	idx := slices.IndexFunc(accounts, func(a *Account) bool {
		return strings.EqualFold(a.Email, email)
	})

	if idx == -1 {
		return fmt.Errorf("account not found: %s", email)
	}

	accounts[idx].Disabled = disabled
	accounts[idx].LastError = lastError
	if !disabled {
		accounts[idx].SuspendedAt = ""
		accounts[idx].SuspendKind = ""
		accounts[idx].SuspendReason = ""
		accounts[idx].AppealURL = ""
	}

	return s.saveUnlocked(accounts)
}

// SetSuspended records a terminal upstream suspension verdict and removes the account from rotation.
// The verdict is persisted next to the disabled flag so operators can read the upstream reason and
// appeal link without re-contacting Google, and so a restart cannot silently resurrect the account.
func (s *Store) SetSuspended(email, kind, reason, appealURL string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	accounts, err := s.loadUnlocked()
	if err != nil {
		return err
	}

	idx := slices.IndexFunc(accounts, func(a *Account) bool {
		return strings.EqualFold(a.Email, email)
	})

	if idx == -1 {
		return fmt.Errorf("account not found: %s", email)
	}

	accounts[idx].Disabled = true
	accounts[idx].LastError = reason
	accounts[idx].SuspendedAt = time.Now().UTC().Format(time.RFC3339)
	accounts[idx].SuspendKind = kind
	accounts[idx].SuspendReason = reason
	accounts[idx].AppealURL = appealURL

	return s.saveUnlocked(accounts)
}

// ModTime returns the file modification time of accounts.json for daemon hot-reloading detection.
// Returns a zero time if the file does not exist.
func (s *Store) ModTime() (time.Time, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	fi, err := os.Stat(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return time.Time{}, nil
		}
		return time.Time{}, fmt.Errorf("stat accounts file %s: %w", s.path, err)
	}
	return fi.ModTime(), nil
}
