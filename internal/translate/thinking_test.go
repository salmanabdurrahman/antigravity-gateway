// Package translate provides bidirectional translation between OpenAI/Anthropic
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"testing"
)

// TestClampThinkingBudget verifies budget token clamping per model family and max tokens adjustments.
func TestClampThinkingBudget(t *testing.T) {
	// Gemini 2.5 Flash with an explicit client ceiling: the budget shrinks instead of the ceiling growing.
	budget, maxOut := ClampThinkingBudget("gemini-2.5-flash", 50000, 4096, 65536, false, true)
	if maxOut != 4096 {
		t.Errorf("expected the explicit maxOut 4096 preserved, got %d", maxOut)
	}
	if budget != 2048 {
		t.Errorf("expected budget halved to 2048 under the cap, got %d", budget)
	}

	// Claude model with an explicit client ceiling: the default budget adapts to it.
	claudeBudget, claudeMaxOut := ClampThinkingBudget("claude-sonnet-4-6", 0, 2048, 64000, true, true)
	if claudeMaxOut != 2048 {
		t.Errorf("expected the explicit maxOut 2048 preserved, got %d", claudeMaxOut)
	}
	if claudeBudget != 1024 {
		t.Errorf("expected Claude budget halved to 1024 under the cap, got %d", claudeBudget)
	}
	if claudeMaxOut <= claudeBudget {
		t.Errorf("expected claudeMaxOut (%d) > claudeBudget (%d)", claudeMaxOut, claudeBudget)
	}

	// Without a client ceiling the historical headroom expansion is preserved.
	grownBudget, grownMaxOut := ClampThinkingBudget("gemini-2.5-flash", 4096, 0, 65536, false, false)
	if grownMaxOut != grownBudget+thinkingHeadroomTokens {
		t.Errorf("expected maxOut %d, got %d", grownBudget+thinkingHeadroomTokens, grownMaxOut)
	}
}

// TestClampThinkingBudgetRespectsModelCeiling verifies the generation ceiling is clamped to the
// concrete model limit instead of the generic Gemini ceiling, so high-budget Claude and gpt-oss
// requests never exceed what upstream accepts.
func TestClampThinkingBudgetRespectsModelCeiling(t *testing.T) {
	t.Run("claude stays within 64k", func(t *testing.T) {
		budget, maxOut := ClampThinkingBudget("claude-sonnet-4-6", 60000, 0, 64000, true, false)
		if maxOut > 64000 {
			t.Errorf("expected maxOut clamped to 64000, got %d", maxOut)
		}
		if budget >= maxOut {
			t.Errorf("expected budget (%d) < maxOut (%d)", budget, maxOut)
		}
	})

	t.Run("non-gemini model uses catalog ceiling", func(t *testing.T) {
		budget, maxOut := ClampThinkingBudget("gpt-oss-120b", 60000, 0, 32768, false, false)
		if maxOut != 32768 {
			t.Errorf("expected maxOut clamped to the catalog ceiling 32768, got %d", maxOut)
		}
		if budget >= maxOut {
			t.Errorf("expected budget (%d) < maxOut (%d)", budget, maxOut)
		}
	})

	t.Run("unknown ceiling falls back to gemini ceiling", func(t *testing.T) {
		_, maxOut := ClampThinkingBudget("gemini-4-hydrogen-high", 32000, 0, 0, false, false)
		if maxOut != 32000+thinkingHeadroomTokens {
			t.Errorf("expected maxOut %d, got %d", 32000+thinkingHeadroomTokens, maxOut)
		}
	})
}

// TestClampThinkingBudgetHardCapDisablesTinyCeiling verifies a one-token client ceiling collapses the
// reasoning budget to zero so callers can omit the thinking configuration instead of sending an invalid
// budget upstream.
func TestClampThinkingBudgetHardCapDisablesTinyCeiling(t *testing.T) {
	budget, maxOut := ClampThinkingBudget("gemini-2.5-flash", 4096, 1, 65536, false, true)
	if maxOut != 1 {
		t.Errorf("expected maxOut 1 preserved, got %d", maxOut)
	}
	if budget != 0 {
		t.Errorf("expected reasoning disabled with budget 0, got %d", budget)
	}
}

// TestInjectInterruptedToolResponses verifies synthetic tool responses are injected
// when a user interrupts a pending model tool call.
func TestInjectInterruptedToolResponses(t *testing.T) {
	contents := []Content{
		{
			Role:  "user",
			Parts: []Part{{Text: "Find file"}},
		},
		{
			Role: "model",
			Parts: []Part{
				{
					FunctionCall: &FunctionCallPart{
						ID:   "call_1",
						Name: "find_file",
						Args: map[string]any{"path": "foo.go"},
					},
				},
			},
		},
		{
			Role: "user",
			Parts: []Part{
				{Text: "Nevermind, stop." + ""}, // User interrupted without sending tool result
			},
		},
	}

	repaired := InjectInterruptedToolResponses(contents)
	if len(repaired) != 3 {
		t.Fatalf("expected 3 contents, got %d", len(repaired))
	}

	lastTurn := repaired[2]
	if len(lastTurn.Parts) < 2 {
		t.Fatalf("expected at least 2 parts in user turn after injection, got %d", len(lastTurn.Parts))
	}

	firstPart := lastTurn.Parts[0]
	if firstPart.FunctionResponse == nil {
		t.Fatalf("expected first part to be synthetic FunctionResponse, got %+v", firstPart)
	}
	if firstPart.FunctionResponse.ID != "call_1" || firstPart.FunctionResponse.Name != "find_file" {
		t.Errorf("unexpected FunctionResponse target: %+v", firstPart.FunctionResponse)
	}
}

// TestInjectInterruptedToolResponses_MultipleCallsSameName verifies that multiple calls
// sharing the same function name are individually verified by ID and not skipped falsely.
func TestInjectInterruptedToolResponses_MultipleCallsSameName(t *testing.T) {
	contents := []Content{
		{
			Role:  "user",
			Parts: []Part{{Text: "Read two files"}},
		},
		{
			Role: "model",
			Parts: []Part{
				{
					FunctionCall: &FunctionCallPart{
						ID:   "call_1",
						Name: "read_file",
						Args: map[string]any{"path": "a.txt"},
					},
				},
				{
					FunctionCall: &FunctionCallPart{
						ID:   "call_2",
						Name: "read_file",
						Args: map[string]any{"path": "b.txt"},
					},
				},
			},
		},
		{
			Role: "user",
			Parts: []Part{
				// User only provided result for call_1, call_2 was interrupted
				{
					FunctionResponse: &FunctionResponse{
						ID:       "call_1",
						Name:     "read_file",
						Response: map[string]any{"output": "content a"},
					},
				},
			},
		},
	}

	repaired := InjectInterruptedToolResponses(contents)
	userParts := repaired[2].Parts
	if len(userParts) != 2 {
		t.Fatalf("expected 2 parts (1 real + 1 synthetic), got %d", len(userParts))
	}

	hasSyntheticCall2 := false
	for _, p := range userParts {
		if p.FunctionResponse != nil && p.FunctionResponse.ID == "call_2" {
			hasSyntheticCall2 = true
			break
		}
	}
	if !hasSyntheticCall2 {
		t.Error("expected synthetic cancellation response for call_2")
	}
}

// TestThinkingLevelForEffort verifies effort labels map onto uppercase Cloud Code Assist
// thinkingLevel enum values, including XHIGH and MAX, while unknown labels are ignored.
func TestThinkingLevelForEffort(t *testing.T) {
	tests := map[string]string{
		"minimal":    "MINIMAL",
		"low":        "LOW",
		"MEDIUM":     "MEDIUM",
		"high":       "HIGH",
		"xhigh":      "XHIGH",
		"extra_high": "XHIGH",
		"max":        "MAX",
		"":           "",
		"unbounded":  "",
	}

	for effort, expected := range tests {
		if got := ThinkingLevelForEffort(effort); got != expected {
			t.Errorf("ThinkingLevelForEffort(%q) = %q; want %q", effort, got, expected)
		}
	}
}

// TestClaudeUsesThinkingLevel verifies generation detection for both family-first and
// version-first Claude identifiers without misclassifying Claude 4.x as level-capable.
func TestClaudeUsesThinkingLevel(t *testing.T) {
	tests := map[string]bool{
		"claude-sonnet-5-5":          true,
		"claude-opus-5-5-thinking":   true,
		"claude-5-5-sonnet":          true,
		"claude-sonnet-4-6":          false,
		"claude-opus-4-6-thinking":   false,
		"claude-3-7-sonnet-20250219": false,
		"gpt-oss-120b":               false,
	}

	for model, expected := range tests {
		if got := ClaudeUsesThinkingLevel(model); got != expected {
			t.Errorf("ClaudeUsesThinkingLevel(%q) = %v; want %v", model, got, expected)
		}
	}
}
