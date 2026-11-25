// Package translate provides bidirectional translation between OpenAI Chat Completions
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
)

// TestForGeminiJSONSchema verifies that const values are mapped to single-element enum slices for Gemini.
func TestForGeminiJSONSchema(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{"const": "x"},
			"size": map[string]any{"type": "number", "minimum": 1.0},
		},
		"additionalProperties": false,
	}

	result := ForGeminiJSONSchema(schema).(map[string]any)
	props := result["properties"].(map[string]any)
	kind := props["kind"].(map[string]any)

	if _, hasConst := kind["const"]; hasConst {
		t.Errorf("expected const to be removed, got: %v", kind)
	}
	enums, ok := kind["enum"].([]any)
	if !ok || len(enums) != 1 || enums[0] != "x" {
		t.Errorf("expected enum ['x'], got: %v", kind["enum"])
	}
}

// TestForClaudeParameters verifies stripping unsupported schema keywords ($schema, $defs, allOf) for Claude models.
func TestForClaudeParameters(t *testing.T) {
	schema := map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"properties": map[string]any{
			"mode": map[string]any{
				"anyOf": []any{
					map[string]any{"const": "a"},
					map[string]any{"const": "b"},
				},
			},
			"n": map[string]any{
				"type": []any{"integer", "null"},
			},
		},
		"required": []any{"mode"},
	}

	result := ForClaudeParameters(schema).(map[string]any)

	if _, hasSchema := result["$schema"]; hasSchema {
		t.Errorf("expected $schema to be dropped")
	}

	props := result["properties"].(map[string]any)
	mode := props["mode"].(map[string]any)
	enums, ok := mode["enum"].([]any)
	if !ok || len(enums) != 2 || enums[0] != "a" || enums[1] != "b" {
		t.Errorf("expected enum ['a', 'b'], got: %v", mode["enum"])
	}
	if mode["type"] != "string" {
		t.Errorf("expected type 'string', got: %v", mode["type"])
	}

	n := props["n"].(map[string]any)
	if n["type"] != "integer" || n["nullable"] != true {
		t.Errorf("expected type 'integer' and nullable true, got: %v", n)
	}
}

// TestForClaudeParametersDropsNonStringEnums verifies that non-string enums are safely converted to string values for Claude.
func TestForClaudeParametersDropsNonStringEnums(t *testing.T) {
	schema := map[string]any{
		"enum": []any{1.0, 2.0},
	}
	result := ForClaudeParameters(schema).(map[string]any)
	if _, hasEnum := result["enum"]; hasEnum {
		t.Errorf("expected numeric enum to be dropped, got: %v", result)
	}
}

// TestForGeminiLegacyParameters verifies schema sanitization under legacy parameters fallback mode.
func TestForGeminiLegacyParameters(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{"const": "x"},
			"size": map[string]any{"type": "number", "minimum": 1.0},
		},
		"additionalProperties": false,
	}

	result := ForGeminiLegacyParameters(schema).(map[string]any)
	if _, hasAddProps := result["additionalProperties"]; hasAddProps {
		t.Errorf("expected additionalProperties to be stripped in legacy mode")
	}
	props := result["properties"].(map[string]any)
	size := props["size"].(map[string]any)
	if _, hasMin := size["minimum"]; hasMin {
		t.Errorf("expected minimum to be stripped in legacy mode")
	}
}

// TestRecipeReplayAndPreservation verifies recording and unpacking thought signatures across multi-turn exchanges.
func TestRecipeReplayAndPreservation(t *testing.T) {
	rb := NewRecipeBuilder()
	rb.Thought("Plan: ", "")
	rb.Thought("read file.", "S-thought")
	rb.Text("Reading.")
	rb.Call("call_123", "S-call")

	if !rb.Useful() {
		t.Fatalf("expected recipe builder to be useful")
	}

	steps := rb.Steps()
	if len(steps) != 3 {
		t.Fatalf("expected 3 steps (merged thought, text, call), got: %d", len(steps))
	}

	detail := RecipeDetail(Recipe{
		Model: "gemini-3.8-flash-high",
		Steps: steps,
	})

	parsed := ReadRecipe([]ReasoningDetail{detail})
	if parsed == nil {
		t.Fatalf("failed to read back recipe from ReasoningDetail")
	}

	if parsed.Model != "gemini-3.8-flash-high" {
		t.Errorf("expected model gemini-3.8-flash-high, got: %s", parsed.Model)
	}
	if len(parsed.Steps) != 3 {
		t.Errorf("expected 3 parsed steps, got: %d", len(parsed.Steps))
	}
	if parsed.Steps[0].Text != "Plan: read file." || parsed.Steps[0].Sig != "S-thought" {
		t.Errorf("unexpected thought step: %+v", parsed.Steps[0])
	}
	if parsed.Steps[2].ID != "call_123" || parsed.Steps[2].Sig != "S-call" {
		t.Errorf("unexpected call step: %+v", parsed.Steps[2])
	}
}

// TestBuildRequestGeminiBasic verifies basic user prompt translation into Gemini generateContent contents.
func TestBuildRequestGeminiBasic(t *testing.T) {
	req := &ChatRequest{
		Model: "gemini-3.8-flash-high",
		Messages: []ChatMessage{
			{Role: "system", Content: "Be brief."},
			{Role: "developer", Content: []any{map[string]any{"type": "text", "text": "Use tools."}}},
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": "text", "text": "Look:"},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
				},
			},
			{Role: "assistant", Content: "Seen."},
			{Role: "user", Content: "Thanks"},
		},
	}

	info := catalog.FamilyDefaults(req.Model)
	geminiReq, err := BuildRequest(req, BuildOptions{Info: info})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}

	if geminiReq.SystemInstruction == nil || len(geminiReq.SystemInstruction.Parts) != 1 {
		t.Fatalf("expected systemInstruction with 1 part")
	}
	expectedSys := "Be brief.\n\nUse tools."
	if geminiReq.SystemInstruction.Parts[0].Text != expectedSys {
		t.Errorf("expected system %q, got %q", expectedSys, geminiReq.SystemInstruction.Parts[0].Text)
	}

	if len(geminiReq.Contents) != 3 {
		t.Fatalf("expected 3 contents, got: %d", len(geminiReq.Contents))
	}

	user0 := geminiReq.Contents[0]
	if user0.Role != "user" || len(user0.Parts) != 2 {
		t.Fatalf("unexpected user content: %+v", user0)
	}
	if user0.Parts[0].Text != "Look:" {
		t.Errorf("expected 'Look:', got: %s", user0.Parts[0].Text)
	}
	if user0.Parts[1].InlineData == nil || user0.Parts[1].InlineData.MimeType != "image/png" || user0.Parts[1].InlineData.Data != "AAAA" {
		t.Errorf("unexpected inlineData: %+v", user0.Parts[1].InlineData)
	}
}

// TestBuildRequestGemini3SkipSignatureAndToolIDs verifies injecting SkipSignature and tool IDs on Gemini 3+ models.
func TestBuildRequestGemini3SkipSignatureAndToolIDs(t *testing.T) {
	req := &ChatRequest{
		Model: "gemini-3.8-flash-high",
		Messages: []ChatMessage{
			{Role: "user", Content: "read it"},
			{
				Role: "assistant",
				ToolCalls: []ChatToolCall{
					{ID: "call_a", Type: "function", Function: ChatFunctionCall{Name: "read", Arguments: `{"path":"a.txt"}`}},
					{ID: "call|b", Type: "function", Function: ChatFunctionCall{Name: "ls", Arguments: `{}`}},
				},
			},
			{Role: "tool", ToolCallID: "call_a", Content: "hello"},
			{Role: "tool", ToolCallID: "call|b", Content: "a.txt"},
		},
	}

	info := catalog.FamilyDefaults(req.Model)
	geminiReq, err := BuildRequest(req, BuildOptions{Info: info})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}

	if len(geminiReq.Contents) != 3 {
		t.Fatalf("expected 3 turns, got: %d", len(geminiReq.Contents))
	}

	modelTurn := geminiReq.Contents[1]
	if len(modelTurn.Parts) != 2 {
		t.Fatalf("expected 2 tool calls in model turn, got: %d", len(modelTurn.Parts))
	}

	part0 := modelTurn.Parts[0]
	if part0.FunctionCall.Name != "read" || part0.FunctionCall.ID != "call_a" {
		t.Errorf("unexpected call 0: %+v", part0.FunctionCall)
	}
	if part0.ThoughtSignature != SkipSignature {
		t.Errorf("expected skip signature on first Gemini 3 tool call, got: %s", part0.ThoughtSignature)
	}

	part1 := modelTurn.Parts[1]
	if part1.FunctionCall.Name != "ls" || part1.FunctionCall.ID != "call_b" {
		t.Errorf("unexpected call 1 (expected id sanitized): %+v", part1.FunctionCall)
	}

	toolTurn := geminiReq.Contents[2]
	if len(toolTurn.Parts) != 2 {
		t.Fatalf("expected 2 responses in tool turn, got: %d", len(toolTurn.Parts))
	}
	if toolTurn.Parts[0].FunctionResponse.ID != "call_a" || toolTurn.Parts[1].FunctionResponse.ID != "call_b" {
		t.Errorf("unexpected tool response IDs: %+v, %+v", toolTurn.Parts[0].FunctionResponse, toolTurn.Parts[1].FunctionResponse)
	}
}

// TestBuildRequestGemini2SkipSignatureWithoutToolCallIDs verifies Gemini 2.x tool replays carry the
// skip signature sentinel while still omitting tool call IDs, which that generation does not require.
func TestBuildRequestGemini2SkipSignatureWithoutToolCallIDs(t *testing.T) {
	req := &ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []ChatMessage{
			{Role: "user", Content: "x"},
			{
				Role: "assistant",
				ToolCalls: []ChatToolCall{
					{ID: "c1", Type: "function", Function: ChatFunctionCall{Name: "f", Arguments: `{}`}},
				},
			},
			{Role: "tool", ToolCallID: "c1", Content: "y"},
		},
	}

	info := catalog.FamilyDefaults(req.Model)
	geminiReq, err := BuildRequest(req, BuildOptions{Info: info})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}

	modelPart := geminiReq.Contents[1].Parts[0]
	if modelPart.FunctionCall.ID != "" {
		t.Errorf("expected empty ID for Gemini 2.5, got: %s", modelPart.FunctionCall.ID)
	}
	if modelPart.ThoughtSignature != SkipSignature {
		t.Errorf("expected skip signature on Gemini 2.5 tool replay, got: %q", modelPart.ThoughtSignature)
	}
}

// TestBuildRequestGemini2MultipleToolCallsSkipSignature verifies that a multi-call Gemini 2.x turn
// mirrors the established behavior: only the first function call carries the sentinel, and later
// calls stay untouched.
func TestBuildRequestGemini2MultipleToolCallsSkipSignature(t *testing.T) {
	req := &ChatRequest{
		Model: "gemini-2.0-flash",
		Messages: []ChatMessage{
			{Role: "user", Content: "run both"},
			{
				Role: "assistant",
				ToolCalls: []ChatToolCall{
					{ID: "c1", Type: "function", Function: ChatFunctionCall{Name: "a", Arguments: `{}`}},
					{ID: "c2", Type: "function", Function: ChatFunctionCall{Name: "b", Arguments: `{}`}},
				},
			},
			{Role: "tool", ToolCallID: "c1", Content: "one"},
			{Role: "tool", ToolCallID: "c2", Content: "two"},
		},
	}

	geminiReq, err := BuildRequest(req, BuildOptions{Info: catalog.FamilyDefaults(req.Model)})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}

	parts := geminiReq.Contents[1].Parts
	if len(parts) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(parts))
	}
	if parts[0].ThoughtSignature != SkipSignature {
		t.Errorf("expected skip signature on the first call, got %q", parts[0].ThoughtSignature)
	}
	if parts[1].ThoughtSignature != "" {
		t.Errorf("expected later calls left untouched, got %q", parts[1].ThoughtSignature)
	}
}

// TestNeedsSkipSignature verifies the signature requirement gate covers Gemini 2.x and newer while
// excluding families that negotiate signatures through their own contract.
func TestNeedsSkipSignature(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gemini-2.0-flash", true},
		{"gemini-2.5-flash", true},
		{"gemini-2.5-pro", true},
		{"gemini-3.8-flash-high", true},
		{"gemini-4-hydrogen-high", true},
		{"claude-sonnet-4-6", false},
		{"claude-opus-4-6-thinking", false},
		{"gpt-oss-120b", false},
		// Aliases without a generation number keep today's behavior: no sentinel is injected.
		{"gemini-flash-latest", false},
		{"", false},
	}

	for _, tc := range cases {
		if got := needsSkipSignature(tc.model); got != tc.want {
			t.Errorf("needsSkipSignature(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// TestResponseTranslatorAndSSEParser verifies streaming translation of Cloud Code Assist SSE events into OpenAI chunks.
func TestResponseTranslatorAndSSEParser(t *testing.T) {
	var deltas []Delta
	tr := NewResponseTranslator("gemini-3.8-flash-high", func(d Delta) {
		deltas = append(deltas, d)
	})

	sseData := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"think","thought":true}]}}]}}

data: {"response":{"candidates":[{"content":{"parts":[{"text":"Hi"},{"text":" there","thoughtSignature":"T"}]}}]}}

: some comment
data: {"response":{"candidates":[{"content":{"parts":[]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":100,"cachedContentTokenCount":40,"candidatesTokenCount":10,"thoughtsTokenCount":5,"totalTokenCount":115}}}

data: [DONE]
`

	err := ReadSSEEvents(strings.NewReader(sseData), func(event map[string]any) error {
		tr.Push(event)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadSSEEvents failed: %v", err)
	}

	if tr.Finish() != "length" {
		t.Errorf("expected finish_reason 'length', got: %s", tr.Finish())
	}
	if tr.Text() != "Hi there" {
		t.Errorf("expected text 'Hi there', got: %s", tr.Text())
	}
	if tr.Reasoning() != "think" {
		t.Errorf("expected reasoning 'think', got: %s", tr.Reasoning())
	}

	u := tr.Usage()
	if u == nil || u.PromptTokens != 100 || u.CompletionTokens != 15 || u.TotalTokens != 115 {
		t.Errorf("unexpected usage: %+v", u)
	}
	if u.PromptTokensDetails == nil || u.PromptTokensDetails.CachedTokens != 40 {
		t.Errorf("unexpected cached tokens: %+v", u.PromptTokensDetails)
	}
	if u.CompletionTokensDetails == nil || u.CompletionTokensDetails.ReasoningTokens != 5 {
		t.Errorf("unexpected reasoning tokens: %+v", u.CompletionTokensDetails)
	}

	details := tr.ReasoningDetails()
	recipe := ReadRecipe(details)
	if recipe == nil || len(recipe.Steps) != 2 {
		t.Fatalf("unexpected recipe: %+v", recipe)
	}
	if recipe.Steps[0].Type != "thought" || recipe.Steps[0].Text != "think" {
		t.Errorf("step 0 mismatch: %+v", recipe.Steps[0])
	}
	if recipe.Steps[1].Type != "text" || recipe.Steps[1].Sig != "T" {
		t.Errorf("step 1 mismatch: %+v", recipe.Steps[1])
	}

	completion := tr.Completion("chatcmpl-123", 1234567890)
	if completion.ID != "chatcmpl-123" || completion.Choices[0].FinishReason != "length" {
		t.Errorf("unexpected completion: %+v", completion)
	}
}

// TestClaudeThinkingDisabledWhenToolForcedOrUnsigned verifies disabling Claude thinking when tools are forced or reasoning is unsigned.
func TestClaudeThinkingDisabledWhenToolForcedOrUnsigned(t *testing.T) {
	req := &ChatRequest{
		Model: "claude-sonnet-4-6",
		Messages: []ChatMessage{
			{Role: "user", Content: "x"},
			{
				Role: "assistant",
				ToolCalls: []ChatToolCall{
					{ID: "toolu_1", Type: "function", Function: ChatFunctionCall{Name: "edit", Arguments: `{"mode":"a"}`}},
				},
			},
			{Role: "tool", ToolCallID: "toolu_1", Content: "done"},
		},
		Tools: []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": "edit",
				},
			},
		},
	}

	info := catalog.FamilyDefaults(req.Model)
	geminiReq, err := BuildRequest(req, BuildOptions{Info: info})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}

	if _, ok := geminiReq.GenerationConfig["thinkingConfig"]; ok {
		t.Errorf("expected thinkingConfig to be omitted for Claude loop without signed thought")
	}
}

// TestClaudeThinkingEnabledWhenSignedThoughtPresent verifies enabling Claude thinking when authentic signed thoughts exist.
func TestClaudeThinkingEnabledWhenSignedThoughtPresent(t *testing.T) {
	tr := NewResponseTranslator("claude-sonnet-4-6", nil)
	tr.Push(map[string]any{
		"response": map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"parts": []any{
							map[string]any{"text": "hmm", "thought": true},
							map[string]any{"text": "", "thought": true, "thoughtSignature": "SIG"},
						},
					},
				},
			},
		},
	})
	tr.Push(map[string]any{
		"response": map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"parts": []any{
							map[string]any{
								"functionCall": map[string]any{
									"name": "edit",
									"id":   "toolu_9",
									"args": map[string]any{},
								},
							},
						},
					},
					"finishReason": "STOP",
				},
			},
		},
	})

	req := &ChatRequest{
		Model: "claude-sonnet-4-6",
		Messages: []ChatMessage{
			{Role: "user", Content: "x"},
			{
				Role:             "assistant",
				ReasoningDetails: tr.ReasoningDetails(),
				ToolCalls: []ChatToolCall{
					{ID: "toolu_9", Type: "function", Function: ChatFunctionCall{Name: "edit", Arguments: `{}`}},
				},
			},
			{Role: "tool", ToolCallID: "toolu_9", Content: "ok"},
		},
	}

	info := catalog.FamilyDefaults(req.Model)
	geminiReq, err := BuildRequest(req, BuildOptions{Info: info})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}

	if _, ok := geminiReq.GenerationConfig["thinkingConfig"]; !ok {
		t.Errorf("expected thinkingConfig enabled when Claude loop has signed thought")
	}
}

// TestBuildEnvelope verifies packaging Gemini requests into agent envelopes with UUID v4 request IDs.
func TestBuildEnvelope(t *testing.T) {
	req := &ChatRequest{
		Model: "gemini-3.1-pro-high",
		Messages: []ChatMessage{
			{Role: "user", Content: "Hello world"},
		},
	}

	info := catalog.FamilyDefaults(req.Model)
	env, err := BuildEnvelope(req, BuildOptions{
		Info:      info,
		ProjectID: "companion-proj-123",
	})
	if err != nil {
		t.Fatalf("BuildEnvelope failed: %v", err)
	}

	if env.Project != "companion-proj-123" {
		t.Errorf("expected project companion-proj-123, got: %s", env.Project)
	}
	if env.Model != "gemini-pro-agent" {
		t.Errorf("expected model mapped to upstream gemini-pro-agent, got: %s", env.Model)
	}
	if env.UserAgent != "antigravity" || env.RequestType != "" {
		t.Errorf("expected plain chat envelope without requestType, got: %s / %s", env.UserAgent, env.RequestType)
	}
	if !strings.HasPrefix(env.RequestID, "agent-") {
		t.Errorf("expected requestId starting with agent-, got: %s", env.RequestID)
	}

	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("failed marshaling envelope: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("failed unmarshaling envelope JSON: %v", err)
	}
	if _, exists := payload["requestType"]; exists {
		t.Errorf("expected requestType omitted for plain chat, got: %v", payload["requestType"])
	}
	if payload["userAgent"] != "antigravity" {
		t.Errorf("expected userAgent 'antigravity', got: %v", payload["userAgent"])
	}
}

// TestBuildEnvelopeAgenticRequestType verifies that tool-enabled and session-scoped turns keep
// requestType "agent" so upstream agentic semantics are preserved.
func TestBuildEnvelopeAgenticRequestType(t *testing.T) {
	tests := []struct {
		name    string
		req     *ChatRequest
		opts    BuildOptions
		wantTyp string
	}{
		{
			name: "tool enabled chat",
			req: &ChatRequest{
				Model:    "gemini-2.5-flash",
				Messages: []ChatMessage{{Role: "user", Content: "Hello"}},
				Tools: []any{
					map[string]any{
						"type": "function",
						"function": map[string]any{
							"name": "lookup",
						},
					},
				},
			},
			wantTyp: "agent",
		},
		{
			name: "session scoped chat",
			req: &ChatRequest{
				Model:    "gemini-2.5-flash",
				Messages: []ChatMessage{{Role: "user", Content: "Hello"}},
			},
			opts:    BuildOptions{SessionID: "session-abc"},
			wantTyp: "agent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.opts.Info = catalog.FamilyDefaults(tt.req.Model)
			env, err := BuildEnvelope(tt.req, tt.opts)
			if err != nil {
				t.Fatalf("BuildEnvelope failed: %v", err)
			}
			if env.RequestType != tt.wantTyp {
				t.Errorf("RequestType = %q; want %q", env.RequestType, tt.wantTyp)
			}

			raw, err := json.Marshal(env)
			if err != nil {
				t.Fatalf("failed marshaling envelope: %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("failed unmarshaling envelope JSON: %v", err)
			}
			if payload["requestType"] != tt.wantTyp {
				t.Errorf("envelope requestType = %v; want %q", payload["requestType"], tt.wantTyp)
			}
		})
	}
}

// TestForGeminiLegacyParametersNumericAndBoolEnums verifies preserving numeric and boolean enums in legacy schemas.
func TestForGeminiLegacyParametersNumericAndBoolEnums(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"num": map[string]any{
				"anyOf": []any{
					map[string]any{"const": 10},
					map[string]any{"const": 20},
				},
			},
			"flag": map[string]any{
				"enum": []any{true, false},
			},
		},
	}

	result := ForGeminiLegacyParameters(schema).(map[string]any)
	props := result["properties"].(map[string]any)

	num := props["num"].(map[string]any)
	numEnums := num["enum"].([]any)
	if len(numEnums) != 2 || numEnums[0] != "10" || numEnums[1] != "20" {
		t.Errorf("expected stringified numeric enums ['10', '20'], got: %v", numEnums)
	}

	flag := props["flag"].(map[string]any)
	flagEnums := flag["enum"].([]any)
	if len(flagEnums) != 2 || flagEnums[0] != "true" || flagEnums[1] != "false" {
		t.Errorf("expected stringified bool enums ['true', 'false'], got: %v", flagEnums)
	}
}

// TestStreamingDeltaToolCallZeroIndexSerialization verifies explicitly preserving index: 0 in streaming tool call deltas.
func TestStreamingDeltaToolCallZeroIndexSerialization(t *testing.T) {
	var capturedDelta Delta
	tr := NewResponseTranslator("gemini-3.8-flash-high", func(d Delta) {
		if len(d.ToolCalls) > 0 {
			capturedDelta = d
		}
	})

	tr.Push(map[string]any{
		"response": map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"parts": []any{
							map[string]any{
								"functionCall": map[string]any{
									"name": "search",
									"args": map[string]any{"q": "golang"},
								},
							},
						},
					},
				},
			},
		},
	})

	if len(capturedDelta.ToolCalls) != 1 {
		t.Fatalf("expected 1 delta tool call, got: %d", len(capturedDelta.ToolCalls))
	}
	if capturedDelta.ToolCalls[0].Index != 0 {
		t.Errorf("expected Index 0, got: %d", capturedDelta.ToolCalls[0].Index)
	}

	b, err := json.Marshal(capturedDelta)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if !strings.Contains(string(b), `"index":0`) {
		t.Errorf("expected JSON to contain '\"index\":0', got: %s", string(b))
	}

	// Verify non-streaming completion does NOT leak "index" into tool calls
	completion := tr.Completion("cmpl-1", 1000)
	compBytes, err := json.Marshal(completion)
	if err != nil {
		t.Fatalf("completion marshal failed: %v", err)
	}
	if strings.Contains(string(compBytes), `"index":0`) || strings.Contains(string(compBytes), `"index":`) {
		msgToolCalls := completion.Choices[0].Message.ToolCalls
		if len(msgToolCalls) != 1 {
			t.Fatalf("expected 1 completion tool call, got: %d", len(msgToolCalls))
		}
		tcBytes, _ := json.Marshal(msgToolCalls[0])
		if strings.Contains(string(tcBytes), `"index"`) {
			t.Errorf("expected no index in non-streaming tool call, got: %s", string(tcBytes))
		}
	}
}

// TestReadSSEEventsLargePayload verifies that large streaming chunks (up to 10 MB) parse without scanner buffer overflow.
func TestReadSSEEventsLargePayload(t *testing.T) {
	largeText := strings.Repeat("A", 100*1024) // 100 KB
	sseInput := "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"" + largeText + "\"}]}}]}}\n\n"

	var receivedText string
	err := ReadSSEEvents(strings.NewReader(sseInput), func(event map[string]any) error {
		resp := event["response"].(map[string]any)
		cand := resp["candidates"].([]any)[0].(map[string]any)
		content := cand["content"].(map[string]any)
		part := content["parts"].([]any)[0].(map[string]any)
		receivedText = part["text"].(string)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadSSEEvents failed on large payload: %v", err)
	}
	if len(receivedText) != 100*1024 {
		t.Errorf("expected 100KB text, got %d bytes", len(receivedText))
	}
}

// TestWrapEnvelope verifies packaging a pre-translated GeminiRequest with companion project
// metadata and unique UUID-based RequestID without re-translating content.
func TestWrapEnvelope(t *testing.T) {
	geminiReq := &GeminiRequest{
		Contents: []Content{
			{Role: "user", Parts: []Part{{Text: "Hello world"}}},
		},
	}

	env := WrapEnvelope(geminiReq, "gemini-pro-agent", "project-12345")
	if env == nil {
		t.Fatal("expected non-nil AgentEnvelope")
	}

	if env.Project != "project-12345" {
		t.Errorf("expected Project 'project-12345', got %s", env.Project)
	}
	if env.Model != "gemini-pro-agent" {
		t.Errorf("expected Model 'gemini-pro-agent', got %s", env.Model)
	}
	if env.Request != geminiReq {
		t.Error("expected Request pointer to match input geminiReq")
	}
	if !strings.HasPrefix(env.RequestID, "agent-") {
		t.Errorf("expected RequestID prefix 'agent-', got %s", env.RequestID)
	}
	if env.UserAgent != "antigravity" || env.RequestType != "" {
		t.Errorf("unexpected UserAgent or RequestType: %s / %s", env.UserAgent, env.RequestType)
	}

	agenticReq := &GeminiRequest{
		Contents:  []Content{{Role: "user", Parts: []Part{{Text: "Hello world"}}}},
		SessionID: "session-abc",
	}
	if got := WrapEnvelope(agenticReq, "gemini-pro-agent", "project-12345").RequestType; got != "agent" {
		t.Errorf("expected session-scoped envelope requestType 'agent', got %q", got)
	}
}

// TestGeminiFourXHighThinkingLevel verifies Gemini 4 models accept the deepest XHIGH
// thinking level without requiring a -tiered model suffix.
func TestGeminiFourXHighThinkingLevel(t *testing.T) {
	req := &ChatRequest{
		Model:           "gemini-4-neon",
		Messages:        []ChatMessage{{Role: "user", Content: "Solve this"}},
		ReasoningEffort: "xhigh",
	}

	info := catalog.FamilyDefaults(req.Model)
	geminiReq, err := BuildRequest(req, BuildOptions{Info: info})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}

	tc, ok := geminiReq.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinkingConfig, got %v", geminiReq.GenerationConfig["thinkingConfig"])
	}
	if tc["thinkingLevel"] != "XHIGH" {
		t.Errorf("thinkingLevel = %v; want XHIGH", tc["thinkingLevel"])
	}
	if tc["includeThoughts"] != true {
		t.Errorf("expected includeThoughts true, got %v", tc["includeThoughts"])
	}
}

// TestClaudeFiveChatThinkingLevel verifies the OpenAI-compatible path maps Claude 5+ reasoning
// effort to thinkingLevel while legacy Claude 4.x keeps numeric thinking_budget compatibility.
func TestClaudeFiveChatThinkingLevel(t *testing.T) {
	req := &ChatRequest{
		Model:           "claude-sonnet-5-5",
		Messages:        []ChatMessage{{Role: "user", Content: "Plan"}},
		ReasoningEffort: "max",
	}

	geminiReq, err := BuildRequest(req, BuildOptions{Info: catalog.FamilyDefaults(req.Model)})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}

	tc, ok := geminiReq.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinkingConfig, got %v", geminiReq.GenerationConfig["thinkingConfig"])
	}
	if tc["thinkingLevel"] != "MAX" || tc["includeThoughts"] != true {
		t.Errorf("unexpected Claude 5 thinkingConfig: %v", tc)
	}
	if _, exists := tc["thinking_budget"]; exists {
		t.Errorf("expected no legacy thinking_budget for Claude 5, got %v", tc)
	}

	legacyReq := &ChatRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []ChatMessage{{Role: "user", Content: "Plan"}},
	}
	legacyGemini, err := BuildRequest(legacyReq, BuildOptions{Info: catalog.FamilyDefaults(legacyReq.Model)})
	if err != nil {
		t.Fatalf("BuildRequest legacy failed: %v", err)
	}
	legacyTC, ok := legacyGemini.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected legacy thinkingConfig, got %v", legacyGemini.GenerationConfig["thinkingConfig"])
	}
	if _, exists := legacyTC["thinking_budget"]; !exists {
		t.Errorf("expected legacy thinking_budget for Claude 4.x, got %v", legacyTC)
	}
}

// TestBuildRequestInlineSystemFallback verifies the prompt-rejection fallback moves scrubbed
// system instructions into the first user turn.
func TestBuildRequestInlineSystemFallback(t *testing.T) {
	req := &ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are OpenCode.\nAnswer concisely."},
			{Role: "user", Content: "Hi"},
		},
	}

	geminiReq, err := BuildRequest(req, BuildOptions{
		Info:         catalog.FamilyDefaults(req.Model),
		InlineSystem: true,
	})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}

	if geminiReq.SystemInstruction != nil {
		t.Errorf("expected no systemInstruction on inline fallback, got %+v", geminiReq.SystemInstruction)
	}
	if len(geminiReq.Contents) == 0 || len(geminiReq.Contents[0].Parts) < 2 {
		t.Fatalf("expected system text prepended to the first user turn, got %+v", geminiReq.Contents)
	}
	firstPart := geminiReq.Contents[0].Parts[0].Text
	if !strings.Contains(firstPart, "You are a coding assistant.") || !strings.Contains(firstPart, "Answer concisely.") {
		t.Errorf("unexpected inlined system text: %q", firstPart)
	}
	if lastText := geminiReq.Contents[0].Parts[len(geminiReq.Contents[0].Parts)-1].Text; lastText != "Hi" {
		t.Errorf("expected original user text preserved last, got %q", lastText)
	}
}

// TestGeminiFourEffortOverridesNumericBudget verifies Gemini 4 effort variants win over a
// catalog-supplied numeric thinking budget so XHIGH is never downgraded to a budget path.
func TestGeminiFourEffortOverridesNumericBudget(t *testing.T) {
	budget := 12000
	info := catalog.CatalogModel{
		ID:              "gemini-4-carbon-high",
		Thinking:        true,
		ThinkingBudget:  &budget,
		MaxOutputTokens: 65536,
	}
	req := &ChatRequest{
		Model:           "gemini-4-carbon-high",
		Messages:        []ChatMessage{{Role: "user", Content: "Solve this"}},
		ReasoningEffort: "xhigh",
	}

	geminiReq, err := BuildRequest(req, BuildOptions{Info: info})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}

	tc, ok := geminiReq.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinkingConfig, got %v", geminiReq.GenerationConfig["thinkingConfig"])
	}
	if tc["thinkingLevel"] != "XHIGH" {
		t.Errorf("thinkingLevel = %v; want XHIGH", tc["thinkingLevel"])
	}
	if _, exists := tc["thinkingBudget"]; exists {
		t.Errorf("expected effort level to replace numeric thinkingBudget, got %v", tc)
	}
}

// TestOpenAIToolMessageImagesBecomeInlineData verifies image_url blocks inside an OpenAI tool role
// message become inlineData parts next to the function response instead of being dropped.
func TestOpenAIToolMessageImagesBecomeInlineData(t *testing.T) {
	req := &ChatRequest{
		Model: "gemini-2.5-flash",
		Messages: []ChatMessage{
			{Role: "user", Content: "capture the page"},
			{
				Role: "assistant",
				ToolCalls: []ChatToolCall{{
					ID:       "call_1",
					Type:     "function",
					Function: ChatFunctionCall{Name: "screenshot", Arguments: "{}"},
				}},
			},
			{
				Role:       "tool",
				ToolCallID: "call_1",
				Name:       "screenshot",
				Content: []any{
					map[string]any{"type": "text", "text": "captured"},
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": "data:image/jpeg;base64,AAAA"},
					},
				},
			},
		},
	}

	geminiReq, err := BuildRequest(req, BuildOptions{Info: catalog.CatalogModel{ID: "gemini-2.5-flash", MaxOutputTokens: 8192}})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}

	var sawInline, sawResponse bool
	for _, content := range geminiReq.Contents {
		for _, part := range content.Parts {
			if part.InlineData != nil && part.InlineData.MimeType == "image/jpeg" && part.InlineData.Data == "AAAA" {
				sawInline = true
			}
			if part.FunctionResponse != nil && part.FunctionResponse.Name == "screenshot" {
				sawResponse = true
			}
		}
	}
	if !sawResponse {
		t.Error("expected a functionResponse part for the tool message")
	}
	if !sawInline {
		t.Error("expected the tool message image to produce an inlineData part")
	}
}

// TestAdjustFixedBudgetZeroCeilingFallback verifies a model without a catalog output ceiling falls
// back to the generic Gemini ceiling instead of collapsing budget and maxOutputTokens to zero and one.
func TestAdjustFixedBudgetZeroCeilingFallback(t *testing.T) {
	budget, maxOut := adjustFixedBudget(catalog.CatalogModel{ID: "custom-model"}, 4096, nil)
	if budget == nil || *budget != 4096 {
		t.Fatalf("expected thinking budget preserved at 4096, got %v", budget)
	}
	if maxOut == nil || *maxOut <= *budget || *maxOut != 4096+8192 {
		t.Fatalf("expected maxOutputTokens %d, got %v", 4096+8192, maxOut)
	}
}

// TestBuildModelPartsCallBeforeTextPreservesSignature verifies that a text thoughtSignature is not
// dropped when a recipe lists a tool call step before the text step that owns the signature.
func TestBuildModelPartsCallBeforeTextPreservesSignature(t *testing.T) {
	details := []ReasoningDetail{RecipeDetail(Recipe{
		Model: "gemini-3.8-pro",
		Steps: []RecipeStep{
			{Type: "call", ID: "call_1", Sig: "sig-call"},
			{Type: "text", Sig: "sig-text"},
		},
	})}

	msg := ChatMessage{
		Role:             "assistant",
		Content:          "done",
		ReasoningDetails: details,
		ToolCalls: []ChatToolCall{
			{ID: "call_1", Type: "function", Function: ChatFunctionCall{Name: "edit", Arguments: `{}`}},
		},
	}

	parts := buildModelParts(msg, "gemini-3.8-pro")
	if len(parts) < 2 {
		t.Fatalf("expected text and function call parts, got %d part(s)", len(parts))
	}

	text := parts[0]
	if text.FunctionCall != nil || text.Text != "done" {
		t.Fatalf("expected text part first, got %+v", text)
	}
	if text.ThoughtSignature != "sig-text" {
		t.Errorf("expected text signature sig-text preserved, got %q", text.ThoughtSignature)
	}

	call := parts[1]
	if call.FunctionCall == nil || call.FunctionCall.Name != "edit" {
		t.Fatalf("expected edit function call part, got %+v", call)
	}
	if call.ThoughtSignature != "sig-call" {
		t.Errorf("expected call signature sig-call preserved, got %q", call.ThoughtSignature)
	}
}
