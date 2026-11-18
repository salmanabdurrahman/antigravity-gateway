// Package translate provides bidirectional translation between OpenAI Chat Completions
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

var blockedReasons = []string{
	"SAFETY",
	"RECITATION",
	"BLOCKLIST",
	"PROHIBITED_CONTENT",
	"SPII",
	"IMAGE_SAFETY",
}

// MapFinishReason maps Google Cloud Code Assist finishReason strings to OpenAI finish_reason values.
// Unknown reasons pass through lowercased so clients can inspect or log them.
func MapFinishReason(reason string, hasToolCalls bool) string {
	if hasToolCalls && (reason == "" || reason == "STOP") {
		return "tool_calls"
	}
	if reason == "" || reason == "STOP" {
		return "stop"
	}
	if reason == "MAX_TOKENS" {
		return "length"
	}
	if slices.Contains(blockedReasons, reason) {
		return "content_filter"
	}
	return strings.ToLower(reason)
}

// TokenUsageDetails provides granular token breakdown for caching and thinking steps.
type TokenUsageDetails struct {
	// CachedTokens reports prompt tokens served directly from context cache.
	CachedTokens int `json:"cached_tokens,omitzero"`

	// ReasoningTokens reports generated chain-of-thought tokens.
	ReasoningTokens int `json:"reasoning_tokens,omitzero"`
}

// Usage captures token counts compatible with OpenAI ChatCompletion.
type Usage struct {
	// PromptTokens is the total input token count.
	PromptTokens int `json:"prompt_tokens"`

	// CompletionTokens is the total output generation token count.
	CompletionTokens int `json:"completion_tokens"`

	// TotalTokens is the combined input and output token count.
	TotalTokens int `json:"total_tokens"`

	// PromptTokensDetails provides granular input token categorization.
	PromptTokensDetails *TokenUsageDetails `json:"prompt_tokens_details,omitzero"`

	// CompletionTokensDetails provides granular output token categorization.
	CompletionTokensDetails *TokenUsageDetails `json:"completion_tokens_details,omitzero"`
}

// OpenAIToolCallFunction specifies the called function name and JSON arguments string.
type OpenAIToolCallFunction struct {
	// Name is the target function name.
	Name string `json:"name"`

	// Arguments is the JSON serialized argument string.
	Arguments string `json:"arguments"`
}

// OpenAIToolCall represents a tool call structure in OpenAI completions choices.
type OpenAIToolCall struct {
	// ID uniquely identifies this tool call.
	ID string `json:"id"`

	// Type is always "function".
	Type string `json:"type"`

	// Function encapsulates the function name and arguments.
	Function OpenAIToolCallFunction `json:"function"`
}

// DeltaToolCall represents an incremental tool call delta in an SSE stream chunk,
// explicitly preserving the zero-index tool call position.
type DeltaToolCall struct {
	// Index identifies the positional slot of the tool call in choices[0].tool_calls.
	Index int `json:"index"`

	// ID is the unique tool call identifier.
	ID string `json:"id,omitempty"`

	// Type specifies the tool type, typically "function".
	Type string `json:"type,omitempty"`

	// Function holds the incremental function name and argument stream chunks.
	Function OpenAIToolCallFunction `json:"function"`
}

// Delta holds incremental streaming response fields for an SSE chunk.
type Delta struct {
	// Role specifies author identity, emitted on the opening chunk.
	Role string `json:"role,omitempty"`

	// Content contains streamed generation text chunks.
	Content string `json:"content,omitempty"`

	// ReasoningContent contains streamed thinking tokens.
	ReasoningContent string `json:"reasoning_content,omitempty"`

	// ReasoningDetails contains serialized thought signatures emitted upon completion.
	ReasoningDetails []ReasoningDetail `json:"reasoning_details,omitempty"`

	// ToolCalls contains streamed tool call invocations.
	ToolCalls []DeltaToolCall `json:"tool_calls,omitempty"`
}

// ChunkChoice represents a single choice in a streaming ChatCompletionChunk.
type ChunkChoice struct {
	// Index is the zero-based index of this completion choice.
	Index int `json:"index"`

	// Delta holds the incremental payload chunk.
	Delta Delta `json:"delta"`

	// FinishReason signals completion status (e.g. "stop", "tool_calls", "length").
	FinishReason any `json:"finish_reason"`
}

// ChatCompletionChunk represents an OpenAI SSE chunk payload.
type ChatCompletionChunk struct {
	// ID is the unique response identifier.
	ID string `json:"id"`

	// Object is always "chat.completion.chunk".
	Object string `json:"object"`

	// Created is the Unix timestamp in seconds when the chunk was produced.
	Created int64 `json:"created"`

	// Model is the model identifier serving the response.
	Model string `json:"model"`

	// Choices holds the streaming choice deltas.
	Choices []ChunkChoice `json:"choices"`

	// Usage optionally reports final token accounting.
	Usage *Usage `json:"usage,omitzero"`
}

// ChatCompletionMessage represents the full message content in a non-streaming ChatCompletion response.
type ChatCompletionMessage struct {
	// Role is always "assistant".
	Role string `json:"role"`

	// Content contains generated text, or nil if only tool calls were returned.
	Content any `json:"content"`

	// ReasoningContent contains full generated chain-of-thought text.
	ReasoningContent string `json:"reasoning_content,omitempty"`

	// ReasoningDetails holds serialized thought signatures and turn layouts for replay.
	ReasoningDetails []ReasoningDetail `json:"reasoning_details,omitempty"`

	// ToolCalls lists all functions invoked by the model.
	ToolCalls []OpenAIToolCall `json:"tool_calls,omitempty"`
}

// CompletionChoice represents a completion choice in a non-streaming ChatCompletion response.
type CompletionChoice struct {
	// Index is the zero-based index of this completion choice.
	Index int `json:"index"`

	// Message contains the complete assistant response.
	Message ChatCompletionMessage `json:"message"`

	// FinishReason indicates why the model stopped ("stop", "tool_calls", "length", "content_filter").
	FinishReason string `json:"finish_reason"`
}

// ChatCompletion represents an OpenAI non-streaming completion response.
type ChatCompletion struct {
	// ID is the unique completion identifier.
	ID string `json:"id"`

	// Object is always "chat.completion".
	Object string `json:"object"`

	// Created is the Unix timestamp in seconds when the completion was generated.
	Created int64 `json:"created"`

	// Model is the model identifier that generated the response.
	Model string `json:"model"`

	// Choices lists the completion results.
	Choices []CompletionChoice `json:"choices"`

	// Usage details the token accounting for the request and response.
	Usage *Usage `json:"usage,omitzero"`
}

// ResponseTranslator processes incoming Cloud Code Assist SSE events and emits OpenAI deltas.
type ResponseTranslator struct {
	// model is the active model identifier.
	model string

	// recipe records thoughts and tool calls for multi-turn signature replay.
	recipe *RecipeBuilder

	// text accumulates visible generation text.
	text strings.Builder

	// reasoning accumulates thinking tokens.
	reasoning strings.Builder

	// toolCalls accumulates tool call objects.
	toolCalls []OpenAIToolCall

	// finishReason tracks upstream stop reasons.
	finishReason string

	// usage records token consumption.
	usage *Usage

	// responseID is the unique completion ID.
	responseID string

	// sawOutput tracks whether any content was emitted.
	sawOutput bool

	// onDelta receives incremental translated deltas.
	onDelta func(Delta)
}

// NewResponseTranslator instantiates a translator bound to a model and an optional delta callback.
func NewResponseTranslator(model string, onDelta func(Delta)) *ResponseTranslator {
	if onDelta == nil {
		onDelta = func(Delta) {}
	}
	return &ResponseTranslator{
		model:   model,
		recipe:  NewRecipeBuilder(),
		onDelta: onDelta,
	}
}

// Push ingests an unmarshaled Google Cloud Code Assist event map and translates contents into deltas.
func (t *ResponseTranslator) Push(event map[string]any) {
	if event == nil {
		return
	}

	response := event
	if respObj, ok := event["response"].(map[string]any); ok {
		response = respObj
	}

	if respID, ok := response["responseId"].(string); ok && t.responseID == "" {
		t.responseID = respID
	}

	if candidates, ok := response["candidates"].([]any); ok && len(candidates) > 0 {
		if cand, ok := candidates[0].(map[string]any); ok {
			if content, ok := cand["content"].(map[string]any); ok {
				if parts, ok := content["parts"].([]any); ok {
					for _, p := range parts {
						if pMap, ok := p.(map[string]any); ok {
							t.processPart(pMap)
						}
					}
				}
			}
			if fReason, ok := cand["finishReason"].(string); ok && fReason != "" {
				t.finishReason = fReason
				t.sawOutput = true
			}
		}
	}

	if uMeta, ok := response["usageMetadata"].(map[string]any); ok {
		t.usage = parseUsage(uMeta)
	}
}

// processPart translates a single upstream candidate part (text, thought, or functionCall) into deltas and recipe steps.
func (t *ResponseTranslator) processPart(part map[string]any) {
	sig, _ := part["thoughtSignature"].(string)

	if fc, ok := part["functionCall"].(map[string]any); ok {
		t.sawOutput = true
		givenID, _ := fc["id"].(string)
		if givenID != "" {
			givenID = CleanToolCallID(givenID)
		}

		callID := givenID
		hasSameID := false
		for _, c := range t.toolCalls {
			if c.ID == callID {
				hasSameID = true
				break
			}
		}
		if callID == "" || hasSameID {
			callID = generateCallID()
		}

		name, _ := fc["name"].(string)
		var argsStr string
		if argsMap, ok := fc["args"].(map[string]any); ok {
			b, _ := json.Marshal(argsMap)
			argsStr = string(b)
		} else {
			argsStr = "{}"
		}

		call := OpenAIToolCall{
			ID:   callID,
			Type: "function",
			Function: OpenAIToolCallFunction{
				Name:      name,
				Arguments: argsStr,
			},
		}

		deltaCall := DeltaToolCall{
			Index: len(t.toolCalls),
			ID:    callID,
			Type:  "function",
			Function: OpenAIToolCallFunction{
				Name:      name,
				Arguments: argsStr,
			},
		}

		t.toolCalls = append(t.toolCalls, call)
		t.recipe.Call(callID, sig)
		t.onDelta(Delta{ToolCalls: []DeltaToolCall{deltaCall}})
		return
	}

	txt, hasText := part["text"].(string)
	if !hasText {
		if sig != "" {
			t.recipe.Text(sig)
		}
		return
	}

	t.sawOutput = true
	isThought, _ := part["thought"].(bool)
	if isThought {
		t.recipe.Thought(txt, sig)
		if txt != "" {
			t.reasoning.WriteString(txt)
			t.onDelta(Delta{ReasoningContent: txt})
		}
		return
	}

	if txt != "" || sig != "" {
		t.recipe.Text(sig)
	}
	if txt != "" {
		t.text.WriteString(txt)
		t.onDelta(Delta{Content: txt})
	}
}

// ReasoningDetails constructs an encrypted reasoning detail item if signatures or thoughts were captured.
func (t *ResponseTranslator) ReasoningDetails() []ReasoningDetail {
	if !t.recipe.Useful() {
		return nil
	}
	return []ReasoningDetail{RecipeDetail(Recipe{
		Model: t.model,
		Steps: t.recipe.Steps(),
	})}
}

// Finish calculates the final OpenAI finish_reason string.
func (t *ResponseTranslator) Finish() string {
	return MapFinishReason(t.finishReason, len(t.toolCalls) > 0)
}

// Text returns all accumulated output text.
func (t *ResponseTranslator) Text() string {
	return t.text.String()
}

// Reasoning returns all accumulated reasoning text.
func (t *ResponseTranslator) Reasoning() string {
	return t.reasoning.String()
}

// ToolCalls returns all accumulated tool calls.
func (t *ResponseTranslator) ToolCalls() []OpenAIToolCall {
	return slices.Clone(t.toolCalls)
}

// Usage returns the parsed token usage metrics if provided by upstream.
func (t *ResponseTranslator) Usage() *Usage {
	return t.usage
}

// SawOutput reports whether any model text, thought, or tool call parts were encountered.
func (t *ResponseTranslator) SawOutput() bool {
	return t.sawOutput
}

// Completion returns a full OpenAI ChatCompletion response representation for non-streaming requests.
func (t *ResponseTranslator) Completion(id string, created int64) *ChatCompletion {
	details := t.ReasoningDetails()

	var contentVal any
	txt := t.text.String()
	if txt != "" {
		contentVal = txt
	}

	msg := ChatCompletionMessage{
		Role:             "assistant",
		Content:          contentVal,
		ReasoningContent: t.reasoning.String(),
		ReasoningDetails: details,
		ToolCalls:        t.toolCalls,
	}

	return &ChatCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   t.model,
		Choices: []CompletionChoice{
			{
				Index:        0,
				Message:      msg,
				FinishReason: t.Finish(),
			},
		},
		Usage: t.usage,
	}
}

// parseUsage extracts token counts and details from Google Cloud Code usageMetadata.
func parseUsage(u map[string]any) *Usage {
	getInt := func(k string) int {
		if v, ok := u[k].(float64); ok {
			return int(v)
		}
		if v, ok := u[k].(int); ok {
			return v
		}
		return 0
	}

	prompt := getInt("promptTokenCount")
	thoughts := getInt("thoughtsTokenCount")
	candidates := getInt("candidatesTokenCount")
	completion := candidates + thoughts
	total := getInt("totalTokenCount")
	if total == 0 {
		total = prompt + completion
	}

	usage := &Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
	}

	if cached := getInt("cachedContentTokenCount"); cached > 0 {
		usage.PromptTokensDetails = &TokenUsageDetails{CachedTokens: cached}
	}
	if thoughts > 0 {
		usage.CompletionTokensDetails = &TokenUsageDetails{ReasoningTokens: thoughts}
	}

	return usage
}

// generateCallID generates a random unique tool call identifier.
func generateCallID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "call_" + base64.RawURLEncoding.EncodeToString(b)
}

var sseBufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 64*1024)
		return &b
	},
}

// ReadSSEEvents parses an SSE stream into individual JSON objects matching `data: {...}` lines.
// It skips SSE comments, ignores [DONE] markers, and handles multi-line data chunks.
// It configures an expanded buffer (up to 10 MB) to accommodate large thinking blocks and code diffs.
func ReadSSEEvents(r io.Reader, handle func(event map[string]any) error) error {
	scanner := bufio.NewScanner(r)
	const maxScanCapacity = 10 * 1024 * 1024 // 10 MB
	bufPtr := sseBufferPool.Get().(*[]byte)
	defer sseBufferPool.Put(bufPtr)
	scanner.Buffer(*bufPtr, maxScanCapacity)
	var dataLines []string
	var totalDataBytes int

	flush := func() error {
		if len(dataLines) == 0 {
			totalDataBytes = 0
			return nil
		}
		var payload string
		if len(dataLines) == 1 {
			payload = strings.TrimSpace(dataLines[0])
		} else {
			payload = strings.TrimSpace(strings.Join(dataLines, "\n"))
		}
		dataLines = nil
		totalDataBytes = 0
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			return nil // Skip malformed events
		}
		return handle(m)
	}

	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimRight(line, "\r")

		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}

		if strings.HasPrefix(line, ":") {
			continue // SSE comment
		}

		if strings.HasPrefix(line, "data:") {
			dataContent := strings.TrimPrefix(line, "data:")
			dataContent = strings.TrimPrefix(dataContent, " ")
			totalDataBytes += len(dataContent)
			if totalDataBytes > maxScanCapacity {
				return fmt.Errorf("sse event payload exceeds maximum capacity of %d bytes", maxScanCapacity)
			}
			dataLines = append(dataLines, dataContent)
		}
	}

	if err := flush(); err != nil {
		return err
	}
	return scanner.Err()
}

// FormatSSEChunk marshals a ChatCompletionChunk into standard SSE data format with trailing newlines.
func FormatSSEChunk(chunk *ChatCompletionChunk) ([]byte, error) {
	b, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString("data: ")
	buf.Write(b)
	buf.WriteString("\n\n")
	return buf.Bytes(), nil
}

// FormatSSEDone formats the final termination signal for SSE streams.
func FormatSSEDone() []byte {
	return []byte("data: [DONE]\n\n")
}
