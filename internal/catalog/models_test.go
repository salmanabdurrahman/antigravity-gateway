// Package catalog discovers upstream model availability, limits, and live quota tracking.
package catalog

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUpstreamModel verifies mapping public display aliases to internal Google Cloud Code model identifiers.
func TestUpstreamModel(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"gemini-3.1-pro-high", "gemini-pro-agent"},
		{"GEMINI-3.1-PRO-HIGH", "gemini-pro-agent"},
		{"gpt-oss-120b", "gpt-oss-120b-medium"},
		{"gemini-3.8-flash-high", "gemini-3.8-flash-high"},
		{"claude-sonnet-4-6", "claude-sonnet-4-6"},
	}

	for _, tc := range tests {
		actual := UpstreamModel(tc.input)
		if actual != tc.expected {
			t.Errorf("UpstreamModel(%q) = %q, expected %q", tc.input, actual, tc.expected)
		}
	}
}

// TestResolveModelAlias verifies user custom mappings and standard upstream alias resolution.
func TestResolveModelAlias(t *testing.T) {
	customMap := map[string]string{
		"gemini-4-argon": "gemini-4-argon-high",
		"claude-custom":  "claude-sonnet-4-6",
	}

	if resolved := ResolveModelAlias("gemini-4-argon", customMap); resolved != "gemini-4-argon-high" {
		t.Errorf("expected gemini-4-argon-high, got %s", resolved)
	}
	if resolved := ResolveModelAlias("GEMINI-4-ARGON", customMap); resolved != "gemini-4-argon-high" {
		t.Errorf("expected case-insensitive mapping gemini-4-argon-high, got %s", resolved)
	}
	// Fallback to standard aliases when not in custom map
	if resolved := ResolveModelAlias("gemini-3.1-pro-high", customMap); resolved != "gemini-pro-agent" {
		t.Errorf("expected gemini-pro-agent from standard alias, got %s", resolved)
	}
}

// TestIsListedChatModel verifies that non-chat, internal IDE, and embedding models are filtered out.
func TestIsListedChatModel(t *testing.T) {
	valid := []string{
		"gemini-3-flash",
		"gemini-3.8-flash-high",
		"gemini-3.5-flash-lite",
		"claude-opus-5-5-high",
		"claude-sonnet-4-6",
		"gpt-oss-120b-medium",
	}

	for _, id := range valid {
		if !IsListedChatModel(id) {
			t.Errorf("expected %q to be a valid chat model", id)
		}
	}

	invalid := []string{
		"gemini",
		"claude",
		"chat_20706",
		"tab_flash_lite_preview",
		"gemini-3.5-flash",
		"gemini-3.1-flash-image",
		"imagen-3",
		"whisper-1",
		"text-embedding-004",
		"speech-tts-1",
		"invalid/../../escape",
		"",
	}

	for _, id := range invalid {
		if IsListedChatModel(id) {
			t.Errorf("expected %q to be excluded as a chat model", id)
		}
	}
}

// TestFamilyDefaults verifies baseline token limits, thinking support, and multimodal capabilities per model family.
func TestFamilyDefaults(t *testing.T) {
	gpt := FamilyDefaults("gpt-oss-120b")
	if gpt.ContextWindow != 131072 || gpt.MaxOutputTokens != 32768 || !gpt.Thinking || gpt.Images {
		t.Errorf("unexpected gpt-oss defaults: %+v", gpt)
	}

	claude := FamilyDefaults("claude-sonnet-4-6")
	if claude.ContextWindow != 1000000 || claude.MaxOutputTokens != 64000 || !claude.Thinking || !claude.Images {
		t.Errorf("unexpected claude defaults: %+v", claude)
	}

	gemini := FamilyDefaults("gemini-3.8-flash-high")
	if gemini.ContextWindow != 1048576 || gemini.MaxOutputTokens != 65536 || !gemini.Thinking || !gemini.Images {
		t.Errorf("unexpected gemini defaults: %+v", gemini)
	}

	other := FamilyDefaults("custom-model-id")
	if other.ContextWindow != 128000 || other.Thinking || other.Images {
		t.Errorf("unexpected default fallback: %+v", other)
	}
}

// TestParseCatalogWithFixture verifies parsing raw catalog responses against realistic upstream payload fixtures.
func TestParseCatalogWithFixture(t *testing.T) {
	fixturePath := filepath.Join("testdata", "catalog.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	snap, err := ParseCatalog(data)
	if err != nil {
		t.Fatalf("ParseCatalog failed: %v", err)
	}

	if len(snap.Models) == 0 {
		t.Fatal("expected parsed models to not be empty")
	}

	// Verify exclusions
	for _, m := range snap.Models {
		if m.ID == "chat_20706" || m.ID == "tab_flash_lite_preview" || m.ID == "gemini-3.5-flash" || m.ID == "gemini-3.1-flash-image" {
			t.Errorf("expected internal model %q to be filtered out", m.ID)
		}
	}

	// Verify inclusion of flash-lite
	foundFlashLite := false
	for _, m := range snap.Models {
		if m.ID == "gemini-3.5-flash-lite" {
			foundFlashLite = true
			break
		}
	}
	if !foundFlashLite {
		t.Errorf("expected gemini-3.5-flash-lite to be present in catalog")
	}

	// Verify quotas map
	if q, ok := snap.Quotas["claude-sonnet-4-6"]; ok {
		if q.RemainingFraction == nil || *q.RemainingFraction != 0.4 {
			t.Errorf("expected claude-sonnet-4-6 remainingFraction=0.4, got %+v", q)
		}
	} else {
		t.Errorf("expected claude-sonnet-4-6 in quotas")
	}
}

// TestSummarizeQuotasAndFormatting verifies aggregating quota percentages and formatting reset times.
func TestSummarizeQuotasAndFormatting(t *testing.T) {
	frac1 := 0.85
	frac2 := 1.0
	quotas := map[string]ModelQuota{
		"gemini-3.8-flash-high": {
			RemainingFraction: &frac1,
			ResetTime:         "2026-11-10T12:00:00Z",
		},
		"claude-sonnet-4-6": {
			RemainingFraction: &frac2,
			ResetTime:         "",
		},
	}

	summary := SummarizeQuotas(quotas)
	if summary.GeminiRemainingFraction == nil || *summary.GeminiRemainingFraction != 0.85 {
		t.Errorf("expected Gemini quota 0.85, got %v", summary.GeminiRemainingFraction)
	}
	if summary.ClaudeRemainingFraction == nil || *summary.ClaudeRemainingFraction != 1.0 {
		t.Errorf("expected Claude quota 1.0, got %v", summary.ClaudeRemainingFraction)
	}

	geminiStr := FormatQuota(summary.GeminiRemainingFraction)
	if geminiStr != "85%" {
		t.Errorf("expected '85%%', got %q", geminiStr)
	}

	claudeStr := FormatQuota(summary.ClaudeRemainingFraction)
	if claudeStr != "100%" {
		t.Errorf("expected '100%%', got %q", claudeStr)
	}

	// Reset time in future
	now := time.Date(2026, 11, 10, 11, 0, 0, 0, time.UTC)
	resetStr := FormatResetTime("2026-11-10T12:30:00Z", now)
	if resetStr != "12:30 UTC" {
		t.Errorf("expected '12:30 UTC', got %q", resetStr)
	}

	// Reset time in past
	pastStr := FormatResetTime("2026-11-10T10:00:00Z", now)
	if pastStr != "Ready" {
		t.Errorf("expected 'Ready' for past reset time, got %q", pastStr)
	}

	// Empty reset time
	emptyStr := FormatResetTime("", now)
	if emptyStr != "Ready" {
		t.Errorf("expected 'Ready' for empty reset time, got %q", emptyStr)
	}

	// Countdown formatting across <1m, <24h, and >=24h horizons
	if got := FormatResetCountdown("2026-11-10T11:00:25Z", now); got != "<1m (11:00 UTC)" {
		t.Errorf("sub-minute countdown = %q, want %q", got, "<1m (11:00 UTC)")
	}
	if got := FormatResetCountdown("2026-11-10T12:30:00Z", now); got != "in 1h 30m (12:30 UTC)" {
		t.Errorf("hours countdown = %q, want %q", got, "in 1h 30m (12:30 UTC)")
	}
	if got := FormatResetCountdown("2026-11-13T14:00:00Z", now); got != "in 3d 3h (Nov 13 14:00 UTC)" {
		t.Errorf("multi-day countdown = %q, want %q", got, "in 3d 3h (Nov 13 14:00 UTC)")
	}
	if got := FormatResetCountdown("2026-11-10T10:00:00Z", now); got != "Ready" {
		t.Errorf("past countdown = %q, want 'Ready'", got)
	}
}

// TestIsListedChatModelExcludesDisabledPlaceholders verifies retired `<model>-disabled` identifiers
// are never advertised as usable chat models.
func TestIsListedChatModelExcludesDisabledPlaceholders(t *testing.T) {
	for _, id := range []string{"gemini-4-argon-disabled", "claude-sonnet-4-6-DISABLED"} {
		if IsListedChatModel(id) {
			t.Errorf("expected disabled placeholder %q to be filtered", id)
		}
	}
}

// TestFamilyDefaultsGemini4Neon verifies the documented 512K context window for Gemini 4 Neon while
// other Gemini families keep their 1M default.
func TestFamilyDefaultsGemini4Neon(t *testing.T) {
	neon := FamilyDefaults("gemini-4-neon")
	if neon.ContextWindow != 524288 {
		t.Errorf("gemini-4-neon context window = %d; want 524288", neon.ContextWindow)
	}
	if neon.MaxOutputTokens != 65536 || !neon.Thinking || !neon.Images {
		t.Errorf("unexpected gemini-4-neon defaults: %+v", neon)
	}

	carbon := FamilyDefaults("gemini-4-carbon")
	if carbon.ContextWindow != 1048576 {
		t.Errorf("gemini-4-carbon context window = %d; want 1048576", carbon.ContextWindow)
	}
}

// TestParseCatalogUsesApiMetadata verifies that disabled placeholders and non-chat providers are
// filtered while agentModelSorts / tieredModelIds override name heuristics and drive the default model.
func TestParseCatalogUsesApiMetadata(t *testing.T) {
	payload := []byte(`{
		"models": {
			"gemini-4-argon-disabled": {"maxTokens": 1048576},
			"chat_123": {"maxTokens": 4096},
			"gemini-4-neon": {"maxTokens": 1048576, "supportsThinking": true},
			"gemini-3.5-flash": {"maxTokens": 1048576},
			"custom-serving-model": {"maxTokens": 8192, "modelProvider": "embedding"},
			"claude-sonnet-5-5-high": {
				"maxTokens": 1000000,
				"quotaInfo": {
					"remainingFraction": 0.9,
					"resetTime": "2026-12-01T00:00:00Z",
					"weeklyQuotaInfo": {"remainingFraction": 0.4, "resetTime": "2026-12-02T00:00:00Z"}
				}
			}
		},
		"agentModelSorts": [{"modelIds": ["gemini-3.5-flash", "chat_123"]}],
		"tieredModelIds": ["gemini-4-neon"],
		"defaultAgentModelId": "gemini-4-argon-disabled"
	}`)

	snap, err := ParseCatalog(payload)
	if err != nil {
		t.Fatalf("ParseCatalog failed: %v", err)
	}

	byID := make(map[string]CatalogModel, len(snap.Models))
	for _, model := range snap.Models {
		byID[model.ID] = model
	}

	if _, ok := byID["gemini-4-argon-disabled"]; ok {
		t.Error("disabled placeholder must be filtered")
	}
	if _, ok := byID["custom-serving-model"]; ok {
		t.Error("non-chat provider model must be filtered")
	}
	if _, ok := byID["chat_123"]; ok {
		t.Error("advertised internal chat tool must stay filtered")
	}
	if _, ok := byID["gemini-3.5-flash"]; !ok {
		t.Error("agentModelSorts must override the Gemini 3.5 name heuristic")
	}
	neon, ok := byID["gemini-4-neon"]
	if !ok {
		t.Fatal("expected gemini-4-neon in catalog")
	}
	if neon.ContextWindow != 524288 {
		t.Errorf("gemini-4-neon context = %d; want 524288", neon.ContextWindow)
	}
	if snap.DefaultModel != "gemini-3.5-flash" {
		t.Errorf("default model = %q; want gemini-3.5-flash from agentModelSorts", snap.DefaultModel)
	}

	quota, ok := snap.Quotas["claude-sonnet-5-5-high"]
	if !ok || quota.Weekly == nil || quota.Weekly.RemainingFraction == nil {
		t.Fatalf("expected weekly quota window, got %+v", quota)
	}
	if *quota.Weekly.RemainingFraction != 0.4 {
		t.Errorf("weekly fraction = %v; want 0.4", *quota.Weekly.RemainingFraction)
	}
}

// TestFallbackModelsBundledManifest verifies the embedded offline manifest ships Gemini 4 and
// Claude 5.5 entries with correct Neon limits.
func TestFallbackModelsBundledManifest(t *testing.T) {
	models := FallbackModels()
	if len(models) < 4 {
		t.Fatalf("expected bundled fallback manifest, got %d models", len(models))
	}

	byID := make(map[string]CatalogModel, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}

	neon, ok := byID["gemini-4-neon"]
	if !ok || neon.ContextWindow != 524288 {
		t.Errorf("fallback manifest missing gemini-4-neon 512K entry: %+v", neon)
	}
	for _, id := range []string{"gemini-4-carbon", "gemini-4-hydrogen-sonar", "claude-sonnet-5-5-high", "claude-opus-5-5-high"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("fallback manifest missing %q", id)
		}
	}

	// Callers must never mutate the shared memoized manifest.
	models[0].ID = "mutated"
	if FallbackModels()[0].ID == "mutated" {
		t.Error("FallbackModels must return a defensive copy")
	}
}
