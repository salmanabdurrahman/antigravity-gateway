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

// readRotatorFixture loads one tracked, sanitized JSON fixture from the package testdata directory.
// Fixtures are committed so upstream Cloud Code Assist envelopes can be asserted without depending
// on untracked local files or live network calls.
func readRotatorFixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading %s fixture: %v", name, err)
	}
	return data
}

// TestRetryDelay_TrackedFixtures verifies upstream rate limit and retry delay extraction
// across sanitized tracked JSON fixtures from Cloud Code Assist.
func TestRetryDelay_TrackedFixtures(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

	// 1. google.rpc.RetryInfo fixture ("retryDelay": "45.5s")
	dataRetryInfo := readRotatorFixture(t, "rate_limit_retry_info.json")

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
	dataResetAfter := readRotatorFixture(t, "rate_limit_reset_after_text.json")

	delayReset, ok := ParseRetryDelay(string(dataResetAfter), nil, now)
	if !ok {
		t.Fatalf("expected ParseRetryDelay to return true for reset after fixture")
	}
	expectedResetDelay := 2*time.Hour + 15*time.Minute + 30*time.Second
	if delayReset != expectedResetDelay {
		t.Errorf("ParseRetryDelay() = %v; want %v", delayReset, expectedResetDelay)
	}

	// 3. google.rpc.ErrorInfo quotaResetDelay fixture ("15m")
	dataQuotaReset := readRotatorFixture(t, "rate_limit_quota_reset_delay.json")

	delayQuota, ok := ParseRetryDelay(string(dataQuotaReset), nil, now)
	if !ok {
		t.Fatalf("expected ParseRetryDelay to return true for quotaResetDelay fixture")
	}
	expectedQuotaDelay := 15 * time.Minute
	if delayQuota != expectedQuotaDelay {
		t.Errorf("ParseRetryDelay() = %v; want %v", delayQuota, expectedQuotaDelay)
	}

	// 4. Bad request fixture (HTTP 400 schema error)
	dataBadRequest := readRotatorFixture(t, "bad_request_schema.json")

	kindBadRequest := Classify(http.StatusBadRequest, string(dataBadRequest))
	if kindBadRequest != FailureBadRequest {
		t.Errorf("Classify() = %v; want %v", kindBadRequest, FailureBadRequest)
	}

	errMsg := ExtractErrorMessage(string(dataBadRequest))
	if errMsg == "" {
		t.Errorf("ExtractErrorMessage() returned empty string for bad request fixture")
	}

	// 5. Server overloaded 503 fixture
	dataServer503 := readRotatorFixture(t, "server_overloaded_503.json")

	kindServer := Classify(http.StatusServiceUnavailable, string(dataServer503))
	if kindServer != FailureServer {
		t.Errorf("Classify() = %v; want %v", kindServer, FailureServer)
	}
}

// TestSuspensionFixtureClassification pins how the current failure classifier reads the Cloud Code
// Assist account-suspension envelopes captured in tracked fixtures. Two shapes already surface as a
// permanent disposition: the structured 403 PERMISSION_DENIED carrying TOS_VIOLATION metadata, and
// the support-routed TOS_VIOLATION_CONTACT_SUPPORT variant. The other two deliberately pin gaps: a
// suspension reported as 412 FAILED_PRECONDITION still falls through as a generic client error, and
// a bare 429 RESOURCE_EXHAUSTED body still classifies as a request-scoped prompt rejection.
// Changing any of those verdicts has to be deliberate, because these assertions fail otherwise.
func TestSuspensionFixtureClassification(t *testing.T) {
	tests := []struct {
		// fixture is the tracked JSON envelope under testdata/.
		fixture string
		// status is the HTTP status code Cloud Code Assist returned with that envelope.
		status int
		// wantKind is the failure classification the rotator currently derives.
		wantKind FailureKind
		// wantPermanent reports whether the envelope counts as a permanent account disposition.
		wantPermanent bool
		// wantMessage is a substring of the upstream message operators are shown.
		wantMessage string
	}{
		{
			fixture:       "tos_403.json",
			status:        http.StatusForbidden,
			wantKind:      FailureForbidden,
			wantPermanent: true,
			wantMessage:   "violation of Terms of Service",
		},
		{
			fixture:       "tos_403_contact_support.json",
			status:        http.StatusForbidden,
			wantKind:      FailureForbidden,
			wantPermanent: true,
			wantMessage:   "Terms of Service",
		},
		{
			fixture:       "tos_412.json",
			status:        http.StatusPreconditionFailed,
			wantKind:      FailureClient,
			wantPermanent: false,
			wantMessage:   "suspended",
		},
		{
			fixture:       "bare429_prompt_rejection.json",
			status:        http.StatusTooManyRequests,
			wantKind:      FailurePromptRejection,
			wantPermanent: false,
			wantMessage:   "Resource has been exhausted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			body := string(readRotatorFixture(t, tt.fixture))

			if got := Classify(tt.status, body); got != tt.wantKind {
				t.Errorf("Classify(%d, %s) = %v; want %v", tt.status, tt.fixture, got, tt.wantKind)
			}
			if got := IsPermanentForbidden(body); got != tt.wantPermanent {
				t.Errorf("IsPermanentForbidden(%s) = %v; want %v", tt.fixture, got, tt.wantPermanent)
			}
			if msg := ExtractErrorMessage(body); !strings.Contains(msg, tt.wantMessage) {
				t.Errorf("ExtractErrorMessage(%s) = %q; want substring %q", tt.fixture, msg, tt.wantMessage)
			}
		})
	}
}

// TestSuspensionFixtureKeepsErrorInfoMetadata guards the structured google.rpc.ErrorInfo metadata
// the suspension verdict depends on: the reason token, the service domain, the upstream status, and
// the operator-facing appeal link must survive in the tracked fixture instead of being reduced to a
// bare message.
func TestSuspensionFixtureKeepsErrorInfoMetadata(t *testing.T) {
	body := string(readRotatorFixture(t, "tos_403.json"))

	for _, want := range []string{"TOS_VIOLATION", "cloudcode-pa.googleapis.com", "appeal_url", "PERMISSION_DENIED"} {
		if !strings.Contains(body, want) {
			t.Errorf("tos_403.json is missing %q; the suspension envelope must stay faithful to upstream", want)
		}
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
