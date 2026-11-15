// Package auth handles Google OAuth 2.0 PKCE authentication, token management,
// companion project discovery, and atomic credential storage.
package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestExtractProjectID_TrackedFixtures verifies companion project discovery across sanitized
// tracked upstream response fixtures without external network dependencies.
func TestExtractProjectID_TrackedFixtures(t *testing.T) {
	// 1. Success fixture with direct cloudaicompanionProject string
	dataSuccess, err := os.ReadFile(filepath.Join("testdata", "load_code_assist_success.json"))
	if err != nil {
		t.Fatalf("reading load_code_assist_success.json fixture: %v", err)
	}

	var parsedSuccess map[string]any
	if err := json.Unmarshal(dataSuccess, &parsedSuccess); err != nil {
		t.Fatalf("unmarshaling success fixture: %v", err)
	}

	projID := ExtractProjectID(parsedSuccess)
	expectedProj := "projects/cloud-ai-companion-sample-01"
	if projID != expectedProj {
		t.Errorf("ExtractProjectID() = %q; want %q", projID, expectedProj)
	}

	tier := DefaultTierID(parsedSuccess)
	expectedTier := "tier-free"
	if tier != expectedTier {
		t.Errorf("DefaultTierID() = %q; want %q", tier, expectedTier)
	}

	// 2. Onboard user done fixture with nested cloudaicompanionProject object
	dataOnboardDone, err := os.ReadFile(filepath.Join("testdata", "onboard_user_done.json"))
	if err != nil {
		t.Fatalf("reading onboard_user_done.json fixture: %v", err)
	}

	var parsedOnboardDone map[string]any
	if err := json.Unmarshal(dataOnboardDone, &parsedOnboardDone); err != nil {
		t.Fatalf("unmarshaling onboard done fixture: %v", err)
	}

	respMap, ok := parsedOnboardDone["response"].(map[string]any)
	if !ok {
		t.Fatalf("expected response field in onboard done fixture")
	}
	onboardProj := ExtractProjectID(respMap)
	expectedOnboardProj := "projects/cloud-ai-companion-sample-02"
	if onboardProj != expectedOnboardProj {
		t.Errorf("ExtractProjectID() for onboard = %q; want %q", onboardProj, expectedOnboardProj)
	}

	// 3. Onboard pending fixture
	dataOnboardPending, err := os.ReadFile(filepath.Join("testdata", "onboard_user_pending.json"))
	if err != nil {
		t.Fatalf("reading onboard_user_pending.json fixture: %v", err)
	}

	var parsedOnboardPending map[string]any
	if err := json.Unmarshal(dataOnboardPending, &parsedOnboardPending); err != nil {
		t.Fatalf("unmarshaling onboard pending fixture: %v", err)
	}
	if done, ok := parsedOnboardPending["done"].(bool); !ok || done {
		t.Errorf("expected done: false in pending fixture")
	}

	// 4. Malformed fixture
	dataMalformed, err := os.ReadFile(filepath.Join("testdata", "load_code_assist_malformed.json"))
	if err != nil {
		t.Fatalf("reading load_code_assist_malformed.json fixture: %v", err)
	}

	var parsedMalformed map[string]any
	if err := json.Unmarshal(dataMalformed, &parsedMalformed); err != nil {
		t.Fatalf("unmarshaling malformed fixture: %v", err)
	}
	if proj := ExtractProjectID(parsedMalformed); proj != "" {
		t.Errorf("expected empty project from malformed fixture, got %q", proj)
	}
}

// TestDiscoverProject_ResilienceFixtures verifies DiscoverProject error handling and fallback
// behavior against upstream errors (HTTP 403, malformed payloads) using tracked fixtures.
func TestDiscoverProject_ResilienceFixtures(t *testing.T) {
	permDeniedData, err := os.ReadFile(filepath.Join("testdata", "load_code_assist_permission_denied.json"))
	if err != nil {
		t.Fatalf("reading permission denied fixture: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(permDeniedData)
	}))
	defer server.Close()

	cfg := &config.Config{
		Endpoints: []string{server.URL},
	}

	_, err = DiscoverProject(t.Context(), cfg, "test-access-token")
	if err == nil {
		t.Fatalf("expected error from DiscoverProject on 403, got nil")
	}
}
