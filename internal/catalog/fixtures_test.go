// Package catalog queries Google Cloud Code Assist to discover available models,
// extracts token limits, maps aliases, and caches catalog metadata.
package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseCatalog_TrackedFixtures verifies model parsing, non-chat filtering, and quota tracking
// using tracked upstream catalog response fixtures.
func TestParseCatalog_TrackedFixtures(t *testing.T) {
	// 1. Catalog schema variation fixture
	dataVar, err := os.ReadFile(filepath.Join("testdata", "catalog_schema_variation.json"))
	if err != nil {
		t.Fatalf("reading catalog_schema_variation.json: %v", err)
	}

	cat, err := ParseCatalog(dataVar)
	if err != nil {
		t.Fatalf("ParseCatalog() failed: %v", err)
	}

	if len(cat.Models) != 1 {
		t.Fatalf("expected 1 chat model after filtering, got %d", len(cat.Models))
	}

	m := cat.Models[0]
	if m.ID != "gemini-3.8-flash-high" {
		t.Errorf("model ID = %q; want %q", m.ID, "gemini-3.8-flash-high")
	}
	if m.DisplayName != "Gemini 3.8 Flash (High)" {
		t.Errorf("displayName = %q; want %q", m.DisplayName, "Gemini 3.8 Flash (High)")
	}

	quota, hasQuota := cat.Quotas["gemini-3.8-flash-high"]
	if !hasQuota {
		t.Fatalf("expected quota info for gemini-3.8-flash-high")
	}
	if quota.RemainingFraction == nil || *quota.RemainingFraction != 0.85 {
		t.Errorf("remainingFraction = %v; want 0.85", quota.RemainingFraction)
	}

	// 2. Malformed catalog fixture
	dataMalformed, err := os.ReadFile(filepath.Join("testdata", "catalog_malformed.json"))
	if err != nil {
		t.Fatalf("reading catalog_malformed.json: %v", err)
	}

	catMalformed, err := ParseCatalog(dataMalformed)
	if err != nil {
		t.Fatalf("ParseCatalog() on malformed schema should not error: %v", err)
	}
	if len(catMalformed.Models) != 0 {
		t.Errorf("expected 0 models from malformed catalog, got %d", len(catMalformed.Models))
	}
}

// TestSanitizedCatalogFixturesZeroSecrets verifies that all catalog test fixtures are sanitized of secrets.
func TestSanitizedCatalogFixturesZeroSecrets(t *testing.T) {
	fixtureFiles, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatalf("globbing testdata fixtures: %v", err)
	}

	forbiddenSubstrings := []string{
		"ya29.",
		"refresh_token",
		"client_secret",
		"Bearer ",
		"AIzaSy",
	}

	for _, file := range fixtureFiles {
		content, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading fixture %s: %v", file, err)
		}

		var parsed any
		if err := json.Unmarshal(content, &parsed); err != nil {
			t.Errorf("fixture %s is not valid JSON: %v", file, err)
		}

		strContent := string(content)
		for _, forbidden := range forbiddenSubstrings {
			if strings.Contains(strContent, forbidden) {
				t.Errorf("fixture %s contains secret %q", file, forbidden)
			}
		}
	}
}
