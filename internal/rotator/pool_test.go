// Package rotator_test provides unit and concurrency test suites for the rotator package.
package rotator_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
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
	a1.SetCooldown("gemini-3.1-pro", now.Add(30*time.Second), now, "rate limit")

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

	a1.SetCooldown("gemini", now.Add(10*time.Second), now, "429")
	a2.SetCooldown("gemini", now.Add(5*time.Second), now, "429")

	picker := rotator.NewPicker(rotator.StrategySticky)
	res := picker.Pick("gemini", states, nil, now)

	if res.State != nil {
		t.Fatalf("expected no available state, got %v", res.State)
	}
	if res.Wait != 5*time.Second {
		t.Errorf("expected soonest wait of 5s, got %v", res.Wait)
	}
}

// TestPickerHybrid_HealthAndQuotaAwareness verifies smart account selection combining
// health scores, remaining quota fractions, and multi-tier fallbacks.
func TestPickerHybrid_HealthAndQuotaAwareness(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	a1 := rotator.NewAccountState(newTestAccount("a1@gmail.com"))
	a2 := rotator.NewAccountState(newTestAccount("a2@gmail.com"))
	states := []*rotator.AccountState{a1, a2}

	picker := rotator.NewPicker(rotator.StrategyHybrid)
	picker.SetQuotaThreshold(0.05) // 5% quota threshold

	// Both accounts have full health, but a2 has critical quota (2% < 5%)
	a1.SetQuotaFraction("gemini", 0.80)
	a2.SetQuotaFraction("gemini", 0.02)

	// Tier 1: a1 is selected because a2 is below quota threshold
	res1 := picker.Pick("gemini", states, nil, now)
	if res1.State == nil || res1.State.Account().Email != "a1@gmail.com" {
		t.Fatalf("expected a1@gmail.com with healthy quota, got %v", res1.State)
	}

	// When a1 is cooling down, picker falls back to a2 (Tier 2: quota fallback)
	a1.SetCooldown("gemini", now.Add(30*time.Second), now, "rate limit")
	res2 := picker.Pick("gemini", states, nil, now)
	if res2.State == nil || res2.State.Account().Email != "a2@gmail.com" {
		t.Fatalf("expected failover to a2@gmail.com under quota fallback tier, got %v", res2.State)
	}

	// Lift a1 cooldown, drop a1 health below usability threshold (< 20)
	a1.SetCooldown("gemini", now.Add(-1*time.Second), now, "")
	for range 9 {
		picker.HealthTracker().RecordRateLimit("a1@gmail.com")
	}
	if picker.HealthTracker().IsUsable("a1@gmail.com") {
		t.Fatal("expected a1 to be marked unhealthy")
	}

	// a2 is usable (health 100), so even with low quota, a2 beats unhealthy a1
	res3 := picker.Pick("gemini", states, nil, now)
	if res3.State == nil || res3.State.Account().Email != "a2@gmail.com" {
		t.Fatalf("expected healthy a2@gmail.com to beat unhealthy a1, got %v", res3.State)
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

// TestPoolSetAccountsPreservesStateAcrossEmailChange verifies that pool reloads key on the immutable
// Google user ID, so an address change keeps cooldowns and counters attached to the same account.
func TestPoolSetAccountsPreservesStateAcrossEmailChange(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	pool, err := rotator.NewPool(nil, &config.Config{}, func() time.Time { return now })
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}

	original := newTestAccount("old-address@example.com")
	original.UserID = "google-sub-42"
	pool.SetAccounts([]*auth.Account{original})

	until := now.Add(time.Minute)
	pool.States()[0].SetCooldown("gemini", until, now, "HTTP 429: quota")

	renamed := &auth.Account{
		Email:        "new-address@example.com",
		UserID:       "google-sub-42",
		RefreshToken: original.RefreshToken,
		ProjectID:    original.ProjectID,
		AddedAt:      original.AddedAt,
	}
	pool.SetAccounts([]*auth.Account{renamed})

	states := pool.States()
	if len(states) != 1 {
		t.Fatalf("expected 1 state after rename, got %d", len(states))
	}
	if states[0].Email() != "new-address@example.com" {
		t.Errorf("expected renamed address applied, got %q", states[0].Email())
	}
	if got := states[0].CoolingUntil("gemini"); !got.Equal(until) {
		t.Errorf("expected cooldown continuity across rename, got %v", got)
	}
}

// TestPoolRefreshIfChangedConcurrentReload verifies that concurrent hot-reload callers serialize
// their stat/load/replace work so a stalled reload cannot clobber newer account state.
func TestPoolRefreshIfChangedConcurrentReload(t *testing.T) {
	tempDir := t.TempDir()
	store := auth.NewStore(filepath.Join(tempDir, "accounts.json"))
	if _, err := store.Upsert(newTestAccount("seed@example.com")); err != nil {
		t.Fatalf("seeding account failed: %v", err)
	}

	var tick atomic.Int64
	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time {
		return base.Add(time.Duration(tick.Add(3)) * time.Second)
	}

	pool, err := rotator.NewPool(store, &config.Config{}, nowFn)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if err := pool.RefreshIfChanged(); err != nil {
					t.Errorf("concurrent RefreshIfChanged failed: %v", err)
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 5 {
			if _, err := store.Upsert(newTestAccount(fmt.Sprintf("extra%d@example.com", i))); err != nil {
				t.Errorf("concurrent upsert failed: %v", err)
			}
		}
	}()

	wg.Wait()

	// A final reload must observe the complete account set written by the concurrent writer.
	if err := pool.RefreshIfChanged(); err != nil {
		t.Fatalf("final RefreshIfChanged failed: %v", err)
	}
	if got := len(pool.States()); got != 6 {
		t.Fatalf("expected 6 states after concurrent reloads, got %d", got)
	}
}

// TestPoolRefreshIfChangedBackwardClock verifies that a backward wall-clock step triggers a clean
// resync instead of freezing hot reload until the clock catches up.
func TestPoolRefreshIfChangedBackwardClock(t *testing.T) {
	tempDir := t.TempDir()
	store := auth.NewStore(filepath.Join(tempDir, "accounts.json"))
	if _, err := store.Upsert(newTestAccount("first@example.com")); err != nil {
		t.Fatalf("seeding first account failed: %v", err)
	}

	mockTime := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	pool, err := rotator.NewPool(store, &config.Config{}, func() time.Time { return mockTime })
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}

	mockTime = mockTime.Add(3 * time.Second)
	if _, err := store.Upsert(newTestAccount("second@example.com")); err != nil {
		t.Fatalf("seeding second account failed: %v", err)
	}
	if err := pool.RefreshIfChanged(); err != nil {
		t.Fatalf("forward-clock refresh failed: %v", err)
	}
	if got := len(pool.States()); got != 2 {
		t.Fatalf("expected 2 states after forward reload, got %d", got)
	}

	// Step the injected clock backwards, as an NTP correction or suspend/resume would.
	mockTime = mockTime.Add(-10 * time.Second)
	if _, err := store.Upsert(newTestAccount("third@example.com")); err != nil {
		t.Fatalf("seeding third account failed: %v", err)
	}
	if err := pool.RefreshIfChanged(); err != nil {
		t.Fatalf("backward-clock refresh failed: %v", err)
	}
	if got := len(pool.States()); got != 3 {
		t.Fatalf("expected 3 states after backward-clock resync, got %d", got)
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

// TestAccountStateTokenLocalDesktopSession verifies that an account imported from a desktop session
// returns its bearer token directly without contacting the OAuth token endpoint, and that an account
// without any usable credential reports the actionable re-import error.
func TestAccountStateTokenLocalDesktopSession(t *testing.T) {
	cfg := config.DefaultConfig()
	// Unroutable token endpoint: any refresh attempt would fail this test.
	cfg.TokenURL = "http://127.0.0.1:1/token"

	state := rotator.NewAccountState(&auth.Account{
		Email:            "desktop@example.com",
		AccessToken:      "desktop-token-123",
		CredentialSource: auth.CredentialSourceLocalDesktop,
		ProjectID:        "proj-desktop",
		AddedAt:          time.Now().UTC().Format(time.RFC3339),
	})

	token, err := state.Token(t.Context(), cfg, time.Now)
	if err != nil {
		t.Fatalf("Token() for local desktop session returned error: %v", err)
	}
	if token != "desktop-token-123" {
		t.Errorf("expected imported bearer token, got masked %q", token[:4]+"...")
	}

	empty := rotator.NewAccountState(&auth.Account{Email: "empty@example.com", ProjectID: "proj-empty"})
	if _, err := empty.Token(t.Context(), cfg, time.Now); !errors.Is(err, auth.ErrLocalSessionExpired) {
		t.Errorf("expected ErrLocalSessionExpired for credential-less account, got %v", err)
	}
}

// TestAccountStateUsableRequiresCredentials verifies that accounts without any stored credential are
// reported as unusable so the rotator never dispatches requests that must fail authentication.
func TestAccountStateUsableRequiresCredentials(t *testing.T) {
	withRefresh := rotator.NewAccountState(newTestAccount("with-refresh@example.com"))
	if !withRefresh.Usable() {
		t.Error("expected account with refresh token to be usable")
	}

	local := rotator.NewAccountState(&auth.Account{Email: "local@example.com", AccessToken: "token", ProjectID: "proj"})
	if !local.Usable() {
		t.Error("expected imported desktop session to be usable")
	}

	credentialLess := rotator.NewAccountState(&auth.Account{Email: "empty@example.com", ProjectID: "proj"})
	if credentialLess.Usable() {
		t.Error("expected account without credentials to be unusable")
	}
}

// TestPoolRefreshUpdatesImportedDesktopToken verifies that hot reloading accounts replaces a state
// whose imported desktop bearer token changed, instead of keeping the stale credential in rotation.
func TestPoolRefreshUpdatesImportedDesktopToken(t *testing.T) {
	tempDir := t.TempDir()
	storePath := filepath.Join(tempDir, "accounts.json")
	store := auth.NewStore(storePath)

	seed := &auth.Account{
		Email:            "desktop@example.com",
		AccessToken:      "old-token",
		CredentialSource: auth.CredentialSourceLocalDesktop,
		ProjectID:        "proj-desktop",
		AddedAt:          time.Now().UTC().Format(time.RFC3339),
	}
	if _, err := store.Upsert(seed); err != nil {
		t.Fatalf("seeding account: %v", err)
	}

	pool, err := rotator.NewPool(store, config.DefaultConfig(), time.Now)
	if err != nil {
		t.Fatalf("NewPool failed: %v", err)
	}

	// Simulate `agw login --local` re-importing a fresh desktop session.
	refreshed := *seed
	refreshed.AccessToken = "new-token"
	if _, err := store.Upsert(&refreshed); err != nil {
		t.Fatalf("updating account: %v", err)
	}

	// Bypass the file check throttle so the reload is observed immediately.
	time.Sleep(rotator.FileCheckInterval + 100*time.Millisecond)
	if err := pool.RefreshIfChanged(); err != nil {
		t.Fatalf("RefreshIfChanged failed: %v", err)
	}

	states := pool.States()
	if len(states) != 1 {
		t.Fatalf("expected 1 state, got %d", len(states))
	}

	cfg := config.DefaultConfig()
	cfg.TokenURL = "http://127.0.0.1:1/token"
	token, err := states[0].Token(t.Context(), cfg, time.Now)
	if err != nil {
		t.Fatalf("Token() failed: %v", err)
	}
	if token != "new-token" {
		t.Errorf("expected re-imported token to be served, got masked %q", token[:3]+"...")
	}
}
