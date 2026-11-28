// Package rotator_test provides unit and concurrency test suites for the rotator package.
package rotator_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// newPacingRotator builds a single-account rotator over a mock upstream so pacing, budget, and breaker
// assertions do not depend on pooled rotation.
func newPacingRotator(t *testing.T, handler func(w http.ResponseWriter, r *http.Request), mutate func(cfg *config.Config)) *rotator.Rotator {
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
		handler(w, r)
	})
	t.Cleanup(mockServer.close)

	cfg := &config.Config{
		Strategy:  string(rotator.StrategySticky),
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}
	if mutate != nil {
		mutate(cfg)
	}

	pool, err := rotator.NewPool(nil, cfg, time.Now)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}
	pool.SetAccounts([]*auth.Account{newTestAccount("paced@example.com")})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())
	return r
}

// pacingBuilder builds the minimal upstream payload the mock handlers ignore.
func pacingBuilder(s *rotator.AccountState) (any, error) {
	return map[string]any{"project": s.ProjectID()}, nil
}

// TestRotatorCapsInFlightPerCredential verifies that a second concurrent request waits for the first to
// release its slot when the in-flight ceiling is one.
func TestRotatorCapsInFlightPerCredential(t *testing.T) {
	var (
		mu           sync.Mutex
		active       int
		maxActive    int
		unblock      = make(chan struct{})
		firstEntered = make(chan struct{}, 1)
	)

	r := newPacingRotator(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()

		select {
		case firstEntered <- struct{}{}:
		default:
		}

		<-unblock

		mu.Lock()
		active--
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"response\":{}}\n\n"))
	}, func(cfg *config.Config) {
		cfg.MaxInFlightPerAccount = 1
	})

	done := make(chan error, 2)
	go func() {
		res, err := r.Generate(t.Context(), "gemini-pro", pacingBuilder)
		if err == nil {
			_ = res.Response.Body.Close()
		}
		done <- err
	}()

	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first dispatch never reached upstream")
	}

	go func() {
		res, err := r.Generate(t.Context(), "gemini-pro", pacingBuilder)
		if err == nil {
			_ = res.Response.Body.Close()
		}
		done <- err
	}()

	// Give the second dispatch enough time to attempt acquisition while the first holds its slot.
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	observed := maxActive
	mu.Unlock()
	if observed != 1 {
		t.Errorf("max concurrent upstream streams = %d; want 1", observed)
	}

	close(unblock)

	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("paced dispatch failed: %v", err)
		}
	}
}

// TestRotatorDispatchBudgetCapsAttempts verifies that an explicit dispatch budget stops the request after
// the configured number of upstream attempts instead of continuing to the next pooled account.
func TestRotatorDispatchBudgetCapsAttempts(t *testing.T) {
	var dispatches atomic.Int64

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
		dispatches.Add(1)

		if body.Project == "proj-first@example.com" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":500,"message":"internal error","status":"INTERNAL"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"response\":{}}\n\n"))
	})
	t.Cleanup(mockServer.close)

	cfg := &config.Config{
		Profile:                 config.ProfileManual,
		Strategy:                string(rotator.StrategySticky),
		TokenURL:                mockServer.server.URL + "/token",
		Endpoints:               []string{mockServer.server.URL},
		MaxDispatchesPerRequest: 1,
	}
	pool, err := rotator.NewPool(nil, cfg, time.Now)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}
	pool.SetAccounts([]*auth.Account{newTestAccount("first@example.com"), newTestAccount("second@example.com")})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())
	r.SetSleepFn(func(ctx context.Context, d time.Duration) error { return nil })

	if _, err := r.Generate(t.Context(), "gemini-pro", pacingBuilder); err == nil {
		t.Fatal("expected the failing first dispatch to surface an error under a budget of one")
	}

	if got := dispatches.Load(); got != 1 {
		t.Errorf("upstream dispatches = %d; want 1 with max_dispatches_per_request=1", got)
	}
}

// TestRotatorPromptRejectionGuard verifies that repeated bare RESOURCE_EXHAUSTED rejections park the account
// instead of retrying forever, and that the parked account stops receiving traffic.
func TestRotatorPromptRejectionGuard(t *testing.T) {
	var dispatches atomic.Int64

	r := newPacingRotator(t, func(w http.ResponseWriter, r *http.Request) {
		dispatches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`))
	}, nil)

	for attempt := 1; attempt < config.DefaultPromptRejectionLimit; attempt++ {
		_, err := r.Generate(t.Context(), "gemini-pro", pacingBuilder)
		var rotErr *rotator.RotatorError
		if !errors.As(err, &rotErr) || !rotErr.PromptRejected {
			t.Fatalf("attempt %d: expected a prompt-rejection error, got %v", attempt, err)
		}
	}

	// The threshold hit converts the rejection into a parked account with a retry hint.
	_, err := r.Generate(t.Context(), "gemini-pro", pacingBuilder)
	var rotErr *rotator.RotatorError
	if !errors.As(err, &rotErr) {
		t.Fatalf("expected a breaker error, got %v", err)
	}
	if rotErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("breaker StatusCode = %d; want 429", rotErr.StatusCode)
	}
	if rotErr.RetryAfter <= 0 {
		t.Errorf("breaker RetryAfter = %v; want the park duration", rotErr.RetryAfter)
	}
	if rotErr.PromptRejected {
		t.Error("breaker error must not advertise itself as a prompt-scoped retry")
	}

	callsAtPark := dispatches.Load()
	if _, err := r.Generate(t.Context(), "gemini-pro", pacingBuilder); err == nil {
		t.Fatal("expected the parked account to reject the next request")
	}
	if got := dispatches.Load(); got != callsAtPark {
		t.Errorf("parked account received %d extra dispatch(es); want 0", got-callsAtPark)
	}
}

// TestRotatorPromptRejectionGuardDisabledInManualProfile verifies the legacy behaviour survives: bare
// rejections stay request-scoped and never cool the account under the manual profile.
func TestRotatorPromptRejectionGuardDisabledInManualProfile(t *testing.T) {
	var dispatches atomic.Int64

	r := newPacingRotator(t, func(w http.ResponseWriter, r *http.Request) {
		dispatches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`))
	}, func(cfg *config.Config) {
		cfg.Profile = config.ProfileManual
	})

	for attempt := range config.DefaultPromptRejectionLimit + 2 {
		_, err := r.Generate(t.Context(), "gemini-pro", pacingBuilder)
		var rotErr *rotator.RotatorError
		if !errors.As(err, &rotErr) || !rotErr.PromptRejected {
			t.Fatalf("attempt %d: expected a prompt-scoped rejection under the manual profile, got %v", attempt, err)
		}
	}

	if got := dispatches.Load(); got != int64(config.DefaultPromptRejectionLimit+2) {
		t.Errorf("upstream dispatches = %d; want one per attempt under the manual profile", got)
	}
}
