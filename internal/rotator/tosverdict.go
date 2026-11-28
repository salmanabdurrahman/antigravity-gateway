// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// SuspensionKind classifies a terminal upstream account-suspension verdict.
type SuspensionKind string

const (
	// SuspensionTermsOfService reports Google disabling the account for violating its terms of
	// service, delivered as HTTP 403 PERMISSION_DENIED with reason TOS_VIOLATION.
	SuspensionTermsOfService SuspensionKind = "tos_violation"
	// SuspensionContactSupport reports a terms suspension routed to Google Cloud Support instead of
	// the self-service appeal form, delivered with reason TOS_VIOLATION_CONTACT_SUPPORT.
	SuspensionContactSupport SuspensionKind = "tos_violation_contact_support"
	// SuspensionValidationRequired reports an account that must be revalidated before serving again.
	SuspensionValidationRequired SuspensionKind = "validation_required"
	// SuspensionPreconditionFailed reports a suspension delivered as HTTP 412 FAILED_PRECONDITION
	// instead of the more common 403 PERMISSION_DENIED envelope.
	SuspensionPreconditionFailed SuspensionKind = "failed_precondition"
)

// Suspension captures one terminal upstream account-suspension verdict. Every field is derived from
// the upstream error envelope and is safe to persist and display: no token, prompt, or account
// secret is ever copied into it.
type Suspension struct {
	// Kind classifies the verdict so callers can branch without re-parsing the upstream body.
	Kind SuspensionKind
	// StatusCode is the HTTP status code that carried the verdict, retained for diagnostics.
	StatusCode int
	// Reason is the raw google.rpc.ErrorInfo reason token, empty for legacy text-only envelopes.
	Reason string
	// Domain is the google.rpc.ErrorInfo service domain, normally cloudcode-pa.googleapis.com.
	Domain string
	// Message is the upstream explanation shown to operators.
	Message string
	// AppealURL is the validated operator appeal link upstream returned, empty when absent or unsafe.
	AppealURL string
}

// suspensionReasonKinds maps google.rpc.ErrorInfo reason tokens to their terminal classification.
// The map stays explicit so an unknown reason never disables an account by itself.
var suspensionReasonKinds = map[string]SuspensionKind{
	"TOS_VIOLATION":                 SuspensionTermsOfService,
	"TOS_VIOLATION_CONTACT_SUPPORT": SuspensionContactSupport,
	"VALIDATION_REQUIRED":           SuspensionValidationRequired,
}

// appealURLHosts is the allowlist of hosts an upstream appeal link may use. Google's suspension
// envelope points at the shared appeal form, so anything else is treated as untrusted metadata and
// dropped instead of being persisted, bubbled up to a client, or rendered in a terminal.
var appealURLHosts = []string{"forms.gle", "docs.google.com"}

// appealURLMaxLength bounds the persisted appeal link so a hostile upstream body cannot inflate the
// credentials file with a huge string.
const appealURLMaxLength = 2048

// upstreamErrorEnvelope is the subset of Google's error envelope that carries a suspension verdict.
type upstreamErrorEnvelope struct {
	// Error wraps the Google error payload returned with a non-2xx response.
	Error struct {
		// Code mirrors the HTTP status code.
		Code int `json:"code"`
		// Message is the human-readable upstream explanation.
		Message string `json:"message"`
		// Status is the canonical google.rpc status name such as PERMISSION_DENIED.
		Status string `json:"status"`
		// Details carries google.rpc.ErrorInfo entries, including the reason and appeal metadata.
		Details []struct {
			// Type is the protobuf any type URL identifying the detail entry.
			Type string `json:"@type"`
			// Reason is the machine-readable cause token used for classification.
			Reason string `json:"reason"`
			// Domain names the service that produced the detail, normally cloudcode-pa.googleapis.com.
			Domain string `json:"domain"`
			// Metadata carries the operator-facing appeal link when upstream supplies one.
			Metadata map[string]string `json:"metadata"`
		} `json:"details"`
	} `json:"error"`
}

// DetectSuspension reports whether an upstream failure is a terminal account suspension instead of a
// transient permission, quota, or request error.
//
// Protocol Quirk: Cloud Code Assist disables an account for terms violations and answers every
// content-generation call with HTTP 403 PERMISSION_DENIED carrying details[].reason=TOS_VIOLATION, a
// cloudcode-pa.googleapis.com domain, and an appeal link in metadata. A rarer variant uses HTTP 412
// FAILED_PRECONDITION with a plain message. Either shape is account-scoped and permanent until Google
// lifts it, so retrying, cooling the account for 30 minutes, or rotating to a sibling account only
// keeps sending traffic to an account Google has already refused. Older deployments returned a
// text-only 403 with the same wording and no details[]; those markers are honored as a fallback.
func DetectSuspension(status int, body string) (*Suspension, bool) {
	if status != http.StatusForbidden && status != http.StatusPreconditionFailed {
		return nil, false
	}

	message := ExtractErrorMessage(body)

	var envelope upstreamErrorEnvelope
	// A malformed body is expected on some proxy paths, so a decode failure falls through to the
	// marker fallback below instead of being treated as a verdict.
	if err := json.Unmarshal([]byte(body), &envelope); err == nil {
		for _, detail := range envelope.Error.Details {
			kind, ok := suspensionReasonKinds[strings.ToUpper(strings.TrimSpace(detail.Reason))]
			if !ok {
				continue
			}
			return &Suspension{
				Kind:       kind,
				StatusCode: status,
				Reason:     detail.Reason,
				Domain:     detail.Domain,
				Message:    message,
				AppealURL:  sanitizeAppealURL(detail.Metadata["appeal_url"]),
			}, true
		}

		if status == http.StatusPreconditionFailed && strings.EqualFold(strings.TrimSpace(envelope.Error.Status), "FAILED_PRECONDITION") && mentionsSuspension(message) {
			return &Suspension{
				Kind:       SuspensionPreconditionFailed,
				StatusCode: status,
				Message:    message,
			}, true
		}
	}

	if status != http.StatusForbidden || !IsPermanentForbidden(body) {
		return nil, false
	}

	kind := SuspensionTermsOfService
	if strings.Contains(strings.ToLower(body), "validation_required") {
		kind = SuspensionValidationRequired
	}
	return &Suspension{Kind: kind, StatusCode: status, Message: message}, true
}

// mentionsSuspension reports whether an upstream message describes a suspended account. Matching is
// restricted to explicit suspension wording so an unrelated request precondition failure keeps its
// ordinary client-error handling.
func mentionsSuspension(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "suspend") || strings.Contains(lower, "terms of service")
}

// sanitizeAppealURL validates an upstream appeal link before it is persisted or shown to an operator.
// Metadata from an error body is untrusted: only HTTPS links on the documented appeal hosts survive,
// and embedded credentials or oversized values are dropped silently.
func sanitizeAppealURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > appealURLMaxLength {
		return ""
	}

	parsed, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.User != nil {
		return ""
	}
	// Google's appeal links never name a port, so an explicit one is a redirect-like trick rather than
	// metadata produced by the suspension envelope.
	if parsed.Port() != "" {
		return ""
	}
	if !slices.Contains(appealURLHosts, strings.ToLower(parsed.Hostname())) {
		return ""
	}
	return parsed.String()
}
