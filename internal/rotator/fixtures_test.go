// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRetryDelay_TrackedFixtures verifies upstream rate limit and retry delay extraction
// across sanitized tracked JSON fixtures from Cloud Code Assist.
func TestRetryDelay_TrackedFixtures(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	// 1. google.rpc.RetryInfo fixture ("retryDelay": "45.5s")
	dataRetryInfo, err := os.ReadFile(filepath.Join("testdata", "rate_limit_retry_info.json"))
	if err != nil {
		t.Fatalf("reading rate_limit_retry_info.json fixture: %v", err)
	}

	delay, ok := ParseRetryDelay(string(dataRetryInfo), nil, now)
	if !ok {
		t.Fatalf("expected ParseRetryDelay to return true for RetryInfo fixture")
	}
	expectedDelay := 45500 * time.Millisecond
	if delay != expectedDelay {
		t.Errorf("ParseRetryDelay() = %v; want %v", delay, expectedDelay)
	}

	kind := Classify(http.StatusTooManyRequests, string(dataRetryInfo))
	if kind != FailureRateLimit {
		t.Errorf("Classify() = %v; want %v", kind, FailureRateLimit)
	}

	// 2. "Your quota will reset after 2h15m30s" natural text fixture
	dataResetAfter, err := os.ReadFile(filepath.Join("testdata", "rate_limit_reset_after_text.json"))
	if err != nil {
		t.Fatalf("reading rate_limit_reset_after_text.json fixture: %v", err)
	}

	delayReset, ok := ParseRetryDelay(string(dataResetAfter), nil, now)
	if !ok {
		t.Fatalf("expected ParseRetryDelay to return true for reset after fixture")
	}
	expectedResetDelay := 2*time.Hour + 15*time.Minute + 30*time.Second
	if delayReset != expectedResetDelay {
		t.Errorf("ParseRetryDelay() = %v; want %v", delayReset, expectedResetDelay)
	}

	// 3. google.rpc.ErrorInfo quotaResetDelay fixture ("15m")
	dataQuotaReset, err := os.ReadFile(filepath.Join("testdata", "rate_limit_quota_reset_delay.json"))
	if err != nil {
		t.Fatalf("reading rate_limit_quota_reset_delay.json fixture: %v", err)
	}

	delayQuota, ok := ParseRetryDelay(string(dataQuotaReset), nil, now)
	if !ok {
		t.Fatalf("expected ParseRetryDelay to return true for quotaResetDelay fixture")
	}
	expectedQuotaDelay := 15 * time.Minute
	if delayQuota != expectedQuotaDelay {
		t.Errorf("ParseRetryDelay() = %v; want %v", delayQuota, expectedQuotaDelay)
	}

	// 4. Bad request fixture (HTTP 400 schema error)
	dataBadRequest, err := os.ReadFile(filepath.Join("testdata", "bad_request_schema.json"))
	if err != nil {
		t.Fatalf("reading bad_request_schema.json fixture: %v", err)
	}

	kindBadRequest := Classify(http.StatusBadRequest, string(dataBadRequest))
	if kindBadRequest != FailureBadRequest {
		t.Errorf("Classify() = %v; want %v", kindBadRequest, FailureBadRequest)
	}

	errMsg := ExtractErrorMessage(string(dataBadRequest))
	if errMsg == "" {
		t.Errorf("ExtractErrorMessage() returned empty string for bad request fixture")
	}

	// 5. Server overloaded 503 fixture
	dataServer503, err := os.ReadFile(filepath.Join("testdata", "server_overloaded_503.json"))
	if err != nil {
		t.Fatalf("reading server_overloaded_503.json fixture: %v", err)
	}

	kindServer := Classify(http.StatusServiceUnavailable, string(dataServer503))
	if kindServer != FailureServer {
		t.Errorf("Classify() = %v; want %v", kindServer, FailureServer)
	}
}

// TestSanitizedFixturesZeroSecrets verifies that test fixtures under testdata/ contain no secret tokens or credentials.
func TestSanitizedFixturesZeroSecrets(t *testing.T) {
	fixtureFiles, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatalf("globbing testdata fixtures: %v", err)
	}

	forbiddenSubstrings := []string{
		"ya29.",
		"refresh_token",
		"client_secret",
		"Bearer ",
		"AIzaSy",
	}

	for _, file := range fixtureFiles {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading fixture %s: %v", file, err)
		}

		// Ensure valid JSON
		var parsed any
		if err := json.Unmarshal(content, &parsed); err != nil {
			t.Errorf("fixture %s is not valid JSON: %v", file, err)
		}

		strContent := string(content)
		for _, forbidden := range forbiddenSubstrings {
			if strings.Contains(strContent, forbidden) {
				t.Errorf("fixture %s contains forbidden secret token %q", file, forbidden)
			}
		}
	}
}
