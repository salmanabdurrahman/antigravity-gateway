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
