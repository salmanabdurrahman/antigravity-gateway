// Package rotator_test provides unit and concurrency test suites for the rotator package.
package rotator_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// TestClassify verifies error classification from upstream HTTP status codes and error bodies.
func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		expected rotator.FailureKind
	}{
		{"429 without quota metadata", 429, "Too Many Requests", rotator.FailureRateLimit},
		{"bare RESOURCE_EXHAUSTED body", 400, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, rotator.FailurePromptRejection},
		{"RESOURCE_EXHAUSTED message only", 429, `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`, rotator.FailurePromptRejection},
		{"RESOURCE_EXHAUSTED with details", 429, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure"}]}}`, rotator.FailureRateLimit},
		{"RESOURCE_EXHAUSTED with retryDelay", 429, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"retryDelay":"34s"}]}}`, rotator.FailureRateLimit},
		{"401 auth", 401, "Unauthorized", rotator.FailureAuth},
		{"403 forbidden", 403, "Forbidden", rotator.FailureForbidden},
		{"400 bad request", 400, "Invalid JSON", rotator.FailureBadRequest},
		{"500 server", 500, "Internal Server Error", rotator.FailureServer},
		{"503 server", 503, "Service Unavailable", rotator.FailureServer},
		{"408 timeout", 408, "Request Timeout", rotator.FailureServer},
		{"overloaded body", 200, "Upstream overloaded, please try again", rotator.FailureServer},
		{"404 client", 404, "Not Found", rotator.FailureClient},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rotator.Classify(tt.status, tt.body)
			if got != tt.expected {
				t.Errorf("Classify(%d, %q) = %v; want %v", tt.status, tt.body, got, tt.expected)
			}
		})
	}
}

// TestClassifyWithHeaders verifies that Retry-After headers and quota reset metadata keep a 429
// classified as quota exhaustion, while a bare wrapper is a request-scoped prompt rejection.
func TestClassifyWithHeaders(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		headers  http.Header
		expected rotator.FailureKind
	}{
		{
			name:     "Retry-After header marks quota exhaustion",
			status:   http.StatusTooManyRequests,
			body:     `{"error":{"status":"RESOURCE_EXHAUSTED"}}`,
			headers:  http.Header{"Retry-After": []string{"30"}},
			expected: rotator.FailureRateLimit,
		},
		{
			name:     "quotaResetTimeStamp marks quota exhaustion",
			status:   http.StatusTooManyRequests,
			body:     `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"quotaResetTimeStamp":"2026-11-14T12:00:30Z"}]}}`,
			expected: rotator.FailureRateLimit,
		},
		{
			name:     "bare wrapper is a prompt rejection",
			status:   http.StatusTooManyRequests,
			body:     `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`,
			expected: rotator.FailurePromptRejection,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rotator.ClassifyWithHeaders(tt.status, tt.body, tt.headers)
			if got != tt.expected {
				t.Errorf("ClassifyWithHeaders() = %v; want %v", got, tt.expected)
			}
		})
	}
}

// TestParseRetryDelay verifies extraction of backoff durations from Retry-After headers, JSON resetTime, and text delays.
func TestParseRetryDelay(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		body     string
		headers  http.Header
		expected time.Duration
		found    bool
	}{
		{
			name:     "Retry-After seconds header",
			body:     "",
			headers:  http.Header{"Retry-After": []string{"7"}},
			expected: 7 * time.Second,
			found:    true,
		},
		{
			name:     "Retry-After HTTP Date header",
			body:     "",
			headers:  http.Header{"Retry-After": []string{now.Add(15 * time.Second).Format(http.TimeFormat)}},
			expected: 15 * time.Second,
			found:    true,
		},
		{
			name:     "retryDelay in json",
			body:     `{"retryDelay": "34.5s"}`,
			headers:  nil,
			expected: 34500 * time.Millisecond,
			found:    true,
		},
		{
			name:     "reset after phrase with hours mins secs",
			body:     "Your quota will reset after 1h2m3s.",
			headers:  nil,
			expected: 3723 * time.Second,
			found:    true,
		},
		{
			name:     "Please retry in ms",
			body:     "Please retry in 250ms",
			headers:  nil,
			expected: 250 * time.Millisecond,
			found:    true,
		},
		{
			name:     "quotaResetDelay json",
			body:     `"quotaResetDelay": "2m"`,
			headers:  nil,
			expected: 120 * time.Second,
			found:    true,
		},
		{
			name:     "resetTime in json",
			body:     `{"error": {"details": [{"@type": "type.googleapis.com/google.rpc.QuotaFailure", "resetTime": "2026-11-14T12:00:30Z"}]}}`,
			headers:  nil,
			expected: 30 * time.Second,
			found:    true,
		},
		{
			name:     "reset_time snake case in json",
			body:     `{"reset_time": "2026-11-14T12:01:00Z"}`,
			headers:  nil,
			expected: 60 * time.Second,
			found:    true,
		},
		{
			name:     "no delay information",
			body:     "standard error",
			headers:  nil,
			expected: 0,
			found:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := rotator.ParseRetryDelay(tt.body, tt.headers, now)
			if ok != tt.found {
				t.Fatalf("ParseRetryDelay() found = %v; want %v", ok, tt.found)
			}
			if ok && got != tt.expected {
				t.Errorf("ParseRetryDelay() duration = %v; want %v", got, tt.expected)
			}
		})
	}
}

// TestExtractErrorMessage verifies extraction of diagnostic error strings from nested Google error envelopes.
func TestExtractErrorMessage(t *testing.T) {
	jsonObj := `{"error":{"message":"Resource has been exhausted (e.g. check quota)."}}`
	if msg := rotator.ExtractErrorMessage(jsonObj); msg != "Resource has been exhausted (e.g. check quota)." {
		t.Errorf("ExtractErrorMessage(jsonObj) = %q; want exact message", msg)
	}

	jsonArr := `[{"error":{"message":"Array error message"}}]`
	if msg := rotator.ExtractErrorMessage(jsonArr); msg != "Array error message" {
		t.Errorf("ExtractErrorMessage(jsonArr) = %q; want exact message", msg)
	}

	rawText := "Simple plain text error message"
	if msg := rotator.ExtractErrorMessage(rawText); msg != rawText {
		t.Errorf("ExtractErrorMessage(rawText) = %q; want %q", msg, rawText)
	}
}

// TestParseRetryDelayMillisecondQuotaReset verifies that a sub-second quotaResetDelay is read as
// milliseconds instead of being split into a minutes component plus a stray second.
func TestParseRetryDelayMillisecondQuotaReset(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		body string
		want time.Duration
	}{
		{"milliseconds", `{"error":{"details":[{"quotaResetDelay":"500ms"}]}}`, 500 * time.Millisecond},
		{"composite hours minutes seconds", `{"quotaResetDelay":"1h2m3s"}`, time.Hour + 2*time.Minute + 3*time.Second},
		{"minutes only", `{"quotaResetDelay":"15m"}`, 15 * time.Minute},
		{"fractional seconds", `{"quotaResetDelay":"0.5s"}`, 500 * time.Millisecond},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := rotator.ParseRetryDelay(tc.body, nil, now)
			if !ok {
				t.Fatalf("ParseRetryDelay(%q) reported no delay", tc.body)
			}
			if got != tc.want {
				t.Errorf("ParseRetryDelay(%q) = %v; want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestCooldownTrackerPrunesWithInjectedClock verifies that expired cooldown entries are pruned using
// the caller-supplied clock, so pruning stays deterministic when the wall clock differs from the
// rotator's injected clock.
func TestCooldownTrackerPrunesWithInjectedClock(t *testing.T) {
	tracker := rotator.NewCooldownTracker()
	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	tracker.SetCooldown("expired-model", base.Add(-time.Minute), base)

	// The injected clock is far ahead of the wall clock; pruning must follow the injected value.
	future := base.Add(24 * time.Hour)
	tracker.SetCooldown("active-model", future.Add(time.Hour), future)

	active := tracker.ActiveCooldowns(future)
	if _, exists := active["expired-model"]; exists {
		t.Errorf("expired cooldown must be pruned using the injected clock, got %v", active)
	}
	if _, exists := active["active-model"]; !exists {
		t.Errorf("active cooldown must be retained, got %v", active)
	}
}

// TestCooldownTracker verifies thread-safe per-model and wildcard expiration tracking in memory.
func TestCooldownTracker(t *testing.T) {
	now := time.Now()
	tracker := rotator.NewCooldownTracker()

	if tracker.IsCooling("gemini", now) {
		t.Fatalf("expected not cooling initially")
	}

	tracker.SetCooldown("gemini", now.Add(10*time.Second), now)
	if !tracker.IsCooling("gemini", now) {
		t.Errorf("expected gemini to be cooling")
	}
	if tracker.IsCooling("claude", now) {
		t.Errorf("expected claude not to be cooling")
	}

	// Wildcard cooldown affects all models
	tracker.SetCooldown("*", now.Add(20*time.Second), now)
	if !tracker.IsCooling("claude", now) {
		t.Errorf("expected claude to be cooling due to wildcard")
	}

	active := tracker.ActiveCooldowns(now)
	if len(active) != 2 {
		t.Errorf("expected 2 active cooldowns, got %d", len(active))
	}
}
