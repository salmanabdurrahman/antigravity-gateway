// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

const (
	// DefaultCooldownDuration is the standard 60-second backoff applied on HTTP 429 when no reset time is specified.
	DefaultCooldownDuration = 60 * time.Second

	// ForbiddenCooldownDuration is the 30-minute cooldown applied when upstream returns HTTP 403 or persistent auth denial.
	ForbiddenCooldownDuration = 30 * time.Minute

	// TokenSkewMargin is the proactive refresh margin (2 minutes) to ensure access tokens do not expire mid-flight.
	TokenSkewMargin = 2 * time.Minute

	// FileCheckInterval throttles disk stat checks to at most once every 2 seconds.
	FileCheckInterval = 2 * time.Second
)

// AccountState represents the live, in-memory runtime operational state of an account.
type AccountState struct {
	// mu synchronizes access to internal state fields, token caches, and error details.
	mu sync.RWMutex

	// account holds the static account identity and persistent configuration.
	account *auth.Account

	// store references the backing credential file store for persisting updated tokens.
	store *auth.Store

	// accessToken caches the currently active short-lived Bearer token.
	accessToken string

	// expiresAt records when the cached accessToken expires.
	expiresAt time.Time

	// refreshingMu coordinates single-flight token renewal across concurrent incoming requests.
	refreshingMu sync.Mutex

	// cooldowns tracks model-specific and wildcard cooldown expiration timestamps.
	cooldowns *CooldownTracker

	// lastError records the most recent error message encountered while dispatching through this account.
	lastError string

	// requests tracks the cumulative count of requests dispatched to this account.
	requests atomic.Int64

	// rateLimits tracks the cumulative count of HTTP 429 rate limit responses received by this account.
	rateLimits atomic.Int64

	// rateLimitStreak tracks consecutive HTTP 429 responses, driving progressive backoff when upstream
	// omits an explicit retry delay. It resets on the next successful completion.
	rateLimitStreak atomic.Int64

	// promptRejections counts bare RESOURCE_EXHAUSTED rejections inside the current guard window, so an
	// account that keeps drawing them can be parked instead of retried forever.
	promptRejections int

	// promptWindowStart records when the current bare-rejection window began; zero before the first one.
	promptWindowStart time.Time

	// bucket coordinates token bucket burst rate limiting.
	bucket *TokenBucket

	// lastUsed records the timestamp of the last request dispatched to this account.
	lastUsed time.Time

	// lastSuccess records the timestamp of the last successful upstream completion.
	lastSuccess time.Time

	// quotaFractions maps model IDs (or "*") to remaining quota fractions (0.0 to 1.0).
	quotaFractions map[string]float64
}

// NewAccountState wraps an auth.Account into an in-memory runtime tracker.
func NewAccountState(acc *auth.Account) *AccountState {
	return &AccountState{
		account:        acc,
		cooldowns:      NewCooldownTracker(),
		bucket:         NewTokenBucket(DefaultTokenBucketCapacity, DefaultTokenBucketRefillRate, time.Now()),
		quotaFractions: make(map[string]float64),
	}
}

// SetStore attaches an optional backing store for persisting updated credentials (e.g. refreshed refresh tokens).
func (s *AccountState) SetStore(store *auth.Store) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.store = store
}

// Account returns a safe copy of the underlying account configuration.
func (s *AccountState) Account() *auth.Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.account == nil {
		return nil
	}
	accCopy := *s.account
	return &accCopy
}

// Email returns the account email address safely.
func (s *AccountState) Email() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.account == nil {
		return ""
	}
	return s.account.Email
}

// CredentialKey returns the opaque connection-pool key for this account's Google credential, or an
// empty string when the state holds no account identity.
func (s *AccountState) CredentialKey() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.account == nil {
		return ""
	}
	return s.account.CredentialKey()
}

// Key returns the account's rotation identity: the immutable Google user ID when known, otherwise the
// lowercased email of a legacy entry. Pool reloads key on it so an email change or alias keeps the
// in-memory cooldowns, token cache, and request counters attached to the same account.
func (s *AccountState) Key() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.account == nil {
		return ""
	}
	return s.account.Key()
}

// ProjectID returns the account project ID safely.
func (s *AccountState) ProjectID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.account == nil {
		return ""
	}
	return s.account.ProjectID
}

// Tier returns the normalized subscription tier used for upstream endpoint ordering.
func (s *AccountState) Tier() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.account == nil {
		return ""
	}
	return s.account.Tier
}

// NextRateLimitStreak increments and returns the consecutive rate-limit counter.
func (s *AccountState) NextRateLimitStreak() int64 {
	return s.rateLimitStreak.Add(1)
}

// ResetRateLimitStreak clears consecutive rate-limit tracking after a successful completion.
func (s *AccountState) ResetRateLimitStreak() {
	s.rateLimitStreak.Store(0)
}

// RecordPromptRejection counts one bare RESOURCE_EXHAUSTED rejection, starting a fresh window when the
// previous one elapsed, and returns the count observed inside the current window.
func (s *AccountState) RecordPromptRejection(now time.Time, window time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.promptWindowStart.IsZero() || now.Sub(s.promptWindowStart) > window {
		s.promptWindowStart = now
		s.promptRejections = 0
	}
	s.promptRejections++
	return s.promptRejections
}

// ResetPromptRejections clears the bare-rejection window after a successful completion, so the guard only
// reacts to a sustained storm rather than to occasional rejections across a long session.
func (s *AccountState) ResetPromptRejections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promptRejections = 0
	s.promptWindowStart = time.Time{}
}

// Usable checks if the account holds credentials and is not disabled.
func (s *AccountState) Usable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.account != nil && !s.account.Disabled && s.account.HasCredentials()
}

// CoolingUntil returns the expiration time until which the account is cooling for the model or wildcard.
func (s *AccountState) CoolingUntil(model string) time.Time {
	return s.cooldowns.CoolingUntil(model)
}

// SetCooldown sets cooldown expiration for a given model or wildcard ("*") and records error reason.
// `now` is the caller's injected clock, used to prune expired cooldown entries deterministically.
func (s *AccountState) SetCooldown(model string, until, now time.Time, reason string) {
	s.cooldowns.SetCooldown(model, until, now)
	s.mu.Lock()
	s.lastError = reason
	s.mu.Unlock()
}

// Refreshable reports whether the account holds an OAuth refresh token that can mint a new access
// token. Accounts imported from a desktop session carry only a short-lived static bearer token, so a
// 401 can never be recovered by resending the same credential.
func (s *AccountState) Refreshable() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.account != nil && strings.TrimSpace(s.account.RefreshToken) != ""
}

// TokenBucket returns the account's token bucket instance safely without lock contention.
func (s *AccountState) TokenBucket() *TokenBucket {
	return s.bucket
}

// LastUsed returns when the account was last selected.
func (s *AccountState) LastUsed() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastUsed
}

// SetLastUsed updates the last selected timestamp.
func (s *AccountState) SetLastUsed(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsed = t
}

// QuotaFraction returns the cached remaining quota fraction for a model (defaults to 1.0 if untracked).
func (s *AccountState) QuotaFraction(model string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.quotaFractions == nil {
		return 1.0
	}
	if frac, exists := s.quotaFractions[model]; exists {
		return frac
	}
	if frac, exists := s.quotaFractions["*"]; exists {
		return frac
	}
	return 1.0
}

// SetQuotaFraction updates the remaining quota fraction for a model.
func (s *AccountState) SetQuotaFraction(model string, fraction float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.quotaFractions == nil {
		s.quotaFractions = make(map[string]float64)
	}
	s.quotaFractions[model] = max(0.0, min(1.0, fraction))
}

// ResetQuotaFraction removes a model's cached quota fraction after a confirmed successful request,
// so a zero recorded during an earlier 429 cannot park the account for that model indefinitely.
func (s *AccountState) ResetQuotaFraction(model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.quotaFractions, model)
}

// ClearAccessToken invalidates the cached access token, forcing refresh on the next request.
func (s *AccountState) ClearAccessToken() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessToken = ""
	s.expiresAt = time.Time{}
}

// Token retrieves a valid access token, performing concurrent-safe token refresh if expired or near expiry.
func (s *AccountState) Token(ctx context.Context, cfg *config.Config, nowFn func() time.Time) (string, error) {
	s.mu.RLock()
	now := nowFn()
	if s.accessToken != "" && s.expiresAt.Sub(now) > TokenSkewMargin {
		token := s.accessToken
		s.mu.RUnlock()
		return token, nil
	}
	refreshToken := ""
	localAccessToken := ""
	if s.account != nil {
		refreshToken = s.account.RefreshToken
		localAccessToken = s.account.AccessToken
	}
	s.mu.RUnlock()

	// Coordinate single-flight refresh across concurrent requests
	s.refreshingMu.Lock()
	defer s.refreshingMu.Unlock()

	s.mu.RLock()
	now = nowFn()
	if s.accessToken != "" && s.expiresAt.Sub(now) > TokenSkewMargin {
		token := s.accessToken
		s.mu.RUnlock()
		return token, nil
	}
	if s.account != nil {
		refreshToken = s.account.RefreshToken
		localAccessToken = s.account.AccessToken
	}
	s.mu.RUnlock()

	// Accounts imported from a desktop session carry a short-lived bearer token and no refresh token.
	// Google's token endpoint cannot exchange it, so return it verbatim instead of attempting a
	// refresh grant that is guaranteed to fail with a misleading OAuth error.
	if refreshToken == "" {
		if localAccessToken == "" {
			return "", auth.ErrLocalSessionExpired
		}
		return localAccessToken, nil
	}

	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	res, err := auth.RefreshAccessToken(refreshCtx, cfg, refreshToken)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	s.accessToken = res.AccessToken
	s.expiresAt = res.Expiry
	var updatedAccount auth.Account
	needsPersist := false
	if res.RefreshToken != "" && s.account != nil && res.RefreshToken != s.account.RefreshToken {
		s.account.RefreshToken = res.RefreshToken
		updatedAccount = *s.account
		needsPersist = true
	}
	store := s.store
	s.mu.Unlock()

	if needsPersist && store != nil {
		// Best-effort storage persistence: updated refresh token is already active in memory.
		_, _ = store.Upsert(&updatedAccount)
	}

	return res.AccessToken, nil
}

// RecordSuccess stamps the account's last successful upstream completion, which feeds the risk
// telemetry so an operator can tell a credential that still works from one that only collects errors.
func (s *AccountState) RecordSuccess(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSuccess = now
}

// Snapshot returns an immutable status snapshot of this account state for diagnostics.
func (s *AccountState) Snapshot(now time.Time) AccountSnapshot {
	s.mu.RLock()
	email := ""
	projectID := ""
	disabled := false
	tier := ""
	suspendKind := ""
	appealURL := ""
	if s.account != nil {
		email = s.account.Email
		projectID = s.account.ProjectID
		disabled = s.account.Disabled
		tier = s.account.Tier
		suspendKind = s.account.SuspendKind
		appealURL = s.account.AppealURL
	}
	lastErr := s.lastError
	lastUsedStr := ""
	if !s.lastUsed.IsZero() {
		lastUsedStr = s.lastUsed.UTC().Format(time.RFC3339)
	}
	lastSuccessStr := ""
	if !s.lastSuccess.IsZero() {
		lastSuccessStr = s.lastSuccess.UTC().Format(time.RFC3339)
	}
	promptRejections := s.promptRejections
	s.mu.RUnlock()

	activeCooldowns := make(map[string]string)
	for m, until := range s.cooldowns.ActiveCooldowns(now) {
		activeCooldowns[m] = until.UTC().Format(time.RFC3339)
	}

	return AccountSnapshot{
		Email:            email,
		ProjectID:        projectID,
		Tier:             tier,
		Disabled:         disabled,
		Cooldowns:        activeCooldowns,
		Requests:         s.requests.Load(),
		RateLimits:       s.rateLimits.Load(),
		PromptRejections: promptRejections,
		LastUsed:         lastUsedStr,
		LastUpstreamOK:   lastSuccessStr,
		LastError:        lastErr,
		SuspendKind:      suspendKind,
		AppealURL:        appealURL,
	}
}

// AccountSnapshot describes the state of an account at a single point in time.
type AccountSnapshot struct {
	// Email is the account's primary email address.
	Email string `json:"email"`

	// ProjectID is the assigned Cloud Code companion project ID.
	ProjectID string `json:"project_id"`

	// Tier is the normalized subscription tier used for endpoint ordering.
	Tier string `json:"tier,omitempty"`

	// Disabled indicates whether the account is temporarily excluded from rotation.
	Disabled bool `json:"disabled"`

	// Cooldowns maps model IDs (or "*") to RFC 3339 cooldown expiration timestamps.
	Cooldowns map[string]string `json:"cooldowns"`

	// Requests is the total number of requests dispatched to this account.
	Requests int64 `json:"requests"`

	// RateLimits is the total count of 429 responses received by this account.
	RateLimits int64 `json:"rate_limits"`

	// Health represents the runtime operational health score (0.0 to 100.0).
	Health float64 `json:"health,omitzero"`

	// PromptRejections holds the bare RESOURCE_EXHAUSTED rejections counted inside the guard window.
	PromptRejections int `json:"prompt_rejections,omitzero"`

	// LastUsed holds the RFC 3339 timestamp of the last request dispatched.
	LastUsed string `json:"last_used,omitempty"`

	// LastUpstreamOK holds the RFC 3339 timestamp of the last successful upstream completion.
	LastUpstreamOK string `json:"last_upstream_ok,omitempty"`

	// LastError holds the last recorded operational error for diagnostics.
	LastError string `json:"last_error,omitempty"`

	// SuspendKind reports the terminal upstream suspension classification when the account is suspended.
	SuspendKind string `json:"suspend_kind,omitempty"`

	// AppealURL carries the validated upstream appeal link for a suspended account.
	AppealURL string `json:"appeal_url,omitempty"`
}

// Pool maintains in-memory account states and synchronizes with the backing Store.
type Pool struct {
	// mu guards concurrent access to the states slice and reload timestamps.
	mu sync.RWMutex

	// reloadMu serializes the stat/load/replace sequence of hot reloads so a stalled reload cannot
	// overwrite newer account state with an older file snapshot.
	reloadMu sync.Mutex

	// store is the persistent credential repository on disk.
	store *auth.Store

	// cfg holds application-level settings and fallback endpoints.
	cfg *config.Config

	// states holds in-memory operational state for every configured account.
	states []*AccountState

	// fileMod tracks the disk modification time of accounts.json for hot-reloading.
	fileMod time.Time

	// lastStatNano throttles disk stat checks locklessly using atomic Unix nanoseconds.
	lastStatNano atomic.Int64

	// healthTracker provides live operational health scoring for account snapshots.
	healthTracker *HealthTracker

	// nowFn provides time for deterministic testing.
	nowFn func() time.Time
}

// NewPool initializes an account pool backed by the provided store and runtime config.
func NewPool(store *auth.Store, cfg *config.Config, nowFn func() time.Time) (*Pool, error) {
	if nowFn == nil {
		nowFn = time.Now
	}
	p := &Pool{
		store: store,
		cfg:   cfg,
		nowFn: nowFn,
	}

	if store != nil {
		accounts, err := store.Load()
		if err != nil {
			return nil, fmt.Errorf("loading accounts into pool: %w", err)
		}
		p.SetAccounts(accounts)
		modTime, _ := store.ModTime()
		p.fileMod = modTime
		p.lastStatNano.Store(nowFn().UnixNano())
	}

	return p, nil
}

// SetAccounts replaces or updates the active accounts slice, preserving existing cooldowns and stats.
func (p *Pool) SetAccounts(accounts []*auth.Account) {
	p.mu.Lock()
	defer p.mu.Unlock()

	oldMap := make(map[string]*AccountState, len(p.states))
	for _, s := range p.states {
		key := s.Key()
		if key != "" {
			oldMap[strings.ToLower(key)] = s
		}
	}

	var newStates []*AccountState
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		if prev, ok := oldMap[strings.ToLower(acc.Key())]; ok {
			prev.mu.Lock()
			// Compare both credential kinds: an account imported from a desktop session stores its
			// bearer token in AccessToken, so comparing refresh tokens alone would keep serving a
			// stale token after the operator re-imports a fresh desktop session.
			sameToken := prev.account != nil &&
				prev.account.RefreshToken == acc.RefreshToken &&
				prev.account.AccessToken == acc.AccessToken
			if sameToken {
				prev.account = acc
				prev.mu.Unlock()
				newStates = append(newStates, prev)
				continue
			}
			prev.mu.Unlock()
		}

		fresh := NewAccountState(acc)
		fresh.SetStore(p.store)
		newStates = append(newStates, fresh)
	}
	p.states = newStates
}

// States returns a cloned slice of the current AccountState pointers.
func (p *Pool) States() []*AccountState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.states)
}

// RefreshIfChanged checks if accounts.json has been modified by CLI login/logout and reloads state.
// It implements a lockless fast-path via atomic timestamps to avoid mutex contention on the request path.
//
// Hot reloads are serialized by reloadMu: without it, two callers spaced beyond FileCheckInterval
// could both run ModTime/Load/SetAccounts and let a stalled older snapshot clobber newer accounts.
// A contended caller keeps the non-blocking fast path and lets the in-flight reload finish its work.
func (p *Pool) RefreshIfChanged() error {
	if p.store == nil {
		return nil
	}

	// A negative delta means the wall clock stepped backwards (NTP correction, suspend/resume). Treat
	// it as an immediate resync instead of freezing hot reload until the clock catches up.
	if elapsed := time.Duration(p.nowFn().UnixNano() - p.lastStatNano.Load()); elapsed >= 0 && elapsed < FileCheckInterval {
		return nil
	}

	if !p.reloadMu.TryLock() {
		return nil
	}
	defer p.reloadMu.Unlock()

	// Re-check under the reload lock: a concurrent reload may have just refreshed the stat window.
	nowNano := p.nowFn().UnixNano()
	if elapsed := time.Duration(nowNano - p.lastStatNano.Load()); elapsed >= 0 && elapsed < FileCheckInterval {
		return nil
	}
	modTime, err := p.store.ModTime()
	if err != nil {
		// A failed stat is usually transient (the file is being replaced); leave the window untouched so
		// the next request retries instead of suppressing hot reload for a full interval.
		return err
	}
	p.lastStatNano.Store(nowNano)
	p.mu.RLock()
	currentMod := p.fileMod
	p.mu.RUnlock()

	if modTime.Equal(currentMod) {
		return nil
	}

	accounts, err := p.store.Load()
	if err != nil {
		return fmt.Errorf("reloading accounts on change: %w", err)
	}

	p.SetAccounts(accounts)

	p.mu.Lock()
	p.fileMod = modTime
	p.mu.Unlock()

	return nil
}

// DisableAccount marks an account as disabled both in memory and persistently in the store.
func (p *Pool) DisableAccount(email, reason string) error {
	p.mu.RLock()
	var target *AccountState
	for _, s := range p.states {
		if strings.EqualFold(s.Email(), email) {
			target = s
			break
		}
	}
	p.mu.RUnlock()

	if target != nil {
		target.mu.Lock()
		target.account.Disabled = true
		target.account.LastError = reason
		target.lastError = reason
		target.mu.Unlock()
	}

	if p.store != nil {
		if err := p.store.SetDisabled(email, true, reason); err != nil {
			return fmt.Errorf("persisting disabled status for %s: %w", email, err)
		}
	}
	return nil
}

// SuspendAccount records a terminal upstream suspension verdict for an account: the account leaves
// rotation immediately, and the verdict, its reason, and its validated appeal link are persisted so
// an operator can act on it without re-contacting Google and a restart cannot resurrect the account.
func (p *Pool) SuspendAccount(email string, suspension *Suspension) error {
	if suspension == nil {
		return fmt.Errorf("suspending account %s: nil suspension verdict", email)
	}

	reason := cmp.Or(strings.TrimSpace(suspension.Message), string(suspension.Kind))

	p.mu.RLock()
	var target *AccountState
	for _, s := range p.states {
		if strings.EqualFold(s.Email(), email) {
			target = s
			break
		}
	}
	p.mu.RUnlock()

	if target != nil {
		target.mu.Lock()
		target.account.Disabled = true
		target.account.LastError = reason
		target.account.SuspendKind = string(suspension.Kind)
		target.account.SuspendReason = reason
		target.account.AppealURL = suspension.AppealURL
		target.lastError = reason
		target.mu.Unlock()
	}

	if p.store != nil {
		if err := p.store.SetSuspended(email, string(suspension.Kind), reason, suspension.AppealURL); err != nil {
			return fmt.Errorf("persisting suspension for %s: %w", email, err)
		}
	}
	return nil
}

// SetHealthTracker attaches a health tracker to populate operational scores in account snapshots.
func (p *Pool) SetHealthTracker(h *HealthTracker) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.healthTracker = h
}

// Snapshots returns runtime snapshots for all managed accounts.
func (p *Pool) Snapshots() []AccountSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := p.nowFn()
	snaps := make([]AccountSnapshot, len(p.states))
	for i, s := range p.states {
		snap := s.Snapshot(now)
		if p.healthTracker != nil {
			snap.Health = p.healthTracker.Score(s.Email())
		}
		snaps[i] = snap
	}
	return snaps
}
