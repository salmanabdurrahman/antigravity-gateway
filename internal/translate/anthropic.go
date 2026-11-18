// Package translate provides bidirectional translation between OpenAI/Anthropic
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// AnthropicRequest represents an incoming request in the Anthropic Messages API format.
type AnthropicRequest struct {
	// Model specifies the requested model identifier (e.g. "claude-sonnet-4-6").
	Model string `json:"model"`

	// Messages contains the full conversational context in Anthropic format.
	Messages []AnthropicMessage `json:"messages"`

	// System optionally specifies system instructions (string or array of text blocks).
	System any `json:"system,omitempty"`

	// MaxTokens defines the upper limit on tokens to generate.
	MaxTokens int `json:"max_tokens"`

	// Temperature controls sampling randomness.
	Temperature *float64 `json:"temperature,omitempty"`

	// TopP controls nucleus sampling.
	TopP *float64 `json:"top_p,omitempty"`

	// TopK controls top-k sampling.
	TopK *int `json:"top_k,omitempty"`

	// StopSequences specifies custom strings where generation stops.
	StopSequences []string `json:"stop_sequences,omitempty"`

	// Stream toggles SSE streaming delivery vs non-streaming response.
	Stream bool `json:"stream,omitzero"`

	// Tools defines function tools available for the model to invoke.
	Tools []AnthropicTool `json:"tools,omitempty"`

	// ToolChoice configures how the model selects tools.
	ToolChoice any `json:"tool_choice,omitempty"`

	// Thinking configures extended reasoning parameters for thinking models.
	Thinking *AnthropicThinking `json:"thinking,omitempty"`

	// Metadata holds arbitrary client metadata such as user tracking IDs.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// AnthropicMessage represents a single message in Anthropic's conversation history.
type AnthropicMessage struct {
	// Role identifies the author ("user" or "assistant").
	Role string `json:"role"`

	// Content contains string text or structured multimodal blocks.
	Content any `json:"content"`
}

// AnthropicTool defines a tool in the Anthropic format.
type AnthropicTool struct {
	// Name is the unique identifier for the function.
	Name string `json:"name"`

	// Description explains the tool purpose and behavior.
	Description string `json:"description,omitempty"`

	// InputSchema specifies the JSON Schema describing acceptable parameters.
	InputSchema any `json:"input_schema"`
}

// AnthropicThinking configures thinking tokens and reasoning modes.
type AnthropicThinking struct {
	// Type specifies thinking mode ("enabled" or "disabled").
	Type string `json:"type"`

	// BudgetTokens specifies the maximum reasoning tokens allocated.
	BudgetTokens int `json:"budget_tokens,omitempty"`
}

// AnthropicContentBlock represents a single typed block in an Anthropic message response.
type AnthropicContentBlock struct {
	// Type specifies block kind ("text", "thinking", "tool_use", or "image").
	Type string `json:"type"`

	// Text contains visible generation text when Type is "text".
	Text string `json:"text,omitempty"`

	// Thinking contains chain-of-thought reasoning text when Type is "thinking".
	Thinking string `json:"thinking,omitempty"`

	// Signature holds the cryptographic signature associated with thinking blocks.
	Signature string `json:"signature,omitempty"`

	// ID uniquely identifies a tool invocation when Type is "tool_use".
	ID string `json:"id,omitempty"`

	// Name is the tool name invoked when Type is "tool_use".
	Name string `json:"name,omitempty"`

	// Input holds the deserialized JSON arguments for tool invocations.
	Input any `json:"input,omitempty"`
}

// AnthropicUsage reports token consumption metrics in Anthropic format.
type AnthropicUsage struct {
	// InputTokens is the count of prompt tokens processed.
	InputTokens int `json:"input_tokens"`

	// OutputTokens is the count of generated output tokens, including thinking tokens.
	OutputTokens int `json:"output_tokens"`

	// CacheReadInputTokens indicates tokens retrieved from prompt cache.
	CacheReadInputTokens int `json:"cache_read_input_tokens"`

	// CacheCreationInputTokens indicates tokens written to prompt cache.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// AnthropicResponse represents a complete non-streaming Anthropic Messages response envelope.
type AnthropicResponse struct {
	// ID is the unique message identifier (e.g. "msg_...").
	ID string `json:"id"`

	// Type is always "message".
	Type string `json:"type"`

	// Role is always "assistant".
	Role string `json:"role"`

	// Content lists the generated response content blocks.
	Content []AnthropicContentBlock `json:"content"`

	// Model indicates the model that generated this response.
	Model string `json:"model"`

	// StopReason indicates why generation concluded ("end_turn", "tool_use", "max_tokens").
	StopReason string `json:"stop_reason"`

	// StopSequence records which stop sequence triggered completion, if any.
	StopSequence *string `json:"stop_sequence"`

	// Usage summarizes prompt and output token consumption.
	Usage AnthropicUsage `json:"usage"`
}

// ValidateAnthropicRequest verifies that essential fields are present in an Anthropic request.
func ValidateAnthropicRequest(req *AnthropicRequest) error {
	if req == nil {
		return errors.New("request cannot be nil")
	}
	if strings.TrimSpace(req.Model) == "" {
		return errors.New("model field is required")
	}
	if len(req.Messages) == 0 {
		return errors.New("messages field cannot be empty")
	}
	for i, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return fmt.Errorf("message[%d] has invalid role %q: expected 'user' or 'assistant'", i, m.Role)
		}
	}
	return nil
}

// BuildAnthropicEnvelope converts an AnthropicRequest into an AgentEnvelope ready for Google Cloud Code.
func BuildAnthropicEnvelope(req *AnthropicRequest, opts BuildOptions) (*AgentEnvelope, error) {
	geminiReq, err := BuildAnthropicRequest(req, opts)
	if err != nil {
		return nil, err
	}

	upstreamModelID := catalog.UpstreamModel(req.Model)

	uuidBytes := make([]byte, 16)
	// crypto/rand.Read in Go 1.24+ never returns an error; error is safely ignorable.
	_, _ = rand.Read(uuidBytes)
	uuidBytes[6] = (uuidBytes[6] & 0x0f) | 0x40 // RFC 4122 version 4
	uuidBytes[8] = (uuidBytes[8] & 0x3f) | 0x80 // RFC 4122 variant
	requestID := fmt.Sprintf("agent-%08x-%04x-%04x-%04x-%012x",
		uuidBytes[0:4], uuidBytes[4:6], uuidBytes[6:8], uuidBytes[8:10], uuidBytes[10:16])

	return &AgentEnvelope{
		Project:     opts.ProjectID,
		Model:       upstreamModelID,
		Request:     geminiReq,
		RequestType: "agent",
		UserAgent:   "antigravity",
		RequestID:   requestID,
	}, nil
}

// BuildAnthropicRequest converts an AnthropicRequest into a GeminiRequest.
func BuildAnthropicRequest(req *AnthropicRequest, opts BuildOptions) (*GeminiRequest, error) {
	if err := ValidateAnthropicRequest(req); err != nil {
		return nil, err
	}

	model := req.Model
	info := opts.Info
	claude := config.IsClaude(model)
	withID := NeedsToolCallID(model)

	var systemTexts []string
	if req.System != nil {
		extractAnthropicSystem(req.System, &systemTexts)
	}

	// Protocol Quirk: Inject interleaved thinking hint for Claude thinking models when tools are present
	if claude && info.Thinking && len(req.Tools) > 0 {
		const interleavedHint = "Interleaved thinking is enabled. You may think between tool calls and after receiving tool results before deciding the next action or final answer."
		systemTexts = append(systemTexts, interleavedHint)
	}

	callNames := make(map[string]string)
	var contents []Content

	for _, msg := range req.Messages {
		cleanedContent := CleanCacheControl(msg.Content)
		switch msg.Role {
		case "assistant":
			parts := buildAnthropicAssistantParts(cleanedContent, callNames, model)
			if len(parts) == 0 {
				parts = []Part{{Text: "."}}
			}
			pushContent(&contents, "model", parts)
		case "user":
			parts := buildAnthropicUserParts(cleanedContent, callNames, withID)
			if len(parts) == 0 {
				parts = []Part{{Text: "."}}
			}
			pushContent(&contents, "user", parts)
		}
	}

	if len(contents) == 0 || contents[0].Role != "user" {
		contents = append([]Content{{Role: "user", Parts: []Part{{Text: "Hello."}}}}, contents...)
	}

	contents = InjectInterruptedToolResponses(contents)

	genConfig := make(map[string]any)
	if req.Temperature != nil {
		genConfig["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		genConfig["topP"] = *req.TopP
	}
	if req.TopK != nil {
		genConfig["topK"] = *req.TopK
	}
	if len(req.StopSequences) > 0 {
		genConfig["stopSequences"] = req.StopSequences
	}

	maxOut := req.MaxTokens
	if maxOut <= 0 {
		maxOut = info.MaxOutputTokens
		if maxOut <= 0 {
			maxOut = 8192
		}
	}

	// Thinking configuration
	thinkingRequested := false
	if req.Thinking != nil {
		if req.Thinking.Type == "enabled" {
			thinkingRequested = true
		}
	} else if info.Thinking {
		thinkingRequested = true
	}

	if claude && (forcesAnthropicTool(req.ToolChoice) || claudeLoopLacksSignedThinking(contents)) {
		thinkingRequested = false
	}
	if thinkingRequested {
		budget := 0
		if req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
			budget = req.Thinking.BudgetTokens
		} else if info.ThinkingBudget != nil {
			budget = *info.ThinkingBudget
		}

		clampedBudget, adjMaxOut := ClampThinkingBudget(model, budget, maxOut, claude)
		maxOut = adjMaxOut

		if claude {
			genConfig["thinkingConfig"] = map[string]any{
				"include_thoughts": true,
				"thinking_budget":  clampedBudget,
			}
		} else {
			genConfig["thinkingConfig"] = map[string]any{
				"includeThoughts": true,
				"thinkingBudget":  clampedBudget,
			}
		}
	}

	genConfig["maxOutputTokens"] = maxOut

	// Tools configuration
	var tools []any
	var toolConfig map[string]any
	if len(req.Tools) > 0 {
		fdecls := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			name := CleanToolCallID(t.Name)
			if name == "" {
				continue
			}
			decl := map[string]any{"name": name}
			if t.Description != "" {
				decl["description"] = t.Description
			}
			if claude {
				if t.InputSchema != nil {
					decl["parameters"] = ForClaudeParameters(t.InputSchema)
				} else {
					decl["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
				}
			} else if opts.LegacySchemas {
				if t.InputSchema != nil {
					decl["parameters"] = ForGeminiLegacyParameters(t.InputSchema)
				}
			} else {
				if t.InputSchema != nil {
					decl["parametersJsonSchema"] = ForGeminiJSONSchema(t.InputSchema)
				}
			}
			fdecls = append(fdecls, decl)
		}
		tools = []any{map[string]any{"functionDeclarations": fdecls}}
		toolConfig = buildAnthropicToolConfig(req.ToolChoice, claude)
	}

	var sysInstruction *SystemInstruction
	if len(systemTexts) > 0 {
		parts := make([]Part, len(systemTexts))
		for i, txt := range systemTexts {
			parts[i] = Part{Text: txt}
		}
		sysInstruction = &SystemInstruction{Parts: parts}
	}

	return &GeminiRequest{
		Contents:          contents,
		SystemInstruction: sysInstruction,
		GenerationConfig:  genConfig,
		Tools:             tools,
		ToolConfig:        toolConfig,
		SessionID:         opts.SessionID,
	}, nil
}

// extractAnthropicSystem collects plain text from string or structured system blocks.
func extractAnthropicSystem(sys any, out *[]string) {
	switch v := sys.(type) {
	case string:
		stripped := StripBillingHeader(v)
		if stripped != "" {
			*out = append(*out, stripped)
		}
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if txt, ok := m["text"].(string); ok {
					stripped := StripBillingHeader(txt)
					if stripped != "" {
						*out = append(*out, stripped)
					}
				}
			}
		}
	}
}

// buildAnthropicAssistantParts converts assistant messages into Google Cloud Code parts.
func buildAnthropicAssistantParts(content any, callNames map[string]string, model string) []Part {
	switch v := content.(type) {
	case string:
		if v != "" {
			return []Part{{Text: v}}
		}
		return nil
	case []any:
		var parts []Part
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := m["type"].(string)
			switch blockType {
			case "thinking":
				text, _ := m["thinking"].(string)
				sig, _ := m["signature"].(string)
				parts = append(parts, Part{
					Thought:          true,
					Text:             text,
					ThoughtSignature: sig,
				})
			case "text":
				if txt, ok := m["text"].(string); ok && txt != "" {
					parts = append(parts, Part{Text: txt})
				}
			case "tool_use":
				id, _ := m["id"].(string)
				name, _ := m["name"].(string)
				input, _ := m["input"].(map[string]any)
				if input == nil {
					input = make(map[string]any)
				}
				cleanID := CleanToolCallID(id)
				if id != "" && name != "" {
					callNames[cleanID] = name
					callNames[id] = name
				}
				fc := &FunctionCallPart{
					ID:   cleanID,
					Name: name,
					Args: input,
				}
				part := Part{FunctionCall: fc}
				// Protocol Quirk: If thoughtSignature is present, preserve it; otherwise supply skip signature
				if sig, ok := m["thoughtSignature"].(string); ok && sig != "" {
					part.ThoughtSignature = sig
				} else if sig, ok := m["signature"].(string); ok && sig != "" {
					part.ThoughtSignature = sig
				} else if major := GeminiMajor(model); major != nil && *major >= 3 {
					part.ThoughtSignature = SkipSignature
				}
				parts = append(parts, part)
			}
		}
		return parts
	default:
		return nil
	}
}

// buildAnthropicUserParts converts user message content (including tool results) into Google parts.
func buildAnthropicUserParts(content any, callNames map[string]string, withID bool) []Part {
	switch v := content.(type) {
	case string:
		stripped := StripBillingHeader(v)
		if stripped != "" {
			return []Part{{Text: stripped}}
		}
		return nil
	case []any:
		var parts []Part
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := m["type"].(string)
			switch blockType {
			case "text":
				if txt, ok := m["text"].(string); ok {
					stripped := StripBillingHeader(txt)
					if stripped != "" {
						parts = append(parts, Part{Text: stripped})
					}
				}
			case "image":
				if src, ok := m["source"].(map[string]any); ok {
					mediaType, _ := src["media_type"].(string)
					data, _ := src["data"].(string)
					if mediaType != "" && data != "" {
						parts = append(parts, Part{
							InlineData: &InlineData{
								MimeType: mediaType,
								Data:     data,
							},
						})
					}
				}
			case "tool_result":
				toolUseID, _ := m["tool_use_id"].(string)
				resContent := m["content"]
				resText := extractText(resContent)
				cleanID := CleanToolCallID(toolUseID)
				name := callNames[cleanID]
				if name == "" {
					name = callNames[toolUseID]
				}
				if name == "" {
					name = "tool"
				}
				fnResp := &FunctionResponse{
					Name:     name,
					Response: map[string]any{"output": resText},
				}
				if withID && cleanID != "" {
					fnResp.ID = cleanID
				}
				parts = append(parts, Part{FunctionResponse: fnResp})
			}
		}
		return parts
	default:
		return nil
	}
}

// ConvertGoogleToAnthropicResponse converts a Google Cloud Code response into an AnthropicResponse.
func ConvertGoogleToAnthropicResponse(googleResp map[string]any, model string) (*AnthropicResponse, error) {
	inner := googleResp
	if respObj, ok := googleResp["response"].(map[string]any); ok {
		inner = respObj
	}

	candidates, _ := inner["candidates"].([]any)
	if len(candidates) == 0 {
		return nil, errors.New("no candidates returned from upstream")
	}

	firstCand, _ := candidates[0].(map[string]any)
	contentObj, _ := firstCand["content"].(map[string]any)
	partsList, _ := contentObj["parts"].([]any)

	var blocks []AnthropicContentBlock
	hasToolCalls := false

	for _, p := range partsList {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}

		if thought, ok := part["thought"].(bool); ok && thought {
			txt, _ := part["text"].(string)
			sig, _ := part["thoughtSignature"].(string)
			blocks = append(blocks, AnthropicContentBlock{
				Type:      "thinking",
				Thinking:  txt,
				Signature: sig,
			})
		} else if txt, ok := part["text"].(string); ok {
			blocks = append(blocks, AnthropicContentBlock{
				Type: "text",
				Text: txt,
			})
		} else if fc, ok := part["functionCall"].(map[string]any); ok {
			name, _ := fc["name"].(string)
			id, _ := fc["id"].(string)
			if id == "" {
				randBytes := make([]byte, 12)
				// crypto/rand.Read in Go 1.24+ never returns an error; error is safely ignorable.
				_, _ = rand.Read(randBytes)
				id = "toolu_" + hex.EncodeToString(randBytes)
			}
			args := fc["args"]
			if args == nil {
				args = make(map[string]any)
			}
			blocks = append(blocks, AnthropicContentBlock{
				Type:  "tool_use",
				ID:    id,
				Name:  name,
				Input: args,
			})
			hasToolCalls = true
		}
	}

	stopReason := "end_turn"
	if finishReason, ok := firstCand["finishReason"].(string); ok {
		switch finishReason {
		case "MAX_TOKENS":
			stopReason = "max_tokens"
		case "TOOL_USE":
			stopReason = "tool_use"
		case "STOP":
			stopReason = "end_turn"
		}
	}
	if hasToolCalls {
		stopReason = "tool_use"
	}

	usageMeta, _ := inner["usageMetadata"].(map[string]any)
	promptTokens := 0
	candidatesTokens := 0
	cachedTokens := 0
	thoughtsTokens := 0
	if usageMeta != nil {
		if v, ok := usageMeta["promptTokenCount"].(float64); ok {
			promptTokens = int(v)
		}
		if v, ok := usageMeta["candidatesTokenCount"].(float64); ok {
			candidatesTokens = int(v)
		}
		if v, ok := usageMeta["cachedContentTokenCount"].(float64); ok {
			cachedTokens = int(v)
		}
		if v, ok := usageMeta["thoughtsTokenCount"].(float64); ok {
			thoughtsTokens = int(v)
		}
	}

	randMsg := make([]byte, 16)
	// crypto/rand.Read in Go 1.24+ never returns an error; error is safely ignorable.
	_, _ = rand.Read(randMsg)
	msgID := "msg_" + hex.EncodeToString(randMsg)

	if len(blocks) == 0 {
		blocks = []AnthropicContentBlock{{Type: "text", Text: ""}}
	}

	return &AnthropicResponse{
		ID:         msgID,
		Type:       "message",
		Role:       "assistant",
		Content:    blocks,
		Model:      model,
		StopReason: stopReason,
		Usage: AnthropicUsage{
			InputTokens:          max(0, promptTokens-cachedTokens),
			OutputTokens:         candidatesTokens + thoughtsTokens,
			CacheReadInputTokens: cachedTokens,
		},
	}, nil
}

// AnthropicStreamTranslator manages translating Google SSE stream events into Anthropic SSE events.
type AnthropicStreamTranslator struct {
	// model records the active model identifier.
	model string

	// messageID is the unique identifier emitted in message_start.
	messageID string

	// hasEmittedStart tracks whether message_start has been sent.
	hasEmittedStart bool

	// blockIndex tracks the sequential content block index.
	blockIndex int

	// currentBlockType tracks active open block kind ("thinking", "text", "tool_use").
	currentBlockType string

	// currentSignature caches thoughtSignature for delta emission.
	currentSignature string

	// stopReason tracks the completion stop reason.
	stopReason string

	// sawOutput tracks whether any visible content or tool call was encountered.
	sawOutput bool

	// accumulatedBlocks collects complete content blocks for non-streaming response assembly.
	accumulatedBlocks []AnthropicContentBlock

	// promptTokens accumulates total prompt token counts.
	promptTokens int

	// outputTokens accumulates total generated output token counts.
	outputTokens int

	// cachedTokens accumulates cache read token counts.
	cachedTokens int

	// emit callback delivers formatted Anthropic SSE event bytes.
	emit func(eventType string, data any)
}

// NewAnthropicStreamTranslator initializes an AnthropicStreamTranslator.
func NewAnthropicStreamTranslator(model string, emit func(eventType string, data any)) *AnthropicStreamTranslator {
	randMsg := make([]byte, 16)
	// crypto/rand.Read in Go 1.24+ never returns an error; error is safely ignorable.
	_, _ = rand.Read(randMsg)
	return &AnthropicStreamTranslator{
		model:     model,
		messageID: "msg_" + hex.EncodeToString(randMsg),
		emit:      emit,
	}
}

// SawOutput reports whether output tokens or tools were generated.
func (t *AnthropicStreamTranslator) SawOutput() bool {
	return t.sawOutput
}

// Push processes a raw Google Cloud Code SSE event map.
func (t *AnthropicStreamTranslator) Push(event map[string]any) {
	inner := event
	if respObj, ok := event["response"].(map[string]any); ok {
		inner = respObj
	}

	if usage, ok := inner["usageMetadata"].(map[string]any); ok {
		if v, ok := usage["promptTokenCount"].(float64); ok {
			t.promptTokens = int(v)
		}
		if v, ok := usage["candidatesTokenCount"].(float64); ok {
			t.outputTokens = int(v)
		}
		if v, ok := usage["thoughtsTokenCount"].(float64); ok {
			t.outputTokens += int(v)
		}
		if v, ok := usage["cachedContentTokenCount"].(float64); ok {
			t.cachedTokens = int(v)
		}
	}

	candidates, _ := inner["candidates"].([]any)
	if len(candidates) == 0 {
		return
	}

	firstCand, _ := candidates[0].(map[string]any)
	contentObj, _ := firstCand["content"].(map[string]any)
	partsList, _ := contentObj["parts"].([]any)

	if !t.hasEmittedStart && len(partsList) > 0 {
		t.hasEmittedStart = true
		if t.emit != nil {
			t.emit("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":            t.messageID,
					"type":          "message",
					"role":          "assistant",
					"content":       []any{},
					"model":         t.model,
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage": map[string]any{
						"input_tokens":                max(0, t.promptTokens-t.cachedTokens),
						"output_tokens":               0,
						"cache_read_input_tokens":     t.cachedTokens,
						"cache_creation_input_tokens": 0,
					},
				},
			})
		}
	}

	for _, p := range partsList {
		part, ok := p.(map[string]any)
		if !ok {
			continue
		}

		if thought, ok := part["thought"].(bool); ok && thought {
			t.sawOutput = true
			txt, _ := part["text"].(string)
			sig, _ := part["thoughtSignature"].(string)

			if t.currentBlockType != "thinking" {
				t.closeCurrentBlock()
				t.currentBlockType = "thinking"
				t.currentSignature = ""
				t.accumulatedBlocks = append(t.accumulatedBlocks, AnthropicContentBlock{
					Type: "thinking",
				})
				if t.emit != nil {
					t.emit("content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": t.blockIndex,
						"content_block": map[string]any{
							"type":     "thinking",
							"thinking": "",
						},
					})
				}
			}

			if sig != "" {
				t.currentSignature = sig
				if len(t.accumulatedBlocks) > 0 {
					lastIdx := len(t.accumulatedBlocks) - 1
					t.accumulatedBlocks[lastIdx].Signature = sig
				}
			}

			if txt != "" {
				if len(t.accumulatedBlocks) > 0 {
					lastIdx := len(t.accumulatedBlocks) - 1
					t.accumulatedBlocks[lastIdx].Thinking += txt
				}
				if t.emit != nil {
					t.emit("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": t.blockIndex,
						"delta": map[string]any{
							"type":     "thinking_delta",
							"thinking": txt,
						},
					})
				}
			}
		} else if txt, ok := part["text"].(string); ok {
			if txt == "" {
				continue
			}
			t.sawOutput = true

			if t.currentBlockType != "text" {
				t.closeCurrentBlock()
				t.currentBlockType = "text"
				t.accumulatedBlocks = append(t.accumulatedBlocks, AnthropicContentBlock{
					Type: "text",
				})
				if t.emit != nil {
					t.emit("content_block_start", map[string]any{
						"type":  "content_block_start",
						"index": t.blockIndex,
						"content_block": map[string]any{
							"type": "text",
							"text": "",
						},
					})
				}
			}

			if len(t.accumulatedBlocks) > 0 {
				lastIdx := len(t.accumulatedBlocks) - 1
				t.accumulatedBlocks[lastIdx].Text += txt
			}

			if t.emit != nil {
				t.emit("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": t.blockIndex,
					"delta": map[string]any{
						"type": "text_delta",
						"text": txt,
					},
				})
			}
		} else if fc, ok := part["functionCall"].(map[string]any); ok {
			t.sawOutput = true
			t.closeCurrentBlock()
			t.currentBlockType = "tool_use"
			t.stopReason = "tool_use"

			name, _ := fc["name"].(string)
			id, _ := fc["id"].(string)
			if id == "" {
				randBytes := make([]byte, 12)
				// crypto/rand.Read in Go 1.24+ never returns an error; error is safely ignorable.
				_, _ = rand.Read(randBytes)
				id = "toolu_" + hex.EncodeToString(randBytes)
			}
			args := fc["args"]
			if args == nil {
				args = make(map[string]any)
			}

			t.accumulatedBlocks = append(t.accumulatedBlocks, AnthropicContentBlock{
				Type:  "tool_use",
				ID:    id,
				Name:  name,
				Input: args,
			})

			if t.emit != nil {
				t.emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": t.blockIndex,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    id,
						"name":  name,
						"input": map[string]any{},
					},
				})

				argBytes, err := json.Marshal(args)
				if err != nil {
					// Fallback to empty object if argument serialization fails
					argBytes = []byte("{}")
				}
				t.emit("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": t.blockIndex,
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": string(argBytes),
					},
				})
			}
		}
	}

	if finishReason, ok := firstCand["finishReason"].(string); ok && t.stopReason == "" {
		switch finishReason {
		case "MAX_TOKENS":
			t.stopReason = "max_tokens"
		case "STOP":
			t.stopReason = "end_turn"
		}
	}
}

// Response constructs an AnthropicResponse from accumulated stream parts.
func (t *AnthropicStreamTranslator) Response() *AnthropicResponse {
	blocks := t.accumulatedBlocks
	if len(blocks) == 0 {
		blocks = []AnthropicContentBlock{{Type: "text", Text: ""}}
	}
	stopReason := t.stopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}
	return &AnthropicResponse{
		ID:         t.messageID,
		Type:       "message",
		Role:       "assistant",
		Content:    blocks,
		Model:      t.model,
		StopReason: stopReason,
		Usage: AnthropicUsage{
			InputTokens:          max(0, t.promptTokens-t.cachedTokens),
			OutputTokens:         t.outputTokens,
			CacheReadInputTokens: t.cachedTokens,
		},
	}
}

// closeCurrentBlock finishes any currently active content block.
func (t *AnthropicStreamTranslator) closeCurrentBlock() {
	if t.currentBlockType == "" {
		return
	}
	if t.currentBlockType == "thinking" && t.currentSignature != "" {
		if t.emit != nil {
			t.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": t.blockIndex,
				"delta": map[string]any{
					"type":      "signature_delta",
					"signature": t.currentSignature,
				},
			})
		}
		t.currentSignature = ""
	}
	if t.emit != nil {
		t.emit("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": t.blockIndex,
		})
	}
	t.blockIndex++
	t.currentBlockType = ""
}

// Finish emits terminal message_delta and message_stop events.
func (t *AnthropicStreamTranslator) Finish() {
	if !t.hasEmittedStart {
		t.hasEmittedStart = true
		if t.emit != nil {
			t.emit("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":            t.messageID,
					"type":          "message",
					"role":          "assistant",
					"content":       []any{},
					"model":         t.model,
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage": map[string]any{
						"input_tokens":                max(0, t.promptTokens-t.cachedTokens),
						"output_tokens":               0,
						"cache_read_input_tokens":     t.cachedTokens,
						"cache_creation_input_tokens": 0,
					},
				},
			})
		}
	}

	t.closeCurrentBlock()

	stopReason := t.stopReason
	if stopReason == "" {
		stopReason = "end_turn"
	}

	if t.emit != nil {
		t.emit("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": map[string]any{
				"output_tokens": t.outputTokens,
			},
		})

		t.emit("message_stop", map[string]any{
			"type": "message_stop",
		})
	}
}

// FormatAnthropicSSEEvent serializes an Anthropic event into SSE line wire protocol format.
func FormatAnthropicSSEEvent(eventType string, data any) ([]byte, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("marshaling anthropic sse event: %w", err)
	}
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", eventType, b)), nil
}

// forcesAnthropicTool checks if the Anthropic tool_choice parameter requires an explicit tool execution.
func forcesAnthropicTool(choice any) bool {
	if choice == nil {
		return false
	}
	if s, ok := choice.(string); ok {
		lower := strings.ToLower(s)
		return lower == "any" || lower == "tool" || lower == "required"
	}
	if m, ok := choice.(map[string]any); ok {
		typ, _ := m["type"].(string)
		lower := strings.ToLower(typ)
		if lower == "any" || lower == "tool" {
			return true
		}
		if _, ok := m["function"].(map[string]any); ok {
			return true
		}
	}
	return false
}

// buildAnthropicToolConfig translates Anthropic tool_choice settings into Google functionCallingConfig objects.
func buildAnthropicToolConfig(choice any, claude bool) map[string]any {
	if choice == nil {
		if claude {
			return map[string]any{
				"functionCallingConfig": map[string]any{
					"mode": "VALIDATED",
				},
			}
		}
		return nil
	}

	mode := "AUTO"
	var allowedNames []string

	switch v := choice.(type) {
	case string:
		switch strings.ToLower(v) {
		case "none":
			mode = "NONE"
		case "any", "required":
			mode = "ANY"
		default:
			mode = "AUTO"
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		switch strings.ToLower(typ) {
		case "none":
			mode = "NONE"
		case "any":
			mode = "ANY"
		case "tool":
			mode = "ANY"
			if name, ok := v["name"].(string); ok && name != "" {
				allowedNames = []string{CleanToolCallID(name)}
			}
		default:
			mode = "AUTO"
		}
	}

	if claude && mode == "AUTO" {
		mode = "VALIDATED"
	}

	cfg := map[string]any{"mode": mode}
	if len(allowedNames) > 0 {
		cfg["allowedFunctionNames"] = allowedNames
	}
	return map[string]any{"functionCallingConfig": cfg}
}
