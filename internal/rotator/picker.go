// Package rotator coordinates account pooling, selection strategies, proactive token refresh, and transparent 429 failover.
package rotator

import (
	"time"
)

// Strategy defines account rotation behavior: sticky or round-robin.
type Strategy string

const (
	// StrategySticky keeps using the current account until it encounters rate limits or errors.
	StrategySticky Strategy = "sticky"

	// StrategyRoundRobin cycles sequentially through eligible accounts on every request.
	StrategyRoundRobin Strategy = "round-robin"
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
	// strategy defines the active rotation algorithm (sticky prompt caching or sequential round-robin).
	strategy Strategy

	// cursors tracks the last used account index per model identifier.
	cursors map[string]int
}

// NewPicker constructs a strategy-aware picker.
func NewPicker(strategy Strategy) *Picker {
	if strategy == "" {
		strategy = StrategySticky
	}
	return &Picker{
		strategy: strategy,
		cursors:  make(map[string]int),
	}
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
