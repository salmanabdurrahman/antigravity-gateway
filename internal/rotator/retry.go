// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FailureKind classifies HTTP failure responses from Google Cloud Code Assist.
type FailureKind string

const (
	// FailureRateLimit indicates HTTP 429 or quota exhaustion (RESOURCE_EXHAUSTED) carrying
	// explicit quota metadata (details[], retryDelay, quota reset time, or Retry-After).
	FailureRateLimit FailureKind = "rate_limit"
	// FailurePromptRejection indicates a bare HTTP 429 / RESOURCE_EXHAUSTED rejection without
	// any quota metadata. Upstream returns this shape when it fingerprints prompt content
	// instead of exhausting account quota, so it must never trigger pooled account cooldowns.
	FailurePromptRejection FailureKind = "prompt_rejection"
	// FailureAuth indicates HTTP 401 unauthenticated or expired token.
	FailureAuth FailureKind = "auth"
	// FailureForbidden indicates HTTP 403 access denied or project not allowed.
	FailureForbidden FailureKind = "forbidden"
	// FailureServer indicates 5xx error or transient upstream unavailability.
	FailureServer FailureKind = "server"
	// FailureBadRequest indicates HTTP 400 client-side malformed payload; terminal error.
	FailureBadRequest FailureKind = "bad_request"
	// FailureClient represents general 4xx client errors that should not trigger failover.
	FailureClient FailureKind = "client"
)

var (
	reResourceExhausted = regexp.MustCompile(`(?i)RESOURCE_EXHAUSTED`)
	reOverloaded        = regexp.MustCompile(`(?i)overloaded|unavailable`)
	// "Your quota will reset after 18h31m10s" / "after 39s"
	reResetAfter = regexp.MustCompile(`(?i)reset after (?:(\d+)h)?(?:(\d+)m)?(\d+(?:\.\d+)?)s`)
	// "resetTime": "2026-11-14T15:04:05Z" or "reset_time": "..."
	reResetTime = regexp.MustCompile(`(?i)"reset_?time"\s*:\s*"([^"]+)"`)
	// "Please retry in 250ms" / "in 10s"
	reRetryIn = regexp.MustCompile(`(?i)Please retry in ([0-9.]+)(ms|s)\b`)
	// google.rpc.RetryInfo: "retryDelay": "34.07s"
	reRetryDelay = regexp.MustCompile(`(?i)"retryDelay"\s*:\s*"([0-9.]+)(ms|s)"`)
	// google.rpc.ErrorInfo metadata: "quotaResetDelay": "1h2m3s"
	reQuotaReset = regexp.MustCompile(`(?i)"quotaResetDelay"\s*:\s*"(?:(\d+)h)?(?:(\d+)m)?([0-9.]+)?(ms|s)?"`)
)

// Classify inspects an HTTP status code and response body to categorize the upstream error.
// Retry-After headers are not visible to this helper; use ClassifyWithHeaders when the response
// headers are available so bare prompt rejections are distinguished from quota exhaustion.
func Classify(status int, body string) FailureKind {
	return ClassifyWithHeaders(status, body, nil)
}

// ClassifyWithHeaders inspects an HTTP status code, response body, and headers to categorize
// the upstream error.
//
// Protocol Quirk: Cloud Code Assist answers a prompt it fingerprints with a bare HTTP 429
// RESOURCE_EXHAUSTED body (message only, no details[]) even while account quota is full.
// Treating that as quota exhaustion cools every healthy account in the pool, so a 429 without
// quota metadata is classified as a request-scoped prompt rejection instead.
func ClassifyWithHeaders(status int, body string, headers http.Header) FailureKind {
	if status == http.StatusTooManyRequests || reResourceExhausted.MatchString(body) {
		// Only the documented RESOURCE_EXHAUSTED fingerprint is ambiguous: Cloud Code returns
		// that status for content-filtered prompts even while quota remains. Generic 429s still
		// follow quota cooldown and failover semantics.
		if reResourceExhausted.MatchString(body) && isBarePromptRejection(body, headers) {
			return FailurePromptRejection
		}
		return FailureRateLimit
	}
	if status == http.StatusUnauthorized {
		return FailureAuth
	}
	if status == http.StatusForbidden {
		return FailureForbidden
	}
	if status == http.StatusBadRequest {
		return FailureBadRequest
	}
	if status >= 500 || status == http.StatusRequestTimeout {
		return FailureServer
	}
	if reOverloaded.MatchString(body) {
		return FailureServer
	}
	return FailureClient
}

// isBarePromptRejection reports whether a 429 / RESOURCE_EXHAUSTED response lacks every quota
// signal: Retry-After header, error details[] array, retry delay, and quota reset timestamps.
func isBarePromptRejection(body string, headers http.Header) bool {
	if headers != nil && strings.TrimSpace(headers.Get("Retry-After")) != "" {
		return false
	}

	lower := strings.ToLower(body)
	for _, marker := range []string{
		"\"details\"",
		"retrydelay",
		"quotaresetdelay",
		"quotaresettimestamp",
		"quotaresettime",
		"reset_time",
		"reset after",
		"retry in",
	} {
		if strings.Contains(lower, marker) {
			return false
		}
	}

	// Absolute reset timestamps are covered by the markers above; this final probe rejects any
	// remaining textual delay shape before falling back to a prompt rejection.
	if _, ok := ParseRetryDelay(body, headers, time.Now()); ok {
		return false
	}

	return true
}

// ParseRetryDelay extracts backoff duration from HTTP headers (Retry-After) or Google's error body.
// Returns 0 and false if no explicit retry delay was found.
func ParseRetryDelay(body string, headers http.Header, now time.Time) (time.Duration, bool) {
	if headers != nil {
		retryAfter := headers.Get("Retry-After")
		if retryAfter != "" {
			if sec, err := strconv.ParseFloat(retryAfter, 64); err == nil && sec > 0 {
				return time.Duration(sec * float64(time.Second)), true
			}
			if t, err := http.ParseTime(retryAfter); err == nil && t.After(now) {
				return t.Sub(now), true
			}
		}
	}

	if match := reResetAfter.FindStringSubmatch(body); len(match) > 0 {
		h, _ := strconv.ParseFloat(match[1], 64)
		m, _ := strconv.ParseFloat(match[2], 64)
		s, _ := strconv.ParseFloat(match[3], 64)
		totalSec := h*3600 + m*60 + s
		if totalSec > 0 {
			return time.Duration(totalSec * float64(time.Second)), true
		}
	}

	if match := reResetTime.FindStringSubmatch(body); len(match) > 0 {
		if t, err := time.Parse(time.RFC3339Nano, match[1]); err == nil && t.After(now) {
			return t.Sub(now), true
		}
		if t, err := time.Parse(time.RFC3339, match[1]); err == nil && t.After(now) {
			return t.Sub(now), true
		}
	}

	if match := reRetryIn.FindStringSubmatch(body); len(match) > 0 {
		val, _ := strconv.ParseFloat(match[1], 64)
		unit := match[2]
		if d := parseDurationUnit(val, unit); d > 0 {
			return d, true
		}
	}

	if match := reRetryDelay.FindStringSubmatch(body); len(match) > 0 {
		val, _ := strconv.ParseFloat(match[1], 64)
		unit := match[2]
		if d := parseDurationUnit(val, unit); d > 0 {
			return d, true
		}
	}

	if match := reQuotaReset.FindStringSubmatch(body); len(match) > 0 {
		h, _ := strconv.ParseFloat(match[1], 64)
		m, _ := strconv.ParseFloat(match[2], 64)
		secVal, _ := strconv.ParseFloat(match[3], 64)
		unit := match[4]
		total := time.Duration(h)*time.Hour + time.Duration(m)*time.Minute
		if secVal > 0 {
			total += parseDurationUnit(secVal, unit)
		}
		if total > 0 {
			return total, true
		}
	}

	return 0, false
}

// parseDurationUnit converts a floating-point magnitude and time unit ("ms", "s") into a time.Duration.
func parseDurationUnit(val float64, unit string) time.Duration {
	if strings.EqualFold(unit, "ms") {
		return time.Duration(val * float64(time.Millisecond))
	}
	return time.Duration(val * float64(time.Second))
}

// ExtractErrorMessage extracts the human-readable message from a Google Cloud Code Assist error JSON body.
func ExtractErrorMessage(body string) string {
	var single struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &single); err == nil && single.Error.Message != "" {
		return single.Error.Message
	}

	var arr []struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &arr); err == nil && len(arr) > 0 && arr[0].Error.Message != "" {
		return arr[0].Error.Message
	}

	runes := []rune(body)
	if len(runes) > 500 {
		return string(runes[:500])
	}
	return body
}

// CooldownTracker manages thread-safe per-model and wildcard expiration timestamps.
type CooldownTracker struct {
	// mu protects concurrent access to the cooldowns map.
	mu sync.RWMutex

	// cooldowns maps model names (or "*") to expiration timestamps.
	cooldowns map[string]time.Time
}

// NewCooldownTracker constructs an empty cooldown tracking map.
func NewCooldownTracker() *CooldownTracker {
	return &CooldownTracker{
		cooldowns: make(map[string]time.Time),
	}
}

// SetCooldown sets the cooldown expiration for a specific model or wildcard ("*") and prunes expired entries.
func (c *CooldownTracker) SetCooldown(model string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cooldowns == nil {
		c.cooldowns = make(map[string]time.Time)
	}
	now := time.Now()
	for k, v := range c.cooldowns {
		if !v.IsZero() && v.Before(now) {
			delete(c.cooldowns, k)
		}
	}
	c.cooldowns[model] = until
}

// CoolingUntil returns the latest time until which the account is cooling for the requested model.
// Checks both model-specific and wildcard ("*") cooldowns.
func (c *CooldownTracker) CoolingUntil(model string) time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cooldowns == nil {
		return time.Time{}
	}
	var maxUntil time.Time
	if t, ok := c.cooldowns[model]; ok && t.After(maxUntil) {
		maxUntil = t
	}
	if t, ok := c.cooldowns["*"]; ok && t.After(maxUntil) {
		maxUntil = t
	}
	return maxUntil
}

// IsCooling checks if the account is currently cooling down for the specified model.
func (c *CooldownTracker) IsCooling(model string, now time.Time) bool {
	until := c.CoolingUntil(model)
	return !until.IsZero() && until.After(now)
}

// ActiveCooldowns returns all active cooldowns as map of model -> expiration time.
func (c *CooldownTracker) ActiveCooldowns(now time.Time) map[string]time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	active := make(map[string]time.Time)
	for m, until := range c.cooldowns {
		if until.After(now) {
			active[m] = until
		}
	}
	return active
}
