// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"slices"
	"time"
)

// Strategy defines account rotation behavior: sticky or round-robin.
type Strategy string

const (
	// StrategySticky keeps using the current account until it encounters rate limits or errors.
	StrategySticky Strategy = "sticky"

	// StrategyRoundRobin cycles sequentially through eligible accounts on every request.
	StrategyRoundRobin Strategy = "round-robin"

	// StrategyHybrid balances traffic using health scores, token bucket limits, quota awareness, and LRU freshness.
	StrategyHybrid Strategy = "hybrid"
)

// PickResult represents the outcome of picking an account for a model.
type PickResult struct {
	// State points to the selected non-cooling AccountState, or nil if none was available.
	State *AccountState

	// Wait indicates the minimum wait duration until the soonest cooling account becomes available.
	Wait time.Duration

	// NoAcct is true when no usable or enabled accounts exist in the pool.
	NoAcct bool
}

// Picker selects candidate accounts from an AccountState slice based on rotation strategy and cooldowns.
type Picker struct {
	// strategy defines the active rotation algorithm (sticky, round-robin, or hybrid).
	strategy Strategy

	// cursors tracks the last used account index per model identifier.
	cursors map[string]int

	// health tracks rolling operational health scores per account.
	health *HealthTracker

	// quotaThreshold defines the fractional quota floor (e.g. 0.05) below which accounts are deprioritized.
	quotaThreshold float64
}

// NewPicker constructs a strategy-aware picker.
func NewPicker(strategy Strategy) *Picker {
	if strategy == "" {
		strategy = StrategySticky
	}
	return &Picker{
		strategy:       strategy,
		cursors:        make(map[string]int),
		health:         NewHealthTracker(),
		quotaThreshold: 0.05,
	}
}

// HealthTracker returns the picker's active health tracker.
func (p *Picker) HealthTracker() *HealthTracker {
	if p.health == nil {
		p.health = NewHealthTracker()
	}
	return p.health
}

// SetHealthTracker configures a custom health tracker.
func (p *Picker) SetHealthTracker(h *HealthTracker) {
	p.health = h
}

// QuotaThreshold returns the fractional quota threshold (e.g. 0.05 for 5%).
func (p *Picker) QuotaThreshold() float64 {
	if p.quotaThreshold <= 0 {
		return 0.05
	}
	return p.quotaThreshold
}

// SetQuotaThreshold updates the fractional quota threshold.
func (p *Picker) SetQuotaThreshold(threshold float64) {
	p.quotaThreshold = threshold
}

// Strategy returns the active rotation strategy.
func (p *Picker) Strategy() Strategy {
	return p.strategy
}

// SetStrategy dynamically updates the rotation strategy.
func (p *Picker) SetStrategy(s Strategy) {
	p.strategy = s
}

// Pick selects the next non-cooling, usable account for the requested model.
// If all accounts are currently cooling down, Pick returns the minimum wait duration until the soonest one frees up.
// Accounts in the skip map are omitted (e.g. permanently failed or already attempted in current request loop).
func (p *Picker) Pick(model string, states []*AccountState, skip map[*AccountState]struct{}, now time.Time) PickResult {
	if p.strategy == StrategyHybrid {
		return p.pickHybrid(model, states, skip, now)
	}

	n := len(states)
	if n == 0 {
		return PickResult{NoAcct: true}
	}

	start := p.cursors[model]
	var minWait time.Duration = -1

	for k := range n {
		idx := (start + k) % n
		s := states[idx]
		if !s.Usable() {
			continue
		}
		if _, skipped := skip[s]; skipped {
			continue
		}

		until := s.CoolingUntil(model)
		if !until.IsZero() && until.After(now) {
			wait := until.Sub(now)
			if minWait < 0 || wait < minWait {
				minWait = wait
			}
			continue
		}

		// Account is usable and not cooling down.
		// Sticky keeps the cursor on this account; Round-Robin advances cursor.
		if p.strategy == StrategyRoundRobin {
			p.cursors[model] = (idx + 1) % n
		} else {
			p.cursors[model] = idx
		}

		return PickResult{State: s}
	}

	if minWait >= 0 {
		return PickResult{Wait: minWait}
	}

	return PickResult{NoAcct: true}
}

// SoonestWait computes the minimum duration until any usable account frees up for the given model without mutating the cursor.
func (p *Picker) SoonestWait(model string, states []*AccountState, now time.Time) time.Duration {
	var minWait time.Duration = -1
	for _, s := range states {
		if !s.Usable() {
			continue
		}
		until := s.CoolingUntil(model)
		if !until.IsZero() && until.After(now) {
			wait := until.Sub(now)
			if minWait < 0 || wait < minWait {
				minWait = wait
			}
		} else {
			return 0
		}
	}
	if minWait < 0 {
		return 0
	}
	return minWait
}

// pickHybrid selects an account based on health score, token bucket limits, quota awareness, and LRU freshness.
func (p *Picker) pickHybrid(model string, states []*AccountState, skip map[*AccountState]struct{}, now time.Time) PickResult {
	n := len(states)
	if n == 0 {
		return PickResult{NoAcct: true}
	}

	type candidate struct {
		state *AccountState
		score float64
	}

	var allUsable []*AccountState
	var minWait time.Duration = -1

	for _, s := range states {
		if !s.Usable() {
			continue
		}
		if _, skipped := skip[s]; skipped {
			continue
		}

		until := s.CoolingUntil(model)
		if !until.IsZero() && until.After(now) {
			wait := until.Sub(now)
			if minWait < 0 || wait < minWait {
				minWait = wait
			}
			continue
		}

		allUsable = append(allUsable, s)
	}

	if len(allUsable) == 0 {
		if minWait >= 0 {
			return PickResult{Wait: minWait}
		}
		return PickResult{NoAcct: true}
	}

	health := p.HealthTracker()
	threshold := p.QuotaThreshold()

	// Tier 1: Normal candidates (healthy, has tokens, quota >= threshold)
	var tier1 []*AccountState
	for _, s := range allUsable {
		if health.IsUsable(s.Email()) && s.TokenBucket().Tokens(now) >= 1.0 && s.QuotaFraction(model) >= threshold {
			tier1 = append(tier1, s)
		}
	}

	// Tier 2: Quota fallback (healthy, has tokens, but quota < threshold)
	var tier2 []*AccountState
	if len(tier1) == 0 {
		for _, s := range allUsable {
			if health.IsUsable(s.Email()) && s.TokenBucket().Tokens(now) >= 1.0 {
				tier2 = append(tier2, s)
			}
		}
	}

	// Tier 3: Emergency (all unhealthy, but have tokens)
	var tier3 []*AccountState
	if len(tier1) == 0 && len(tier2) == 0 {
		for _, s := range allUsable {
			if s.TokenBucket().Tokens(now) >= 1.0 {
				tier3 = append(tier3, s)
			}
		}
	}

	// Tier 4: Last resort (all usable accounts, even if token bucket is temporarily empty)
	selectedTier := tier1
	if len(selectedTier) == 0 {
		selectedTier = tier2
	}
	if len(selectedTier) == 0 {
		selectedTier = tier3
	}
	if len(selectedTier) == 0 {
		selectedTier = allUsable
	}

	// Calculate scores for selected candidates
	candidates := make([]candidate, len(selectedTier))
	for i, s := range selectedTier {
		// Health component: 0..100 * 2.0 = 0..200
		hScore := health.Score(s.Email()) * 2.0

		// Token component: 0..50
		tokens := s.TokenBucket().Tokens(now)
		tScore := (tokens / DefaultTokenBucketCapacity * 100.0) * 0.5

		// Quota component: 0..30
		qScore := (s.QuotaFraction(model) * 100.0) * 0.3

		// LRU freshness: 0..18 (capped at 1 hour)
		timeSince := now.Sub(s.LastUsed()).Seconds()
		if timeSince > 3600 {
			timeSince = 3600
		}
		if timeSince < 0 {
			timeSince = 0
		}
		lruScore := timeSince * 0.005

		candidates[i] = candidate{
			state: s,
			score: hScore + tScore + qScore + lruScore,
		}
	}

	// Sort descending by score
	slices.SortFunc(candidates, func(a, b candidate) int {
		if a.score > b.score {
			return -1
		} else if a.score < b.score {
			return 1
		}
		return 0
	})

	best := candidates[0].state
	_ = best.TokenBucket().Consume(now)
	best.SetLastUsed(now)

	return PickResult{State: best}
}
