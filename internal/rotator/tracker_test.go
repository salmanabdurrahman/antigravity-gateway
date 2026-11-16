// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"testing"
	"time"
)

// TestHealthTrackerScoring verifies health score bonuses, penalties, and usability thresholds.
func TestHealthTrackerScoring(t *testing.T) {
	tracker := NewHealthTracker()
	email := "test@example.com"

	// Default score is 100
	if score := tracker.Score(email); score != 100.0 {
		t.Errorf("expected default score 100.0, got %v", score)
	}
	if !tracker.IsUsable(email) {
		t.Error("expected account with score 100 to be usable")
	}

	// Rate limit penalty (-10)
	tracker.RecordRateLimit(email)
	if score := tracker.Score(email); score != 90.0 {
		t.Errorf("expected score 90.0 after rate limit, got %v", score)
	}

	// Server failure penalty (-5)
	tracker.RecordFailure(email)
	if score := tracker.Score(email); score != 85.0 {
		t.Errorf("expected score 85.0 after failure, got %v", score)
	}

	// Success bonus (+1)
	tracker.RecordSuccess(email)
	if score := tracker.Score(email); score != 86.0 {
		t.Errorf("expected score 86.0 after success, got %v", score)
	}

	// Multiple rate limits drop score below usability threshold (< 20)
	for range 7 {
		tracker.RecordRateLimit(email)
	}
	if score := tracker.Score(email); score >= MinUsableHealthScore {
		t.Errorf("expected score < 20, got %v", score)
	}
	if tracker.IsUsable(email) {
		t.Error("expected unhealthy account to report IsUsable false")
	}
}

// TestTokenBucketConsumptionAndRefill verifies token deduction, refunding, and time-based replenishment.
func TestTokenBucketConsumptionAndRefill(t *testing.T) {
	baseTime := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	// Bucket with 2 tokens max, refills at 1 token per second
	bucket := NewTokenBucket(2.0, 1.0, baseTime)

	if tokens := bucket.Tokens(baseTime); tokens != 2.0 {
		t.Errorf("expected 2.0 initial tokens, got %v", tokens)
	}

	// Consume 1 token
	if !bucket.Consume(baseTime) {
		t.Fatal("expected first consume to succeed")
	}
	// Consume 2nd token
	if !bucket.Consume(baseTime) {
		t.Fatal("expected second consume to succeed")
	}
	// 3rd consume should fail
	if bucket.Consume(baseTime) {
		t.Fatal("expected third consume to fail due to empty bucket")
	}

	// Wait duration should be 1 second
	wait := bucket.TimeUntilNextToken(baseTime)
	if wait < 900*time.Millisecond || wait > 1100*time.Millisecond {
		t.Errorf("expected ~1s wait until next token, got %v", wait)
	}

	// Advance time by 1 second: bucket should refill 1 token
	nextTime := baseTime.Add(1 * time.Second)
	if !bucket.Consume(nextTime) {
		t.Fatal("expected consume after 1s refill to succeed")
	}

	// Refund should restore token
	bucket.Refund()
	if tokens := bucket.Tokens(nextTime); tokens < 1.0 {
		t.Errorf("expected at least 1.0 token after refund, got %v", tokens)
	}
}
