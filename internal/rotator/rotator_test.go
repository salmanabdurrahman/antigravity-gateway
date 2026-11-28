// Package rotator_test provides unit and concurrency test suites for the rotator package.
package rotator_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// mockGoogleServer simulates upstream Google Cloud Code Assist and token endpoints.
type mockGoogleServer struct {
	// server is the underlying test HTTP server.
	server *httptest.Server

	// requestCount counts incoming requests.
	requestCount atomic.Int64

	// projectsSeen records project IDs observed in request payloads.
	projectsSeen []string

	// handler handles incoming HTTP requests.
	handler http.HandlerFunc
}

// newMockGoogleServer constructs a simulated Google Cloud Code Assist test server.
func newMockGoogleServer(handler http.HandlerFunc) *mockGoogleServer {
	m := &mockGoogleServer{handler: handler}
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requestCount.Add(1)
		m.handler(w, r)
	}))
	return m
}

// close shuts down the underlying mock HTTP server.
func (m *mockGoogleServer) close() {
	m.server.Close()
}

// TestRotatorStickyAndFailover429 verifies sticky account selection and immediate failover to another account on HTTP 429.
func TestRotatorStickyAndFailover429(t *testing.T) {
	ctx := t.Context()
	var calls []string

	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		// If authorization token refresh endpoint:
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
		calls = append(calls, body.Project)

		// Third call triggers 429 for project-a
		if body.Project == "proj-a@example.com" && len(calls) == 3 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    429,
					"status":  "RESOURCE_EXHAUSTED",
					"message": "Resource exhausted",
					"details": []any{
						map[string]any{
							"@type":      "type.googleapis.com/google.rpc.RetryInfo",
							"retryDelay": "3600s",
						},
					},
				},
			})
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"test\":\"ok\"}\n\n"))
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		Strategy:  string(rotator.StrategySticky),
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}

	a1 := newTestAccount("a@example.com")
	a2 := newTestAccount("b@example.com")

	pool, err := rotator.NewPool(nil, cfg, time.Now)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}
	pool.SetAccounts([]*auth.Account{a1, a2})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())

	builder := func(s *rotator.AccountState) (any, error) {
		return map[string]any{"project": s.Account().ProjectID}, nil
	}

	// First 2 calls use account a
	for i := range 2 {
		res, err := r.Generate(ctx, "gemini-pro", builder)
		if err != nil {
			t.Fatalf("call %d failed: %v", i, err)
		}
		if res.State.Account().Email != "a@example.com" {
			t.Errorf("call %d: expected a@example.com, got %s", i, res.State.Account().Email)
		}
		_ = res.Response.Body.Close()
	}

	// Third call hits 429 on account a, transparently retries on account b
	res3, err := r.Generate(ctx, "gemini-pro", builder)
	if err != nil {
		t.Fatalf("third call failed: %v", err)
	}
	if res3.State.Account().Email != "b@example.com" {
		t.Errorf("third call: expected failover to b@example.com, got %s", res3.State.Account().Email)
	}
	_ = res3.Response.Body.Close()

	// Calls should be [a, a, a(429), b(200)]
	if len(calls) != 4 {
		t.Errorf("expected 4 calls, got %d", len(calls))
	}
	if calls[0] != "proj-a@example.com" || calls[1] != "proj-a@example.com" || calls[2] != "proj-a@example.com" || calls[3] != "proj-b@example.com" {
		t.Errorf("unexpected call sequence: %v", calls)
	}

	// Account a remains cooled for "gemini-pro", so next call still goes to b
	res4, err := r.Generate(ctx, "gemini-pro", builder)
	if err != nil {
		t.Fatalf("fourth call failed: %v", err)
	}
	if res4.State.Account().Email != "b@example.com" {
		t.Errorf("expected b@example.com for gemini-pro, got %s", res4.State.Account().Email)
	}
	_ = res4.Response.Body.Close()

	// But for a different model, account a is not cooling, so sticky picks a
	resOther, err := r.Generate(ctx, "claude-3-7-sonnet", builder)
	if err != nil {
		t.Fatalf("other model call failed: %v", err)
	}
	if resOther.State.Account().Email != "a@example.com" {
		t.Errorf("expected a@example.com for different model, got %s", resOther.State.Account().Email)
	}
	_ = resOther.Response.Body.Close()
}

// TestRotatorBareResourceExhaustedDoesNotCoolAccount verifies that a bare RESOURCE_EXHAUSTED
// response is surfaced as a request-scoped prompt rejection without cooling or rotating the
// healthy account pool.
func TestRotatorBareResourceExhaustedDoesNotCoolAccount(t *testing.T) {
	ctx := t.Context()
	var upstreamCalls atomic.Int64

	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-access-token",
				"expires_in":   3600,
				"token_type":   "Bearer",
			})
			return
		}

		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    429,
				"status":  "RESOURCE_EXHAUSTED",
				"message": "Resource has been exhausted (e.g. check quota).",
			},
		})
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		Strategy:  string(rotator.StrategySticky),
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}

	pool, err := rotator.NewPool(nil, cfg, time.Now)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}
	pool.SetAccounts([]*auth.Account{newTestAccount("a@example.com")})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())

	builder := func(s *rotator.AccountState) (any, error) {
		return map[string]any{"project": s.Account().ProjectID}, nil
	}

	_, err = r.Generate(ctx, "gemini-pro", builder)
	if err == nil {
		t.Fatal("expected prompt rejection error, got nil")
	}

	var rotErr *rotator.RotatorError
	if !errors.As(err, &rotErr) {
		t.Fatalf("expected *RotatorError, got %T: %v", err, err)
	}
	if !rotErr.PromptRejected {
		t.Errorf("expected PromptRejected=true, got false: %v", rotErr)
	}
	if rotErr.StatusCode != http.StatusBadRequest {
		t.Errorf("expected HTTP 400 for prompt rejection, got %d", rotErr.StatusCode)
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Errorf("expected the rejection to surface without rotation (1 upstream call), got %d", got)
	}

	states := pool.States()
	if len(states) != 1 {
		t.Fatalf("expected 1 pool state, got %d", len(states))
	}
	if until := states[0].CoolingUntil("gemini-pro"); !until.IsZero() {
		t.Errorf("expected no cooldown for prompt rejection, got until %v", until)
	}

	// The next request must reach upstream again instead of waiting for a cooldown to expire.
	if _, err := r.Generate(ctx, "gemini-pro", builder); err == nil {
		t.Fatal("expected repeated prompt rejection, got nil")
	}
	if got := upstreamCalls.Load(); got != 2 {
		t.Errorf("expected a second upstream attempt, got %d calls", got)
	}
}

// TestRotatorShortWaitAbsorption verifies that short cooldown waits (<= 20s) are absorbed via sleep and retried on the same account.
func TestRotatorShortWaitAbsorption(t *testing.T) {
	ctx := t.Context()
	var callCount int

	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-token", "expires_in": 3600})
			return
		}

		callCount++
		if callCount == 1 {
			// Return 429 with 300ms delay
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"details": []any{
						map[string]any{
							"@type":      "type.googleapis.com/google.rpc.RetryInfo",
							"retryDelay": "0.3s",
						},
					},
				},
			})
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}

	a1 := newTestAccount("solo@example.com")
	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{a1})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())
	r.SetMaxWait(2 * time.Second)

	builder := func(s *rotator.AccountState) (any, error) {
		return map[string]any{"ok": true}, nil
	}

	// Should absorb 300ms wait and succeed on second attempt
	res, err := r.Generate(ctx, "gemini-pro", builder)
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}
	_ = res.Response.Body.Close()

	if callCount != 2 {
		t.Errorf("expected 2 calls, got %d", callCount)
	}
}

// TestRotatorLongWaitFailsImmediatelyWithRetryAfter verifies that long cooldown waits (> 20s) fail immediately with HTTP 429 and Retry-After.
func TestRotatorLongWaitFailsImmediatelyWithRetryAfter(t *testing.T) {
	ctx := t.Context()

	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-token", "expires_in": 3600})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"details": []any{
					map[string]any{
						"@type":      "type.googleapis.com/google.rpc.RetryInfo",
						"retryDelay": "600s",
					},
				},
			},
		})
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}

	a1 := newTestAccount("solo@example.com")
	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{a1})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())
	r.SetMaxWait(20 * time.Second)

	builder := func(s *rotator.AccountState) (any, error) {
		return map[string]any{"ok": true}, nil
	}

	// First call hits 600s cooldown
	_, err := r.Generate(ctx, "gemini-pro", builder)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	var rotErr *rotator.RotatorError
	if !errors.As(err, &rotErr) {
		t.Fatalf("expected RotatorError, got %T: %v", err, err)
	}
	if rotErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected HTTP 429, got %d", rotErr.StatusCode)
	}
	if rotErr.RetryAfter < 590*time.Second {
		t.Errorf("expected RetryAfter ~600s, got %v", rotErr.RetryAfter)
	}
}

// TestRotatorTerminal400DoesNotRotate verifies that client-side 400 Bad Request errors do not rotate accounts.
func TestRotatorTerminal400DoesNotRotate(t *testing.T) {
	ctx := t.Context()
	var callCount int

	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-token", "expires_in": 3600})
			return
		}
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    400,
				"message": "Invalid argument: contents is empty",
			},
		})
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}

	a1 := newTestAccount("a@example.com")
	a2 := newTestAccount("b@example.com")
	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{a1, a2})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())

	builder := func(s *rotator.AccountState) (any, error) {
		return map[string]any{"bad": "request"}, nil
	}

	_, err := r.Generate(ctx, "gemini-pro", builder)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	var rotErr *rotator.RotatorError
	if !errors.As(err, &rotErr) || rotErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected HTTP 400 RotatorError, got %v", err)
	}

	// Must not retry on other accounts
	if callCount != 1 {
		t.Errorf("expected exactly 1 call for 400 error, got %d", callCount)
	}
}

// TestRotatorWithAccount verifies targeted execution pinned to a specific requested account email.
func TestRotatorWithAccount(t *testing.T) {
	ctx := t.Context()

	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "valid-token", "expires_in": 3600})
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:  config.ProfileManual,
		TokenURL: mockServer.server.URL + "/token",
	}

	a1 := newTestAccount("user@example.com")
	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{a1})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)

	executed := false
	err := r.WithAccount(ctx, func(ctx context.Context, token string, state *rotator.AccountState) error {
		executed = true
		if token != "valid-token" {
			t.Errorf("got token %s, want valid-token", token)
		}
		if state.Account().Email != "user@example.com" {
			t.Errorf("got email %s, want user@example.com", state.Account().Email)
		}
		return nil
	})

	if err != nil {
		t.Fatalf("WithAccount failed: %v", err)
	}
	if !executed {
		t.Errorf("fn was not executed")
	}
}

// TestRotatorWaitAbsorptionDoesNotExhaustAttempts verifies that absorbed cooldown waits are not
// charged against the dispatch budget, so a pool of one account still retries after a wait that
// lands on what used to be the final loop iteration.
func TestRotatorWaitAbsorptionDoesNotExhaustAttempts(t *testing.T) {
	ctx := t.Context()
	var calls atomic.Int64

	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-token", "expires_in": 3600})
			return
		}
		attempt := calls.Add(1)
		if attempt <= 3 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"status": "RESOURCE_EXHAUSTED",
					"details": []any{
						map[string]any{
							"@type":      "type.googleapis.com/google.rpc.RetryInfo",
							"retryDelay": "0.05s",
						},
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}
	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{newTestAccount("solo@example.com")})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())
	r.SetMaxWait(2 * time.Second)

	res, err := r.Generate(ctx, "gemini-pro", func(s *rotator.AccountState) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("expected success after absorbed waits, got %v", err)
	}
	_ = res.Response.Body.Close()

	if got := calls.Load(); got != 4 {
		t.Errorf("expected 4 upstream calls, got %d", got)
	}
}

// TestRotatorBackoffSleepCanceled verifies that context cancellation during a server-error backoff
// sleep propagates instead of being discarded.
func TestRotatorBackoffSleepCanceled(t *testing.T) {
	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-token", "expires_in": 3600})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}
	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{newTestAccount("solo@example.com")})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())
	r.SetSleepFn(func(ctx context.Context, d time.Duration) error {
		return context.Canceled
	})

	_, err := r.Generate(t.Context(), "gemini-pro", func(s *rotator.AccountState) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled from backoff sleep, got %v", err)
	}
}

// TestRotatorHybridTokenBucketRefund verifies that a hybrid selection token consumed before an
// upstream 401 retry is refunded, keeping consume and refund balanced.
func TestRotatorHybridTokenBucketRefund(t *testing.T) {
	var calls atomic.Int64

	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-token", "expires_in": 3600})
			return
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "unauthorized"}})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		Strategy:  string(rotator.StrategyHybrid),
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}
	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	account := newTestAccount("hybrid@example.com")
	pool.SetAccounts([]*auth.Account{account})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())

	before := pool.States()[0].TokenBucket().Tokens(time.Now())
	res, err := r.Generate(t.Context(), "gemini-pro", func(s *rotator.AccountState) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	_ = res.Response.Body.Close()

	after := pool.States()[0].TokenBucket().Tokens(time.Now())
	if after < before-1.5 || after > before-0.5 {
		t.Errorf("expected exactly one consumed token, before=%.2f after=%.2f", before, after)
	}
}

// TestRotatorPermanentForbiddenDisablesAccount verifies a terms-of-service 403 permanently disables
// the account instead of applying a transient cooldown, while transient 403s still fail over. The
// protective pool pause stays disabled here so this test keeps isolating per-account behaviour; the
// pause itself is covered by the dedicated suspension tests.
func TestRotatorPermanentForbiddenDisablesAccount(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantBanned bool
	}{
		{
			name:       "permanent suspension",
			body:       `{"error":{"message":"This account has been disabled for a violation of terms of service."}}`,
			wantBanned: true,
		},
		{
			name:       "validation required",
			body:       `{"error":{"status":"VALIDATION_REQUIRED","message":"Verify your account to continue."}}`,
			wantBanned: true,
		},
		{
			name:       "transient project permission",
			body:       `{"error":{"message":"Caller does not have permission for this project"}}`,
			wantBanned: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-token", "expires_in": 3600})
					return
				}
				var body struct {
					Project string `json:"project"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.Project == "proj-banned@example.com" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			})
			defer mockServer.close()

			cfg := &config.Config{
				Profile:   config.ProfileManual,
				TokenURL:  mockServer.server.URL + "/token",
				Endpoints: []string{mockServer.server.URL},
				// Keep the protective pool pause out of the picture: this case isolates how a single
				// account is treated after upstream refuses it.
				DisableSuspendPause: true,
			}
			pool, _ := rotator.NewPool(nil, cfg, time.Now)
			pool.SetAccounts([]*auth.Account{
				newTestAccount("banned@example.com"),
				newTestAccount("healthy@example.com"),
			})

			r := rotator.NewRotator(pool, cfg, nil, time.Now)
			r.SetHTTPClient(mockServer.server.Client())

			res, err := r.Generate(t.Context(), "gemini-pro", func(s *rotator.AccountState) (any, error) {
				return map[string]any{"project": s.ProjectID()}, nil
			})
			if err != nil {
				t.Fatalf("expected failover to healthy account, got %v", err)
			}
			_ = res.Response.Body.Close()
			if res.State.Email() != "healthy@example.com" {
				t.Errorf("expected healthy account to serve, got %s", res.State.Email())
			}

			states := pool.States()
			banned := states[0]
			if tc.wantBanned {
				if banned.Usable() {
					t.Error("permanently banned account must be disabled")
				}
				if until := banned.CoolingUntil("*"); !until.IsZero() {
					t.Errorf("permanently banned account must not receive a cooldown, got %v", until)
				}
			} else {
				if !banned.Usable() {
					t.Error("transient 403 must not disable the account")
				}
				if until := banned.CoolingUntil("*"); until.IsZero() {
					t.Error("transient 403 must apply a cooldown")
				}
			}
		})
	}
}

// TestRotatorStaticToken401CoolsDownImmediately verifies that an account holding only a static
// desktop bearer token is never retried with the identical credential after a 401; it cools down on
// the first refusal instead of doubling latency with a guaranteed-failing resend.
func TestRotatorStaticToken401CoolsDownImmediately(t *testing.T) {
	var dispatches atomic.Int64
	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			t.Errorf("static desktop token must never hit the OAuth token endpoint")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		dispatches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Request had invalid authentication credentials"}}`))
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}
	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{{
		Email:            "desktop@example.com",
		AccessToken:      "static-desktop-token",
		CredentialSource: auth.CredentialSourceLocalDesktop,
		ProjectID:        "proj-desktop",
		AddedAt:          time.Now().UTC().Format(time.RFC3339),
	}})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())

	_, err := r.Generate(t.Context(), "gemini-pro", func(s *rotator.AccountState) (any, error) {
		return map[string]any{"project": s.ProjectID()}, nil
	})
	var rotErr *rotator.RotatorError
	if !errors.As(err, &rotErr) || rotErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected HTTP 401 rotator error, got %v", err)
	}

	if got := dispatches.Load(); got != 1 {
		t.Errorf("expected exactly 1 upstream dispatch for a static token, got %d", got)
	}
	if until := pool.States()[0].CoolingUntil("*"); until.IsZero() {
		t.Error("static-token 401 must cool the account down immediately")
	}
}

// TestRotatorProgressiveCooldownWithoutRetryInfo verifies escalating backoff for repeated 429s that
// carry no explicit retry metadata.
func TestRotatorProgressiveCooldownWithoutRetryInfo(t *testing.T) {
	baseTime := time.Now()
	current := baseTime
	var nowMu sync.Mutex

	nowFn := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return current
	}
	advance := func(d time.Duration) {
		nowMu.Lock()
		defer nowMu.Unlock()
		current = current.Add(d)
	}

	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-token", "expires_in": 7200})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "quota exceeded"}})
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}
	pool, _ := rotator.NewPool(nil, cfg, nowFn)
	pool.SetAccounts([]*auth.Account{newTestAccount("progress@example.com")})

	r := rotator.NewRotator(pool, cfg, nil, nowFn)
	r.SetHTTPClient(mockServer.server.Client())

	builder := func(s *rotator.AccountState) (any, error) {
		return map[string]any{"ok": true}, nil
	}

	wantCooldowns := []time.Duration{60 * time.Second, 5 * time.Minute, 30 * time.Minute}
	for i, want := range wantCooldowns {
		if _, err := r.Generate(t.Context(), "gemini-pro", builder); err == nil {
			t.Fatalf("call %d: expected 429 error", i)
		}
		until := pool.States()[0].CoolingUntil("gemini-pro")
		if got := until.Sub(nowFn()); got < want-2*time.Second || got > want+2*time.Second {
			t.Errorf("call %d: cooldown = %v; want ~%v", i, got, want)
		}
		if got := pool.States()[0].QuotaFraction("gemini-pro"); got != 0 {
			t.Errorf("call %d: quota fraction = %v; want 0 after confirmed 429", i, got)
		}
		advance(want + time.Second)
	}
}

// TestRotatorSuccessClearsZeroQuotaFraction verifies a confirmed successful request clears a stale
// zero quota fraction recorded during an earlier 429, so hybrid scoring can recover the account.
func TestRotatorSuccessClearsZeroQuotaFraction(t *testing.T) {
	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-token", "expires_in": 3600})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		Strategy:  string(rotator.StrategyHybrid),
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}
	pool, _ := rotator.NewPool(nil, cfg, time.Now)
	pool.SetAccounts([]*auth.Account{newTestAccount("recover@example.com")})
	pool.States()[0].SetQuotaFraction("gemini-pro", 0)

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())

	res, err := r.Generate(t.Context(), "gemini-pro", func(s *rotator.AccountState) (any, error) {
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	_ = res.Response.Body.Close()

	if got := pool.States()[0].QuotaFraction("gemini-pro"); got != 1.0 {
		t.Errorf("quota fraction after success = %v; want 1.0", got)
	}
}

// TestRotatorGenerateWithExcludePrefersOtherUsableAccount verifies soft-exclusion rotates sticky
// retries to another ready account, and falls back to the excluded account when alternatives are
// unavailable or cooling down.
func TestRotatorGenerateWithExcludePrefersOtherUsableAccount(t *testing.T) {
	mockServer := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-token", "expires_in": 3600})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"test\":\"ok\"}\n\n"))
	})
	defer mockServer.close()

	cfg := &config.Config{
		Profile:   config.ProfileManual,
		Strategy:  string(rotator.StrategySticky),
		TokenURL:  mockServer.server.URL + "/token",
		Endpoints: []string{mockServer.server.URL},
	}

	a1 := newTestAccount("first@example.com")
	a2 := newTestAccount("second@example.com")

	pool, err := rotator.NewPool(nil, cfg, time.Now)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}
	pool.SetAccounts([]*auth.Account{a1, a2})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(mockServer.server.Client())
	r.SetSleepFn(func(context.Context, time.Duration) error { return nil })

	builder := func(s *rotator.AccountState) (any, error) {
		return map[string]any{"project": s.Account().ProjectID}, nil
	}

	// With first@example.com excluded and second@example.com ready, second@example.com must be picked.
	exclude := map[string]struct{}{"first@example.com": {}}
	res, err := r.GenerateWithExclude(t.Context(), "gemini-pro", exclude, builder)
	if err != nil {
		t.Fatalf("GenerateWithExclude failed: %v", err)
	}
	_ = res.Response.Body.Close()
	if res.State.Email() != "second@example.com" {
		t.Fatalf("expected second@example.com when first is excluded, got %s", res.State.Email())
	}

	// When second@example.com is cooling down for 1 hour, excluding first@example.com must fall back
	// to first@example.com immediately instead of failing with a 429 cooldown wait.
	pool.States()[1].SetCooldown("gemini-pro", time.Now().Add(time.Hour), time.Now(), "429")
	res, err = r.GenerateWithExclude(t.Context(), "gemini-pro", exclude, builder)
	if err != nil {
		t.Fatalf("GenerateWithExclude fallback failed: %v", err)
	}
	_ = res.Response.Body.Close()
	if res.State.Email() != "first@example.com" {
		t.Fatalf("expected fallback to first@example.com when alternative is cooling, got %s", res.State.Email())
	}
}
