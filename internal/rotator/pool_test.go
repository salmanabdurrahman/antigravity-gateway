// Package rotator_test provides unit and concurrency test suites for the rotator package.
package rotator_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// newTestAccount constructs a test account with predictable email and project fixtures.
func newTestAccount(email string) *auth.Account {
	return &auth.Account{
		Email:        email,
		RefreshToken: "rt-" + email,
		ProjectID:    "proj-" + email,
		AddedAt:      time.Now().UTC().Format(time.RFC3339),
	}
}

// TestPickerSticky verifies sticky account selection and immediate failover when rate limited.
func TestPickerSticky(t *testing.T) {
	now := time.Now()
	a1 := rotator.NewAccountState(newTestAccount("a1@gmail.com"))
	a2 := rotator.NewAccountState(newTestAccount("a2@gmail.com"))
	states := []*rotator.AccountState{a1, a2}

	picker := rotator.NewPicker(rotator.StrategySticky)

	// In sticky mode, multiple picks for same model stay on the same account
	res1 := picker.Pick("gemini-3.1-pro", states, nil, now)
	if res1.State == nil || res1.State.Account().Email != "a1@gmail.com" {
		t.Fatalf("expected a1@gmail.com, got %v", res1.State)
	}

	res2 := picker.Pick("gemini-3.1-pro", states, nil, now)
	if res2.State == nil || res2.State.Account().Email != "a1@gmail.com" {
		t.Fatalf("expected a1@gmail.com to stick, got %v", res2.State)
	}

	// When a1 is cooling down, picker moves to a2
	a1.SetCooldown("gemini-3.1-pro", now.Add(30*time.Second), "rate limit")

	res3 := picker.Pick("gemini-3.1-pro", states, nil, now)
	if res3.State == nil || res3.State.Account().Email != "a2@gmail.com" {
		t.Fatalf("expected failover to a2@gmail.com, got %v", res3.State)
	}

	// a1 is not cooling for another model, so sticky picks a1 for other model
	resOther := picker.Pick("claude-3-7-sonnet", states, nil, now)
	if resOther.State == nil || resOther.State.Account().Email != "a1@gmail.com" {
		t.Fatalf("expected a1@gmail.com for other model, got %v", resOther.State)
	}
}

// TestPickerRoundRobin verifies cyclical account selection across consecutive requests.
func TestPickerRoundRobin(t *testing.T) {
	now := time.Now()
	a1 := rotator.NewAccountState(newTestAccount("a1@gmail.com"))
	a2 := rotator.NewAccountState(newTestAccount("a2@gmail.com"))
	a3 := rotator.NewAccountState(newTestAccount("a3@gmail.com"))
	states := []*rotator.AccountState{a1, a2, a3}

	picker := rotator.NewPicker(rotator.StrategyRoundRobin)

	var sequence []string
	for range 4 {
		res := picker.Pick("gemini", states, nil, now)
		if res.State == nil {
			t.Fatalf("expected valid account state")
		}
		sequence = append(sequence, res.State.Account().Email)
	}

	expected := []string{"a1@gmail.com", "a2@gmail.com", "a3@gmail.com", "a1@gmail.com"}
	for i, email := range sequence {
		if email != expected[i] {
			t.Errorf("step %d: got %s, want %s", i, email, expected[i])
		}
	}
}

// TestPickerAllCoolingWait verifies computing the minimum wait duration when all accounts are in cooldown.
func TestPickerAllCoolingWait(t *testing.T) {
	now := time.Now()
	a1 := rotator.NewAccountState(newTestAccount("a1@gmail.com"))
	a2 := rotator.NewAccountState(newTestAccount("a2@gmail.com"))
	states := []*rotator.AccountState{a1, a2}

	a1.SetCooldown("gemini", now.Add(10*time.Second), "429")
	a2.SetCooldown("gemini", now.Add(5*time.Second), "429")

	picker := rotator.NewPicker(rotator.StrategySticky)
	res := picker.Pick("gemini", states, nil, now)

	if res.State != nil {
		t.Fatalf("expected no available state, got %v", res.State)
	}
	if res.Wait != 5*time.Second {
		t.Errorf("expected soonest wait of 5s, got %v", res.Wait)
	}
}

// TestPoolHotReload verifies that changes to accounts.json on disk trigger automatic in-memory pool updates.
func TestPoolHotReload(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "accounts.json")
	store := auth.NewStore(storePath)

	a1 := newTestAccount("user1@example.com")
	if _, err := store.Upsert(a1); err != nil {
		t.Fatalf("upsert a1 failed: %v", err)
	}

	mockTime := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return mockTime }

	pool, err := rotator.NewPool(store, &config.Config{}, nowFn)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}

	if len(pool.States()) != 1 {
		t.Fatalf("expected 1 state, got %d", len(pool.States()))
	}

	// Simulate adding a second account from CLI
	a2 := newTestAccount("user2@example.com")
	if _, err := store.Upsert(a2); err != nil {
		t.Fatalf("upsert a2 failed: %v", err)
	}

	// Advance time by 3 seconds to bypass the 2-second rate limit
	mockTime = mockTime.Add(3 * time.Second)

	if err := pool.RefreshIfChanged(); err != nil {
		t.Fatalf("refresh if changed failed: %v", err)
	}

	states := pool.States()
	if len(states) != 2 {
		t.Fatalf("expected 2 states after hot reload, got %d", len(states))
	}
}

// TestPoolDisableAccount verifies marking an account disabled and immediately removing it from rotation.
func TestPoolDisableAccount(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "accounts.json")
	store := auth.NewStore(storePath)

	a1 := newTestAccount("disabled@example.com")
	if _, err := store.Upsert(a1); err != nil {
		t.Fatalf("upsert failed: %v", err)
	}

	pool, err := rotator.NewPool(store, &config.Config{}, time.Now)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}

	if err := pool.DisableAccount(a1.Email, "refresh token revoked"); err != nil {
		t.Fatalf("disable account failed: %v", err)
	}

	states := pool.States()
	if len(states) != 1 || states[0].Usable() {
		t.Fatalf("expected account to be marked unusable")
	}

	// Verify persistence in backing store
	reloaded, err := store.Load()
	if err != nil {
		t.Fatalf("store reload failed: %v", err)
	}
	if len(reloaded) != 1 || !reloaded[0].Disabled {
		t.Fatalf("expected store account to have disabled=true")
	}
}
