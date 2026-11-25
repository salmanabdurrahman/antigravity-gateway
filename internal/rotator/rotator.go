// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// RotatorError represents a domain error when dispatching across accounts in the rotator.
type RotatorError struct {
	// StatusCode is the HTTP response status code corresponding to this error.
	StatusCode int

	// RetryAfter is the suggested backoff duration parsed from upstream headers or error bodies.
	RetryAfter time.Duration

	// ErrorMessage is the contextual error message detailing the failure reason.
	ErrorMessage string

	// PromptRejected reports a request-scoped prompt rejection (bare RESOURCE_EXHAUSTED) rather
	// than account quota exhaustion. Callers may retry once with the system instruction moved
	// into the first user turn before surfacing the error.
	PromptRejected bool
}

// Error formats the RotatorError into a descriptive error string.
func (e *RotatorError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("rotator error (HTTP %d, retry after %s): %s", e.StatusCode, e.RetryAfter, e.ErrorMessage)
	}
	return fmt.Sprintf("rotator error (HTTP %d): %s", e.StatusCode, e.ErrorMessage)
}

// AttemptResult captures the successful upstream HTTP response along with the account state that served it.
type AttemptResult struct {
	// Response is the active upstream HTTP response handle.
	Response *http.Response

	// State identifies the specific AccountState that completed the request.
	State *AccountState
}

// RequestPayloadBuilder is a callback that generates the raw JSON request body tailored to a chosen account.
// This allows embedding the account's companion ProjectID into the Google Cloud Code envelope.
type RequestPayloadBuilder func(state *AccountState) (any, error)

// Rotator orchestrates account picking, transparent 429 failover, cooldown tracking, and token refresh.
type Rotator struct {
	// mu protects mutable fields such as the HTTP client and sleep function.
	mu sync.RWMutex

	// pool manages the collection of active accounts and their token caches.
	pool *Pool

	// picker selects candidate accounts according to the configured Strategy.
	picker *Picker

	// cfg holds application-level settings and fallback endpoints.
	cfg *config.Config

	// client is the HTTP client used to dispatch upstream requests.
	client *http.Client

	// logger is the structured slog instance used for operational diagnostics.
	logger *slog.Logger

	// nowFn provides the current time, injectable for deterministic testing.
	nowFn func() time.Time

	// sleepFn executes backoff pauses, injectable for deterministic testing.
	sleepFn func(ctx context.Context, d time.Duration) error

	// maxWait is the maximum cooldown wait duration agw will absorb before failing.
	maxWait time.Duration
}

// NewRotator creates a configured Rotator coordinating accounts from the provided pool.
func NewRotator(pool *Pool, cfg *config.Config, logger *slog.Logger, nowFn func() time.Time) *Rotator {
	if nowFn == nil {
		nowFn = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	strat := StrategySticky
	if cfg != nil && cfg.Strategy != "" {
		strat = Strategy(cfg.Strategy)
	}

	picker := NewPicker(strat)
	if cfg != nil && cfg.QuotaThreshold > 0 {
		picker.SetQuotaThreshold(cfg.QuotaThreshold)
	}
	if pool != nil {
		pool.SetHealthTracker(picker.HealthTracker())
	}

	return &Rotator{
		pool:    pool,
		picker:  picker,
		cfg:     cfg,
		client:  config.DefaultHTTPClient(0), // Stream requests can be long-running
		logger:  logger,
		nowFn:   nowFn,
		sleepFn: defaultSleep,
		maxWait: MaxWaitDuration,
	}
}

// defaultSleep pauses execution for duration d while respecting context cancellation.
func defaultSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// SetMaxWait configures the maximum wait duration agw will absorb when all accounts are cooling down.
func (r *Rotator) SetMaxWait(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxWait = d
}

// SetHTTPClient configures a custom HTTP client (useful for unit testing with httptest).
func (r *Rotator) SetHTTPClient(client *http.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.client = client
}

// sleep pauses execution for duration d using the configured sleep function under read lock.
func (r *Rotator) sleep(ctx context.Context, d time.Duration) error {
	r.mu.RLock()
	fn := r.sleepFn
	r.mu.RUnlock()
	if fn != nil {
		return fn(ctx, d)
	}
	return defaultSleep(ctx, d)
}

// SetSleepFn configures a custom sleep handler for deterministic testing.
func (r *Rotator) SetSleepFn(fn func(ctx context.Context, d time.Duration) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sleepFn = fn
}

// Pool returns the underlying account pool.
func (r *Rotator) Pool() *Pool {
	return r.pool
}

// Strategy returns the active account selection strategy.
func (r *Rotator) Strategy() Strategy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.picker.Strategy()
}

// SetStrategy updates the active account selection strategy.
func (r *Rotator) SetStrategy(s Strategy) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.picker.SetStrategy(s)
}

// WithAccount executes a read or metadata operation against any non-cooling account (e.g. catalog fetch).
func (r *Rotator) WithAccount(ctx context.Context, fn func(ctx context.Context, token string, state *AccountState) error) error {
	if r == nil {
		return errors.New("rotator is nil")
	}
	if err := r.pool.RefreshIfChanged(); err != nil {
		r.logger.Warn("failed checking accounts file reload", "error", err)
	}

	states := r.pool.States()
	usable := slices.DeleteFunc(slices.Clone(states), func(s *AccountState) bool {
		return !s.Usable()
	})

	if len(usable) == 0 {
		return &RotatorError{
			StatusCode:   http.StatusServiceUnavailable,
			ErrorMessage: "No usable Google accounts. Run: agw login",
		}
	}

	var lastErr error
	for _, s := range usable {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		token, err := s.Token(ctx, r.cfg, r.nowFn)
		if err != nil {
			var oauthErr *auth.OAuthError
			if errors.As(err, &oauthErr) && oauthErr.InvalidGrant {
				_ = r.pool.DisableAccount(s.Email(), "refresh token revoked")
			}
			lastErr = err
			continue
		}

		err = fn(ctx, token, s)
		if err == nil {
			return nil
		}
		lastErr = err

		var rotErr *RotatorError
		if errors.As(err, &rotErr) && rotErr.StatusCode == http.StatusUnauthorized {
			s.ClearAccessToken()
		}
	}

	if lastErr != nil {
		return lastErr
	}
	return &RotatorError{
		StatusCode:   http.StatusServiceUnavailable,
		ErrorMessage: "All accounts failed to execute operation",
	}
}

// Generate dispatches a request to Google Cloud Code Assist, automatically handling rate limits,
// token refresh, cooldowns, and seamless account rotation.
func (r *Rotator) Generate(ctx context.Context, model string, builder RequestPayloadBuilder) (*AttemptResult, error) {
	if err := r.pool.RefreshIfChanged(); err != nil {
		r.logger.Warn("failed checking accounts file reload", "error", err)
	}

	states := r.pool.States()
	hasUsable := false
	for _, s := range states {
		if s.Usable() {
			hasUsable = true
			break
		}
	}

	if !hasUsable {
		return nil, &RotatorError{
			StatusCode:   http.StatusServiceUnavailable,
			ErrorMessage: "No usable Google accounts. Run: agw login",
		}
	}

	skip := make(map[*AccountState]struct{})
	retried401 := make(map[*AccountState]struct{})
	var lastRotErr *RotatorError
	var waited time.Duration
	serverErrors := 0

	r.mu.RLock()
	maxWait := r.maxWait
	r.mu.RUnlock()

	maxAttempts := len(states)*2 + 2

	for attempt := range maxAttempts {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		now := r.nowFn()
		r.mu.Lock()
		pick := r.picker.Pick(model, states, skip, now)
		r.mu.Unlock()

		if pick.NoAcct {
			break
		}

		if pick.Wait > 0 {
			if waited+pick.Wait > maxWait {
				return nil, &RotatorError{
					StatusCode:   http.StatusTooManyRequests,
					RetryAfter:   pick.Wait,
					ErrorMessage: fmt.Sprintf("All accounts are rate limited for %s. Next account frees up in %s.", model, pick.Wait.Round(time.Second)),
				}
			}

			r.logger.Info("All accounts cooling; absorbing wait duration", "model", model, "wait", pick.Wait)
			if err := r.sleep(ctx, pick.Wait); err != nil {
				return nil, err
			}
			waited += pick.Wait
			continue
		}

		s := pick.State
		token, err := s.Token(ctx, r.cfg, r.nowFn)
		if err != nil {
			var oauthErr *auth.OAuthError
			if errors.As(err, &oauthErr) && oauthErr.InvalidGrant {
				_ = r.pool.DisableAccount(s.Email(), "refresh token revoked")
			} else {
				s.SetCooldown("*", now.Add(DefaultCooldownDuration), err.Error())
			}
			skip[s] = struct{}{}
			lastRotErr = &RotatorError{
				StatusCode:   http.StatusBadGateway,
				ErrorMessage: fmt.Sprintf("Token refresh failed for %s: %s", s.Email(), err.Error()),
			}
			continue
		}

		payload, err := builder(s)
		if err != nil {
			return nil, fmt.Errorf("building request payload for account %s: %w", s.Email(), err)
		}

		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("marshaling request payload: %w", err)
		}

		resp, err := r.post(ctx, model, token, bodyBytes)
		s.requests.Add(1)

		if err != nil {
			r.logger.Warn("Upstream network call failed", "account", s.Email(), "error", err)
			serverErrors++
			r.picker.HealthTracker().RecordFailure(s.Email())
			s.TokenBucket().Refund()
			skip[s] = struct{}{}
			lastRotErr = &RotatorError{
				StatusCode:   http.StatusBadGateway,
				ErrorMessage: fmt.Sprintf("Upstream network error: %s", err.Error()),
			}
			if serverErrors >= 3 {
				break
			}
			_ = r.sleep(ctx, time.Duration(500*serverErrors)*time.Millisecond)
			continue
		}

		// Success!
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			r.picker.HealthTracker().RecordSuccess(s.Email())
			return &AttemptResult{Response: resp, State: s}, nil
		}

		// Read error body bounded to 1MB
		errBodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		errBody := string(errBodyBytes)

		kind := ClassifyWithHeaders(resp.StatusCode, errBody, resp.Header)
		errMsg := ExtractErrorMessage(errBody)

		switch kind {
		case FailurePromptRejection:
			// Request-scoped rejection: the prompt triggered upstream content filtering, so the
			// account stays healthy and must not receive a pooled cooldown.
			s.TokenBucket().Refund()
			r.logger.Warn("Prompt rejected by upstream without quota metadata", "account", s.Email(), "model", model)
			return nil, &RotatorError{
				StatusCode:     http.StatusBadRequest,
				ErrorMessage:   fmt.Sprintf("Google rejected the prompt: %s", errMsg),
				PromptRejected: true,
			}

		case FailureRateLimit:
			r.picker.HealthTracker().RecordRateLimit(s.Email())
			s.rateLimits.Add(1)
			delay, ok := ParseRetryDelay(errBody, resp.Header, r.nowFn())
			if !ok {
				delay = DefaultCooldownDuration
			}
			s.SetCooldown(model, r.nowFn().Add(delay), fmt.Sprintf("HTTP 429: %s", errMsg))
			lastRotErr = &RotatorError{
				StatusCode:   http.StatusTooManyRequests,
				RetryAfter:   delay,
				ErrorMessage: fmt.Sprintf("Rate limited: %s", errMsg),
			}
			r.logger.Warn("Account rate limited (429)", "account", s.Email(), "model", model, "cooldown", delay)

		case FailureAuth:
			if _, alreadyRetried := retried401[s]; !alreadyRetried {
				retried401[s] = struct{}{}
				s.ClearAccessToken()
				r.logger.Info("Retrying account after 401 token invalidation", "account", s.Email())
				continue
			}
			// Repeated auth failure: apply forbidden cooldown
			s.SetCooldown("*", r.nowFn().Add(ForbiddenCooldownDuration), fmt.Sprintf("HTTP 401: %s", errMsg))
			lastRotErr = &RotatorError{
				StatusCode:   http.StatusUnauthorized,
				ErrorMessage: fmt.Sprintf("Google auth refused %s: %s", s.Email(), errMsg),
			}
			skip[s] = struct{}{}

		case FailureForbidden:
			r.picker.HealthTracker().RecordFailure(s.Email())
			s.TokenBucket().Refund()
			s.SetCooldown("*", r.nowFn().Add(ForbiddenCooldownDuration), fmt.Sprintf("HTTP 403: %s", errMsg))
			lastRotErr = &RotatorError{
				StatusCode:   http.StatusForbidden,
				ErrorMessage: fmt.Sprintf("Google forbidden %s: %s", s.Email(), errMsg),
			}
			skip[s] = struct{}{}

		case FailureServer:
			r.picker.HealthTracker().RecordFailure(s.Email())
			s.TokenBucket().Refund()
			serverErrors++
			lastRotErr = &RotatorError{
				StatusCode:   http.StatusBadGateway,
				ErrorMessage: fmt.Sprintf("Google server error (HTTP %d): %s", resp.StatusCode, errMsg),
			}
			// On persistent server error, skip current account and fall back to next account
			skip[s] = struct{}{}
			if serverErrors >= 3 {
				break
			}
			_ = r.sleep(ctx, time.Duration(500*serverErrors)*time.Millisecond)

		case FailureBadRequest:
			// Malformed payloads fail identically on all accounts. Terminal error; do not rotate.
			return nil, &RotatorError{
				StatusCode:   http.StatusBadRequest,
				ErrorMessage: errMsg,
			}

		default:
			return nil, &RotatorError{
				StatusCode:   resp.StatusCode,
				ErrorMessage: errMsg,
			}
		}

		_ = attempt // continue rotation
	}

	// If no account could serve the model, compute soonest wait time for Retry-After header
	r.mu.RLock()
	soonestWait := r.picker.SoonestWait(model, states, r.nowFn())
	r.mu.RUnlock()

	retryAfter := time.Duration(0)
	if soonestWait > 0 {
		retryAfter = soonestWait
	} else if lastRotErr != nil {
		retryAfter = lastRotErr.RetryAfter
	}

	if lastRotErr != nil {
		if lastRotErr.StatusCode == http.StatusTooManyRequests || lastRotErr.StatusCode == http.StatusServiceUnavailable {
			lastRotErr.RetryAfter = retryAfter
		}
		return nil, lastRotErr
	}

	return nil, &RotatorError{
		StatusCode:   http.StatusServiceUnavailable,
		RetryAfter:   retryAfter,
		ErrorMessage: fmt.Sprintf("No usable account could serve %s", model),
	}
}

// post dispatches an HTTP POST request to Google Cloud Code Assist, cycling through configured endpoints on 5xx errors.
func (r *Rotator) post(ctx context.Context, model, token string, body []byte) (*http.Response, error) {
	endpoints := config.DefaultEndpoints()
	if r.cfg != nil && len(r.cfg.Endpoints) > 0 {
		endpoints = r.cfg.Endpoints
	}

	var lastErr error
	var lastResp *http.Response

	for i, endpoint := range endpoints {
		isLast := i == len(endpoints)-1
		url := fmt.Sprintf("%s/v1internal:streamGenerateContent?alt=sse", endpoint)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("creating http request: %w", err)
		}

		headers := config.BuildHTTPHeaders(r.cfg, token, model)
		for k, v := range headers {
			req.Header[k] = v
		}
		req.Header.Set("Accept", "text/event-stream")

		r.mu.RLock()
		client := r.client
		r.mu.RUnlock()

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		lastResp = resp
		// If endpoint returns 5xx, 403, or 404 and fallback endpoints exist, try next endpoint
		if !isLast && (resp.StatusCode >= 500 || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound) {
			_ = resp.Body.Close()
			continue
		}
		return resp, nil
	}

	if lastResp != nil {
		return lastResp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("no upstream endpoint reachable")
}
