// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestExtractProjectID tests parsing companion project identifiers from varied Google Cloud Code JSON shapes.
func TestExtractProjectID(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]any
		expected string
	}{
		{
			name: "cloudaicompanionProject string",
			input: map[string]any{
				"cloudaicompanionProject": "companion-project-123",
			},
			expected: "companion-project-123",
		},
		{
			name: "cloudaicompanionProject nested object",
			input: map[string]any{
				"cloudaicompanionProject": map[string]any{
					"id": "companion-project-nested",
				},
			},
			expected: "companion-project-nested",
		},
		{
			name: "projectId fallback",
			input: map[string]any{
				"projectId": "project-id-fallback",
			},
			expected: "project-id-fallback",
		},
		{
			name: "project object fallback",
			input: map[string]any{
				"project": map[string]any{
					"id": "project-object-id",
				},
			},
			expected: "project-object-id",
		},
		{
			name:     "empty map",
			input:    map[string]any{},
			expected: "",
		},
		{
			name:     "nil map",
			input:    nil,
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractProjectID(tc.input)
			if got != tc.expected {
				t.Errorf("ExtractProjectID() = %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestDefaultTierID tests extraction of the default or current subscription tier from loadCodeAssist responses.
func TestDefaultTierID(t *testing.T) {
	data := map[string]any{
		"allowedTiers": []any{
			map[string]any{"id": "tier-standard", "isDefault": false},
			map[string]any{"id": "tier-pro", "isDefault": true},
		},
		"currentTier": map[string]any{
			"id": "tier-fallback",
		},
	}
	tier := DefaultTierID(data)
	if tier != "tier-pro" {
		t.Errorf("expected default tier tier-pro, got %s", tier)
	}

	fallbackData := map[string]any{
		"currentTier": map[string]any{
			"id": "current-only",
		},
	}
	if tier = DefaultTierID(fallbackData); tier != "current-only" {
		t.Errorf("expected current-only, got %s", tier)
	}

	if tier = DefaultTierID(nil); tier != "free-tier" {
		t.Errorf("expected free-tier on nil, got %s", tier)
	}
}

// TestGetUserEmail verifies retrieving user email addresses from Google's userinfo endpoint.
func TestGetUserEmail(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" {
			t.Errorf("expected Bearer test-access-token, got %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"email": "developer@example.com",
		})
	}))
	defer ts.Close()

	cfg := config.DefaultConfig()
	cfg.UserInfoURL = ts.URL

	ctx := t.Context()
	email, err := GetUserEmail(ctx, cfg, "test-access-token")
	if err != nil {
		t.Fatalf("GetUserEmail failed: %v", err)
	}

	if email != "developer@example.com" {
		t.Errorf("email mismatch: got %s, want developer@example.com", email)
	}
}

// TestGetUserIdentity verifies that the immutable Google subject identifier and email are resolved
// from a single userinfo round trip.
func TestGetUserIdentity(t *testing.T) {
	requests := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer identity-token" {
			t.Errorf("expected Bearer identity-token, got %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub":   "109876543210987654321",
			"email": "identity@example.com",
		})
	}))
	defer ts.Close()

	cfg := config.DefaultConfig()
	cfg.UserInfoURL = ts.URL

	identity, err := GetUserIdentity(t.Context(), cfg, "identity-token")
	if err != nil {
		t.Fatalf("GetUserIdentity failed: %v", err)
	}
	if identity.ID != "109876543210987654321" {
		t.Errorf("expected immutable subject id, got %q", identity.ID)
	}
	if identity.Email != "identity@example.com" {
		t.Errorf("expected email identity@example.com, got %q", identity.Email)
	}
	if requests != 1 {
		t.Errorf("expected a single userinfo round trip, got %d", requests)
	}
}

// TestDiscoverProjectLoadSuccess verifies discovering an existing companion project via loadCodeAssist.
func TestDiscoverProjectLoadSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:loadCodeAssist" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"cloudaicompanionProject": "discovered-project-abc",
		})
	}))
	defer ts.Close()

	cfg := config.DefaultConfig()
	cfg.Endpoints = []string{ts.URL}

	ctx := t.Context()
	projectID, err := DiscoverProject(ctx, cfg, "token-123")
	if err != nil {
		t.Fatalf("DiscoverProject failed: %v", err)
	}
	if projectID != "discovered-project-abc" {
		t.Errorf("project ID mismatch: got %s", projectID)
	}
}

// TestDiscoverProjectOnboardSuccess verifies onboarding a new account when no companion project initially exists.
func TestDiscoverProjectOnboardSuccess(t *testing.T) {
	var onboardAttempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			// No companion project returned yet
			_ = json.NewEncoder(w).Encode(map[string]any{
				"allowedTiers": []any{
					map[string]any{"id": "tier-plus", "isDefault": true},
				},
			})
			return
		}

		if r.URL.Path == "/v1internal:onboardUser" {
			att := onboardAttempts.Add(1)
			if att < 2 {
				// Still running
				_ = json.NewEncoder(w).Encode(map[string]any{
					"done": false,
				})
				return
			}
			// Done on second attempt
			_ = json.NewEncoder(w).Encode(map[string]any{
				"done": true,
				"response": map[string]any{
					"cloudaicompanionProject": "onboarded-project-xyz",
				},
			})
			return
		}

		http.NotFound(w, r)
	}))
	defer ts.Close()

	cfg := config.DefaultConfig()
	cfg.Endpoints = []string{ts.URL}

	ctx := t.Context()
	projectID, err := DiscoverProject(ctx, cfg, "token-123")
	if err != nil {
		t.Fatalf("DiscoverProject failed: %v", err)
	}
	if projectID != "onboarded-project-xyz" {
		t.Errorf("project ID mismatch: got %s", projectID)
	}
}

// TestDiscoverProjectNoProjectError verifies error handling when upstream fails to provision a project.
func TestDiscoverProjectNoProjectError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "not permitted",
		})
	}))
	defer ts.Close()

	cfg := config.DefaultConfig()
	cfg.Endpoints = []string{ts.URL}

	ctx := t.Context()
	_, err := DiscoverProject(ctx, cfg, "token-123")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNoCompanionProject) {
		t.Errorf("expected ErrNoCompanionProject, got %v", err)
	}
}

// TestTierFromLoadCodeAssist verifies normalization of subscription tier metadata reported by
// loadCodeAssist across consumer, business, and enterprise payload shapes.
func TestTierFromLoadCodeAssist(t *testing.T) {
	tests := []struct {
		name string
		data map[string]any
		want string
	}{
		{
			name: "current tier enterprise",
			data: map[string]any{
				"currentTier": map[string]any{"id": "tier-enterprise-standard", "name": "Enterprise"},
			},
			want: "enterprise",
		},
		{
			name: "paid tier workspace business",
			data: map[string]any{
				"paidTier": map[string]any{"id": "workspace-business", "displayName": "Google Workspace"},
			},
			want: "business",
		},
		{
			name: "allowed tiers default consumer",
			data: map[string]any{
				"allowedTiers": []any{
					map[string]any{"id": "tier-premium"},
				},
			},
			want: "personal",
		},
		{
			name: "paid tier wins over consumer current tier",
			data: map[string]any{
				"currentTier": map[string]any{"id": "tier-free"},
				"paidTier":    map[string]any{"id": "tier-enterprise-standard"},
			},
			want: "enterprise",
		},
		{
			name: "unknown tier",
			data: map[string]any{
				"currentTier": map[string]any{"id": "tier-experimental"},
			},
			want: "",
		},
		{
			name: "nil payload",
			data: nil,
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := TierFromLoadCodeAssist(tc.data); got != tc.want {
				t.Errorf("TierFromLoadCodeAssist() = %q; want %q", got, tc.want)
			}
		})
	}
}
