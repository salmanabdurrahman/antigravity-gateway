// Package translate provides bidirectional translation between OpenAI/Anthropic
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"regexp"
	"slices"
	"strconv"
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

// thinkingHeadroomTokens reserves output space above the thinking budget so a reasoning model can
// still emit its final answer after finishing the chain of thought.
const thinkingHeadroomTokens = 8192

// minClaudeThinkingBudget is the smallest numeric thinking budget upstream accepts for Claude models.
// Cloud Code rejects smaller values with "budget_tokens: Input should be greater than or equal to 1024",
// so a tighter client output cap disables reasoning instead of sending an invalid budget.
const minClaudeThinkingBudget = 1024

// claudeVersionRegex extracts the major/minor generation number from Claude model identifiers,
// covering both family-first (claude-sonnet-4-6) and version-first (claude-4-6-sonnet) spellings.
var claudeVersionRegex = regexp.MustCompile(`(?i)^claude-(?:[a-z]+-)*(\d+)(?:[.-](\d+))?`)

// ClaudeUsesThinkingLevel reports whether a Claude model expects the categorical
// `thinkingLevel` field instead of the legacy numeric `thinking_budget` field.
//
// Protocol Quirk: Claude 5 and later generations expose reasoning effort as an enum
// (`LOW`/`MEDIUM`/`HIGH`/`MAX`) and reject numeric thinking budgets, while Claude 4.x
// still requires `thinking_budget`.
func ClaudeUsesThinkingLevel(model string) bool {
	match := claudeVersionRegex.FindStringSubmatch(strings.ToLower(model))
	if len(match) < 2 {
		return false
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return false
	}
	return major >= 5
}

// ThinkingLevelForEffort maps an OpenAI `reasoning_effort` or Anthropic
// `output_config.effort` label to the Cloud Code Assist `thinkingLevel` enum value.
// It returns an empty string when the effort label is absent or unrecognized.
//
// Protocol Quirk: Cloud Code Assist serializes `thinkingLevel` as an uppercase protojson
// enum string. Lowercase or unknown values are silently ignored upstream.
func ThinkingLevelForEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal":
		return "MINIMAL"
	case "low":
		return "LOW"
	case "medium":
		return "MEDIUM"
	case "high":
		return "HIGH"
	case "xhigh", "extra_high", "extra-high":
		return "XHIGH"
	case "max":
		return "MAX"
	default:
		return ""
	}
}

// ClampThinkingBudget enforces model-specific reasoning token boundaries and ensures
// that maxOutputTokens strictly exceeds the thinking budget as mandated by Google Cloud Code.
//
// Protocol Quirk: Google Cloud Code Assist returns HTTP 400 if thinkingBudget >= maxOutputTokens.
// Additionally, Gemini 2.5 models return HTTP 400 if thinking budget exceeds 24,576 tokens.
// This function clamps the budget and, when no client ceiling is supplied, expands maxOutputTokens to
// guarantee valid envelopes.
//
// hardCap marks maxOutputTokens as an explicit client output limit: the returned ceiling never exceeds
// it and the reasoning budget shrinks to fit instead, so `max_tokens` behaves as a hard maximum. A
// returned budget of zero means the ceiling is too small for reasoning and callers must omit the
// thinking configuration entirely.
//
// modelMaxOutputTokens is the catalog ceiling for the concrete upstream model. Non-Gemini and
// non-Claude families (for example gpt-oss-120b at 32,768 tokens) must never be expanded to the
// generic Gemini ceiling, because upstream rejects any request above their own limit.
func ClampThinkingBudget(model string, requestedBudget, maxOutputTokens, modelMaxOutputTokens int, isClaude, hardCap bool) (budget int, adjustedMaxTokens int) {
	lower := strings.ToLower(model)
	budget = requestedBudget

	ceiling := modelMaxOutputTokens
	if ceiling <= 0 {
		ceiling = MaxGeminiOutputTokens
	}

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
	if hardCap {
		// A client-supplied ceiling is authoritative: never raise it to make room for reasoning.
		if adjustedMaxTokens <= 0 {
			adjustedMaxTokens = ceiling
		}
	} else if adjustedMaxTokens <= 0 {
		adjustedMaxTokens = budget + thinkingHeadroomTokens
	} else if adjustedMaxTokens <= budget {
		adjustedMaxTokens = budget + thinkingHeadroomTokens
	}

	// Keep the generation ceiling inside what the concrete model actually accepts, then shrink the
	// budget so the invariant thinkingBudget < maxOutputTokens still holds after the clamp. Halving is
	// used instead of subtracting the headroom because a small ceiling would otherwise collapse the
	// budget to zero and silently disable reasoning.
	if adjustedMaxTokens > ceiling {
		adjustedMaxTokens = ceiling
	}
	if budget >= adjustedMaxTokens {
		// Half the ceiling keeps visible output usable while preserving the budget < maxOutputTokens
		// invariant. A ceiling of one token collapses the budget to zero, which disables reasoning for
		// that request instead of sending an invalid configuration upstream.
		budget = adjustedMaxTokens / 2
	}

	if isClaude && budget < minClaudeThinkingBudget {
		// Claude enforces a 1024-token floor, so a tighter ceiling disables reasoning entirely.
		budget = 0
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
		return slices.Clone(contents)
	}

	result := make([]Content, len(contents))
	for idx, c := range contents {
		result[idx] = Content{
			Role:  c.Role,
			Parts: slices.Clone(c.Parts),
		}
	}

	for i := 0; i < len(result); i++ {
		turn := result[i]

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
		if i+1 < len(result) {
			nextTurn := result[i+1]
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
					// Prepend synthetic tool responses before the user's new message parts without mutating caller
					result[i+1].Parts = append(syntheticParts, result[i+1].Parts...)
				}
			}
		}
	}

	return result
}
