// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// releaseTracker records how many times a release function ran.
type releaseTracker struct {
	// calls counts invocations.
	calls atomic.Int64
}

// release returns a release function that increments the tracker once per call.
func (r *releaseTracker) release() func() {
	return func() { r.calls.Add(1) }
}

// TestPacerCapsConcurrentStreamsPerCredential verifies that a credential cannot exceed its in-flight
// ceiling and that releasing a slot admits the next waiter.
func TestPacerCapsConcurrentStreamsPerCredential(t *testing.T) {
	pacer := NewPacer(1)

	releaseFirst, err := pacer.Acquire(t.Context(), "k:one")
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}

	acquired := make(chan func(), 1)
	go func() {
		release, acquireErr := pacer.Acquire(context.Background(), "k:one")
		if acquireErr == nil {
			acquired <- release
		}
	}()

	select {
	case <-acquired:
		t.Fatal("second acquire succeeded while the ceiling was reached")
	case <-time.After(50 * time.Millisecond):
	}

	releaseFirst()

	select {
	case releaseSecond := <-acquired:
		releaseSecond()
	case <-time.After(time.Second):
		t.Fatal("second acquire never completed after the slot was released")
	}
}

// TestPacerSeparatesCredentials verifies that the ceiling is per credential, not global.
func TestPacerSeparatesCredentials(t *testing.T) {
	pacer := NewPacer(1)

	releaseA, err := pacer.Acquire(t.Context(), "k:alpha")
	if err != nil {
		t.Fatalf("acquire for alpha failed: %v", err)
	}
	defer releaseA()

	releaseB, err := pacer.Acquire(t.Context(), "k:beta")
	if err != nil {
		t.Fatalf("acquire for beta failed: %v", err)
	}
	releaseB()
}

// TestPacerHonorsContextCancellation verifies that a waiter returns the context error instead of blocking
// forever when the client gives up.
func TestPacerHonorsContextCancellation(t *testing.T) {
	pacer := NewPacer(1)

	release, err := pacer.Acquire(t.Context(), "k:one")
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer release()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := pacer.Acquire(ctx, "k:one"); err == nil {
		t.Fatal("expected the canceled context to abort the acquire")
	}
}

// TestPacerDisabledAdmitsEverything verifies that a non-positive ceiling keeps the legacy unbounded
// behaviour.
func TestPacerDisabledAdmitsEverything(t *testing.T) {
	pacer := NewPacer(0)

	first, err := pacer.Acquire(t.Context(), "k:one")
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	second, err := pacer.Acquire(t.Context(), "k:one")
	if err != nil {
		t.Fatalf("second acquire failed: %v", err)
	}

	first()
	second()
}

// TestPacerAppliesLimitChanges verifies that raising the ceiling takes effect for new acquires without
// panicking holders of the previous semaphore.
func TestPacerAppliesLimitChanges(t *testing.T) {
	pacer := NewPacer(1)

	releaseOld, err := pacer.Acquire(t.Context(), "k:one")
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}

	pacer.SetMaxInFlight(2)

	releaseA, err := pacer.Acquire(t.Context(), "k:one")
	if err != nil {
		t.Fatalf("acquire after raising the limit failed: %v", err)
	}
	releaseB, err := pacer.Acquire(t.Context(), "k:one")
	if err != nil {
		t.Fatalf("second acquire after raising the limit failed: %v", err)
	}

	releaseOld()
	releaseA()
	releaseB()
}

// TestPacedBodyReleasesSlotOnce verifies that finishing a stream and closing it releases exactly one slot,
// which is what keeps the ceiling accurate across normal and error paths.
func TestPacedBodyReleasesSlotOnce(t *testing.T) {
	tracker := &releaseTracker{}
	body := newPacedBody(io.NopCloser(strings.NewReader("stream")), tracker.release())

	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("reading paced body: %v", err)
	}
	if got := tracker.calls.Load(); got != 1 {
		t.Errorf("release calls after EOF = %d; want 1", got)
	}

	if err := body.Close(); err != nil {
		t.Fatalf("closing paced body: %v", err)
	}
	if got := tracker.calls.Load(); got != 1 {
		t.Errorf("release calls after Close = %d; want 1", got)
	}
}

// TestBackoffWithJitterStaysWithinBounds verifies that retry backoff is spread but never shortened.
func TestBackoffWithJitterStaysWithinBounds(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 987654321, time.UTC)
	base := 500 * time.Millisecond

	got := backoffWithJitter(base, now)
	if got < base {
		t.Errorf("backoffWithJitter(%v) = %v; must never be shorter than the base", base, got)
	}
	if maxJitter := base + base/5; got >= maxJitter {
		t.Errorf("backoffWithJitter(%v) = %v; want less than %v", base, got, maxJitter)
	}
	if zero := backoffWithJitter(0, now); zero != 0 {
		t.Errorf("backoffWithJitter(0) = %v; want 0", zero)
	}
}
