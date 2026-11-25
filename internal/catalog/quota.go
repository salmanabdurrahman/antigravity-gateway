// Package catalog discovers upstream model availability, limits, and live quota tracking.
package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// FetchAccountQuotaSummary queries `v1internal:retrieveUserQuotaSummary` for one account and returns
// the rolling five-hour and weekly allowance windows.
//
// Protocol Quirk: Google exposes the rolling and weekly allowances through a quota summary endpoint
// whose JSON field names vary between surfaces (`weeklyQuotaInfo`, `buckets`, `quotaWindow`, ...).
// The response is therefore decoded generically and windows are classified by their surrounding
// field names instead of a rigid struct, so an unexpected schema degrades to "no extra quota data"
// rather than failing the whole catalog refresh.
func FetchAccountQuotaSummary(ctx context.Context, cfg *config.Config, httpClient *http.Client, accessToken, projectID, tier string) (*AccountQuotaSummary, error) {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultFetchTimeout}
	}

	endpoints := config.DefaultEndpoints()
	if cfg != nil {
		endpoints = cfg.EndpointsForTier(tier)
	}

	reqPayload, err := json.Marshal(map[string]string{
		"project": projectID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling quota summary request: %w", err)
	}

	headers := config.BuildHTTPHeaders(cfg, accessToken, "")

	var lastErr error
	for _, endpoint := range endpoints {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		targetURL := fmt.Sprintf("%s/v1internal:retrieveUserQuotaSummary", strings.TrimRight(endpoint, "/"))
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(reqPayload))
		if err != nil {
			lastErr = fmt.Errorf("creating quota summary request for %s: %w", targetURL, err)
			continue
		}
		httpReq.Header = headers.Clone()

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("dispatching quota summary request to %s: %w", targetURL, err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("reading quota summary from %s: %w", targetURL, readErr)
			continue
		}

		if resp.StatusCode == http.StatusOK {
			return ParseAccountQuotaSummary(body), nil
		}

		errMsg := rotator.ExtractErrorMessage(string(body))
		if errMsg == "" {
			errMsg = http.StatusText(resp.StatusCode)
		}
		lastErr = fmt.Errorf("quota summary failed on %s (HTTP %d): %s", endpoint, resp.StatusCode, errMsg)

		// 404 means the surface does not expose the quota summary endpoint at all; 403/5xx are
		// endpoint-scoped for this account tier. All three fall through to the next endpoint.
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500 {
			continue
		}
		return nil, lastErr
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no quota summary endpoint reachable")
}

// ParseAccountQuotaSummary inspects a raw quota summary payload and extracts classified windows.
// Returns nil when the payload contains no recognizable quota window.
func ParseAccountQuotaSummary(data []byte) *AccountQuotaSummary {
	if len(data) == 0 {
		return nil
	}

	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil
	}

	summary := &AccountQuotaSummary{}
	walkQuotaWindows(decoded, "", summary)
	if summary.FiveHour == nil && summary.Weekly == nil {
		return nil
	}
	return summary
}

// walkQuotaWindows recursively scans decoded JSON for quota window objects and classifies each one.
func walkQuotaWindows(value any, path string, summary *AccountQuotaSummary) {
	switch v := value.(type) {
	case map[string]any:
		if fraction, reset, ok := quotaWindowFields(v); ok {
			window := QuotaWindow{ResetTime: reset}
			if fraction != nil {
				window.RemainingFraction = fraction
			}
			if !window.Untouched() {
				label := strings.ToLower(path + " " + providerText(v["window"]) + " " + providerText(v["type"]) + " " + providerText(v["name"]))
				weekly := strings.Contains(label, "week")
				fiveHour := strings.Contains(label, "five") ||
					strings.Contains(label, "5h") ||
					strings.Contains(label, "5-hour") ||
					strings.Contains(label, "rolling") ||
					strings.Contains(label, "daily") ||
					strings.Contains(label, "hour")

				switch {
				case weekly:
					summary.Weekly = mergeQuotaWindow(summary.Weekly, window)
				case fiveHour:
					summary.FiveHour = mergeQuotaWindow(summary.FiveHour, window)
				default:
					// A window without an explicit label is treated as the rolling window, which is
					// the default shape of model-level quotaInfo entries.
					summary.FiveHour = mergeQuotaWindow(summary.FiveHour, window)
				}
			}
		}

		for key, child := range v {
			nextPath := key
			if path != "" {
				nextPath = path + "." + key
			}
			walkQuotaWindows(child, nextPath, summary)
		}

	case []any:
		for index, child := range v {
			walkQuotaWindows(child, path+"["+strconv.Itoa(index)+"]", summary)
		}
	}
}

// quotaWindowFields extracts a numeric fraction and optional reset timestamp from a JSON object.
func quotaWindowFields(obj map[string]any) (*float64, string, bool) {
	var fraction *float64
	for _, key := range []string{"remainingFraction", "remaining_fraction"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		if parsed, ok := numericValue(raw); ok {
			clamped := max(0.0, min(1.0, parsed))
			fraction = &clamped
			break
		}
	}
	if fraction == nil {
		return nil, "", false
	}

	reset := ""
	for _, key := range []string{"resetTime", "reset_time", "resetAt"} {
		if raw, ok := obj[key].(string); ok && strings.TrimSpace(raw) != "" {
			reset = strings.TrimSpace(raw)
			break
		}
	}

	return fraction, reset, true
}

// numericValue converts a JSON scalar into a float64 when it represents a finite number.
func numericValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return 0, false
		}
		return v, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// mergeQuotaWindow keeps the most constrained fraction (and its reset time) across duplicate windows.
func mergeQuotaWindow(current *QuotaWindow, candidate QuotaWindow) *QuotaWindow {
	if current == nil {
		candidateCopy := candidate
		return &candidateCopy
	}
	if candidate.RemainingFraction == nil {
		if current.ResetTime == "" {
			current.ResetTime = candidate.ResetTime
		}
		return current
	}
	if current.RemainingFraction == nil || *candidate.RemainingFraction < *current.RemainingFraction {
		candidateCopy := candidate
		if candidate.ResetTime == "" {
			candidateCopy.ResetTime = current.ResetTime
		}
		return &candidateCopy
	}
	return current
}

// cloneModelQuotas deep-copies a per-model quota map, including fraction pointers and weekly windows.
func cloneModelQuotas(quotas map[string]ModelQuota) map[string]ModelQuota {
	if quotas == nil {
		return nil
	}
	cloned := make(map[string]ModelQuota, len(quotas))
	for id, quota := range quotas {
		copyQuota := ModelQuota{ResetTime: quota.ResetTime}
		if quota.RemainingFraction != nil {
			fraction := *quota.RemainingFraction
			copyQuota.RemainingFraction = &fraction
		}
		copyQuota.Weekly = cloneQuotaWindow(quota.Weekly)
		cloned[id] = copyQuota
	}
	return cloned
}

// cloneQuotaWindowSummary returns a deep copy of an account quota summary.
func cloneQuotaWindowSummary(summary *AccountQuotaSummary) *AccountQuotaSummary {
	if summary == nil {
		return nil
	}
	return &AccountQuotaSummary{
		FiveHour: cloneQuotaWindow(summary.FiveHour),
		Weekly:   cloneQuotaWindow(summary.Weekly),
	}
}

// MergeQuotaSummary combines two account quota summaries, keeping the most constrained windows.
func MergeQuotaSummary(dst *AccountQuotaSummary, src *AccountQuotaSummary) *AccountQuotaSummary {
	if src == nil {
		return dst
	}
	if dst == nil {
		return &AccountQuotaSummary{
			FiveHour: cloneQuotaWindow(src.FiveHour),
			Weekly:   cloneQuotaWindow(src.Weekly),
		}
	}
	if src.FiveHour != nil {
		dst.FiveHour = mergeQuotaWindow(dst.FiveHour, *src.FiveHour)
	}
	if src.Weekly != nil {
		dst.Weekly = mergeQuotaWindow(dst.Weekly, *src.Weekly)
	}
	return dst
}

// cloneQuotaWindow returns a deep copy of a quota window so callers never share pointer state.
func cloneQuotaWindow(window *QuotaWindow) *QuotaWindow {
	if window == nil {
		return nil
	}
	copied := *window
	if window.RemainingFraction != nil {
		fraction := *window.RemainingFraction
		copied.RemainingFraction = &fraction
	}
	return &copied
}
