// Package translate provides bidirectional translation between OpenAI Chat Completions
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// SkipSignature is the documented fallback signature injected for functionCall parts
// when an assistant turn lacks an authentic Gemini thoughtSignature.
//
// Protocol Quirk: Gemini 3+ rejects tool turns with HTTP 400 if thoughtSignature is omitted.
// Injecting "skip_thought_signature_validator" bypasses validation when replaying foreign or client-stripped turns.
const SkipSignature = "skip_thought_signature_validator"

var (
	cleanIDRegex      = regexp.MustCompile(`[^a-zA-Z0-9_-]`)
	cleanNameRegex    = regexp.MustCompile(`[^a-zA-Z0-9_]`)
	geminiMajorRegex  = regexp.MustCompile(`^gemini-(\d+)`)
	dataURLRegex      = regexp.MustCompile(`^data:([^;,]+);base64,(.+)$`)
	schemaRejectRegex = regexp.MustCompile(`(?i)parameters_?json_?schema`)
)

// CleanToolCallID normalizes tool call identifiers to match [A-Za-z0-9_-]{1,64} required by Google endpoints.
// An empty identifier yields an empty string so callers can omit the field entirely instead of
// emitting a bogus synthetic ID that would not match the paired tool response.
func CleanToolCallID(id string) string {
	cleaned := cleanIDRegex.ReplaceAllString(id, "_")
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	return cleaned
}

// CleanToolName normalizes a tool function name into the [a-zA-Z_][a-zA-Z0-9_]{0,63} shape that
// Google Cloud Code Assist accepts for function declarations.
//
// Protocol Quirk: upstream rejects function names that start with a digit or contain dots, dashes,
// or other separators commonly emitted by third-party MCP servers. The exact same normalization is
// applied to declarations and to every replayed tool call or tool response turn, otherwise the model
// is asked to invoke a name that no longer exists in the declaration list.
func CleanToolName(name string) string {
	cleaned := cleanNameRegex.ReplaceAllString(name, "_")
	if cleaned == "" {
		return ""
	}
	if first := cleaned[0]; first >= '0' && first <= '9' {
		cleaned = "_" + cleaned
	}
	if len(cleaned) > 64 {
		cleaned = cleaned[:64]
	}
	return cleaned
}

// GeminiMajor extracts the major version integer from a Gemini model identifier (e.g. "gemini-3.1" -> 3).
func GeminiMajor(model string) *int {
	lower := strings.ToLower(model)
	m := geminiMajorRegex.FindStringSubmatch(lower)
	if len(m) >= 2 {
		if v, err := strconv.Atoi(m[1]); err == nil {
			return &v
		}
	}
	return nil
}

// NeedsToolCallID determines whether an upstream model requires an explicit id property on tool calls.
func NeedsToolCallID(model string) bool {
	if config.IsClaude(model) || strings.HasPrefix(strings.ToLower(model), "gpt-oss-") {
		return true
	}
	major := GeminiMajor(model)
	return major != nil && *major >= 3
}

// ChatMessage represents a single message in an OpenAI Chat Completions request.
type ChatMessage struct {
	// Role identifies the author ("system", "user", "assistant", or "tool").
	Role string `json:"role"`

	// Content contains string text or structured multimodal parts.
	Content any `json:"content,omitempty"`

	// Name optionally identifies the author or tool function name.
	Name string `json:"name,omitempty"`

	// ToolCalls contains assistant tool invocation requests.
	ToolCalls []ChatToolCall `json:"tool_calls,omitempty"`

	// ToolCallID correlates a tool result message to its originating assistant tool call.
	ToolCallID string `json:"tool_call_id,omitempty"`

	// ReasoningDetails holds serialized thought signatures and turn layouts for replay.
	ReasoningDetails []ReasoningDetail `json:"reasoning_details,omitempty"`
}

// ChatToolCall represents a tool invocation requested by the assistant.
type ChatToolCall struct {
	// ID uniquely identifies this tool call invocation.
	ID string `json:"id"`

	// Type specifies the tool type, typically "function".
	Type string `json:"type"`

	// Function contains the function name and serialized argument payload.
	Function ChatFunctionCall `json:"function"`
}

// ChatFunctionCall describes the function name and JSON arguments string.
type ChatFunctionCall struct {
	// Name is the target function identifier.
	Name string `json:"name"`

	// Arguments is the JSON-encoded argument object string.
	Arguments string `json:"arguments"`
}

// ChatRequest represents the standard OpenAI chat completions request payload.
type ChatRequest struct {
	// Model specifies the requested model identifier.
	Model string `json:"model"`

	// Messages contains the full conversational context.
	Messages []ChatMessage `json:"messages"`

	// Stream toggles SSE streaming delivery vs non-streaming response.
	Stream bool `json:"stream,omitzero"`

	// Tools defines available function tools.
	Tools []any `json:"tools,omitempty"`

	// ToolChoice controls tool invocation constraints ("none", "auto", "required", or specific function).
	ToolChoice any `json:"tool_choice,omitempty"`

	// MaxTokens bounds maximum completion token output.
	MaxTokens *int `json:"max_tokens,omitzero"`

	// MaxCompletionTokens is the modern alias for MaxTokens.
	MaxCompletionTokens *int `json:"max_completion_tokens,omitzero"`

	// Temperature controls sampling stochasticity.
	Temperature *float64 `json:"temperature,omitzero"`

	// TopP controls nucleus sampling probability mass.
	TopP *float64 `json:"top_p,omitzero"`

	// Stop specifies sequence stops.
	Stop any `json:"stop,omitempty"`

	// ReasoningEffort guides reasoning token budget level ("low", "medium", "high").
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

// Part represents a polymorphic Google Cloud Code Assist part structure.
type Part struct {
	// Text contains visible generation or user prompt content.
	Text string `json:"text,omitempty"`

	// Thought indicates whether this part contains internal chain-of-thought reasoning.
	Thought bool `json:"thought,omitzero"`

	// ThoughtSignature stores the cryptographic signature emitted by Gemini or Claude.
	ThoughtSignature string `json:"thoughtSignature,omitempty"`

	// InlineData holds binary image data for multimodal vision prompts.
	InlineData *InlineData `json:"inlineData,omitzero"`

	// FunctionCall contains model tool execution requests.
	FunctionCall *FunctionCallPart `json:"functionCall,omitzero"`

	// FunctionResponse contains tool execution results provided back to the model.
	FunctionResponse *FunctionResponse `json:"functionResponse,omitzero"`
}

// InlineData holds base64 binary assets for multimodal vision prompts.
type InlineData struct {
	// MimeType is the media content type (e.g. "image/png", "image/jpeg").
	MimeType string `json:"mimeType"`

	// Data is the raw base64-encoded binary payload.
	Data string `json:"data"`
}

// FunctionCallPart represents a model tool execution request.
type FunctionCallPart struct {
	// Name is the target function name.
	Name string `json:"name"`

	// Args contains parsed JSON arguments passed to the function.
	Args map[string]any `json:"args"`

	// ID is the tool call identifier required by Gemini 3 and Claude models.
	ID string `json:"id,omitempty"`
}

// FunctionResponse represents a tool execution result returned to the model.
type FunctionResponse struct {
	// Name is the corresponding function name.
	Name string `json:"name"`

	// Response is the structured output map returned by the tool.
	Response map[string]any `json:"response"`

	// ID is the corresponding tool call identifier.
	ID string `json:"id,omitempty"`
}

// Content represents a turn in the Google conversation history.
type Content struct {
	// Role is either "user" or "model".
	Role string `json:"role"`

	// Parts contains the ordered chunks making up this conversational turn.
	Parts []Part `json:"parts"`
}

// SystemInstruction provides global behavioral prompting for Gemini models.
type SystemInstruction struct {
	// Parts contains the behavioral prompt text parts.
	Parts []Part `json:"parts"`
}

// GeminiRequest represents the inner payload sent to Cloud Code Assist generateContent.
type GeminiRequest struct {
	// Contents holds the alternating user and model turns.
	Contents []Content `json:"contents"`

	// SystemInstruction contains top-level system guidelines.
	SystemInstruction *SystemInstruction `json:"systemInstruction,omitzero"`

	// GenerationConfig specifies sampling parameters, token ceilings, and thinking configurations.
	GenerationConfig map[string]any `json:"generationConfig"`

	// Tools enumerates function declarations available to the model.
	Tools []any `json:"tools,omitempty"`

	// ToolConfig configures function invocation policies.
	ToolConfig map[string]any `json:"toolConfig,omitempty"`

	// SessionID is the optional session identifier for conversation continuity.
	SessionID string `json:"sessionId,omitempty"`

	// ContinuationToken resumes a long-decode generation that upstream truncated mid-stream.
	//
	// Protocol Quirk: Gemini 4 long decode stops a candidate with finishReason "CONTINUATION" plus an
	// opaque continuationToken. The follow-up hop must resend the original payload unchanged
	// (contents, systemInstruction, tools, maxOutputTokens) with this token set at the top level;
	// appending the partial model text instead invalidates the continuation.
	ContinuationToken string `json:"continuationToken,omitempty"`
}

// AgentEnvelope wraps the GeminiRequest with companion project metadata required by Google endpoints.
type AgentEnvelope struct {
	// Project is the Google Cloud companion project identifier owning user quota.
	Project string `json:"project"`

	// Model is the internal upstream model identifier.
	Model string `json:"model"`

	// Request contains the inner Gemini request payload.
	Request *GeminiRequest `json:"request"`

	// RequestType specifies the envelope mode. It is "agent" only for tool-enabled or
	// session-scoped agentic turns and omitted for plain chat completions so upstream applies
	// standard quota bucketing instead of stricter agentic limits.
	RequestType string `json:"requestType,omitempty"`

	// UserAgent designates the client environment identifier (e.g. "antigravity").
	UserAgent string `json:"userAgent"`

	// RequestID is a unique UUID v4 tracking this interaction turn.
	RequestID string `json:"requestId"`
}

// BuildOptions supplies model catalog metadata and schema mode for request transformation.
type BuildOptions struct {
	// Info contains model limits, family classifications, and thinking budgets.
	Info catalog.CatalogModel

	// LegacySchemas when true formats tool parameters under parameters instead of parametersJsonSchema.
	LegacySchemas bool

	// SessionID optionally identifies the interactive session.
	SessionID string

	// ProjectID is the Google Cloud companion project identifier.
	ProjectID string

	// InlineSystem moves system instructions into the first user turn instead of the envelope's
	// systemInstruction field. It is used for the single prompt-rejection retry before the
	// gateway surfaces the upstream error.
	InlineSystem bool
}

// ErrInvalidRequest indicates a malformed OpenAI Chat completions payload.
var ErrInvalidRequest = errors.New("invalid chat completions request")

// ValidateChatRequest checks basic structural validity of an OpenAI chat request.
func ValidateChatRequest(req *ChatRequest) error {
	if req == nil {
		return fmt.Errorf("%w: request body must not be nil", ErrInvalidRequest)
	}
	if req.Model == "" {
		return fmt.Errorf("%w: 'model' is required", ErrInvalidRequest)
	}
	if len(req.Messages) == 0 {
		return fmt.Errorf("%w: 'messages' must be a non-empty array", ErrInvalidRequest)
	}
	for i, m := range req.Messages {
		if m.Role == "" {
			return fmt.Errorf("%w: message at index %d requires 'role'", ErrInvalidRequest, i)
		}
	}
	return nil
}

// IsSchemaRejection detects whether Google rejected tool schemas under parametersJsonSchema.
func IsSchemaRejection(statusCode int, body string) bool {
	if statusCode != 400 {
		return false
	}
	return schemaRejectRegex.MatchString(body)
}

// WrapEnvelope packages an already translated GeminiRequest with companion project metadata and a fresh RequestID.
//
// Protocol Quirk: `requestType: "agent"` subjects a request to stricter agentic quota bucketing,
// so it is emitted only for tool-enabled or session-scoped turns and omitted for plain chat.
func WrapEnvelope(geminiReq *GeminiRequest, upstreamModelID, projectID string) *AgentEnvelope {
	uuidBytes := make([]byte, 16)
	_, _ = rand.Read(uuidBytes)
	uuidBytes[6] = (uuidBytes[6] & 0x0f) | 0x40 // RFC 4122 version 4
	uuidBytes[8] = (uuidBytes[8] & 0x3f) | 0x80 // RFC 4122 variant
	requestID := fmt.Sprintf("agent-%08x-%04x-%04x-%04x-%012x",
		uuidBytes[0:4], uuidBytes[4:6], uuidBytes[6:8], uuidBytes[8:10], uuidBytes[10:16])

	requestType := ""
	if geminiReq != nil && (len(geminiReq.Tools) > 0 || geminiReq.SessionID != "") {
		requestType = "agent"
	}

	return &AgentEnvelope{
		Project:     projectID,
		Model:       upstreamModelID,
		Request:     geminiReq,
		RequestType: requestType,
		UserAgent:   "antigravity",
		RequestID:   requestID,
	}
}

// BuildEnvelope builds the full Cloud Code Assist agent envelope containing companion project metadata.
func BuildEnvelope(req *ChatRequest, opts BuildOptions) (*AgentEnvelope, error) {
	geminiReq, err := BuildRequest(req, opts)
	if err != nil {
		return nil, err
	}
	return WrapEnvelope(geminiReq, catalog.UpstreamModel(req.Model), opts.ProjectID), nil
}

// BuildRequest translates an OpenAI ChatRequest into a GeminiRequest.
func BuildRequest(req *ChatRequest, opts BuildOptions) (*GeminiRequest, error) {
	if err := ValidateChatRequest(req); err != nil {
		return nil, err
	}

	model := req.Model
	info := opts.Info
	claude := config.IsClaude(model)
	withID := NeedsToolCallID(model)

	var systemTexts []string
	var contents []Content
	callNames := make(map[string]string)

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system", "developer":
			txt := extractText(msg.Content)
			if txt != "" {
				systemTexts = append(systemTexts, txt)
			}
		case "assistant":
			for _, tc := range msg.ToolCalls {
				callNames[tc.ID] = CleanToolName(tc.Function.Name)
			}
			parts := buildModelParts(msg, model)
			pushContent(&contents, "model", parts)
		case "tool":
			id := msg.ToolCallID
			output := extractText(msg.Content)
			name := msg.Name
			if name == "" {
				name = callNames[id]
			}
			if name = CleanToolName(name); name == "" {
				name = "tool"
			}
			respPart := FunctionResponse{
				Name:     name,
				Response: map[string]any{"output": output},
			}
			if withID && id != "" {
				respPart.ID = CleanToolCallID(id)
			}
			// Tool messages can carry image blocks next to their text output; forward them as
			// inlineData parts so multimodal tool loops keep their visual context.
			parts := []Part{{FunctionResponse: &respPart}}
			for _, inline := range extractInlineImages(msg.Content) {
				parts = append(parts, Part{InlineData: inline})
			}
			pushContent(&contents, "user", parts)
		default:
			parts := buildUserParts(msg.Content)
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

	stopSequences := extractStopSequences(req.Stop)
	if len(stopSequences) > 0 {
		genConfig["stopSequences"] = stopSequences
	}

	var requestedTokens *int
	if req.MaxCompletionTokens != nil && *req.MaxCompletionTokens > 0 {
		requestedTokens = req.MaxCompletionTokens
	} else if req.MaxTokens != nil && *req.MaxTokens > 0 {
		requestedTokens = req.MaxTokens
	}

	var maxOut *int
	if requestedTokens != nil {
		limit := info.MaxOutputTokens
		if limit <= 0 {
			limit = 65536
		}
		val := min(*requestedTokens, limit)
		maxOut = &val
	}

	thinkingWanted := info.Thinking && !(claude && (forcesTool(req.ToolChoice) || claudeLoopLacksSignedThinking(contents)))
	if thinkingWanted {
		budget := -1
		if info.ThinkingBudget != nil {
			budget = *info.ThinkingBudget
		}

		if claude {
			if ClaudeUsesThinkingLevel(model) {
				// Protocol Quirk: Claude 5+ expects a categorical thinkingLevel plus
				// includeThoughts; the legacy numeric thinking_budget field is rejected.
				level := ThinkingLevelForEffort(req.ReasoningEffort)
				if level == "" {
					level = "HIGH"
				}
				genConfig["thinkingConfig"] = map[string]any{"includeThoughts": true, "thinkingLevel": level}
			} else if budget == -1 {
				genConfig["thinkingConfig"] = map[string]any{"include_thoughts": true}
			} else {
				adjBudget, adjMaxOut := adjustFixedBudget(info, budget, maxOut)
				maxOut = adjMaxOut
				tc := map[string]any{"include_thoughts": true}
				if adjBudget != nil {
					tc["thinking_budget"] = *adjBudget
				}
				genConfig["thinkingConfig"] = tc
			}
		} else {
			// Gemini 3+ and tiered routing variants accept a categorical thinkingLevel,
			// including the deepest XHIGH effort requested by Gemini 4 clients.
			effortLevel := ""
			if geminiSupportsThinkingLevel(model) {
				effortLevel = ThinkingLevelForEffort(req.ReasoningEffort)
			}

			// Gemini 4 reasoning is expressed as effort variants, so an explicit
			// reasoning_effort must win over a catalog-supplied numeric budget.
			if effortLevel != "" && (budget == -1 || geminiPrefersThinkingLevel(model)) {
				genConfig["thinkingConfig"] = map[string]any{"includeThoughts": true, "thinkingLevel": effortLevel}
			} else if budget == -1 {
				genConfig["thinkingConfig"] = map[string]any{"includeThoughts": true}
				if maxOut != nil {
					limit := info.MaxOutputTokens
					if limit <= 0 {
						limit = 65536
					}
					bumped := min(*maxOut+8192, limit)
					maxOut = &bumped
				}
			} else {
				adjBudget, adjMaxOut := adjustFixedBudget(info, budget, maxOut)
				maxOut = adjMaxOut
				tc := map[string]any{"includeThoughts": true}
				if adjBudget != nil {
					tc["thinkingBudget"] = *adjBudget
				}
				genConfig["thinkingConfig"] = tc
			}
		}
	}

	if maxOut != nil {
		genConfig["maxOutputTokens"] = *maxOut
	}

	result := &GeminiRequest{
		Contents:         contents,
		GenerationConfig: genConfig,
	}

	systemText := ScrubAgentMarkers(strings.Join(systemTexts, "\n\n"))
	if systemText != "" {
		if opts.InlineSystem {
			result.Contents = inlineSystemText(contents, systemText)
		} else {
			result.SystemInstruction = &SystemInstruction{
				Parts: []Part{{Text: systemText}},
			}
		}
	}

	decls := convertTools(req.Tools, model, opts.LegacySchemas)
	if len(decls) > 0 {
		result.Tools = []any{map[string]any{"functionDeclarations": decls}}
		if cfg := buildToolConfig(req.ToolChoice); cfg != nil {
			result.ToolConfig = cfg
		}
	}

	if opts.SessionID != "" {
		result.SessionID = opts.SessionID
	}

	return result, nil
}

// geminiSupportsThinkingLevel reports whether a Gemini family model accepts the categorical
// `thinkingLevel` field. Gemini 3 and newer expose native reasoning levels, and `-tiered`
// routing variants forward the same contract.
func geminiSupportsThinkingLevel(model string) bool {
	if major := GeminiMajor(model); major != nil && *major >= 3 {
		return true
	}
	return strings.HasSuffix(strings.ToLower(model), "-tiered")
}

// geminiPrefersThinkingLevel reports whether a Gemini generation models reasoning as named effort
// variants rather than a token budget. Gemini 4 exposes XHIGH/MAX tiers that a numeric budget
// cannot express.
func geminiPrefersThinkingLevel(model string) bool {
	major := GeminiMajor(model)
	return major != nil && *major >= 4
}

// inlineSystemText merges system instruction text into the first user turn. Existing plain user
// text is preserved after the system text; turns that lead with tool responses get a separate
// leading user turn so function responses stay adjacent to their model call.
func inlineSystemText(contents []Content, text string) []Content {
	if text == "" {
		return contents
	}

	out := slices.Clone(contents)
	if len(out) > 0 && out[0].Role == "user" && !slices.ContainsFunc(out[0].Parts, func(p Part) bool {
		return p.FunctionResponse != nil
	}) {
		out[0].Parts = append([]Part{{Text: text}}, out[0].Parts...)
		return out
	}

	return append([]Content{{Role: "user", Parts: []Part{{Text: text}}}}, out...)
}

// extractText flattens string or complex array message content into a concatenated plain text string.
func extractText(content any) string {
	switch v := content.(type) {
	case string:
		return StripBillingHeader(v)
	case []any:
		var sb strings.Builder
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if t, ok := m["type"].(string); ok && t == "text" {
					if txt, ok := m["text"].(string); ok {
						sb.WriteString(StripBillingHeader(txt))
					}
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// buildUserParts transforms OpenAI message content into Google Cloud Code parts, handling data: image URLs.
func buildUserParts(content any) []Part {
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
			itemType, _ := m["type"].(string)
			if itemType == "text" {
				if txt, ok := m["text"].(string); ok && txt != "" {
					parts = append(parts, Part{Text: txt})
				}
			} else if itemType == "image_url" {
				var urlStr string
				if rawURL, ok := m["image_url"].(string); ok {
					urlStr = rawURL
				} else if imgMap, ok := m["image_url"].(map[string]any); ok {
					urlStr, _ = imgMap["url"].(string)
				}
				if strings.HasPrefix(urlStr, "data:") {
					if inline := parseDataURL(urlStr); inline != nil {
						parts = append(parts, Part{InlineData: inline})
						continue
					}
				}
				parts = append(parts, Part{Text: "[image omitted: only data: URLs are supported]"})
			}
		}
		return parts
	default:
		return nil
	}
}

// extractInlineImages collects every image block inside a tool result payload as inline data.
//
// Protocol Quirk: tool results reach the gateway in two shapes. Anthropic bundles typed `image`
// blocks (with a base64 `source`) next to `text` blocks, while OpenAI tool messages expose
// `image_url` blocks whose URL may be a data: URL. Both must become inlineData parts, otherwise
// multimodal MCP and browser tools are silently blinded. Non-data URLs are skipped because Google
// Cloud Code Assist cannot fetch remote images.
func extractInlineImages(content any) []*InlineData {
	items, ok := content.([]any)
	if !ok {
		return nil
	}

	var images []*InlineData
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch itemType, _ := m["type"].(string); itemType {
		case "image":
			src, ok := m["source"].(map[string]any)
			if !ok {
				continue
			}
			mediaType, _ := src["media_type"].(string)
			data, _ := src["data"].(string)
			if mediaType != "" && data != "" {
				images = append(images, &InlineData{MimeType: mediaType, Data: data})
			}
		case "image_url":
			var urlStr string
			if rawURL, ok := m["image_url"].(string); ok {
				urlStr = rawURL
			} else if imgMap, ok := m["image_url"].(map[string]any); ok {
				urlStr, _ = imgMap["url"].(string)
			}
			if !strings.HasPrefix(urlStr, "data:") {
				continue
			}
			if inline := parseDataURL(urlStr); inline != nil {
				images = append(images, inline)
			}
		}
	}
	return images
}

// parseDataURL extracts MIME type and raw base64 data from a data: URL string.
func parseDataURL(urlStr string) *InlineData {
	matches := dataURLRegex.FindStringSubmatch(urlStr)
	if len(matches) == 3 {
		return &InlineData{
			MimeType: matches[1],
			Data:     matches[2],
		}
	}
	return nil
}

// buildModelParts reconstructs assistant turns using recorded recipes, restoring thought signatures and tool calls.
func buildModelParts(msg ChatMessage, model string) []Part {
	claude := config.IsClaude(model)
	withID := NeedsToolCallID(model)
	recipe := ReadRecipe(msg.ReasoningDetails)
	textContent := extractText(msg.Content)

	calls := msg.ToolCalls
	callPart := func(tc ChatToolCall, sig string) Part {
		var argsMap map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &argsMap); err != nil {
			argsMap = make(map[string]any)
		}
		fc := &FunctionCallPart{
			Name: CleanToolName(tc.Function.Name),
			Args: argsMap,
		}
		if withID {
			fc.ID = CleanToolCallID(tc.ID)
		}
		part := Part{FunctionCall: fc}
		if sig != "" {
			part.ThoughtSignature = sig
		}
		return part
	}

	var parts []Part
	placedCalls := make(map[string]bool)
	textPlaced := false

	placeText := func(sig string) {
		if textPlaced {
			return
		}
		textPlaced = true
		if textContent != "" || sig != "" {
			p := Part{Text: textContent}
			if sig != "" {
				p.ThoughtSignature = sig
			}
			parts = append(parts, p)
		}
	}

	if recipe != nil {
		for _, step := range recipe.Steps {
			switch step.Type {
			case "thought":
				if claude && step.Sig == "" {
					continue
				}
				if step.Text == "" && step.Sig == "" {
					continue
				}
				p := Part{
					Thought: true,
					Text:    step.Text,
				}
				if step.Sig != "" {
					p.ThoughtSignature = step.Sig
				}
				parts = append(parts, p)
			case "text":
				placeText(step.Sig)
			case "call":
				var matchedCall *ChatToolCall
				for i := range calls {
					if calls[i].ID == step.ID && !placedCalls[calls[i].ID] {
						matchedCall = &calls[i]
						break
					}
				}
				if matchedCall == nil {
					continue
				}
				if !textPlaced && textContent != "" {
					placeText("")
				}
				placedCalls[matchedCall.ID] = true
				parts = append(parts, callPart(*matchedCall, step.Sig))
			}
		}
	}

	placeText("")

	for _, tc := range calls {
		if !placedCalls[tc.ID] {
			parts = append(parts, callPart(tc, ""))
		}
	}

	// Protocol Quirk: Gemini 3+ strictly validates signatures on the first tool call of a turn.
	// When absent (e.g. from foreign model, or client stripped reasoning_details), inject SkipSignature.
	major := GeminiMajor(model)
	if major != nil && *major >= 3 {
		for i := range parts {
			if parts[i].FunctionCall != nil {
				if parts[i].ThoughtSignature == "" {
					parts[i].ThoughtSignature = SkipSignature
				}
				break
			}
		}
	}

	return parts
}

// pushContent appends a role turn to contents, merging adjacent same-role chunks where permitted.
func pushContent(contents *[]Content, role string, parts []Part) {
	if len(parts) == 0 {
		return
	}
	if len(*contents) > 0 {
		lastIdx := len(*contents) - 1
		last := &(*contents)[lastIdx]
		lastHasResp := slices.ContainsFunc(last.Parts, func(p Part) bool { return p.FunctionResponse != nil })
		newHasResp := slices.ContainsFunc(parts, func(p Part) bool { return p.FunctionResponse != nil })

		if last.Role == role && lastHasResp == newHasResp {
			last.Parts = append(last.Parts, parts...)
			return
		}
	}
	*contents = append(*contents, Content{Role: role, Parts: parts})
}

// convertTools converts OpenAI function tools into Google Cloud Code Assist declarations.
func convertTools(tools []any, model string, legacy bool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	claude := config.IsClaude(model)
	var decls []map[string]any

	for _, t := range tools {
		tMap, ok := t.(map[string]any)
		if !ok || tMap["type"] != "function" {
			continue
		}
		fn, ok := tMap["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name = CleanToolName(name); name == "" {
			continue
		}

		decl := map[string]any{"name": name}
		if desc, ok := fn["description"].(string); ok && desc != "" {
			decl["description"] = desc
		}

		params, hasParams := fn["parameters"].(map[string]any)
		if claude {
			if hasParams {
				decl["parameters"] = ForClaudeParameters(params)
			} else {
				decl["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
		} else if hasParams && legacy {
			decl["parameters"] = ForGeminiLegacyParameters(params)
		} else if hasParams {
			decl["parametersJsonSchema"] = ForGeminiJSONSchema(params)
		}
		decls = append(decls, decl)
	}
	return decls
}

// buildToolConfig translates OpenAI tool_choice settings into Google functionCallingConfig objects.
func buildToolConfig(choice any) map[string]any {
	if s, ok := choice.(string); ok {
		if s == "none" {
			return map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}}
		}
		if s == "required" {
			return map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}}
		}
	}
	if m, ok := choice.(map[string]any); ok {
		if fn, ok := m["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && name != "" {
				return map[string]any{
					"functionCallingConfig": map[string]any{
						"mode":                 "ANY",
						"allowedFunctionNames": []string{name},
					},
				}
			}
		}
	}
	return nil
}

// forcesTool checks if the tool_choice parameter requires an explicit tool execution.
func forcesTool(choice any) bool {
	if s, ok := choice.(string); ok && s == "required" {
		return true
	}
	if m, ok := choice.(map[string]any); ok {
		if _, ok := m["function"].(map[string]any); ok {
			return true
		}
	}
	return false
}

// adjustFixedBudget bounds requested reasoning budgets within model minimums and maximum output limits.
// When the catalog has no output ceiling for the model, the generic Gemini ceiling is assumed so the
// budget is never collapsed to zero (which would disable reasoning) or to a one-token output limit.
func adjustFixedBudget(info catalog.CatalogModel, budget int, maxOut *int) (*int, *int) {
	if strings.Contains(strings.ToLower(info.ID), "2.5") && budget > Gemini25MaxThinkingBudget {
		budget = Gemini25MaxThinkingBudget
	}

	limit := info.MaxOutputTokens
	if limit <= 0 {
		limit = MaxGeminiOutputTokens
	}
	minBudget := 0
	if info.MinThinkingBudget != nil {
		minBudget = *info.MinThinkingBudget
	}
	b := min(max(minBudget, budget), limit-1)
	if b < minBudget {
		return nil, maxOut
	}

	out := maxOut
	if out == nil || *out <= b {
		adj := max(b+1, min(b+8192, limit))
		out = &adj
	}
	return &b, out
}

// claudeLoopLacksSignedThinking checks if an assistant turn preceding tool results lacks signed thinking blocks.
func claudeLoopLacksSignedThinking(contents []Content) bool {
	for i := len(contents) - 1; i >= 0; i-- {
		c := contents[i]
		if c.Role == "user" {
			if slices.ContainsFunc(c.Parts, func(p Part) bool { return p.FunctionResponse != nil }) {
				continue
			}
			return false
		}
		hasCalls := slices.ContainsFunc(c.Parts, func(p Part) bool { return p.FunctionCall != nil })
		if !hasCalls {
			return false
		}
		if len(c.Parts) == 0 {
			return true
		}
		first := c.Parts[0]
		return !(first.Thought && first.ThoughtSignature != "")
	}
	return false
}

// extractStopSequences parses string, []string, or []any into a clean slice of stop sequence strings.
func extractStopSequences(stop any) []string {
	switch s := stop.(type) {
	case string:
		if s != "" {
			return []string{s}
		}
	case []any:
		var res []string
		for _, item := range s {
			if str, ok := item.(string); ok && str != "" {
				res = append(res, str)
			}
		}
		return res
	case []string:
		return s
	}
	return nil
}
