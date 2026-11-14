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

// Account represents a configured Google Antigravity account with associated credentials and companion project.
type Account struct {
	// Email is the primary Google user email address associated with this account.
	Email string `json:"email"`

	// RefreshToken is the long-lived OAuth refresh token used to mint fresh access tokens.
	RefreshToken string `json:"refresh_token"`

	// ProjectID is the Google Cloud Code companion project identifier hosting quota resources.
	ProjectID string `json:"project_id"`

	// AddedAt records the RFC 3339 timestamp when this account was authenticated.
	AddedAt string `json:"added_at"`

	// Disabled indicates whether this account has been manually or automatically removed from rotation.
	Disabled bool `json:"disabled,omitzero"`

	// LastError records the most recent fatal authentication or authorization error message.
	LastError string `json:"last_error,omitempty"`
}

// String returns a redacted representation of Account to prevent token leakage in logs.
func (a *Account) String() string {
	if a == nil {
		return "<nil>"
	}
	refMasked := "<empty>"
	if len(a.RefreshToken) > 8 {
		refMasked = a.RefreshToken[:4] + "..." + a.RefreshToken[len(a.RefreshToken)-4:]
	} else if a.RefreshToken != "" {
		refMasked = "***"
	}
	return fmt.Sprintf("Account{Email:%s, ProjectID:%s, RefreshToken:%s, Disabled:%t, AddedAt:%s}",
		a.Email, a.ProjectID, refMasked, a.Disabled, a.AddedAt)
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
		if acc == nil || strings.TrimSpace(acc.Email) == "" || strings.TrimSpace(acc.RefreshToken) == "" {
			continue
		}
		valid = append(valid, acc)
	}

	return valid, nil
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

// Upsert adds a new account or updates an existing account matching the same email (case-insensitive).
// Returns true if a new account was inserted, or false if an existing account was updated.
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
		return strings.EqualFold(a.Email, account.Email)
	})

	isNew := index == -1
	if isNew {
		if account.AddedAt == "" {
			account.AddedAt = time.Now().UTC().Format(time.RFC3339)
		}
		accounts = append(accounts, account)
	} else {
		// Preserve original AddedAt date when updating credentials
		if account.AddedAt == "" {
			account.AddedAt = accounts[index].AddedAt
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
// Used when Google invalidates a refresh token or revokes access.
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
