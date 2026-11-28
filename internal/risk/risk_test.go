// Package risk records local, privacy-preserving risk telemetry for the gateway.
package risk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// timeFromIndex returns a deterministic timestamp offset by i minutes, used to order snapshots.
func timeFromIndex(i int) time.Time {
	return time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
}

// sampleSnapshot builds a snapshot with a distinctive timestamp so retention assertions can order entries.
func sampleSnapshot(capturedAt string) Snapshot {
	return Snapshot{
		CapturedAt:   capturedAt,
		Profile:      "safe",
		RotationMode: "single",
		Accounts: []Account{{
			Credential:       "k:abcdef0123456789",
			Email:            MaskEmail("owner@example.com"),
			Requests:         12,
			RateLimits:       2,
			PromptRejections: 1,
		}},
	}
}

// TestStoreLoadMissingFile verifies that the first capture needs no initialization step.
func TestStoreLoadMissingFile(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "risk.json"))

	document, err := store.Load()
	if err != nil {
		t.Fatalf("Load on a missing file failed: %v", err)
	}
	if document.Version != FileVersion {
		t.Errorf("Version = %d; want %d", document.Version, FileVersion)
	}
	if len(document.Snapshots) != 0 {
		t.Errorf("Snapshots = %d; want 0", len(document.Snapshots))
	}
}

// TestStoreAppendWritesOwnerOnlyFile verifies the telemetry file exists, parses, carries the snapshot, and
// stays readable only by its owner.
func TestStoreAppendWritesOwnerOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "risk.json")
	store := NewStore(path)

	if err := store.Append(sampleSnapshot("2026-11-28T00:00:00Z")); err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat telemetry file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("telemetry file mode = %o; want 600", mode)
		}
	}

	document, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(document.Snapshots) != 1 {
		t.Fatalf("Snapshots = %d; want 1", len(document.Snapshots))
	}
	if got := document.Snapshots[0].Accounts[0].Email; got != "o***@example.com" {
		t.Errorf("stored email = %q; want the masked form", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading telemetry file: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("telemetry file is not valid JSON: %v", err)
	}
	if _, ok := raw["snapshots"]; !ok {
		t.Error("telemetry document is missing the snapshots field")
	}
}

// TestStoreAppendRetainsBoundedHistory verifies that the newest snapshots survive once the retention cap is
// exceeded.
func TestStoreAppendRetainsBoundedHistory(t *testing.T) {
	store := NewStore(filepath.Join(t.TempDir(), "risk.json"))

	for i := range MaxSnapshots + 3 {
		if err := store.Append(sampleSnapshot(TimestampNow(timeFromIndex(i)))); err != nil {
			t.Fatalf("Append %d failed: %v", i, err)
		}
	}

	document, err := store.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(document.Snapshots) != MaxSnapshots {
		t.Fatalf("Snapshots = %d; want the retention cap %d", len(document.Snapshots), MaxSnapshots)
	}
	if got := document.Snapshots[0].CapturedAt; got != TimestampNow(timeFromIndex(3)) {
		t.Errorf("oldest retained snapshot = %q; want index 3 after eviction", got)
	}
	if got := document.Snapshots[len(document.Snapshots)-1].CapturedAt; got != TimestampNow(timeFromIndex(MaxSnapshots+2)) {
		t.Errorf("newest retained snapshot = %q; want the last append", got)
	}
}

// TestStoreAppendRefusesCorruptDocument verifies that a corrupt telemetry file is reported instead of being
// silently overwritten.
func TestStoreAppendRefusesCorruptDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "risk.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("seeding corrupt telemetry file: %v", err)
	}

	store := NewStore(path)
	if err := store.Append(sampleSnapshot("2026-11-28T00:00:00Z")); err == nil {
		t.Fatal("expected Append to refuse a corrupt telemetry document")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading telemetry file: %v", err)
	}
	if string(data) != "{not json" {
		t.Errorf("corrupt telemetry file was overwritten: %q", string(data))
	}
}

// TestMaskEmail verifies masking across address shapes without leaking the local part.
func TestMaskEmail(t *testing.T) {
	tests := map[string]string{
		"owner@example.com":   "o***@example.com",
		"a@example.com":       "*@example.com",
		" spaced@example.com": "s***@example.com",
		"not-an-email":        "n***",
		"":                    "",
	}

	for raw, want := range tests {
		if got := MaskEmail(raw); got != want {
			t.Errorf("MaskEmail(%q) = %q; want %q", raw, got, want)
		}
	}
}

// TestRedactEmails verifies that free-text error messages cannot smuggle a full address into the telemetry
// file.
func TestRedactEmails(t *testing.T) {
	tests := map[string]string{
		"quota exhausted for owner@example.com": "quota exhausted for o***@example.com",
		"two: a@b.co and c@d.org":               "two: *@b.co and *@d.org",
		"no address here":                       "no address here",
		"":                                      "",
		"Google suspended x@y for a terms violation": "Google suspended *@y for a terms violation",
		"mention @media stays untouched":             "mention @media stays untouched",
	}

	for raw, want := range tests {
		if got := RedactEmails(raw); got != want {
			t.Errorf("RedactEmails(%q) = %q; want %q", raw, got, want)
		}
	}
}

// TestTruncate verifies the error-text bound counts runes and only marks text it actually cut.
func TestTruncate(t *testing.T) {
	if got := Truncate("short", 10); got != "short" {
		t.Errorf("Truncate(short) = %q; want the input unchanged", got)
	}

	exact := strings.Repeat("a", 10)
	if got := Truncate(exact, 10); got != exact {
		t.Errorf("Truncate(exact boundary) = %q; want the input unchanged with no ellipsis", got)
	}

	long := strings.Repeat("é", 12)
	got := Truncate(long, 10)
	if runes := []rune(got); len(runes) != 11 || runes[10] != '…' {
		t.Errorf("Truncate(long) = %q (%d runes); want 10 runes plus an ellipsis", got, len([]rune(got)))
	}

	if got := Truncate("anything", 0); got != "" {
		t.Errorf("Truncate with a non-positive limit = %q; want empty", got)
	}
}
