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
}
