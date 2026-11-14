// Package translate_test provides end-to-end integration and translation test suites.
package translate_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/translate"
)

// TestEndToEndTranslationRoundtrip simulates a full bidirectional pipeline:
// 1. Client creates OpenAI ChatRequest with tools and system instruction.
// 2. Gateway builds Cloud Code Assist request envelope.
// 3. Mock Google endpoint responds with SSE stream (thoughts, signed parts, tool calls, usage).
// 4. Gateway translates SSE events into OpenAI chunks and final non-streaming response.
// 5. Subsequent multi-turn request replays thought signature and tool responses.
func TestEndToEndTranslationRoundtrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// 1. Initial Request from Client
	req1 := &translate.ChatRequest{
		Model: "gemini-3.8-flash-high",
		Messages: []translate.ChatMessage{
			{Role: "system", Content: "You are an expert coding assistant."},
			{Role: "user", Content: "List directory contents."},
		},
		Tools: []any{
			map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "list_files",
					"description": "Lists repository files",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"path": map[string]any{"type": "string"},
						},
						"required": []any{"path"},
					},
				},
			},
		},
	}

	modelInfo := catalog.FamilyDefaults(req1.Model)
	env1, err := translate.BuildEnvelope(req1, translate.BuildOptions{
		Info:      modelInfo,
		ProjectID: "companion-proj-xyz",
	})
	if err != nil {
		t.Fatalf("BuildEnvelope failed: %v", err)
	}

	if env1.Project != "companion-proj-xyz" {
		t.Errorf("expected project companion-proj-xyz, got: %s", env1.Project)
	}
	if env1.Model != "gemini-3.8-flash-high" {
		t.Errorf("expected model gemini-3.8-flash-high, got: %s", env1.Model)
	}

	// 2. Upstream SSE Stream Generation
	mockSSEResponse := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"Thinking about directory listing...","thought":true}]}}]}}

data: {"response":{"candidates":[{"content":{"parts":[{"text":"Let me invoke the tool.","thought":true,"thoughtSignature":"SIG-12345"}]}}]}}

data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"list_files","args":{"path":"."},"id":"call_dir_1"},"thoughtSignature":"SIG-CALL-999"}]}}]}}

data: {"response":{"candidates":[{"content":{"parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":25,"cachedContentTokenCount":0,"candidatesTokenCount":12,"thoughtsTokenCount":15,"totalTokenCount":52}}}

data: [DONE]
`

	var receivedDeltas []translate.Delta
	translator := translate.NewResponseTranslator(req1.Model, func(d translate.Delta) {
		receivedDeltas = append(receivedDeltas, d)
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, mockSSEResponse)
	}))
	defer server.Close()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext failed: %v", err)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		t.Fatalf("mock request failed: %v", err)
	}
	defer resp.Body.Close()

	err = translate.ReadSSEEvents(resp.Body, func(event map[string]any) error {
		translator.Push(event)
		return nil
	})
	if err != nil {
		t.Fatalf("ReadSSEEvents failed: %v", err)
	}

	// Verify streaming aggregation
	if translator.Finish() != "tool_calls" {
		t.Errorf("expected finish reason 'tool_calls', got: %s", translator.Finish())
	}
	if len(translator.ToolCalls()) != 1 {
		t.Fatalf("expected 1 tool call, got: %d", len(translator.ToolCalls()))
	}
	toolCall := translator.ToolCalls()[0]
	if toolCall.Function.Name != "list_files" {
		t.Errorf("expected function name 'list_files', got: %s", toolCall.Function.Name)
	}

	// Verify delta index: 0 is explicitly marshaled in JSON
	foundZeroIndex := false
	for _, d := range receivedDeltas {
		if len(d.ToolCalls) > 0 {
			b, _ := json.Marshal(d)
			if strings.Contains(string(b), `"index":0`) {
				foundZeroIndex = true
			}
		}
	}
	if !foundZeroIndex {
		t.Errorf("expected streaming delta to serialize '\"index\":0'")
	}

	// Verify reasoning details extraction for subsequent turn
	reasoningDetails := translator.ReasoningDetails()
	if reasoningDetails == nil {
		t.Fatalf("expected reasoningDetails to be populated with recipe")
	}

	// 3. Multi-turn Tool Response Replay
	req2 := &translate.ChatRequest{
		Model: "gemini-3.8-flash-high",
		Messages: []translate.ChatMessage{
			{Role: "user", Content: "List directory contents."},
			{
				Role:             "assistant",
				Content:          nil,
				ReasoningDetails: reasoningDetails,
				ToolCalls: []translate.ChatToolCall{
					{
						ID:   toolCall.ID,
						Type: "function",
						Function: translate.ChatFunctionCall{
							Name:      toolCall.Function.Name,
							Arguments: toolCall.Function.Arguments,
						},
					},
				},
			},
			{
				Role:       "tool",
				ToolCallID: toolCall.ID,
				Name:       toolCall.Function.Name,
				Content:    `["README.md", "main.go", "TODOS.md"]`,
			},
		},
	}

	geminiReq2, err := translate.BuildRequest(req2, translate.BuildOptions{
		Info: modelInfo,
	})
	if err != nil {
		t.Fatalf("BuildRequest for turn 2 failed: %v", err)
	}

	if len(geminiReq2.Contents) != 3 {
		t.Fatalf("expected 3 contents in multi-turn replay, got: %d", len(geminiReq2.Contents))
	}

	// Verify model turn restored thought signatures in exact positions
	modelTurn := geminiReq2.Contents[1]
	if len(modelTurn.Parts) != 2 {
		t.Fatalf("expected 2 parts in model turn (thought + functionCall), got: %d", len(modelTurn.Parts))
	}

	thoughtPart := modelTurn.Parts[0]
	if !thoughtPart.Thought || thoughtPart.ThoughtSignature != "SIG-12345" {
		t.Errorf("expected restored thought with signature 'SIG-12345', got: %+v", thoughtPart)
	}

	callPart := modelTurn.Parts[1]
	if callPart.FunctionCall == nil || callPart.ThoughtSignature != "SIG-CALL-999" {
		t.Errorf("expected restored functionCall with signature 'SIG-CALL-999', got: %+v", callPart)
	}

	// Verify tool turn
	toolTurn := geminiReq2.Contents[2]
	if len(toolTurn.Parts) != 1 || toolTurn.Parts[0].FunctionResponse == nil {
		t.Fatalf("expected 1 functionResponse in tool turn, got: %+v", toolTurn)
	}
	fr := toolTurn.Parts[0].FunctionResponse
	if fr.Name != "list_files" || fr.ID != translate.CleanToolCallID(toolCall.ID) {
		t.Errorf("unexpected functionResponse values: %+v", fr)
	}
}
