// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
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

	// Suspension carries the terminal upstream account-suspension verdict when the failure is one,
	// including the validated appeal link. Callers surface it instead of applying generic cooldowns.
	Suspension *Suspension
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

	// client is the HTTP client used to dispatch upstream requests when a caller pins one.
	client *http.Client

	// clientPinned records that the caller supplied an explicit client (tests, embedding). A pinned
	// client bypasses per-credential wire selection so behaviour stays deterministic.
	clientPinned bool

	// logger is the structured slog instance used for operational diagnostics.
	logger *slog.Logger

	// nowFn provides the current time, injectable for deterministic testing.
	nowFn func() time.Time

	// sleepFn executes backoff pauses, injectable for deterministic testing.
	sleepFn func(ctx context.Context, d time.Duration) error

	// maxWait is the maximum cooldown wait duration agw will absorb before failing.
	maxWait time.Duration

	// cooldown is the base 429 backoff applied when upstream omits an explicit retry delay.
	cooldown time.Duration

	// suspendPause is how long all upstream dispatch stays paused after a terminal suspension verdict.
	suspendPause time.Duration

	// suspendUntil is the wall-clock end of the current protective pause, zero when none is armed.
	suspendUntil time.Time

	// lastSuspension remembers the verdict that armed the current pause so callers can surface the
	// upstream appeal link instead of only a generic pause message.
	lastSuspension *Suspension

	// rotationMode records whether dispatch may spread across the pool or must stay on one primary
	// account. The safe operating profile resolves to single-account dispatch.
	rotationMode string

	// pacer bounds concurrent upstream streams per credential.
	pacer *Pacer

	// rejectionLimit is how many bare RESOURCE_EXHAUSTED rejections inside the guard window park an
	// account; zero disables the guard.
	rejectionLimit int

	// rejectionCooldown is how long the guard parks an account that keeps drawing bare rejections.
	rejectionCooldown time.Duration
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

	maxWait := config.DefaultConfig().MaxWaitDuration()
	cooldown := config.DefaultConfig().CooldownDuration()
	suspendPause := config.DefaultConfig().SuspendPauseDuration()
	rotationMode := config.DefaultConfig().EffectiveRotationMode()
	maxInFlight := config.DefaultConfig().MaxInFlightPerAccountLimit()
	rejectionLimit, rejectionCooldown := config.DefaultConfig().PromptRejectionGuard()
	if cfg != nil {
		maxWait = cfg.MaxWaitDuration()
		cooldown = cfg.CooldownDuration()
		suspendPause = cfg.SuspendPauseDuration()
		rotationMode = cfg.EffectiveRotationMode()
		maxInFlight = cfg.MaxInFlightPerAccountLimit()
		rejectionLimit, rejectionCooldown = cfg.PromptRejectionGuard()
	}

	return &Rotator{
		pool:              pool,
		picker:            picker,
		cfg:               cfg,
		client:            config.DefaultHTTPClient(0), // Stream requests can be long-running
		logger:            logger,
		nowFn:             nowFn,
		sleepFn:           defaultSleep,
		maxWait:           maxWait,
		cooldown:          cooldown,
		suspendPause:      suspendPause,
		rotationMode:      rotationMode,
		pacer:             NewPacer(maxInFlight),
		rejectionLimit:    rejectionLimit,
		rejectionCooldown: rejectionCooldown,
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

// primaryRotationStates narrows a state list to the single account the safe profile serves requests
// from: the first usable entry in pool order. It returns nil when nothing is usable so callers keep
// their existing empty-pool handling.
func primaryRotationStates(states []*AccountState) []*AccountState {
	for _, s := range states {
		if s.Usable() {
			return []*AccountState{s}
		}
	}
	return nil
}

// SetSuspendPause configures how long upstream dispatch stays paused after a terminal suspension
// verdict. A zero duration disables the protective pause, so the pool fails over immediately.
func (r *Rotator) SetSuspendPause(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.suspendPause = d
}

// dispatchBudget resolves how many upstream dispatches this request may perform. The safe profile caps
// the legacy attempt budget so a failing request cannot cycle the pool.
func (r *Rotator) dispatchBudget(legacy int) int {
	if r.cfg == nil {
		return legacy
	}
	return r.cfg.DispatchBudget(legacy)
}

// suspensionPauseError reports the remaining protective pause after an upstream suspension verdict.
// It returns nil when no pause is armed or it already expired, and clears expired state on the way out
// so a long-running daemon does not carry a stale verdict forever. The common case takes only the read
// lock because this check sits on the dispatch hot path of every request.
func (r *Rotator) suspensionPauseError() *RotatorError {
	r.mu.RLock()
	until := r.suspendUntil
	suspension := r.lastSuspension
	r.mu.RUnlock()

	if until.IsZero() {
		return nil
	}

	remaining := until.Sub(r.nowFn())
	if remaining <= 0 {
		r.mu.Lock()
		// Re-check under the write lock: a suspension may have re-armed the pause between the read and
		// the upgrade, and clearing it here would drop a fresh verdict.
		if r.suspendUntil.Equal(until) {
			r.suspendUntil = time.Time{}
			r.lastSuspension = nil
		}
		r.mu.Unlock()
		return nil
	}

	return &RotatorError{
		StatusCode:   http.StatusForbidden,
		RetryAfter:   remaining,
		ErrorMessage: suspensionPauseMessage(suspension, remaining),
		Suspension:   suspension,
	}
}

// armSuspensionPause starts the protective pause for pause after a suspension verdict, remembering the
// verdict so every paused request carries the upstream appeal link.
func (r *Rotator) armSuspensionPause(pause time.Duration, suspension *Suspension) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.suspendUntil = r.nowFn().Add(pause)
	r.lastSuspension = suspension
}

// suspensionPauseMessage formats the operator-facing explanation for a paused gateway.
func suspensionPauseMessage(suspension *Suspension, remaining time.Duration) string {
	message := fmt.Sprintf("Google suspended an account, so the gateway paused all upstream traffic for %s. Review the account before continuing.", remaining.Round(time.Second))
	if suspension != nil && suspension.AppealURL != "" {
		message = fmt.Sprintf("%s Appeal: %s", message, suspension.AppealURL)
	}
	return message
}

// suspensionError builds the terminal error returned when upstream suspends an account, carrying the
// verdict so callers can surface the appeal link instead of a generic permission error.
func suspensionError(email string, suspension *Suspension) *RotatorError {
	detail := cmp.Or(strings.TrimSpace(suspension.Message), "no upstream detail")
	message := fmt.Sprintf("Google suspended %s (%s): %s", email, suspension.Kind, detail)
	if suspension.AppealURL != "" {
		message = fmt.Sprintf("%s Appeal: %s", message, suspension.AppealURL)
	}
	return &RotatorError{
		StatusCode:   http.StatusForbidden,
		ErrorMessage: message,
		Suspension:   suspension,
	}
}

// progressiveCooldown returns the backoff for the given consecutive rate-limit streak.
// Google does not always include RetryInfo, so repeated 429 responses escalate
// 60s -> 5m -> 30m -> 2h instead of hammering the endpoint every minute.
func (r *Rotator) progressiveCooldown(streak int64) time.Duration {
	r.mu.RLock()
	base := r.cooldown
	r.mu.RUnlock()
	if base <= 0 {
		base = DefaultCooldownDuration
	}

	multipliers := []time.Duration{1, 5, 30, 120}
	tier := min(max(streak, 1), int64(len(multipliers))) - 1
	return base * multipliers[tier]
}

// refundToken returns a hybrid token-bucket token consumed during account selection. Sticky and
// round-robin strategies never consume a token, so refunding unconditionally would inflate their
// buckets beyond capacity.
func (r *Rotator) refundToken(state *AccountState, charged bool) {
	if state == nil || !charged {
		return
	}
	if r.Strategy() != StrategyHybrid {
		return
	}
	state.TokenBucket().Refund()
}

// SetHTTPClient configures a custom HTTP client (useful for unit testing with httptest).
func (r *Rotator) SetHTTPClient(client *http.Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.client = client
	r.clientPinned = client != nil
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

// ForEachAccount executes fn against every usable account, collecting errors instead of stopping at
// the first success. Catalog refresh uses it to aggregate per-account quotas; callers that only need
// one account should keep using WithAccount.
func (r *Rotator) ForEachAccount(ctx context.Context, fn func(ctx context.Context, token string, state *AccountState) error) error {
	if r == nil {
		return errors.New("rotator is nil")
	}
	if err := r.pool.RefreshIfChanged(); err != nil {
		r.logger.Warn("failed checking accounts file reload", "error", err)
	}

	states := slices.DeleteFunc(slices.Clone(r.pool.States()), func(s *AccountState) bool {
		return !s.Usable()
	})
	if r.rotationMode == config.RotationModeSingle {
		states = primaryRotationStates(states)
	}
	if len(states) == 0 {
		return &RotatorError{
			StatusCode:   http.StatusServiceUnavailable,
			ErrorMessage: "No usable Google accounts. Run: agw login",
		}
	}

	var errs []error
	executed := 0
	for _, s := range states {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		token, err := s.Token(ctx, r.cfg, r.nowFn)
		if err != nil {
			var oauthErr *auth.OAuthError
			if errors.As(err, &oauthErr) && oauthErr.InvalidGrant {
				_ = r.pool.DisableAccount(s.Email(), "refresh token revoked")
			}
			errs = append(errs, fmt.Errorf("account %s token refresh: %w", s.Email(), err))
			continue
		}

		executed++
		if err := fn(ctx, token, s); err != nil {
			errs = append(errs, fmt.Errorf("account %s: %w", s.Email(), err))
		}
	}

	if executed == 0 && len(errs) == 0 {
		return &RotatorError{
			StatusCode:   http.StatusServiceUnavailable,
			ErrorMessage: "No usable Google account could execute the operation",
		}
	}
	return errors.Join(errs...)
}

// Generate dispatches a request to Google Cloud Code Assist, automatically handling rate limits,
// token refresh, cooldowns, and seamless account rotation.
func (r *Rotator) Generate(ctx context.Context, model string, builder RequestPayloadBuilder) (*AttemptResult, error) {
	return r.GenerateWithExclude(ctx, model, nil, builder)
}

// GenerateWithExclude dispatches a request like Generate while preferring accounts whose email is
// not in excludeEmails. When every non-excluded account is unavailable or cooling down, it clears
// the soft exclusion and falls back to excluded usable accounts so empty-stream retries still run.
func (r *Rotator) GenerateWithExclude(ctx context.Context, model string, excludeEmails map[string]struct{}, builder RequestPayloadBuilder) (*AttemptResult, error) {
	if err := r.pool.RefreshIfChanged(); err != nil {
		r.logger.Warn("failed checking accounts file reload", "error", err)
	}

	// A protective pause armed by a previous suspension verdict stops every dispatch, including
	// requests that would otherwise fail over to a sibling account: the enforcement wave that hit one
	// credential is not request-scoped, and continuing to send traffic is what escalates a first
	// suspension into a permanent one.
	if pauseErr := r.suspensionPauseError(); pauseErr != nil {
		return nil, pauseErr
	}

	states := r.pool.States()
	if r.rotationMode == config.RotationModeSingle {
		// Single-account dispatch never fails over to a sibling credential; the primary account either
		// serves the request or the request fails with the upstream reason.
		states = primaryRotationStates(states)
	}
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
	// Track soft-excluded accounts separately from accounts that failed during this dispatch so a
	// fallback can un-skip only the soft exclusions without retrying broken or cooling accounts.
	softExcluded := make(map[*AccountState]struct{})
	if len(excludeEmails) > 0 {
		for _, s := range states {
			if !s.Usable() {
				continue
			}
			if _, excluded := excludeEmails[s.Email()]; excluded {
				skip[s] = struct{}{}
				softExcluded[s] = struct{}{}
			}
		}
	}
	retried401 := make(map[*AccountState]struct{})
	var lastRotErr *RotatorError
	var waited time.Duration
	serverErrors := 0

	r.mu.RLock()
	maxWait := r.maxWait
	r.mu.RUnlock()

	// maxAttempts bounds upstream dispatches only. Absorbing a cooldown wait must not consume the
	// dispatch budget, otherwise a single-account pool can sleep through its final iteration and
	// exit without ever retrying after the cooldown expired.
	maxAttempts := r.dispatchBudget(len(states)*2 + 2)
	dispatches := 0

rotation:
	for dispatches < maxAttempts {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		now := r.nowFn()
		r.mu.Lock()
		pick := r.picker.Pick(model, states, skip, now)
		r.mu.Unlock()

		if pick.NoAcct {
			if len(softExcluded) > 0 {
				for s := range softExcluded {
					delete(skip, s)
				}
				clear(softExcluded)
				continue
			}
			break
		}

		if pick.Wait > 0 {
			if len(softExcluded) > 0 {
				// A non-excluded account is cooling while a soft-excluded account may be ready
				// immediately, so fall back to the excluded pool before absorbing cooldown waits.
				for s := range softExcluded {
					delete(skip, s)
				}
				clear(softExcluded)
				continue
			}
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

		dispatches++
		tokenCharged := pick.TokenConsumed
		s := pick.State
		token, err := s.Token(ctx, r.cfg, r.nowFn)
		if err != nil {
			r.refundToken(s, tokenCharged)
			var oauthErr *auth.OAuthError
			if errors.As(err, &oauthErr) && oauthErr.InvalidGrant {
				_ = r.pool.DisableAccount(s.Email(), "refresh token revoked")
			} else {
				s.SetCooldown("*", now.Add(r.progressiveCooldown(1)), now, err.Error())
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
			r.refundToken(s, tokenCharged)
			return nil, fmt.Errorf("building request payload for account %s: %w", s.Email(), err)
		}

		bodyBytes, err := json.Marshal(payload)
		if err != nil {
			r.refundToken(s, tokenCharged)
			return nil, fmt.Errorf("marshaling request payload: %w", err)
		}

		release, paceErr := r.pacer.Acquire(ctx, s.CredentialKey())
		if paceErr != nil {
			r.refundToken(s, tokenCharged)
			return nil, paceErr
		}

		resp, err := r.post(ctx, model, s.Tier(), token, s.CredentialKey(), bodyBytes)
		s.requests.Add(1)

		if err != nil {
			release()
			r.logger.Warn("Upstream network call failed", "account", s.Email(), "error", err)
			serverErrors++
			r.picker.HealthTracker().RecordFailure(s.Email())
			r.refundToken(s, tokenCharged)
			skip[s] = struct{}{}
			lastRotErr = &RotatorError{
				StatusCode:   http.StatusBadGateway,
				ErrorMessage: fmt.Sprintf("Upstream network error: %s", err.Error()),
			}
			if serverErrors >= 3 {
				break
			}
			if err := r.sleep(ctx, backoffWithJitter(time.Duration(500*serverErrors)*time.Millisecond, r.nowFn())); err != nil {
				return nil, err
			}
			continue
		}

		// Success!
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			s.ResetRateLimitStreak()
			s.ResetPromptRejections()
			s.RecordSuccess(r.nowFn())
			s.ResetQuotaFraction(model)
			r.picker.HealthTracker().RecordSuccess(s.Email())
			resp.Body = newPacedBody(resp.Body, release)
			return &AttemptResult{Response: resp, State: s}, nil
		}

		// Read error body bounded to 1MB
		errBodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		release()
		errBody := string(errBodyBytes)

		// Terminal suspension verdict: Google refused the account itself, not the request. Cooling it for
		// 30 minutes, retrying the same payload, or rotating to a sibling account only keeps sending
		// traffic to an account Google has already refused, so the account leaves rotation and an
		// optional protective pause stops the whole pool instead.
		if suspension, ok := DetectSuspension(resp.StatusCode, errBody); ok {
			r.refundToken(s, tokenCharged)
			r.picker.HealthTracker().RecordFailure(s.Email())
			skip[s] = struct{}{}
			if suspendErr := r.pool.SuspendAccount(s.Email(), suspension); suspendErr != nil {
				r.logger.Warn("failed persisting upstream account suspension", "account", s.Email(), "error", suspendErr)
			}
			r.logger.Warn("Upstream suspended account; removed from rotation",
				"account", s.Email(),
				"kind", suspension.Kind,
				"appeal_url_present", suspension.AppealURL != "",
			)
			lastRotErr = suspensionError(s.Email(), suspension)

			r.mu.RLock()
			pause := r.suspendPause
			r.mu.RUnlock()
			if pause > 0 {
				r.armSuspensionPause(pause, suspension)
				break rotation
			}
			continue
		}

		kind := ClassifyWithHeaders(resp.StatusCode, errBody, resp.Header)
		errMsg := ExtractErrorMessage(errBody)

		switch kind {
		case FailurePromptRejection:
			// Request-scoped rejection: the prompt triggered upstream content filtering, so the account
			// itself stays healthy. A sustained storm of them is different: field reports tie bare 429
			// storms to account suspensions, so the guard parks the account instead of retrying forever.
			r.refundToken(s, tokenCharged)
			if r.rejectionLimit > 0 {
				now := r.nowFn()
				count := s.RecordPromptRejection(now, config.PromptRejectionWindow)
				if count >= r.rejectionLimit {
					s.ResetPromptRejections()
					s.SetCooldown("*", now.Add(r.rejectionCooldown), now, fmt.Sprintf("prompt rejection storm (%d bare 429s)", count))
					skip[s] = struct{}{}
					r.logger.Warn("Prompt rejection storm; parking account", "account", s.Email(), "rejections", count, "cooldown", r.rejectionCooldown)
					lastRotErr = &RotatorError{
						StatusCode: http.StatusTooManyRequests,
						RetryAfter: r.rejectionCooldown,
						ErrorMessage: fmt.Sprintf("Google rejected %d requests for %s without quota metadata; pausing that account for %s. Reduce concurrency, shorten the system prompt, or switch model.",
							count, s.Email(), r.rejectionCooldown),
					}
					continue
				}
			}
			r.logger.Warn("Prompt rejected by upstream without quota metadata", "account", s.Email(), "model", model)
			return nil, &RotatorError{
				StatusCode:     http.StatusBadRequest,
				ErrorMessage:   fmt.Sprintf("Google rejected the prompt: %s", errMsg),
				PromptRejected: true,
			}

		case FailureRateLimit:
			r.refundToken(s, tokenCharged)
			r.picker.HealthTracker().RecordRateLimit(s.Email())
			s.rateLimits.Add(1)
			// A confirmed quota rejection means this model currently has no remaining allowance.
			s.SetQuotaFraction(model, 0)
			delay, ok := ParseRetryDelay(errBody, resp.Header, r.nowFn())
			if !ok {
				delay = r.progressiveCooldown(s.NextRateLimitStreak())
			}
			now := r.nowFn()
			s.SetCooldown(model, now.Add(delay), now, fmt.Sprintf("HTTP 429: %s", errMsg))
			lastRotErr = &RotatorError{
				StatusCode:   http.StatusTooManyRequests,
				RetryAfter:   delay,
				ErrorMessage: fmt.Sprintf("Rate limited: %s", errMsg),
			}
			r.logger.Warn("Account rate limited (429)", "account", s.Email(), "model", model, "cooldown", delay)

		case FailureAuth:
			r.refundToken(s, tokenCharged)
			// A desktop-session account holds a static bearer token that Google's token endpoint cannot
			// exchange, so resending the identical token can never succeed: cool it down immediately.
			if _, alreadyRetried := retried401[s]; !alreadyRetried && s.Refreshable() {
				retried401[s] = struct{}{}
				s.ClearAccessToken()
				r.logger.Info("Retrying account after 401 token invalidation", "account", s.Email())
				continue
			}
			// Repeated auth failure: apply forbidden cooldown
			now := r.nowFn()
			s.SetCooldown("*", now.Add(ForbiddenCooldownDuration), now, fmt.Sprintf("HTTP 401: %s", errMsg))
			lastRotErr = &RotatorError{
				StatusCode:   http.StatusUnauthorized,
				ErrorMessage: fmt.Sprintf("Google auth refused %s: %s", s.Email(), errMsg),
			}
			skip[s] = struct{}{}

		case FailureForbidden:
			r.picker.HealthTracker().RecordFailure(s.Email())
			r.refundToken(s, tokenCharged)
			skip[s] = struct{}{}
			now := r.nowFn()
			s.SetCooldown("*", now.Add(ForbiddenCooldownDuration), now, fmt.Sprintf("HTTP 403: %s", errMsg))
			lastRotErr = &RotatorError{
				StatusCode:   http.StatusForbidden,
				ErrorMessage: fmt.Sprintf("Google forbidden %s: %s", s.Email(), errMsg),
			}

		case FailureServer:
			r.picker.HealthTracker().RecordFailure(s.Email())
			r.refundToken(s, tokenCharged)
			serverErrors++
			lastRotErr = &RotatorError{
				StatusCode:   http.StatusBadGateway,
				ErrorMessage: fmt.Sprintf("Google server error (HTTP %d): %s", resp.StatusCode, errMsg),
			}
			// On persistent server error, skip current account and fall back to next account
			skip[s] = struct{}{}
			if serverErrors >= 3 {
				break rotation
			}
			if err := r.sleep(ctx, backoffWithJitter(time.Duration(500*serverErrors)*time.Millisecond, r.nowFn())); err != nil {
				return nil, err
			}

		case FailureBadRequest:
			// Malformed payloads fail identically on all accounts. Terminal error; do not rotate.
			r.refundToken(s, tokenCharged)
			return nil, &RotatorError{
				StatusCode:   http.StatusBadRequest,
				ErrorMessage: errMsg,
			}

		default:
			r.refundToken(s, tokenCharged)
			return nil, &RotatorError{
				StatusCode:   resp.StatusCode,
				ErrorMessage: errMsg,
			}
		}
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
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

// post dispatches an HTTP POST request to Google Cloud Code Assist, cycling through endpoints ordered
// for the account tier and falling back on 5xx, 403, and 404 responses.
// post dispatches one generation request across the tier's endpoints, reusing the credential-scoped
// wire client unless a caller pinned an explicit HTTP client.
func (r *Rotator) post(ctx context.Context, model, tier, token, credentialKey string, body []byte) (*http.Response, error) {
	endpoints := config.DefaultEndpoints()
	if r.cfg != nil {
		endpoints = r.cfg.EndpointsForTier(tier)
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
		pinned := r.clientPinned
		r.mu.RUnlock()
		if !pinned {
			client = config.CredentialHTTPClient(0, credentialKey)
		}

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
