// Package translate provides bidirectional translation between OpenAI/Anthropic
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"strings"
)

// Gemini25MaxThinkingBudget defines the hard token ceiling (24,576) enforced by Gemini 2.5 models.
const Gemini25MaxThinkingBudget = 24576

// DefaultClaudeThinkingBudget defines the standard thinking allocation for Claude thinking models.
const DefaultClaudeThinkingBudget = 32000

// DefaultGeminiThinkingBudget defines the standard thinking allocation for Gemini thinking models.
const DefaultGeminiThinkingBudget = 16000

// MaxGeminiOutputTokens defines the global generation ceiling for Gemini endpoints.
const MaxGeminiOutputTokens = 65536

// ClampThinkingBudget enforces model-specific reasoning token boundaries and ensures
// that maxOutputTokens strictly exceeds the thinking budget as mandated by Google Cloud Code.
//
// Protocol Quirk: Google Cloud Code Assist returns HTTP 400 if thinkingBudget >= maxOutputTokens.
// Additionally, Gemini 2.5 models return HTTP 400 if thinking budget exceeds 24,576 tokens.
// This function clamps the budget and expands maxOutputTokens to guarantee valid envelopes.
func ClampThinkingBudget(model string, requestedBudget int, maxOutputTokens int, isClaude bool) (budget int, adjustedMaxTokens int) {
	lower := strings.ToLower(model)
	budget = requestedBudget

	if isClaude {
		if budget <= 0 {
			budget = DefaultClaudeThinkingBudget
		}
	} else {
		// Gemini models
		if budget <= 0 {
			budget = DefaultGeminiThinkingBudget
		}
		if strings.Contains(lower, "2.5") && budget > Gemini25MaxThinkingBudget {
			budget = Gemini25MaxThinkingBudget
		}
	}

	adjustedMaxTokens = maxOutputTokens
	if adjustedMaxTokens <= 0 {
		adjustedMaxTokens = budget + 8192
	} else if adjustedMaxTokens <= budget {
		adjustedMaxTokens = budget + 8192
	}

	if !isClaude && adjustedMaxTokens > MaxGeminiOutputTokens {
		adjustedMaxTokens = MaxGeminiOutputTokens
		if budget >= adjustedMaxTokens {
			budget = adjustedMaxTokens - 8192
		}
	}

	return budget, adjustedMaxTokens
}

// InjectInterruptedToolResponses scans conversation turns and guarantees that every
// model functionCall is satisfied by a corresponding functionResponse in the subsequent user turn.
//
// Protocol Quirk: If a model invokes tools but the user interrupts (sending plain text instead
// of tool outputs), Google Cloud Code Assist rejects the next turn with HTTP 400
// "Expected tool response but found text". Injecting synthetic cancellation acknowledgments
// repairs the conversational state safely.
func InjectInterruptedToolResponses(contents []Content) []Content {
	if len(contents) <= 1 {
		return contents
	}

	var result []Content

	for i := 0; i < len(contents); i++ {
		turn := contents[i]
		result = append(result, turn)

		if turn.Role != "model" {
			continue
		}

		// Collect all function calls initiated in this model turn
		type pendingCall struct {
			id   string
			name string
		}
		var pending []pendingCall
		for _, p := range turn.Parts {
			if p.FunctionCall != nil {
				pending = append(pending, pendingCall{
					id:   p.FunctionCall.ID,
					name: p.FunctionCall.Name,
				})
			}
		}

		if len(pending) == 0 {
			continue
		}

		// Inspect the subsequent turn
		if i+1 < len(contents) {
			nextTurn := contents[i+1]
			if nextTurn.Role == "user" {
				// Find which calls are already answered by functionResponse in the next turn
				answeredIDs := make(map[string]bool)
				answeredNameCount := make(map[string]int)
				for _, np := range nextTurn.Parts {
					if np.FunctionResponse != nil {
						if np.FunctionResponse.ID != "" {
							answeredIDs[np.FunctionResponse.ID] = true
						}
						if np.FunctionResponse.Name != "" {
							answeredNameCount[np.FunctionResponse.Name]++
						}
					}
				}

				// Find unanswered calls, prioritizing explicit ID matching
				var syntheticParts []Part
				for _, call := range pending {
					isAnswered := false
					if call.id != "" {
						isAnswered = answeredIDs[call.id]
					} else if call.name != "" && answeredNameCount[call.name] > 0 {
						isAnswered = true
						answeredNameCount[call.name]--
					}
					if !isAnswered {
						syntheticParts = append(syntheticParts, Part{
							FunctionResponse: &FunctionResponse{
								ID:   call.id,
								Name: call.name,
								Response: map[string]any{
									"output": "[Tool execution was interrupted by user.]",
								},
							},
						})
					}
				}

				if len(syntheticParts) > 0 {
					// Prepend synthetic tool responses before the user's new message parts
					contents[i+1].Parts = append(syntheticParts, contents[i+1].Parts...)
				}
			}
		}
	}

	return result
}
