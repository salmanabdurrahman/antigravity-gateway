// Package rotator_test provides unit and concurrency test suites for the rotator package.
package rotator_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// rotationProbe records which account projects received upstream dispatches during a rotation-mode test.
type rotationProbe struct {
	// projects receives the project identifier decoded from every upstream generation request.
	projects []string
	// calls counts upstream generation requests, excluding token refreshes.
	calls atomic.Int64
}

// newRotationModeServer simulates Cloud Code Assist and answering per project, recording every dispatch.
func newRotationModeServer(t *testing.T, probe *rotationProbe, answer func(project string) (int, string)) *mockGoogleServer {
	t.Helper()

	server := newMockGoogleServer(func(w http.ResponseWriter, r *http.Request) {
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

		probe.calls.Add(1)
		probe.projects = append(probe.projects, body.Project)

		status, payload := answer(body.Project)
		w.Header().Set("Content-Type", "application/json")
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "text/event-stream")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(payload))
	})
	t.Cleanup(server.close)
	return server
}

// newRotationModeRotator builds a rotator over two usable accounts using the supplied configuration
// mutator so each case can choose the profile or an explicit rotation mode.
func newRotationModeRotator(t *testing.T, server *mockGoogleServer, mutate func(cfg *config.Config)) *rotator.Rotator {
	t.Helper()

	cfg := &config.Config{
		Strategy:  string(rotator.StrategySticky),
		TokenURL:  server.server.URL + "/token",
		Endpoints: []string{server.server.URL},
	}
	if mutate != nil {
		mutate(cfg)
	}

	pool, err := rotator.NewPool(nil, cfg, time.Now)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}
	pool.SetAccounts([]*auth.Account{newTestAccount("primary@example.com"), newTestAccount("secondary@example.com")})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(server.server.Client())
	return r
}

// quotaRejection answers a generation request with a quota 429 that carries an explicit retry delay, so
// the affected account cools down for that model instead of being treated as a prompt rejection.
func quotaRejection() (int, string) {
	return http.StatusTooManyRequests, `{
		"error": {
			"code": 429,
			"message": "Individual quota reached. Resets in 1h.",
			"status": "RESOURCE_EXHAUSTED",
			"details": [{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "3600s"}]
		}
	}`
}

// healthyAnswer answers a generation request with a minimal successful SSE stream.
func healthyAnswer() (int, string) {
	return http.StatusOK, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}}\n\n"
}

// TestRotatorSingleModeDoesNotFailover verifies that the safe profile keeps every request on the primary
// account: a quota rejection on that account surfaces immediately instead of spreading traffic onto a
// sibling credential.
func TestRotatorSingleModeDoesNotFailover(t *testing.T) {
	probe := &rotationProbe{}
	server := newRotationModeServer(t, probe, func(string) (int, string) { return quotaRejection() })
	r := newRotationModeRotator(t, server, nil)

	if got := config.DefaultConfig().EffectiveRotationMode(); got != config.RotationModeSingle {
		t.Fatalf("default rotation mode = %q; want %q", got, config.RotationModeSingle)
	}

	_, err := r.Generate(t.Context(), "gemini-pro", func(s *rotator.AccountState) (any, error) {
		return map[string]any{"project": s.ProjectID()}, nil
	})
	if err == nil {
		t.Fatal("expected the primary account's quota rejection to fail the request")
	}

	var rotErr *rotator.RotatorError
	if !errors.As(err, &rotErr) || rotErr.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected a 429 rotator error, got %v", err)
	}
	if calls := probe.calls.Load(); calls != 1 {
		t.Errorf("upstream calls = %d; want exactly 1 (no sibling failover)", calls)
	}
	if len(probe.projects) != 1 || probe.projects[0] != "proj-primary@example.com" {
		t.Errorf("dispatched projects = %v; want only the primary account", probe.projects)
	}
}

// TestRotatorSingleModeSkipsDisabledPrimary verifies that the primary account is the first usable entry in
// pool order, so a disabled credential hands the session to the next one without counting as rotation.
func TestRotatorSingleModeSkipsDisabledPrimary(t *testing.T) {
	probe := &rotationProbe{}
	server := newRotationModeServer(t, probe, func(string) (int, string) { return healthyAnswer() })

	cfg := &config.Config{
		Strategy:  string(rotator.StrategySticky),
		TokenURL:  server.server.URL + "/token",
		Endpoints: []string{server.server.URL},
	}
	pool, err := rotator.NewPool(nil, cfg, time.Now)
	if err != nil {
		t.Fatalf("new pool failed: %v", err)
	}
	disabled := newTestAccount("primary@example.com")
	disabled.Disabled = true
	pool.SetAccounts([]*auth.Account{disabled, newTestAccount("secondary@example.com")})

	r := rotator.NewRotator(pool, cfg, nil, time.Now)
	r.SetHTTPClient(server.server.Client())

	res, err := r.Generate(t.Context(), "gemini-pro", func(s *rotator.AccountState) (any, error) {
		return map[string]any{"project": s.ProjectID()}, nil
	})
	if err != nil {
		t.Fatalf("expected the next usable account to serve, got: %v", err)
	}
	defer func() { _ = res.Response.Body.Close() }()

	if got := res.State.Account().Email; got != "secondary@example.com" {
		t.Errorf("served by %q; want secondary@example.com", got)
	}
}

// TestRotatorPoolModeFailsOverWhenConfigured verifies the manual profile still fails over: the opt-out has
// to remain a working escape hatch for operators who accept pooled rotation.
func TestRotatorPoolModeFailsOverWhenConfigured(t *testing.T) {
	probe := &rotationProbe{}
	server := newRotationModeServer(t, probe, func(project string) (int, string) {
		if project == "proj-primary@example.com" {
			return quotaRejection()
		}
		return healthyAnswer()
	})
	r := newRotationModeRotator(t, server, func(cfg *config.Config) {
		cfg.Profile = config.ProfileManual
	})

	res, err := r.Generate(t.Context(), "gemini-pro", func(s *rotator.AccountState) (any, error) {
		return map[string]any{"project": s.ProjectID()}, nil
	})
	if err != nil {
		t.Fatalf("expected failover to the secondary account, got: %v", err)
	}
	defer func() { _ = res.Response.Body.Close() }()

	if got := res.State.Account().Email; got != "secondary@example.com" {
		t.Errorf("served by %q; want secondary@example.com", got)
	}
}

// TestRotatorForEachAccountFollowsRotationMode verifies that pooled background work only touches the
// primary account under the safe profile, while the manual profile still refreshes every usable account.
func TestRotatorForEachAccountFollowsRotationMode(t *testing.T) {
	probe := &rotationProbe{}
	server := newRotationModeServer(t, probe, func(string) (int, string) { return healthyAnswer() })

	cases := []struct {
		// name labels the rotation mode under test.
		name string
		// mutate configures the profile or rotation mode.
		mutate func(cfg *config.Config)
		// wantAccounts is how many usable accounts the iteration should visit.
		wantAccounts int
	}{
		{name: "safe single", mutate: nil, wantAccounts: 1},
		{name: "manual pool", mutate: func(cfg *config.Config) { cfg.Profile = config.ProfileManual }, wantAccounts: 2},
		{name: "explicit pool override", mutate: func(cfg *config.Config) { cfg.RotationMode = config.RotationModePool }, wantAccounts: 2},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newRotationModeRotator(t, server, tc.mutate)

			visited := 0
			err := r.ForEachAccount(t.Context(), func(ctx context.Context, token string, state *rotator.AccountState) error {
				visited++
				return nil
			})
			if err != nil {
				t.Fatalf("ForEachAccount failed: %v", err)
			}
			if visited != tc.wantAccounts {
				t.Errorf("visited %d account(s); want %d", visited, tc.wantAccounts)
			}
		})
	}
}
