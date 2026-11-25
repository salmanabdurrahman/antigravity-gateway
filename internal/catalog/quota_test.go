// Package catalog discovers upstream model availability, limits, and live quota tracking.
package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// TestParseAccountQuotaSummary verifies tolerant classification of rolling and weekly quota windows
// across the field layouts Google has used for quota summaries.
func TestParseAccountQuotaSummary(t *testing.T) {
	payload := []byte(`{
		"quotaSummary": {
			"fiveHour": {"remainingFraction": 0.7, "resetTime": "2026-12-01T05:00:00Z"},
			"weeklyQuotaInfo": {"remainingFraction": 0.4, "resetTime": "2026-12-05T00:00:00Z"}
		}
	}`)

	summary := ParseAccountQuotaSummary(payload)
	if summary == nil {
		t.Fatal("expected parsed quota summary")
	}
	if summary.FiveHour == nil || summary.FiveHour.RemainingFraction == nil || *summary.FiveHour.RemainingFraction != 0.7 {
		t.Errorf("unexpected five-hour window: %+v", summary.FiveHour)
	}
	if summary.Weekly == nil || summary.Weekly.RemainingFraction == nil || *summary.Weekly.RemainingFraction != 0.4 {
		t.Errorf("unexpected weekly window: %+v", summary.Weekly)
	}

	// Windows labelled through a sibling field rather than the JSON path.
	labelled := ParseAccountQuotaSummary([]byte(`{
		"buckets": [
			{"window": "weekly", "remainingFraction": 0.25},
			{"window": "five_hour", "remainingFraction": 0.5}
		]
	}`))
	if labelled == nil || labelled.Weekly == nil || labelled.FiveHour == nil {
		t.Fatalf("expected labelled windows, got %+v", labelled)
	}
	if *labelled.Weekly.RemainingFraction != 0.25 || *labelled.FiveHour.RemainingFraction != 0.5 {
		t.Errorf("unexpected labelled windows: %+v", labelled)
	}
}

// TestParseAccountQuotaSummaryFiltersUntouched verifies untouched 100% windows do not populate a
// summary, while partially consumed windows with reset times survive.
func TestParseAccountQuotaSummaryFiltersUntouched(t *testing.T) {
	payload := []byte(`{
		"weeklyQuotaInfo": {"remainingFraction": 1.0},
		"fiveHour": {"remainingFraction": 1.0, "resetTime": "2026-12-01T05:00:00Z"}
	}`)

	summary := ParseAccountQuotaSummary(payload)
	if summary == nil {
		t.Fatal("expected the partially reset five-hour window")
	}
	if summary.Weekly != nil {
		t.Errorf("untouched weekly window must be filtered, got %+v", summary.Weekly)
	}
	if summary.FiveHour == nil || summary.FiveHour.ResetTime == "" {
		t.Errorf("expected five-hour window with reset time, got %+v", summary.FiveHour)
	}

	if ParseAccountQuotaSummary([]byte(`{"weeklyQuotaInfo": {"remainingFraction": 1.0}}`)) != nil {
		t.Error("untouched windows must produce no summary")
	}
	if ParseAccountQuotaSummary([]byte(`not json`)) != nil {
		t.Error("invalid JSON must produce no summary")
	}
}

// TestFetchAccountQuotaSummary verifies the quota summary endpoint request and tolerant parsing.
func TestFetchAccountQuotaSummary(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:retrieveUserQuotaSummary" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"quotaSummary": map[string]any{
				"weeklyQuotaInfo": map[string]any{
					"remainingFraction": 0.35,
					"resetTime":         "2026-12-05T00:00:00Z",
				},
				"fiveHour": map[string]any{
					"remainingFraction": 0.8,
					"resetTime":         "2026-12-01T05:00:00Z",
				},
			},
		})
	}))
	defer ts.Close()

	cfg := &config.Config{Endpoints: []string{ts.URL}}
	summary, err := FetchAccountQuotaSummary(t.Context(), cfg, ts.Client(), "token", "project", config.TierPersonal)
	if err != nil {
		t.Fatalf("FetchAccountQuotaSummary failed: %v", err)
	}
	if summary == nil || summary.Weekly == nil || summary.FiveHour == nil {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if *summary.Weekly.RemainingFraction != 0.35 || *summary.FiveHour.RemainingFraction != 0.8 {
		t.Errorf("unexpected fractions: %+v", summary)
	}
}

// mockIteratorRunner implements both AccountRunner and AccountIterator for multi-account refresh tests.
type mockIteratorRunner struct {
	// states holds per-account runtime states passed to the handler.
	states []*rotator.AccountState
}

// WithAccount executes the handler once against the first configured account.
func (m *mockIteratorRunner) WithAccount(ctx context.Context, handler func(ctx context.Context, token string, state *rotator.AccountState) error) error {
	if len(m.states) == 0 {
		return nil
	}
	return handler(ctx, "mock-token", m.states[0])
}

// ForEachAccount executes the handler against every configured account.
func (m *mockIteratorRunner) ForEachAccount(ctx context.Context, handler func(ctx context.Context, token string, state *rotator.AccountState) error) error {
	for _, state := range m.states {
		if err := handler(ctx, "mock-token", state); err != nil {
			return err
		}
	}
	return nil
}

// TestModelCatalogRefreshAggregatesAccounts verifies that a refresh updates quota state for every
// account, merges model lists across accounts, and keeps the most permissive per-model quota.
func TestModelCatalogRefreshAggregatesAccounts(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			var body struct {
				Project string `json:"project"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Project == "free-project" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"models": map[string]any{
						"gemini-4-neon": map[string]any{
							"maxTokens": 1048576,
							"quotaInfo": map[string]any{"remainingFraction": 0.2},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": map[string]any{
					"claude-sonnet-5-5-high": map[string]any{
						"maxTokens": 1000000,
						"quotaInfo": map[string]any{"remainingFraction": 0.9},
					},
				},
			})
		case "/v1internal:retrieveUserQuotaSummary":
			var body struct {
				Project string `json:"project"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fiveHour := 0.8
			weekly := 0.6
			if body.Project == "paid-project" {
				fiveHour = 0.5
				weekly = 0.9
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"fiveHour":        map[string]any{"remainingFraction": fiveHour},
				"weeklyQuotaInfo": map[string]any{"remainingFraction": weekly},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	freeState := rotator.NewAccountState(&auth.Account{Email: "free@example.com", ProjectID: "free-project", Tier: config.TierPersonal})
	paidState := rotator.NewAccountState(&auth.Account{Email: "paid@example.com", ProjectID: "paid-project", Tier: config.TierEnterprise})

	runner := &mockIteratorRunner{states: []*rotator.AccountState{freeState, paidState}}
	cfg := &config.Config{Endpoints: []string{ts.URL}}
	cat := NewModelCatalog(cfg, runner, filepath.Join(t.TempDir(), "models-cache.json"), nil)
	cat.SetHTTPClient(ts.Client())

	if err := cat.Refresh(t.Context()); err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}

	snap, _ := cat.Snapshot()
	if len(snap.Models) != 2 {
		t.Fatalf("expected merged models from both accounts, got %+v", snap.Models)
	}
	if got := snap.Quotas["gemini-4-neon"].RemainingFraction; got == nil || *got != 0.2 {
		t.Errorf("unexpected gemini quota: %+v", snap.Quotas["gemini-4-neon"])
	}
	if got := snap.Quotas["claude-sonnet-5-5-high"].RemainingFraction; got == nil || *got != 0.9 {
		t.Errorf("unexpected claude quota: %+v", snap.Quotas["claude-sonnet-5-5-high"])
	}

	if snap.AccountQuota == nil || snap.AccountQuota.FiveHour == nil || snap.AccountQuota.Weekly == nil {
		t.Fatalf("expected merged account quota summary, got %+v", snap.AccountQuota)
	}
	if *snap.AccountQuota.FiveHour.RemainingFraction != 0.5 {
		t.Errorf("five-hour quota = %v; want most constrained 0.5", *snap.AccountQuota.FiveHour.RemainingFraction)
	}
	if *snap.AccountQuota.Weekly.RemainingFraction != 0.6 {
		t.Errorf("weekly quota = %v; want most constrained 0.6", *snap.AccountQuota.Weekly.RemainingFraction)
	}

	if got := freeState.QuotaFraction("*"); got != 0.8 {
		t.Errorf("free account quota fraction = %v; want 0.8", got)
	}
	if got := paidState.QuotaFraction("*"); got != 0.5 {
		t.Errorf("paid account quota fraction = %v; want 0.5", got)
	}
}
