// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"sync"
	"time"
)

const (
	// DefaultMaxHealthScore defines the maximum operational health score for an account.
	DefaultMaxHealthScore = 100.0

	// MinUsableHealthScore defines the threshold below which an account is marked unhealthy.
	MinUsableHealthScore = 20.0

	// SuccessHealthBonus is the score increment applied upon receiving HTTP 200.
	SuccessHealthBonus = 1.0

	// FailureHealthPenalty is the score decrement applied upon encountering server or network errors.
	FailureHealthPenalty = 5.0

	// RateLimitHealthPenalty is the score decrement applied upon encountering HTTP 429.
	RateLimitHealthPenalty = 10.0

	// DefaultTokenBucketCapacity defines the maximum burst tokens in an account's bucket.
	DefaultTokenBucketCapacity = 50.0

	// DefaultTokenBucketRefillRate defines the replenishment rate in tokens per second (0.1 = 6 tokens/minute).
	DefaultTokenBucketRefillRate = 0.1
)

// HealthTracker tracks rolling operational reliability scores per account email address.
type HealthTracker struct {
	// mu synchronizes access to the internal scores map.
	mu sync.RWMutex

	// scores maps account email addresses to their current operational health score (0.0 to 100.0).
	scores map[string]float64
}

// NewHealthTracker initializes a HealthTracker.
func NewHealthTracker() *HealthTracker {
	return &HealthTracker{
		scores: make(map[string]float64),
	}
}

// Score returns the current operational health score for an account (defaults to 100.0).
func (h *HealthTracker) Score(email string) float64 {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if score, exists := h.scores[email]; exists {
		return score
	}
	return DefaultMaxHealthScore
}

// IsUsable reports whether the account's health score meets or exceeds the minimum usable threshold.
func (h *HealthTracker) IsUsable(email string) bool {
	return h.Score(email) >= MinUsableHealthScore
}

// RecordSuccess rewards an account upon completing a successful API completion request.
func (h *HealthTracker) RecordSuccess(email string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	curr := DefaultMaxHealthScore
	if s, exists := h.scores[email]; exists {
		curr = s
	}
	h.scores[email] = min(DefaultMaxHealthScore, curr+SuccessHealthBonus)
}

// RecordRateLimit penalizes an account when upstream returns HTTP 429 rate limiting.
func (h *HealthTracker) RecordRateLimit(email string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	curr := DefaultMaxHealthScore
	if s, exists := h.scores[email]; exists {
		curr = s
	}
	h.scores[email] = max(0.0, curr-RateLimitHealthPenalty)
}

// RecordFailure penalizes an account when a request encounters non-rate-limit upstream or network failures.
func (h *HealthTracker) RecordFailure(email string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	curr := DefaultMaxHealthScore
	if s, exists := h.scores[email]; exists {
		curr = s
	}
	h.scores[email] = max(0.0, curr-FailureHealthPenalty)
}

// TokenBucket coordinates client-side burst rate limiting and smooth request throttling per account.
type TokenBucket struct {
	// mu synchronizes token updates and timestamp calculations.
	mu sync.Mutex

	// capacity is the maximum burst token capacity.
	capacity float64

	// refillRate defines the tokens regenerated per second.
	refillRate float64

	// tokens holds the current available token balance.
	tokens float64

	// lastRefill records the timestamp of the last token recalculation.
	lastRefill time.Time
}

// NewTokenBucket creates a TokenBucket with specified capacity and refill rate.
func NewTokenBucket(capacity float64, refillRate float64, now time.Time) *TokenBucket {
	if capacity <= 0 {
		capacity = DefaultTokenBucketCapacity
	}
	if refillRate <= 0 {
		refillRate = DefaultTokenBucketRefillRate
	}
	return &TokenBucket{
		capacity:   capacity,
		refillRate: refillRate,
		tokens:     capacity,
		lastRefill: now,
	}
}

// refill updates the token balance based on elapsed time since the previous calculation.
func (b *TokenBucket) refill(now time.Time) {
	elapsed := now.Sub(b.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}
	b.tokens = min(b.capacity, b.tokens+elapsed*b.refillRate)
	b.lastRefill = now
}

// Consume attempts to deduct 1 token from the bucket. Returns true if a token was available.
func (b *TokenBucket) Consume(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(now)
	if b.tokens >= 1.0 {
		b.tokens -= 1.0
		return true
	}
	return false
}

// Refund restores 1 token to the bucket (e.g. when an in-flight request failed prematurely).
func (b *TokenBucket) Refund() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = min(b.capacity, b.tokens+1.0)
}

// Tokens reports the current token balance safely.
func (b *TokenBucket) Tokens(now time.Time) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(now)
	return b.tokens
}

// TimeUntilNextToken calculates the remaining wait duration until at least 1 token is available.
func (b *TokenBucket) TimeUntilNextToken(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refill(now)
	if b.tokens >= 1.0 {
		return 0
	}
	needed := 1.0 - b.tokens
	secs := needed / b.refillRate
	return time.Duration(secs * float64(time.Second))
}
