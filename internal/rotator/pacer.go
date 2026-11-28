// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"context"
	"io"
	"sync"
	"time"
)

// Pacer bounds how many upstream streams one credential may run at the same time. The native client is an
// interactive single-session process; a coding-agent fan-out that opens many parallel streams from the same
// credential is exactly the burst shape that upstream abuse detection reacts to.
type Pacer struct {
	// mu guards the limit and the per-credential slot map.
	mu sync.Mutex

	// maxInFlight is the per-credential concurrency ceiling; a non-positive value disables pacing.
	maxInFlight int

	// slots maps a credential key to its semaphore slot.
	slots map[string]*pacerSlot
}

// pacerSlot holds the semaphore for one credential.
type pacerSlot struct {
	// sem blocks once the credential reached its in-flight ceiling.
	sem chan struct{}
}

// NewPacer builds a pacer enforcing maxInFlight concurrent streams per credential. A non-positive limit
// disables pacing entirely.
func NewPacer(maxInFlight int) *Pacer {
	if maxInFlight < 0 {
		maxInFlight = 0
	}
	return &Pacer{
		maxInFlight: maxInFlight,
		slots:       make(map[string]*pacerSlot),
	}
}

// SetMaxInFlight updates the ceiling. Existing slots are dropped so the next acquire rebuilds a semaphore
// with the new capacity; holders of the previous semaphore still release into it without blocking.
func (p *Pacer) SetMaxInFlight(maxInFlight int) {
	if maxInFlight < 0 {
		maxInFlight = 0
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.maxInFlight = maxInFlight
	p.slots = make(map[string]*pacerSlot)
}

// Acquire reserves one in-flight slot for the credential and returns a release function. It returns nil
// when the context is canceled before a slot frees up, and a no-op release when pacing is disabled.
func (p *Pacer) Acquire(ctx context.Context, credentialKey string) (func(), error) {
	p.mu.Lock()
	limit := p.maxInFlight
	if limit <= 0 {
		p.mu.Unlock()
		return func() {}, nil
	}

	slot, ok := p.slots[credentialKey]
	if !ok || cap(slot.sem) != limit {
		slot = &pacerSlot{sem: make(chan struct{}, limit)}
		p.slots[credentialKey] = slot
	}
	sem := slot.sem
	p.mu.Unlock()

	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var once sync.Once
	return func() {
		once.Do(func() { <-sem })
	}, nil
}

// pacedBody releases the credential's in-flight slot when the caller closes or exhausts the response body,
// so a pacing slot covers the whole upstream stream rather than only its headers.
type pacedBody struct {
	// ReadCloser is the upstream response body being paced.
	io.ReadCloser

	// once guards the release so a read failure followed by a close releases exactly one slot.
	once sync.Once

	// release returns the reserved in-flight slot.
	release func()
}

// newPacedBody wraps an upstream body so finishing the stream returns its pacing slot.
func newPacedBody(body io.ReadCloser, release func()) *pacedBody {
	return &pacedBody{ReadCloser: body, release: release}
}

// Read delegates to the upstream body and releases the slot on any terminal read.
func (b *pacedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.release)
	}
	return n, err
}

// Close closes the upstream body and releases the slot.
func (b *pacedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

// backoffWithJitter spreads a retry backoff by up to twenty percent so parallel clients do not retry in
// lockstep. The spread is derived from the clock, which keeps tests with an injected clock deterministic.
func backoffWithJitter(base time.Duration, now time.Time) time.Duration {
	if base <= 0 {
		return 0
	}

	spread := int64(base / 5)
	if spread <= 0 {
		return base
	}
	return base + time.Duration(now.UnixNano()%spread)
}
