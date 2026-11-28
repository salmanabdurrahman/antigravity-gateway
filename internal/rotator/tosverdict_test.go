// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"net/http"
	"strings"
	"testing"
)

// TestDetectSuspensionFixtures verifies that the tracked Cloud Code Assist suspension envelopes are
// recognized as terminal account verdicts, with the structured google.rpc.ErrorInfo reason, service
// domain, and validated appeal link preserved for operators.
func TestDetectSuspensionFixtures(t *testing.T) {
	tests := []struct {
		// fixture is the tracked JSON envelope under testdata/.
		fixture string
		// status is the HTTP status code Cloud Code Assist returned with that envelope.
		status int
		// wantKind is the suspension classification the verdict carries.
		wantKind SuspensionKind
		// wantReason is the raw google.rpc.ErrorInfo reason token, empty for unstructured envelopes.
		wantReason string
		// wantAppealURL is the validated appeal link expected in the verdict.
		wantAppealURL string
		// wantMessage is a substring of the upstream explanation.
		wantMessage string
	}{
		{
			fixture:       "tos_403.json",
			status:        http.StatusForbidden,
			wantKind:      SuspensionTermsOfService,
			wantReason:    "TOS_VIOLATION",
			wantAppealURL: "https://forms.gle/EXAMPLEEXAMPLE",
			wantMessage:   "violation of Terms of Service",
		},
		{
			fixture:     "tos_403_contact_support.json",
			status:      http.StatusForbidden,
			wantKind:    SuspensionContactSupport,
			wantReason:  "TOS_VIOLATION_CONTACT_SUPPORT",
			wantMessage: "Terms of Service",
		},
		{
			fixture:     "tos_412.json",
			status:      http.StatusPreconditionFailed,
			wantKind:    SuspensionPreconditionFailed,
			wantMessage: "suspended",
		},
	}

	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			suspension, ok := DetectSuspension(tt.status, string(readRotatorFixture(t, tt.fixture)))
			if !ok {
				t.Fatalf("DetectSuspension(%d, %s) = false; want a terminal suspension verdict", tt.status, tt.fixture)
			}
			if suspension.Kind != tt.wantKind {
				t.Errorf("Kind = %q; want %q", suspension.Kind, tt.wantKind)
			}
			if suspension.StatusCode != tt.status {
				t.Errorf("StatusCode = %d; want %d", suspension.StatusCode, tt.status)
			}
			if suspension.Reason != tt.wantReason {
				t.Errorf("Reason = %q; want %q", suspension.Reason, tt.wantReason)
			}
			if suspension.AppealURL != tt.wantAppealURL {
				t.Errorf("AppealURL = %q; want %q", suspension.AppealURL, tt.wantAppealURL)
			}
			if !strings.Contains(suspension.Message, tt.wantMessage) {
				t.Errorf("Message = %q; want substring %q", suspension.Message, tt.wantMessage)
			}
		})
	}
}

// TestDetectSuspensionIgnoresTransientFailures verifies that quota rejections, recoverable permission
// errors, unrelated preconditions, and malformed bodies are never mistaken for a terminal suspension,
// because misclassifying them would permanently disable a healthy account.
func TestDetectSuspensionIgnoresTransientFailures(t *testing.T) {
	tests := []struct {
		// name labels the upstream failure shape under test.
		name string
		// status is the HTTP status code returned upstream.
		status int
		// body is the upstream response body.
		body string
	}{
		{
			name:   "bare quota rejection",
			status: http.StatusTooManyRequests,
			body:   string(readRotatorFixture(t, "bare429_prompt_rejection.json")),
		},
		{
			name:   "recoverable permission denied",
			status: http.StatusForbidden,
			body:   `{"error":{"code":403,"message":"The caller does not have permission","status":"PERMISSION_DENIED"}}`,
		},
		{
			name:   "unrelated precondition failure",
			status: http.StatusPreconditionFailed,
			body:   `{"error":{"code":412,"message":"Request precondition failed","status":"FAILED_PRECONDITION"}}`,
		},
		{
			name:   "malformed body",
			status: http.StatusForbidden,
			body:   `{"error": {`,
		},
		{
			name:   "terms wording under a non-verdict status",
			status: http.StatusBadRequest,
			body:   `{"error":{"code":400,"message":"This service has been disabled in this account for violation of Terms of Service."}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if suspension, ok := DetectSuspension(tt.status, tt.body); ok {
				t.Fatalf("DetectSuspension(%d) = %+v; want no suspension verdict", tt.status, suspension)
			}
		})
	}
}

// TestDetectSuspensionLegacyMarkerFallback verifies that older text-only suspension envelopes without
// structured details[] metadata still classify as terminal, preserving the behaviour the rotator had
// before structured verdicts existed.
func TestDetectSuspensionLegacyMarkerFallback(t *testing.T) {
	body := `{"error":{"code":403,"message":"This service has been disabled in this account for violation of Terms of Service. Please submit an appeal to continue using this product.","status":"PERMISSION_DENIED"}}`

	suspension, ok := DetectSuspension(http.StatusForbidden, body)
	if !ok {
		t.Fatalf("DetectSuspension returned false for a legacy text-only suspension envelope")
	}
	if suspension.Kind != SuspensionTermsOfService {
		t.Errorf("Kind = %q; want %q", suspension.Kind, SuspensionTermsOfService)
	}
	if suspension.AppealURL != "" {
		t.Errorf("AppealURL = %q; want empty when upstream supplied no link", suspension.AppealURL)
	}
}

// TestSanitizeAppealURL verifies that upstream metadata is treated as untrusted: only HTTPS links on the
// documented appeal hosts survive, and every other shape is dropped before it can be persisted or shown.
func TestSanitizeAppealURL(t *testing.T) {
	tests := []struct {
		// name labels the candidate link shape.
		name string
		// raw is the upstream metadata value.
		raw string
		// want is the sanitized link, empty when rejected.
		want string
	}{
		{name: "short form link", raw: "https://forms.gle/hGzM9MEUv2azZsrb9", want: "https://forms.gle/hGzM9MEUv2azZsrb9"},
		{name: "docs form link", raw: "https://docs.google.com/forms/d/e/abc/viewform", want: "https://docs.google.com/forms/d/e/abc/viewform"},
		{name: "surrounding whitespace", raw: "  https://forms.gle/abc  ", want: "https://forms.gle/abc"},
		{name: "plain http", raw: "http://forms.gle/abc", want: ""},
		{name: "non-http scheme", raw: "javascript:alert(1)", want: ""},
		{name: "unlisted host", raw: "https://evil.example/appeal", want: ""},
		{name: "host suffix trick", raw: "https://forms.gle.evil.example/abc", want: ""},
		{name: "embedded credentials", raw: "https://user:pass@forms.gle/abc", want: ""},
		{name: "explicit port", raw: "https://forms.gle:8443/abc", want: ""},
		{name: "oversized value", raw: "https://forms.gle/" + strings.Repeat("a", appealURLMaxLength), want: ""},
		{name: "empty", raw: "   ", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeAppealURL(tt.raw); got != tt.want {
				t.Errorf("sanitizeAppealURL(%q) = %q; want %q", tt.raw, got, tt.want)
			}
		})
	}
}
