// Package translate provides bidirectional translation between OpenAI/Anthropic
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
)

// TestValidateAnthropicRequest verifies validation checks on required fields.
func TestValidateAnthropicRequest(t *testing.T) {
	if err := ValidateAnthropicRequest(nil); err == nil {
		t.Fatal("expected error for nil request, got nil")
	}

	reqNoModel := &AnthropicRequest{
		Messages: []AnthropicMessage{{Role: "user", Content: "hi"}},
	}
	if err := ValidateAnthropicRequest(reqNoModel); err == nil {
		t.Fatal("expected error for empty model, got nil")
	}

	reqNoMsg := &AnthropicRequest{
		Model: "claude-sonnet-4-6",
	}
	if err := ValidateAnthropicRequest(reqNoMsg); err == nil {
		t.Fatal("expected error for empty messages, got nil")
	}

	reqBadRole := &AnthropicRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []AnthropicMessage{{Role: "system", Content: "invalid"}},
	}
	if err := ValidateAnthropicRequest(reqBadRole); err == nil {
		t.Fatal("expected error for invalid message role, got nil")
	}
}

// TestBuildAnthropicRequest verifies full conversion of Anthropic messages, tools, and thinking to GeminiRequest.
func TestBuildAnthropicRequest(t *testing.T) {
	req := &AnthropicRequest{
		Model:  "claude-sonnet-4-6-thinking",
		System: "You are a coding assistant.\nx-anthropic-billing-header: cc_version=2.1.274;\nPlease code cleanly.",
		Messages: []AnthropicMessage{
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type":          "text",
						"text":          "Check weather",
						"cache_control": map[string]any{"type": "ephemeral"},
					},
				},
			},
			{
				Role: "assistant",
				Content: []any{
					map[string]any{
						"type":      "thinking",
						"thinking":  "I need to call get_weather.",
						"signature": "sig_12345",
					},
					map[string]any{
						"type":  "tool_use",
						"id":    "toolu_01",
						"name":  "get_weather",
						"input": map[string]any{"city": "Jakarta"},
					},
				},
			},
			{
				Role: "user",
				Content: []any{
					map[string]any{
						"type":        "tool_result",
						"tool_use_id": "toolu_01",
						"content":     "Sunny 32C",
					},
				},
			},
		},
		MaxTokens: 4096,
		Thinking: &AnthropicThinking{
			Type:         "enabled",
			BudgetTokens: 2048,
		},
		Tools: []AnthropicTool{
			{
				Name:        "get_weather",
				Description: "Get weather for a city",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
					},
					"required": []any{"city"},
				},
			},
		},
	}

	opts := BuildOptions{
		ProjectID: "test-proj-123",
		Info: catalog.CatalogModel{
			ID:              "claude-sonnet-4-6-thinking",
			Thinking:        true,
			MaxOutputTokens: 8192,
		},
	}

	env, err := BuildAnthropicEnvelope(req, opts)
	if err != nil {
		t.Fatalf("BuildAnthropicEnvelope failed: %v", err)
	}

	if env.Project != "test-proj-123" {
		t.Errorf("expected project test-proj-123, got %s", env.Project)
	}

	geminiReq := env.Request
	if geminiReq.SystemInstruction == nil || len(geminiReq.SystemInstruction.Parts) == 0 {
		t.Fatal("expected system instructions, got nil")
	}

	// Verify billing header was stripped
	sysText := geminiReq.SystemInstruction.Parts[0].Text
	if strings.Contains(sysText, "x-anthropic-billing-header") {
		t.Errorf("expected billing header stripped, got: %s", sysText)
	}

	// Verify tools were registered
	if len(geminiReq.Tools) == 0 {
		t.Fatal("expected tools in request, got none")
	}
	toolMap, ok := geminiReq.Tools[0].(map[string]any)
	if !ok {
		t.Fatalf("expected tool map, got %T", geminiReq.Tools[0])
	}
	fdecls, ok := toolMap["functionDeclarations"].([]map[string]any)
	if !ok || len(fdecls) == 0 {
		t.Fatalf("expected functionDeclarations slice, got %v", toolMap["functionDeclarations"])
	}
	fdecl := fdecls[0]
	if fdecl["name"] != "get_weather" {
		t.Errorf("expected tool get_weather, got %v", fdecl["name"])
	}
	// For Claude models, Google Cloud Code requires parameters under "parameters"
	if _, ok := fdecl["parameters"]; !ok {
		t.Errorf("expected 'parameters' field for Claude model tool, got: %v", fdecl)
	}

	// Verify Claude thinking config
	tc, ok := geminiReq.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinkingConfig map, got %v", geminiReq.GenerationConfig["thinkingConfig"])
	}
	if tc["include_thoughts"] != true {
		t.Errorf("expected include_thoughts true, got %v", tc["include_thoughts"])
	}
	if tc["thinking_budget"] != 2048 {
		t.Errorf("expected thinking_budget 2048, got %v", tc["thinking_budget"])
	}

	// Verify tool_result converted to FunctionResponse
	lastContent := geminiReq.Contents[len(geminiReq.Contents)-1]
	if lastContent.Role != "user" {
		t.Errorf("expected role user for tool_result, got %s", lastContent.Role)
	}
	if len(lastContent.Parts) == 0 || lastContent.Parts[0].FunctionResponse == nil {
		t.Fatalf("expected FunctionResponse part, got %+v", lastContent.Parts)
	}
	if lastContent.Parts[0].FunctionResponse.Name != "get_weather" {
		t.Errorf("expected function response name get_weather, got %s", lastContent.Parts[0].FunctionResponse.Name)
	}
}

// TestAnthropicToolSchemaVariants verifies tool declaration mapping for Gemini and Claude models under legacy/standard modes.
func TestAnthropicToolSchemaVariants(t *testing.T) {
	req := &AnthropicRequest{
		Model: "gemini-2.5-flash",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "Run tool\nx-anthropic-billing-header: cc_version=2.1;\nnow"},
		},
		Tools: []AnthropicTool{
			{
				Name:        "calc",
				Description: "Calculator",
				InputSchema: map[string]any{
					"type":       "object",
					"properties": map[string]any{"expr": map[string]any{"type": "string"}},
				},
			},
		},
		MaxTokens: 512,
	}

	optsStandard := BuildOptions{
		ProjectID: "proj-1",
		Info: catalog.CatalogModel{
			ID: "gemini-2.5-flash",
		},
		LegacySchemas: false,
	}

	envStd, err := BuildAnthropicEnvelope(req, optsStandard)
	if err != nil {
		t.Fatalf("BuildAnthropicEnvelope standard failed: %v", err)
	}

	// Verify user message billing header was stripped
	userText := envStd.Request.Contents[0].Parts[0].Text
	if strings.Contains(userText, "x-anthropic-billing-header") {
		t.Errorf("expected billing header stripped from user message, got: %s", userText)
	}

	toolMapStd := envStd.Request.Tools[0].(map[string]any)
	declsStd := toolMapStd["functionDeclarations"].([]map[string]any)
	if _, ok := declsStd[0]["parametersJsonSchema"]; !ok {
		t.Errorf("expected parametersJsonSchema for standard Gemini tool, got: %v", declsStd[0])
	}

	// Legacy schemas mode
	optsLegacy := BuildOptions{
		ProjectID: "proj-1",
		Info: catalog.CatalogModel{
			ID: "gemini-2.5-flash",
		},
		LegacySchemas: true,
	}

	envLegacy, err := BuildAnthropicEnvelope(req, optsLegacy)
	if err != nil {
		t.Fatalf("BuildAnthropicEnvelope legacy failed: %v", err)
	}
	toolMapLegacy := envLegacy.Request.Tools[0].(map[string]any)
	declsLegacy := toolMapLegacy["functionDeclarations"].([]map[string]any)
	if _, ok := declsLegacy[0]["parameters"]; !ok {
		t.Errorf("expected parameters for legacy Gemini tool, got: %v", declsLegacy[0])
	}
}

// TestConvertGoogleToAnthropicResponse verifies parsing non-streaming Google response to AnthropicResponse.
func TestConvertGoogleToAnthropicResponse(t *testing.T) {
	googleResp := map[string]any{
		"response": map[string]any{
			"candidates": []any{
				map[string]any{
					"finishReason": "STOP",
					"content": map[string]any{
						"parts": []any{
							map[string]any{
								"thought":          true,
								"text":             "Let me think about this.",
								"thoughtSignature": "sig_abc",
							},
							map[string]any{
								"text": "Here is your answer.",
							},
						},
					},
				},
			},
			"usageMetadata": map[string]any{
				"promptTokenCount":        float64(50),
				"candidatesTokenCount":    float64(30),
				"cachedContentTokenCount": float64(10),
			},
		},
	}

	resp, err := ConvertGoogleToAnthropicResponse(googleResp, "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("ConvertGoogleToAnthropicResponse failed: %v", err)
	}

	if resp.Model != "claude-sonnet-4-6" {
		t.Errorf("expected model claude-sonnet-4-6, got %s", resp.Model)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("expected stop_reason end_turn, got %s", resp.StopReason)
	}
	if len(resp.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(resp.Content))
	}
	if resp.Content[0].Type != "thinking" || resp.Content[0].Thinking != "Let me think about this." {
		t.Errorf("block 0 mismatch: %+v", resp.Content[0])
	}
	if resp.Content[1].Type != "text" || resp.Content[1].Text != "Here is your answer." {
		t.Errorf("block 1 mismatch: %+v", resp.Content[1])
	}
	if resp.Usage.InputTokens != 40 || resp.Usage.OutputTokens != 30 || resp.Usage.CacheReadInputTokens != 10 {
		t.Errorf("usage mismatch: %+v", resp.Usage)
	}
}

// TestAnthropicStreamTranslator verifies real-time SSE event generation in Anthropic wire format.
func TestAnthropicStreamTranslator(t *testing.T) {
	var events []string
	translator := NewAnthropicStreamTranslator("claude-sonnet-4-6", func(eventType string, data any) {
		b, err := FormatAnthropicSSEEvent(eventType, data)
		if err == nil {
			events = append(events, string(b))
		}
	})

	// Push first chunk with thinking
	translator.Push(map[string]any{
		"response": map[string]any{
			"candidates": []any{
				map[string]any{
					"content": map[string]any{
						"parts": []any{
							map[string]any{
								"thought":          true,
								"text":             "Reasoning step 1. ",
								"thoughtSignature": "sig_part1",
							},
						},
					},
				},
			},
			"usageMetadata": map[string]any{
				"promptTokenCount": float64(15),
			},
		},
	})

	// Push second chunk with visible text
	translator.Push(map[string]any{
		"response": map[string]any{
			"candidates": []any{
				map[string]any{
					"finishReason": "STOP",
					"content": map[string]any{
						"parts": []any{
							map[string]any{
								"text": "Final response.",
							},
						},
					},
				},
			},
			"usageMetadata": map[string]any{
				"candidatesTokenCount": float64(10),
			},
		},
	})

	translator.Finish()

	if !translator.SawOutput() {
		t.Fatal("expected SawOutput true, got false")
	}

	joined := strings.Join(events, "")
	if !strings.Contains(joined, "event: message_start") {
		t.Error("missing message_start event")
	}
	if !strings.Contains(joined, "event: content_block_start") {
		t.Error("missing content_block_start event")
	}
	if !strings.Contains(joined, "event: content_block_delta") {
		t.Error("missing content_block_delta event")
	}
	if !strings.Contains(joined, "event: message_delta") {
		t.Error("missing message_delta event")
	}
	if !strings.Contains(joined, "event: message_stop") {
		t.Error("missing message_stop event")
	}
	if !strings.Contains(joined, "signature_delta") {
		t.Error("missing signature_delta event")
	}
}

// TestAnthropicThinkingDisabledTakesPrecedence verifies that an explicit {"thinking": {"type": "disabled"}}
// payload overrides model default thinking capabilities and omits thinkingConfig.
func TestAnthropicThinkingDisabledTakesPrecedence(t *testing.T) {
	info := catalog.CatalogModel{
		ID:              "claude-sonnet-4-6-thinking",
		Thinking:        true,
		MaxOutputTokens: 8192,
	}

	req := &AnthropicRequest{
		Model: "claude-sonnet-4-6-thinking",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "Hello world"},
		},
		Thinking: &AnthropicThinking{
			Type: "disabled",
		},
	}

	env, err := BuildAnthropicEnvelope(req, BuildOptions{
		Info:      info,
		ProjectID: "test-project",
	})
	if err != nil {
		t.Fatalf("BuildAnthropicEnvelope failed: %v", err)
	}

	genConfig := env.Request.GenerationConfig
	if _, exists := genConfig["thinkingConfig"]; exists {
		t.Errorf("expected thinkingConfig to be omitted when thinking is disabled, but it exists: %v", genConfig["thinkingConfig"])
	}
}

// TestAnthropicForcesToolDisablesClaudeThinking verifies that when a tool is forced via tool_choice,
// thinking is suppressed for Claude models to prevent upstream HTTP 400 rejections.
func TestAnthropicForcesToolDisablesClaudeThinking(t *testing.T) {
	info := catalog.CatalogModel{
		ID:              "claude-sonnet-4-6",
		Thinking:        true,
		MaxOutputTokens: 8192,
	}

	req := &AnthropicRequest{
		Model: "claude-sonnet-4-6",
		Messages: []AnthropicMessage{
			{Role: "user", Content: "Run calculation"},
		},
		Tools: []AnthropicTool{
			{Name: "calculator", Description: "Calculates numbers"},
		},
		ToolChoice: map[string]any{
			"type": "tool",
			"name": "calculator",
		},
	}

	env, err := BuildAnthropicEnvelope(req, BuildOptions{
		Info:      info,
		ProjectID: "test-project",
	})
	if err != nil {
		t.Fatalf("BuildAnthropicEnvelope failed: %v", err)
	}

	genConfig := env.Request.GenerationConfig
	if _, exists := genConfig["thinkingConfig"]; exists {
		t.Errorf("expected thinkingConfig suppressed when tool is forced, but it was set: %v", genConfig["thinkingConfig"])
	}
}

// TestAnthropicStreamTranslatorFinishEmitsMessageStartWhenEmpty verifies that an empty stream
// that terminates immediately emits message_start before message_delta and message_stop.
func TestAnthropicStreamTranslatorFinishEmitsMessageStartWhenEmpty(t *testing.T) {
	var eventTypes []string
	translator := NewAnthropicStreamTranslator("claude-sonnet-4-6", func(eventType string, data any) {
		eventTypes = append(eventTypes, eventType)
	})

	translator.Finish()

	if len(eventTypes) < 3 {
		t.Fatalf("expected at least 3 events (message_start, message_delta, message_stop), got %d: %v", len(eventTypes), eventTypes)
	}
	if eventTypes[0] != "message_start" {
		t.Errorf("expected first event to be message_start, got %s", eventTypes[0])
	}
	if eventTypes[1] != "message_delta" {
		t.Errorf("expected second event to be message_delta, got %s", eventTypes[1])
	}
	if eventTypes[2] != "message_stop" {
		t.Errorf("expected third event to be message_stop, got %s", eventTypes[2])
	}
}

// TestAnthropicAdaptiveThinkingEnablesThinking verifies that the modern
// {"thinking":{"type":"adaptive"}} payload enables reasoning instead of silently disabling it
// for legacy Claude generations that still use a numeric thinking budget.
func TestAnthropicAdaptiveThinkingEnablesThinking(t *testing.T) {
	info := catalog.FamilyDefaults("claude-sonnet-4-6")
	req := &AnthropicRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []AnthropicMessage{{Role: "user", Content: "Hello world"}},
		Thinking: &AnthropicThinking{Type: "adaptive"},
	}

	env, err := BuildAnthropicEnvelope(req, BuildOptions{Info: info, ProjectID: "test-project"})
	if err != nil {
		t.Fatalf("BuildAnthropicEnvelope failed: %v", err)
	}

	tc, ok := env.Request.GenerationConfig["thinkingConfig"].(map[string]any)
	if !ok {
		t.Fatalf("expected thinkingConfig for adaptive thinking, got %v", env.Request.GenerationConfig["thinkingConfig"])
	}
	if tc["include_thoughts"] != true {
		t.Errorf("expected include_thoughts true, got %v", tc["include_thoughts"])
	}
	if _, ok := tc["thinking_budget"]; !ok {
		t.Errorf("expected legacy Claude thinking_budget, got %v", tc)
	}
}

// TestAnthropicClaudeFiveThinkingLevel verifies Claude 5+ requests emit the categorical
// thinkingLevel enum with includeThoughts and omit the legacy numeric thinking_budget field.
func TestAnthropicClaudeFiveThinkingLevel(t *testing.T) {
	info := catalog.FamilyDefaults("claude-sonnet-5-5")

	tests := []struct {
		name     string
		effort   string
		expected string
	}{
		{name: "max effort", effort: "max", expected: "MAX"},
		{name: "xhigh effort", effort: "xhigh", expected: "XHIGH"},
		{name: "medium effort", effort: "medium", expected: "MEDIUM"},
		{name: "default effort", effort: "", expected: "HIGH"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &AnthropicRequest{
				Model:    "claude-sonnet-5-5",
				Messages: []AnthropicMessage{{Role: "user", Content: "Plan the refactor"}},
				Thinking: &AnthropicThinking{Type: "adaptive"},
			}
			if tt.effort != "" {
				req.OutputConfig = &AnthropicOutputConfig{Effort: tt.effort}
			}

			env, err := BuildAnthropicEnvelope(req, BuildOptions{Info: info, ProjectID: "test-project"})
			if err != nil {
				t.Fatalf("BuildAnthropicEnvelope failed: %v", err)
			}

			tc, ok := env.Request.GenerationConfig["thinkingConfig"].(map[string]any)
			if !ok {
				t.Fatalf("expected thinkingConfig, got %v", env.Request.GenerationConfig["thinkingConfig"])
			}
			if tc["includeThoughts"] != true {
				t.Errorf("expected includeThoughts true, got %v", tc["includeThoughts"])
			}
			if tc["thinkingLevel"] != tt.expected {
				t.Errorf("thinkingLevel = %v; want %q", tc["thinkingLevel"], tt.expected)
			}
			if _, exists := tc["thinking_budget"]; exists {
				t.Errorf("expected no legacy thinking_budget for Claude 5, got %v", tc)
			}
		})
	}
}

// TestAnthropicSystemScrubAndInlineFallback verifies system instruction scrubbing and the
// prompt-rejection fallback that moves system text into the first user turn.
func TestAnthropicSystemScrubAndInlineFallback(t *testing.T) {
	info := catalog.FamilyDefaults("gemini-2.5-flash")
	system := "You are Claude Code, Anthropic's official CLI for Claude.\nKeep responses short."
	req := &AnthropicRequest{
		Model:    "gemini-2.5-flash",
		System:   system,
		Messages: []AnthropicMessage{{Role: "user", Content: "Hello"}},
	}

	env, err := BuildAnthropicEnvelope(req, BuildOptions{Info: info, ProjectID: "test-project"})
	if err != nil {
		t.Fatalf("BuildAnthropicEnvelope failed: %v", err)
	}
	if env.Request.SystemInstruction == nil {
		t.Fatal("expected systemInstruction for the standard build")
	}
	sysText := env.Request.SystemInstruction.Parts[0].Text
	if strings.Contains(sysText, "Claude Code") || strings.Contains(sysText, "Anthropic") {
		t.Errorf("expected third-party agent identity scrubbed, got %q", sysText)
	}
	if !strings.Contains(sysText, "You are a coding assistant.") || !strings.Contains(sysText, "Keep responses short.") {
		t.Errorf("expected neutral identity plus preserved instructions, got %q", sysText)
	}

	env, err = BuildAnthropicEnvelope(req, BuildOptions{Info: info, ProjectID: "test-project", InlineSystem: true})
	if err != nil {
		t.Fatalf("BuildAnthropicEnvelope inline failed: %v", err)
	}
	if env.Request.SystemInstruction != nil {
		t.Errorf("expected no systemInstruction on inline fallback, got %+v", env.Request.SystemInstruction)
	}
	if len(env.Request.Contents) == 0 || len(env.Request.Contents[0].Parts) < 2 {
		t.Fatalf("expected system text prepended to the first user turn, got %+v", env.Request.Contents)
	}
	firstPart := env.Request.Contents[0].Parts[0].Text
	if !strings.Contains(firstPart, "Keep responses short.") {
		t.Errorf("expected system text in the first user part, got %q", firstPart)
	}
	if lastText := env.Request.Contents[0].Parts[len(env.Request.Contents[0].Parts)-1].Text; lastText != "Hello" {
		t.Errorf("expected original user text preserved last, got %q", lastText)
	}
}

// collectAnthropicEvents builds an Anthropic stream translator that records every emitted SSE event
// string, mirroring how the gateway serializes events onto the wire.
func collectAnthropicEvents(t *testing.T, model string) (*AnthropicStreamTranslator, *[]string) {
	t.Helper()
	var events []string
	translator := NewAnthropicStreamTranslator(model, func(eventType string, data any) {
		b, err := FormatAnthropicSSEEvent(eventType, data)
		if err != nil {
			t.Errorf("formatting anthropic sse event: %v", err)
			return
		}
		events = append(events, string(b))
	})
	return translator, &events
}

// TestAnthropicStreamUsageNotDoubleCounted verifies repeated cumulative usageMetadata frames never
// inflate output_tokens with duplicated thinking or candidate token counts.
func TestAnthropicStreamUsageNotDoubleCounted(t *testing.T) {
	translator, events := collectAnthropicEvents(t, "claude-sonnet-4-6")

	translator.Push(map[string]any{
		"response": map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{
					"thought": true, "text": "thinking...", "thoughtSignature": "sig_1",
				}}},
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount":     float64(20),
				"candidatesTokenCount": float64(2),
				"thoughtsTokenCount":   float64(30),
			},
		},
	})
	// A second frame repeats the same cumulative thinking count and omits candidatesTokenCount.
	translator.Push(map[string]any{
		"response": map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "answer"}}},
				"finishReason": "STOP",
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount":   float64(20),
				"thoughtsTokenCount": float64(30),
			},
		},
	})
	translator.Finish()

	resp := translator.Response()
	if resp.Usage.OutputTokens != 32 {
		t.Errorf("expected output_tokens 32 (2 candidates + 30 thinking), got %d", resp.Usage.OutputTokens)
	}

	joined := strings.Join(*events, "")
	if !strings.Contains(joined, `"output_tokens":32`) {
		t.Errorf("expected final message_delta usage to report 32 output tokens, got:\n%s", joined)
	}
}

// TestAnthropicToolResultImagesBecomeInlineData verifies images embedded inside an Anthropic
// tool_result block are forwarded as inlineData parts instead of being dropped.
func TestAnthropicToolResultImagesBecomeInlineData(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 1024,
		Messages: []AnthropicMessage{
			{Role: "user", Content: "screenshot please"},
			{Role: "assistant", Content: []any{
				map[string]any{
					"type": "tool_use", "id": "toolu_1", "name": "capture", "input": map[string]any{},
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content": []any{
						map[string]any{"type": "text", "text": "captured"},
						map[string]any{
							"type": "image",
							"source": map[string]any{
								"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo=",
							},
						},
					},
				},
			}},
		},
	}

	geminiReq, err := BuildAnthropicRequest(req, BuildOptions{Info: catalog.CatalogModel{ID: "claude-sonnet-4-6", MaxOutputTokens: 64000}})
	if err != nil {
		t.Fatalf("BuildAnthropicRequest failed: %v", err)
	}

	var sawInline, sawResponse bool
	for _, content := range geminiReq.Contents {
		for _, part := range content.Parts {
			if part.InlineData != nil && part.InlineData.MimeType == "image/png" && part.InlineData.Data == "iVBORw0KGgo=" {
				sawInline = true
			}
			if part.FunctionResponse != nil {
				sawResponse = true
			}
		}
	}
	if !sawResponse {
		t.Error("expected tool_result to produce a functionResponse part")
	}
	if !sawInline {
		t.Error("expected tool_result image to produce an inlineData part")
	}
}

// TestAnthropicStreamPreservesEmptyTextSignature verifies a signature attached to an empty text part
// is captured instead of being dropped before the signature field is read.
func TestAnthropicStreamPreservesEmptyTextSignature(t *testing.T) {
	translator, events := collectAnthropicEvents(t, "claude-sonnet-4-6")

	translator.Push(map[string]any{
		"response": map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{
					map[string]any{"thought": true, "text": "reasoning"},
					map[string]any{"text": "", "thoughtSignature": "sig_empty_text"},
				}},
				"finishReason": "STOP",
			}},
		},
	})
	translator.Finish()

	resp := translator.Response()
	if len(resp.Content) == 0 || resp.Content[0].Type != "thinking" {
		t.Fatalf("expected a leading thinking block, got %+v", resp.Content)
	}
	if resp.Content[0].Signature != "sig_empty_text" {
		t.Errorf("expected signature captured from empty text part, got %q", resp.Content[0].Signature)
	}
	if joined := strings.Join(*events, ""); !strings.Contains(joined, "sig_empty_text") {
		t.Errorf("expected signature_delta carrying the captured signature, got:\n%s", joined)
	}
}

// TestAnthropicStreamDistinctThinkingSignatures verifies consecutive thinking parts with different
// signatures open separate blocks instead of merging and overwriting the earlier signature.
func TestAnthropicStreamDistinctThinkingSignatures(t *testing.T) {
	translator, _ := collectAnthropicEvents(t, "claude-sonnet-4-6")

	translator.Push(map[string]any{
		"response": map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{
					map[string]any{"thought": true, "text": "first", "thoughtSignature": "sig_first"},
					map[string]any{"thought": true, "text": "second", "thoughtSignature": "sig_second"},
				}},
				"finishReason": "STOP",
			}},
		},
	})
	translator.Finish()

	resp := translator.Response()
	if len(resp.Content) != 2 {
		t.Fatalf("expected two distinct thinking blocks, got %d: %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Signature != "sig_first" || resp.Content[1].Signature != "sig_second" {
		t.Errorf("expected distinct signatures preserved, got %q and %q", resp.Content[0].Signature, resp.Content[1].Signature)
	}
}

// TestAnthropicStopReasonMapping verifies safety, tool, and length finish reasons map to the
// Anthropic stop_reason vocabulary instead of defaulting to end_turn.
func TestAnthropicStopReasonMapping(t *testing.T) {
	cases := []struct {
		finishReason string
		want         string
	}{
		{"STOP", "end_turn"},
		{"MAX_TOKENS", "max_tokens"},
		{"TOOL_USE", "tool_use"},
		{"SAFETY", "refusal"},
		{"RECITATION", "refusal"},
		{"BLOCKLIST", "refusal"},
		{"PROHIBITED_CONTENT", "refusal"},
		{"SPII", "refusal"},
	}

	for _, tc := range cases {
		googleResp := map[string]any{
			"response": map[string]any{
				"candidates": []any{map[string]any{
					"finishReason": tc.finishReason,
					"content":      map[string]any{"parts": []any{map[string]any{"text": "partial"}}},
				}},
			},
		}
		resp, err := ConvertGoogleToAnthropicResponse(googleResp, "claude-sonnet-4-6")
		if err != nil {
			t.Fatalf("ConvertGoogleToAnthropicResponse(%s) failed: %v", tc.finishReason, err)
		}
		if resp.StopReason != tc.want {
			t.Errorf("finishReason %s: expected stop_reason %q, got %q", tc.finishReason, tc.want, resp.StopReason)
		}
	}

	// Streaming must also surface TOOL_USE rather than folding it into end_turn.
	translator, _ := collectAnthropicEvents(t, "claude-sonnet-4-6")
	translator.Push(map[string]any{
		"response": map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"parts": []any{map[string]any{"text": "need a tool"}}},
				"finishReason": "TOOL_USE",
			}},
		},
	})
	translator.Finish()
	if stop := translator.Response().StopReason; stop != "tool_use" {
		t.Errorf("expected streaming stop_reason tool_use, got %q", stop)
	}
}

// TestAnthropicToolNameSanitization verifies function declarations and replayed tool turns share the
// same [a-zA-Z_] prefixed name normalization, and that empty call IDs are never replaced with "call".
func TestAnthropicToolNameSanitization(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 512,
		Tools: []AnthropicTool{
			{Name: "9lives.tool-name", InputSchema: map[string]any{"type": "object"}},
		},
		Messages: []AnthropicMessage{
			{Role: "user", Content: "run it"},
			{Role: "assistant", Content: []any{
				map[string]any{
					"type": "tool_use", "id": "toolu_9", "name": "9lives.tool-name", "input": map[string]any{},
				},
			}},
			{Role: "user", Content: []any{
				map[string]any{
					"type": "tool_result", "tool_use_id": "toolu_9", "content": "ok",
				},
			}},
		},
	}

	geminiReq, err := BuildAnthropicRequest(req, BuildOptions{Info: catalog.CatalogModel{ID: "claude-sonnet-4-6", MaxOutputTokens: 64000}})
	if err != nil {
		t.Fatalf("BuildAnthropicRequest failed: %v", err)
	}

	decls, ok := geminiReq.Tools[0].(map[string]any)["functionDeclarations"].([]map[string]any)
	if !ok || len(decls) != 1 {
		t.Fatalf("expected one function declaration, got %#v", geminiReq.Tools)
	}
	declName, _ := decls[0]["name"].(string)
	if declName != "_9lives_tool_name" {
		t.Errorf("expected sanitized declaration name _9lives_tool_name, got %q", declName)
	}

	var callName, responseName string
	for _, content := range geminiReq.Contents {
		for _, part := range content.Parts {
			if part.FunctionCall != nil {
				callName = part.FunctionCall.Name
			}
			if part.FunctionResponse != nil {
				responseName = part.FunctionResponse.Name
			}
		}
	}
	if callName != declName || responseName != declName {
		t.Errorf("expected declaration/call/response names to match (%q), got call %q and response %q", declName, callName, responseName)
	}

	if got := CleanToolCallID(""); got != "" {
		t.Errorf("expected empty tool call ID to stay empty, got %q", got)
	}
}

// TestAnthropicStreamContinuationToken verifies long-decode continuation tokens are captured from the
// upstream candidate and can be cleared before the next hop.
func TestAnthropicStreamContinuationToken(t *testing.T) {
	translator, _ := collectAnthropicEvents(t, "claude-sonnet-4-6")

	translator.Push(map[string]any{
		"response": map[string]any{
			"candidates": []any{map[string]any{
				"content":           map[string]any{"parts": []any{map[string]any{"text": "partial"}}},
				"finishReason":      "CONTINUATION",
				"continuationToken": "token-abc",
			}},
		},
	})

	if token := translator.ContinuationToken(); token != "token-abc" {
		t.Fatalf("expected continuation token captured, got %q", token)
	}
	// A CONTINUATION hop must not be treated as a terminal stop reason.
	translator.Finish()
	if stop := translator.Response().StopReason; stop == "continuation" {
		t.Errorf("expected CONTINUATION to be excluded from terminal stop reasons, got %q", stop)
	}

	translator.ClearContinuationToken()
	if token := translator.ContinuationToken(); token != "" {
		t.Errorf("expected cleared continuation token, got %q", token)
	}
}

// TestBuildAnthropicRequestClampsMaxTokens verifies an explicit max_tokens above the catalog output
// ceiling is clamped to the ceiling the upstream model actually accepts.
func TestBuildAnthropicRequestClampsMaxTokens(t *testing.T) {
	req := &AnthropicRequest{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 500000,
		Messages:  []AnthropicMessage{{Role: "user", Content: "hi"}},
	}

	geminiReq, err := BuildAnthropicRequest(req, BuildOptions{Info: catalog.CatalogModel{ID: "claude-sonnet-4-6", MaxOutputTokens: 64000}})
	if err != nil {
		t.Fatalf("BuildAnthropicRequest failed: %v", err)
	}

	if got, _ := geminiReq.GenerationConfig["maxOutputTokens"].(int); got != 64000 {
		t.Errorf("expected maxOutputTokens clamped to 64000, got %v", geminiReq.GenerationConfig["maxOutputTokens"])
	}
}

// TestBuildAnthropicRequestGemini2SkipSignature verifies that replayed Gemini 2.x tool turns receive
// the skip signature sentinel, authentic client signatures are preserved, and non-Gemini families are
// never given the sentinel.
func TestBuildAnthropicRequestGemini2SkipSignature(t *testing.T) {
	buildToolTurn := func(model string, toolUse map[string]any) Part {
		t.Helper()
		req := &AnthropicRequest{
			Model:     model,
			MaxTokens: 256,
			Messages: []AnthropicMessage{
				{Role: "user", Content: "call the tool"},
				{Role: "assistant", Content: []any{toolUse}},
				{Role: "user", Content: []any{
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
				}},
			},
		}

		geminiReq, err := BuildAnthropicRequest(req, BuildOptions{Info: catalog.CatalogModel{ID: model, MaxOutputTokens: 64000}})
		if err != nil {
			t.Fatalf("BuildAnthropicRequest(%s) failed: %v", model, err)
		}

		for _, content := range geminiReq.Contents {
			for _, part := range content.Parts {
				if part.FunctionCall != nil {
					return part
				}
			}
		}
		t.Fatalf("BuildAnthropicRequest(%s) produced no function call part", model)
		return Part{}
	}

	t.Run("unsigned gemini 2.x replay gets the sentinel", func(t *testing.T) {
		part := buildToolTurn("gemini-2.5-flash", map[string]any{
			"type": "tool_use", "id": "toolu_1", "name": "capture", "input": map[string]any{},
		})
		if part.ThoughtSignature != SkipSignature {
			t.Errorf("expected skip signature for gemini-2.5-flash, got %q", part.ThoughtSignature)
		}
	})

	t.Run("authentic signature is preserved", func(t *testing.T) {
		part := buildToolTurn("gemini-2.5-flash", map[string]any{
			"type": "tool_use", "id": "toolu_1", "name": "capture", "input": map[string]any{},
			"thoughtSignature": "sig_authentic",
		})
		if part.ThoughtSignature != "sig_authentic" {
			t.Errorf("expected authentic signature preserved, got %q", part.ThoughtSignature)
		}
	})

	t.Run("legacy signature field is preserved", func(t *testing.T) {
		part := buildToolTurn("gemini-2.5-flash", map[string]any{
			"type": "tool_use", "id": "toolu_1", "name": "capture", "input": map[string]any{},
			"signature": "sig_legacy",
		})
		if part.ThoughtSignature != "sig_legacy" {
			t.Errorf("expected legacy signature preserved, got %q", part.ThoughtSignature)
		}
	})

	t.Run("claude replay stays unsigned", func(t *testing.T) {
		part := buildToolTurn("claude-sonnet-4-6", map[string]any{
			"type": "tool_use", "id": "toolu_1", "name": "capture", "input": map[string]any{},
		})
		if part.ThoughtSignature != "" {
			t.Errorf("expected no sentinel for claude-sonnet-4-6, got %q", part.ThoughtSignature)
		}
	})
}
