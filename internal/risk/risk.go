// Package risk records local, privacy-preserving risk telemetry for the gateway. It stores bounded
// snapshots of per-account traffic counters and the active safety posture so an operator can see whether a
// credential is drifting toward the abuse heuristics upstream enforces, without ever writing prompts,
// tokens, or full email addresses to disk.
package risk

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// FileVersion identifies the on-disk telemetry schema version.
	FileVersion = 1

	// MaxSnapshots bounds how many historical snapshots the file retains. Older entries are dropped so an
	// unattended daemon cannot grow the file without limit.
	MaxSnapshots = 24

	// dirPerm matches the configuration directory permissions.
	dirPerm os.FileMode = 0o700

	// filePerm keeps telemetry owner-only, like every other file under the configuration root.
	filePerm os.FileMode = 0o600
)

// Account captures one account's traffic counters and suspension state at capture time. Identity is
// stored as an opaque credential digest plus a masked email so the file stays useful without becoming a
// directory of personal data.
type Account struct {
	// Credential is the opaque connection-pool digest for the account credential.
	Credential string `json:"credential,omitempty"`

	// Email is the masked account address, for example `a***@example.com`.
	Email string `json:"email,omitempty"`

	// Suspended reports whether upstream returned a terminal suspension verdict for the account.
	Suspended bool `json:"suspended,omitzero"`

	// SuspendKind classifies the suspension when one was recorded.
	SuspendKind string `json:"suspend_kind,omitempty"`

	// Requests is the cumulative number of requests dispatched to the account.
	Requests int64 `json:"requests,omitzero"`

	// RateLimits is the cumulative number of HTTP 429 responses received by the account.
	RateLimits int64 `json:"rate_limits,omitzero"`

	// PromptRejections is the bare RESOURCE_EXHAUSTED count inside the current guard window.
	PromptRejections int `json:"prompt_rejections,omitzero"`

	// LastUpstreamOK is the RFC 3339 timestamp of the last successful upstream completion.
	LastUpstreamOK string `json:"last_upstream_ok,omitempty"`

	// LastError is the last recorded operational error for the account.
	LastError string `json:"last_error,omitempty"`
}

// Snapshot captures the gateway risk posture and per-account counters at one moment in time.
type Snapshot struct {
	// CapturedAt is the RFC 3339 timestamp of the capture.
	CapturedAt string `json:"captured_at"`

	// Profile is the resolved operating profile at capture time.
	Profile string `json:"profile"`

	// RotationMode is the resolved rotation mode at capture time.
	RotationMode string `json:"rotation_mode"`

	// MaxInFlight is the per-credential concurrency ceiling, zero when unbounded.
	MaxInFlight int `json:"max_in_flight,omitzero"`

	// PromptRejectionLimit is the bare-429 threshold that parks an account, zero when the guard is off.
	PromptRejectionLimit int `json:"prompt_rejection_limit,omitzero"`

	// Accounts lists the per-account counters captured with this snapshot.
	Accounts []Account `json:"accounts,omitempty"`
}

// File is the persisted telemetry document.
type File struct {
	// Version is the schema version written with the document.
	Version int `json:"version"`

	// Snapshots holds the retained snapshots, newest last.
	Snapshots []Snapshot `json:"snapshots"`
}

// Store provides thread-safe, atomic persistence for the telemetry file.
type Store struct {
	// mu serializes read-modify-write cycles so concurrent captures cannot drop each other.
	mu sync.Mutex

	// path is the telemetry file location on disk.
	path string
}

// NewStore initializes a telemetry store targeting path.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Path returns the telemetry file location managed by the store.
func (s *Store) Path() string {
	return s.path
}

// Load reads the telemetry document. A missing file yields an empty document with the current schema
// version, so the first capture never requires an explicit initialization step.
func (s *Store) Load() (*File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.loadUnlocked()
}

// loadUnlocked reads the document while the caller holds the lock.
func (s *Store) loadUnlocked() (*File, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &File{Version: FileVersion}, nil
		}
		return nil, fmt.Errorf("reading risk telemetry %s: %w", s.path, err)
	}

	var document File
	if unmarshalErr := json.Unmarshal(data, &document); unmarshalErr != nil {
		return nil, fmt.Errorf("parsing risk telemetry %s: %w", s.path, unmarshalErr)
	}
	if document.Version == 0 {
		document.Version = FileVersion
	}
	return &document, nil
}

// Append records one snapshot, dropping the oldest entries beyond MaxSnapshots, and rewrites the document
// atomically with owner-only permissions.
//
// A corrupt or unreadable document is reported instead of silently replaced: telemetry is diagnostic data,
// and quietly discarding an operator's history would hide the failure that produced it.
func (s *Store) Append(snapshot Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	document, err := s.loadUnlocked()
	if err != nil {
		return err
	}

	document.Version = FileVersion
	document.Snapshots = append(document.Snapshots, snapshot)
	if len(document.Snapshots) > MaxSnapshots {
		document.Snapshots = document.Snapshots[len(document.Snapshots)-MaxSnapshots:]
	}

	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling risk telemetry: %w", err)
	}
	data = append(data, '\n')

	if err := writeFileAtomic(s.path, data); err != nil {
		return fmt.Errorf("persisting risk telemetry to %s: %w", s.path, err)
	}
	return nil
}

// MaskEmail reduces an address to `first-character***@domain` so telemetry can attribute counters to an
// account without storing the full identity.
func MaskEmail(email string) string {
	email = strings.TrimSpace(email)
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" {
		return maskToken(email)
	}

	runes := []rune(local)
	if len(runes) <= 1 {
		return "*@" + domain
	}
	return string(runes[:1]) + "***@" + domain
}

// emailPattern matches an email-like token inside free text so quoted addresses can be masked before they
// reach the telemetry file.
// The pattern intentionally accepts a bare host as well as a dotted TLD: upstream messages are free text,
// and missing an address there would defeat the masking invariant. A token without a local part before the
// @ never matches, so ordinary prose is untouched.
var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+`)

// RedactEmails masks every email-like token in free text. Upstream messages occasionally quote the account
// address, and the telemetry file must never become a directory of identities just because it stored an
// error string verbatim.
func RedactEmails(text string) string {
	if text == "" {
		return ""
	}
	return emailPattern.ReplaceAllStringFunc(text, MaskEmail)
}

// maskToken masks a value that is not an email address by keeping its first rune only.
func maskToken(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) == 0 {
		return ""
	}
	return string(runes[:1]) + "***"
}

// writeFileAtomic replaces path with data using a temporary file in the same directory, chmodded to 0600
// before any payload is written and flushed before the rename, so an interrupted write cannot corrupt or
// leak the telemetry file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("ensuring directory %s exists: %w", dir, err)
	}

	tempFile, err := os.CreateTemp(dir, ".risk-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temporary telemetry file in %s: %w", dir, err)
	}
	tempPath := tempFile.Name()

	cleanup := func() {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
	}

	if err := tempFile.Chmod(filePerm); err != nil {
		cleanup()
		return fmt.Errorf("setting telemetry file permissions: %w", err)
	}
	if _, err := tempFile.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("writing telemetry payload: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("flushing telemetry payload: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("closing temporary telemetry file: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replacing telemetry file %s: %w", path, err)
	}
	return nil
}

// TimestampNow formats now as the RFC 3339 timestamp used by snapshots.
func TimestampNow(now time.Time) string {
	return now.UTC().Format(time.RFC3339)
}
