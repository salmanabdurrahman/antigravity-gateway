// Package rotator_test provides unit and concurrency test suites for the rotator package.
package rotator_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// suspensionEnvelope is the Cloud Code Assist response body returned for an account Google suspended
// for a terms violation. It mirrors the tracked tos_403 fixture with a distinct appeal link so the
// assertions prove the value travels from upstream metadata into the persisted account record.
const suspensionEnvelope = `{
  "error": {
    "code": 403,
    "message": "This service has been disabled in this account for violation of Terms of Service. Please submit an appeal to continue using this product.",
    "status": "PERMISSION_DENIED",
    "details": [
      {
        "@type": "type.googleapis.com/google.rpc.ErrorInfo",
        "reason": "TOS_VIOLATION",
        "domain": "cloudcode-pa.googleapis.com",
        "metadata": {
          "uiMessage": "true",
          "appeal_url": "https://forms.gle/SUSPENDEDCASE",
          "appeal_url_link_text": "Submit Appeal"
        }
      }
    ]
  }
}`

// newSuspensionTestRig builds a rotator backed by a temp credential store whose first account is
// suspended upstream and whose second account keeps answering normally.
//
// The configured suspend pause is controlled by the caller through mutate so one test can assert the
// protective pool pause while another asserts immediate failover with the pause disabled. The clock is
// injectable so pause expiry can be exercised without wall-clock sleeps.
func newSuspensionTestRig(t *testing.T, nowFn func() time.Time, mutate func(cfg *config.Config)) (*rotator.Rotator, *auth.Store, *mockGoogleServer) {
	t.Helper()

	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-access-token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}

		var body struct {
			Project string `json:"project"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		if body.Project == "proj-a@example.com" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(suspensionEnvelope))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"test\":\"ok\"}\n\n"))
	})
	t.Cleanup(mockServer.close)

	store := auth.NewStore(filepath.Join(t.TempDir(), "accounts.json"))
	if err := store.Save([]*auth.Account{newTestAccount("a@example.com"), newTestAccount("b@example.com")}); err != nil {
		t.Fatalf("saving test accounts: %v", err)
	}

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		Strategy:  string(rotator.StrategySticky),
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}
	if mutate != nil {
		mutate(cfg)
	}

	pool, err := rotator.NewPool(store, cfg, nowFn)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}

	r := rotator.NewRotator(pool, cfg, nil, nowFn)
	r.SetHTTPClient(mockServer.server.Client())

	return r, store, mockServer
}

// suspensionTestBuilder builds the minimal upstream payload the mock server inspects.
func suspensionTestBuilder(s *rotator.AccountState) (any, error) {
	return map[string]any{"project": s.Account().ProjectID}, nil
}

// TestRotatorSuspensionDisablesAccountAndPausesPool verifies that a terminal suspension verdict removes
// the account from rotation with its verdict persisted, arms the protective pool pause, and refuses the
// next request without contacting upstream again.
func TestRotatorSuspensionDisablesAccountAndPausesPool(t *testing.T) {
	r, store, mockServer := newSuspensionTestRig(t, time.Now, nil)

	_, err := r.Generate(t.Context(), "gemini-pro", suspensionTestBuilder)
	if err == nil {
		t.Fatal("expected the suspended account to fail the request")
	}

	var rotErr *rotator.RotatorError
	if !errors.As(err, &rotErr) {
		t.Fatalf("expected *rotator.RotatorError, got %T: %v", err, err)
	}
	if rotErr.StatusCode != http.StatusForbidden {
		t.Errorf("StatusCode = %d; want %d", rotErr.StatusCode, http.StatusForbidden)
	}
	if rotErr.Suspension == nil {
		t.Fatalf("expected the suspension verdict on the error, got none: %v", rotErr)
	}
	if rotErr.Suspension.Kind != rotator.SuspensionTermsOfService {
		t.Errorf("Suspension.Kind = %q; want %q", rotErr.Suspension.Kind, rotator.SuspensionTermsOfService)
	}
	if rotErr.Suspension.AppealURL != "https://forms.gle/SUSPENDEDCASE" {
		t.Errorf("Suspension.AppealURL = %q; want the upstream appeal link", rotErr.Suspension.AppealURL)
	}

	stored, err := store.Load()
	if err != nil {
		t.Fatalf("reloading accounts: %v", err)
	}
	suspended := stored[0]
	if suspended.Email != "a@example.com" {
		t.Fatalf("first stored account = %q; want a@example.com", suspended.Email)
	}
	if !suspended.Disabled {
		t.Error("suspended account is still enabled in the credential store")
	}
	if suspended.SuspendKind != string(rotator.SuspensionTermsOfService) {
		t.Errorf("SuspendKind = %q; want %q", suspended.SuspendKind, rotator.SuspensionTermsOfService)
	}
	if suspended.AppealURL != "https://forms.gle/SUSPENDEDCASE" {
		t.Errorf("AppealURL = %q; want the upstream appeal link", suspended.AppealURL)
	}
	if suspended.SuspendedAt == "" || suspended.SuspendReason == "" {
		t.Errorf("suspension metadata incomplete: at=%q reason=%q", suspended.SuspendedAt, suspended.SuspendReason)
	}

	// A suspension is terminal, so the account must not sit on an ordinary cooldown that would cycle it
	// back into rotation after 30 minutes.
	for _, snapshot := range rotatorSnapshot(t, r) {
		if snapshot.Email == "a@example.com" && len(snapshot.Cooldowns) != 0 {
			t.Errorf("suspended account carries cooldowns %v; want none", snapshot.Cooldowns)
		}
	}

	// The protective pause must reject the next request before any upstream dispatch happens.
	callsAfterSuspension := mockServer.requestCount.Load()
	_, err = r.Generate(t.Context(), "gemini-pro", suspensionTestBuilder)
	if err == nil {
		t.Fatal("expected the protective pause to reject the next request")
	}
	if !errors.As(err, &rotErr) {
		t.Fatalf("expected *rotator.RotatorError for the paused gateway, got %T: %v", err, err)
	}
	if rotErr.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v; want the remaining pause duration", rotErr.RetryAfter)
	}
	if rotErr.Suspension == nil || rotErr.Suspension.AppealURL == "" {
		t.Errorf("paused request lost the suspension appeal link: %+v", rotErr.Suspension)
	}
	if calls := mockServer.requestCount.Load(); calls != callsAfterSuspension {
		t.Errorf("upstream received %d request(s) during the protective pause; want 0", calls-callsAfterSuspension)
	}
}

// TestRotatorSuspensionFailsOverWhenPauseDisabled verifies the opt-out path: with the protective pause
// disabled the request fails over to a healthy sibling account immediately, while the suspended account
// still leaves rotation permanently.
func TestRotatorSuspensionFailsOverWhenPauseDisabled(t *testing.T) {
	r, store, _ := newSuspensionTestRig(t, time.Now, func(cfg *config.Config) {
		cfg.DisableSuspendPause = true
	})

	res, err := r.Generate(t.Context(), "gemini-pro", suspensionTestBuilder)
	if err != nil {
		t.Fatalf("expected failover to the healthy account, got error: %v", err)
	}
	defer func() { _ = res.Response.Body.Close() }()

	if got := res.State.Account().Email; got != "b@example.com" {
		t.Errorf("served by %q; want b@example.com", got)
	}

	stored, err := store.Load()
	if err != nil {
		t.Fatalf("reloading accounts: %v", err)
	}
	if !stored[0].Disabled || stored[0].SuspendKind == "" {
		t.Errorf("suspended account was not persisted as suspended: %+v", stored[0])
	}
}

// rotatorSnapshot returns the pool snapshots exposed by the rotator, skipping the test when the pool is
// not reachable through the public API.
func rotatorSnapshot(t *testing.T, r *rotator.Rotator) []rotator.AccountSnapshot {
	t.Helper()

	pool := r.Pool()
	if pool == nil {
		t.Skip("rotator pool is not exposed for snapshots")
	}
	return pool.Snapshots()
}

// TestRotatorSuspensionPauseExpires verifies that the protective pool pause ends on schedule: once the
// configured window elapses, dispatch resumes on the remaining healthy account instead of rejecting
// every request forever.
func TestRotatorSuspensionPauseExpires(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	r, _, mockServer := newSuspensionTestRig(t, clock, func(cfg *config.Config) {
		cfg.SuspendPauseSeconds = 60
	})

	if _, err := r.Generate(t.Context(), "gemini-pro", suspensionTestBuilder); err == nil {
		t.Fatal("expected the suspended account to fail the request")
	}
	callsWhileSuspended := mockServer.requestCount.Load()

	if _, err := r.Generate(t.Context(), "gemini-pro", suspensionTestBuilder); err == nil {
		t.Fatal("expected the protective pause to reject the next request")
	}
	if calls := mockServer.requestCount.Load(); calls != callsWhileSuspended {
		t.Errorf("upstream received %d request(s) during the protective pause; want 0", calls-callsWhileSuspended)
	}

	now = now.Add(61 * time.Second)

	res, err := r.Generate(t.Context(), "gemini-pro", suspensionTestBuilder)
	if err != nil {
		t.Fatalf("expected dispatch to resume after the pause expired, got error: %v", err)
	}
	defer func() { _ = res.Response.Body.Close() }()

	if got := res.State.Account().Email; got != "b@example.com" {
		t.Errorf("served by %q; want b@example.com", got)
	}
	if calls := mockServer.requestCount.Load(); calls <= callsWhileSuspended {
		t.Errorf("upstream received no request after the pause expired")
	}
}
