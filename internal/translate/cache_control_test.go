// Package translate provides bidirectional translation between OpenAI/Anthropic
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"reflect"
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
