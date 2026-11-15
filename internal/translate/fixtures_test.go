// Package translate provides bidirectional translation between OpenAI Chat Completions
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResponseTranslator_TrackedFixtures verifies upstream Cloud Code Assist SSE parsing,
// thinking tokens, and tool call signature tracking using tracked test fixtures.
func TestResponseTranslator_TrackedFixtures(t *testing.T) {
	// 1. Thinking and text candidate fixture
	dataGemini, err := os.ReadFile(filepath.Join("testdata", "sse_gemini_thinking_and_text.json"))
	if err != nil {
		t.Fatalf("reading sse_gemini_thinking_and_text.json: %v", err)
	}

	var eventGemini map[string]any
	if err := json.Unmarshal(dataGemini, &eventGemini); err != nil {
		t.Fatalf("unmarshaling gemini event fixture: %v", err)
	}

	var deltas []Delta
	tr := NewResponseTranslator("gemini-3.8-flash-high", func(d Delta) {
		deltas = append(deltas, d)
	})
	tr.Push(eventGemini)

	if tr.Text() != "Here is the concise answer based on analysis." {
		t.Errorf("Text() = %q", tr.Text())
	}
	if tr.Reasoning() != "Analyzing the input problem and planning solution..." {
		t.Errorf("Reasoning() = %q", tr.Reasoning())
	}
	if tr.Finish() != "stop" {
		t.Errorf("Finish() = %q; want 'stop'", tr.Finish())
	}
	if tr.Usage() == nil || tr.Usage().TotalTokens != 53 {
		t.Errorf("Usage() total tokens mismatch: %+v", tr.Usage())
	}
	if !tr.SawOutput() {
		t.Errorf("expected SawOutput() to be true")
	}

	details := tr.ReasoningDetails()
	if len(details) == 0 {
		t.Fatalf("expected non-empty reasoning details for signature replay")
	}

	// 2. Tool call with signature fixture
	dataTool, err := os.ReadFile(filepath.Join("testdata", "sse_tool_call_with_signature.json"))
	if err != nil {
		t.Fatalf("reading sse_tool_call_with_signature.json: %v", err)
	}

	var eventTool map[string]any
	if err := json.Unmarshal(dataTool, &eventTool); err != nil {
		t.Fatalf("unmarshaling tool event fixture: %v", err)
	}

	trTool := NewResponseTranslator("gemini-3.8-flash-high", nil)
	trTool.Push(eventTool)

	calls := trTool.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "edit_file" {
		t.Errorf("function name = %q; want 'edit_file'", calls[0].Function.Name)
	}
	if trTool.Finish() != "tool_calls" {
		t.Errorf("Finish() = %q; want 'tool_calls'", trTool.Finish())
	}

	// 3. Safety finish reason fixture
	dataSafety, err := os.ReadFile(filepath.Join("testdata", "sse_finish_safety.json"))
	if err != nil {
		t.Fatalf("reading sse_finish_safety.json: %v", err)
	}

	var eventSafety map[string]any
	if err := json.Unmarshal(dataSafety, &eventSafety); err != nil {
		t.Fatalf("unmarshaling safety event fixture: %v", err)
	}

	trSafety := NewResponseTranslator("gemini-3.8-flash-high", nil)
	trSafety.Push(eventSafety)

	if trSafety.Finish() != "content_filter" {
		t.Errorf("Finish() = %q; want 'content_filter'", trSafety.Finish())
	}

	// 4. Malformed upstream event (should not panic or corrupt state)
	dataMalformed, err := os.ReadFile(filepath.Join("testdata", "sse_malformed_event.json"))
	if err != nil {
		t.Fatalf("reading sse_malformed_event.json: %v", err)
	}

	var eventMalformed map[string]any
	if err := json.Unmarshal(dataMalformed, &eventMalformed); err != nil {
		t.Fatalf("unmarshaling malformed event fixture: %v", err)
	}

	trMalformed := NewResponseTranslator("gemini-3.8-flash-high", nil)
	trMalformed.Push(eventMalformed)
	if trMalformed.SawOutput() {
		t.Errorf("expected SawOutput() false on malformed event")
	}
}

// TestSanitizedTranslateFixturesZeroSecrets verifies testdata fixtures contain no secret tokens.
func TestSanitizedTranslateFixturesZeroSecrets(t *testing.T) {
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
