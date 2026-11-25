// Package translate provides bidirectional translation between OpenAI/Anthropic
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"reflect"
	"strings"
	"testing"
)

// TestCleanCacheControl verifies that cache_control keys are removed recursively
// from maps and slices without mutating other nested keys.
func TestCleanCacheControl(t *testing.T) {
	input := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"type": "text",
				"text": "Hello world",
				"cache_control": map[string]any{
					"type": "ephemeral",
				},
			},
			map[string]any{
				"type": "tool_use",
				"id":   "tool_123",
				"cache_control": map[string]any{
					"type": "ephemeral",
				},
				"input": map[string]any{
					"foo": "bar",
				},
			},
		},
		"cache_control": map[string]any{
			"type": "ephemeral",
		},
	}

	expected := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"type": "text",
				"text": "Hello world",
			},
			map[string]any{
				"type": "tool_use",
				"id":   "tool_123",
				"input": map[string]any{
					"foo": "bar",
				},
			},
		},
	}

	cleaned := CleanCacheControl(input)
	if !reflect.DeepEqual(cleaned, expected) {
		t.Fatalf("CleanCacheControl mismatch:\ngot:  %+v\nwant: %+v", cleaned, expected)
	}
}

// TestStripBillingHeader verifies that Claude Code billing pseudo-headers are removed
// while preserving surrounding instructions.
func TestStripBillingHeader(t *testing.T) {
	input := "You are a helpful coding assistant.\nx-anthropic-billing-header: cc_version=2.1.274; cc_entrypoint=sdk-cli;\nPlease write tests."
	expected := "You are a helpful coding assistant.\nPlease write tests."

	result := StripBillingHeader(input)
	if result != expected {
		t.Fatalf("StripBillingHeader mismatch:\ngot:  %q\nwant: %q", result, expected)
	}
}

// TestCleanCacheControlMessagesAndShallowCopy verifies batch message cleaning and map copying.
func TestCleanCacheControlMessagesAndShallowCopy(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role":          "user",
			"cache_control": map[string]any{"type": "ephemeral"},
			"content":       "Hello",
		},
	}

	cleanedMsgs := CleanCacheControlMessages(msgs)
	if len(cleanedMsgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(cleanedMsgs))
	}
	m, ok := cleanedMsgs[0].(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T", cleanedMsgs[0])
	}
	if _, hasCache := m["cache_control"]; hasCache {
		t.Error("expected cache_control stripped in CleanCacheControlMessages")
	}

	singleMap := map[string]any{
		"keep":          "val",
		"cache_control": "drop",
	}
	shallow := ShallowCopyWithoutCacheControl(singleMap)
	if _, hasCache := shallow["cache_control"]; hasCache {
		t.Error("expected cache_control dropped in ShallowCopyWithoutCacheControl")
	}
	if shallow["keep"] != "val" {
		t.Errorf("expected keep=val, got %v", shallow["keep"])
	}

	if CleanCacheControlMessages(nil) != nil {
		t.Error("expected nil for nil input in CleanCacheControlMessages")
	}
	if ShallowCopyWithoutCacheControl(nil) != nil {
		t.Error("expected nil for nil input in ShallowCopyWithoutCacheControl")
	}
}

// TestCleanCacheControlFastPathNoAlloc verifies that CleanCacheControl returns the original map
// reference without allocating new maps when no cache_control keys exist anywhere in the payload.
func TestCleanCacheControlFastPathNoAlloc(t *testing.T) {
	input := map[string]any{
		"role":    "user",
		"content": "Hello standard message with no cache control",
		"nested": map[string]any{
			"key": "value",
		},
	}

	cleaned := CleanCacheControl(input)
	if !reflect.DeepEqual(cleaned, input) {
		t.Fatalf("CleanCacheControl modified clean payload: got %+v", cleaned)
	}

	cleanedMap := cleaned.(map[string]any)
	if reflect.ValueOf(cleanedMap).Pointer() != reflect.ValueOf(input).Pointer() {
		t.Error("expected CleanCacheControl to return original map reference on fast-path")
	}
}

// TestScrubAgentMarkers verifies billing pseudo-headers are dropped and third-party agent
// identity lines are replaced with a neutral identity without touching ordinary prose.
func TestScrubAgentMarkers(t *testing.T) {
	input := strings.Join([]string{
		"x-anthropic-billing-header: cc_version=2.1.274; cc_entrypoint=cli",
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"  You are Hermes Agent, built by Nous Research.",
		"You are OpenClaw and answer concisely.",
		"Prose that mentions Claude Code but is not an identity line.",
	}, "\n")

	got := ScrubAgentMarkers(input)

	for _, leaked := range []string{
		"x-anthropic-billing-header",
		"You are Claude Code",
		"Anthropic's official CLI",
		"You are Hermes Agent",
		"You are OpenClaw",
	} {
		if strings.Contains(got, leaked) {
			t.Errorf("expected %q scrubbed, got %q", leaked, got)
		}
	}

	if count := strings.Count(got, neutralAgentIdentityLine); count != 3 {
		t.Errorf("expected 3 neutral identity lines, got %d in %q", count, got)
	}
	if !strings.Contains(got, "Prose that mentions Claude Code but is not an identity line.") {
		t.Errorf("expected ordinary prose preserved, got %q", got)
	}
}

// TestScrubAgentMarkersEmptyInput verifies the empty-text fast path.
func TestScrubAgentMarkersEmptyInput(t *testing.T) {
	if got := ScrubAgentMarkers(""); got != "" {
		t.Errorf("ScrubAgentMarkers(\"\") = %q; want empty string", got)
	}
}
