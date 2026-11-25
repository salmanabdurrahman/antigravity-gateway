// Package rotator_test provides unit and concurrency test suites for the rotator package.
package rotator_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
