// Package catalog discovers upstream model availability, limits, and live quota tracking.
package catalog

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

var (
	safeIDRegex    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*$`)
	imageWordRegex = regexp.MustCompile(`(^|-)(image|imagen)(-|$)`)
)

// UpstreamAliases maps public display model identifiers to internal Google Cloud Code Assist identifiers.
var UpstreamAliases = map[string]string{
	"gemini-3.1-pro-high": "gemini-pro-agent",
	"gpt-oss-120b":        "gpt-oss-120b-medium",
}

// CatalogModel represents an available chat completion model with its limits and capabilities.
type CatalogModel struct {
	// ID is the unique model identifier string (e.g. "gemini-2.5-pro", "claude-3-7-sonnet").
	ID string `json:"id"`

	// DisplayName is the human-readable label returned by upstream Google Cloud Code.
	DisplayName string `json:"displayName,omitempty"`

	// ContextWindow specifies the maximum input context length in tokens.
	ContextWindow int `json:"contextWindow"`

	// MaxOutputTokens specifies the maximum generation ceiling in tokens.
	MaxOutputTokens int `json:"maxOutputTokens"`

	// Thinking indicates whether the model supports chain-of-thought reasoning tokens.
	Thinking bool `json:"thinking"`

	// ThinkingBudget defines the default reasoning budget allocation in tokens, if supported.
	ThinkingBudget *int `json:"thinkingBudget,omitzero"`

	// MinThinkingBudget defines the lowest permissible reasoning token threshold.
	MinThinkingBudget *int `json:"minThinkingBudget,omitzero"`

	// Images indicates whether the model accepts multimodal image input payloads.
	Images bool `json:"images"`
}

// ModelQuota contains current rate limit and remaining usage metrics for a model.
type ModelQuota struct {
	// RemainingFraction represents the fraction (0.0 to 1.0) of quota remaining before rate limiting.
	RemainingFraction *float64 `json:"remainingFraction,omitzero"`

	// ResetTime holds the RFC 3339 timestamp when the current quota tier resets.
	ResetTime string `json:"resetTime,omitempty"`
}

// CatalogSnapshot holds a collection of chat models and their current quota states.
type CatalogSnapshot struct {
	// Models lists all active chat completion models discovered from upstream.
	Models []CatalogModel `json:"models"`

	// Quotas maps model identifiers to their live remaining quota metrics.
	Quotas map[string]ModelQuota `json:"quotas,omitempty"`

	// DefaultModel holds the default agent model identifier designated by Google.
	DefaultModel string `json:"defaultModel,omitempty"`
}

// UpstreamModel resolves public model IDs to upstream internal model identifiers.
func UpstreamModel(id string) string {
	if target, ok := UpstreamAliases[strings.ToLower(id)]; ok {
		return target
	}
	return id
}

// ResolveModelAlias resolves a model identifier using custom configuration mappings and standard aliases.
func ResolveModelAlias(id string, customMapping map[string]string) string {
	if customMapping != nil {
		if target, ok := customMapping[id]; ok && target != "" {
			return target
		}
		if target, ok := customMapping[strings.ToLower(id)]; ok && target != "" {
			return target
		}
	}
	return UpstreamModel(id)
}

// IsListedChatModel determines if an upstream identifier represents a valid conversational chat model,
// filtering out internal IDE tools, image generation models, embeddings, and tab completion variants.
func IsListedChatModel(id string) bool {
	if len(id) == 0 || len(id) > 128 || !safeIDRegex.MatchString(id) || strings.Contains(id, "..") {
		return false
	}
	lower := strings.ToLower(id)
	if lower == "gemini" || lower == "claude" {
		return false
	}
	if strings.HasPrefix(lower, "chat_") || strings.HasPrefix(lower, "tab_") {
		return false
	}
	if imageWordRegex.MatchString(lower) ||
		strings.Contains(lower, "whisper") ||
		strings.Contains(lower, "tts") ||
		strings.Contains(lower, "embedding") {
		return false
	}
	// Gemini 3.5 entries are IDE internals except flash-lite.
	if strings.HasPrefix(lower, "gemini-3.5-") && lower != "gemini-3.5-flash-lite" {
		return false
	}
	return true
}

// FamilyDefaults returns baseline limits and capabilities based on model family naming conventions.
func FamilyDefaults(id string) CatalogModel {
	lower := strings.ToLower(id)
	if strings.HasPrefix(lower, "gpt-oss") {
		tb := 8192
		return CatalogModel{
			ID:              id,
			ContextWindow:   131072,
			MaxOutputTokens: 32768,
			Thinking:        true,
			ThinkingBudget:  &tb,
			Images:          false,
		}
	}
	if config.IsClaude(lower) {
		tb := 32768
		return CatalogModel{
			ID:              id,
			ContextWindow:   1000000,
			MaxOutputTokens: 64000,
			Thinking:        true,
			ThinkingBudget:  &tb,
			Images:          true,
		}
	}
	if strings.HasPrefix(lower, "gemini") {
		tb := -1
		return CatalogModel{
			ID:              id,
			ContextWindow:   1048576,
			MaxOutputTokens: 65536,
			Thinking:        true,
			ThinkingBudget:  &tb,
			Images:          true,
		}
	}
	return CatalogModel{
		ID:              id,
		ContextWindow:   128000,
		MaxOutputTokens: 32768,
		Thinking:        false,
		Images:          false,
	}
}

// rawCatalogResponse maps the upstream fetchAvailableModels JSON envelope.
type rawCatalogResponse struct {
	// Models maps model identifiers to raw upstream model capability descriptors.
	Models map[string]rawModelInfo `json:"models"`

	// DefaultAgentModelID contains the default agent model string reported by Google.
	DefaultAgentModelID string `json:"defaultAgentModelId"`
}

// rawModelInfo details token limits, thinking capabilities, and quota for an upstream model.
type rawModelInfo struct {
	// MaxTokens is the upstream context window size.
	MaxTokens *int `json:"maxTokens"`

	// MaxOutputTokens is the upstream output token limit.
	MaxOutputTokens *int `json:"maxOutputTokens"`

	// SupportsThinking indicates upstream reasoning support.
	SupportsThinking *bool `json:"supportsThinking"`

	// SupportsImages indicates upstream multimodal image acceptance.
	SupportsImages *bool `json:"supportsImages"`

	// ThinkingBudget is the default reasoning token budget.
	ThinkingBudget *int `json:"thinkingBudget"`

	// MinThinkingBudget is the minimum permitted reasoning token budget.
	MinThinkingBudget *int `json:"minThinkingBudget"`

	// DisplayName is the upstream human-readable name.
	DisplayName string `json:"displayName"`

	// QuotaInfo describes the remaining quota and reset timestamp.
	QuotaInfo *rawQuotaInfo `json:"quotaInfo"`
}

// rawQuotaInfo extracts fractional quota availability from upstream.
type rawQuotaInfo struct {
	// RemainingFraction is a float between 0.0 and 1.0 indicating remaining capacity.
	RemainingFraction *float64 `json:"remainingFraction"`

	// ResetTime is the ISO-8601/RFC 3339 reset timestamp string.
	ResetTime string `json:"resetTime"`
}

// ParseCatalog parses the raw JSON response from Google Cloud Code Assist v1internal:fetchAvailableModels,
// applying chat model filtering, family defaults, and sorting models alphabetically by ID.
func ParseCatalog(data []byte) (*CatalogSnapshot, error) {
	var resp rawCatalogResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parsing catalog JSON: %w", err)
	}

	snapshot := &CatalogSnapshot{
		Models:       make([]CatalogModel, 0, len(resp.Models)),
		Quotas:       make(map[string]ModelQuota),
		DefaultModel: strings.TrimSpace(resp.DefaultAgentModelID),
	}

	for rawID, info := range resp.Models {
		id := strings.TrimSpace(rawID)
		if !IsListedChatModel(id) {
			continue
		}

		base := FamilyDefaults(id)
		model := CatalogModel{
			ID:              id,
			ContextWindow:   base.ContextWindow,
			MaxOutputTokens: base.MaxOutputTokens,
			Thinking:        base.Thinking,
			ThinkingBudget:  base.ThinkingBudget,
			Images:          base.Images,
		}

		if info.MaxTokens != nil && *info.MaxTokens > 0 {
			model.ContextWindow = *info.MaxTokens
		}
		if info.MaxOutputTokens != nil && *info.MaxOutputTokens > 0 {
			model.MaxOutputTokens = *info.MaxOutputTokens
		}
		if dName := strings.TrimSpace(info.DisplayName); dName != "" {
			model.DisplayName = dName
		}
		if info.SupportsThinking != nil {
			model.Thinking = *info.SupportsThinking
		}
		if info.SupportsImages != nil {
			model.Images = *info.SupportsImages
		}
		if info.ThinkingBudget != nil && (*info.ThinkingBudget == -1 || *info.ThinkingBudget >= 0) {
			tb := *info.ThinkingBudget
			model.ThinkingBudget = &tb
		}
		if info.MinThinkingBudget != nil && *info.MinThinkingBudget >= 0 {
			mtb := *info.MinThinkingBudget
			model.MinThinkingBudget = &mtb
		}
		if !model.Thinking {
			model.ThinkingBudget = nil
			model.MinThinkingBudget = nil
		}

		snapshot.Models = append(snapshot.Models, model)

		if info.QuotaInfo != nil {
			var q ModelQuota
			if info.QuotaInfo.RemainingFraction != nil && *info.QuotaInfo.RemainingFraction >= 0 && *info.QuotaInfo.RemainingFraction <= 1 {
				rf := *info.QuotaInfo.RemainingFraction
				q.RemainingFraction = &rf
			}
			if rTime := strings.TrimSpace(info.QuotaInfo.ResetTime); rTime != "" {
				q.ResetTime = rTime
			}
			if q.RemainingFraction != nil || q.ResetTime != "" {
				snapshot.Quotas[id] = q
			}
		}
	}

	slices.SortFunc(snapshot.Models, func(a, b CatalogModel) int {
		return strings.Compare(a.ID, b.ID)
	})

	return snapshot, nil
}

// QuotaSummary represents aggregated quota percentages and reset time for account display.
type QuotaSummary struct {
	// GeminiRemainingFraction holds the minimum remaining quota fraction across Gemini models.
	GeminiRemainingFraction *float64

	// ClaudeRemainingFraction holds the minimum remaining quota fraction across Claude models.
	ClaudeRemainingFraction *float64

	// GeminiResetTime indicates when Gemini quota will reset.
	GeminiResetTime string

	// ClaudeResetTime indicates when Claude quota will reset.
	ClaudeResetTime string
}

// SummarizeQuotas aggregates per-model quotas into Gemini and Claude family remaining fractions and reset times.
func SummarizeQuotas(quotas map[string]ModelQuota) QuotaSummary {
	var summary QuotaSummary

	// Sort model IDs to guarantee deterministic iteration order
	keys := make([]string, 0, len(quotas))
	for k := range quotas {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	for _, id := range keys {
		q := quotas[id]
		if q.RemainingFraction == nil && q.ResetTime == "" {
			continue
		}

		lower := strings.ToLower(id)
		if strings.HasPrefix(lower, "gemini") {
			if q.RemainingFraction != nil {
				if summary.GeminiRemainingFraction == nil || *q.RemainingFraction < *summary.GeminiRemainingFraction {
					summary.GeminiRemainingFraction = q.RemainingFraction
					if q.ResetTime != "" {
						summary.GeminiResetTime = q.ResetTime
					}
				}
			} else if summary.GeminiResetTime == "" && q.ResetTime != "" {
				summary.GeminiResetTime = q.ResetTime
			}
		} else if config.IsClaude(lower) || strings.HasPrefix(lower, "gpt-oss") {
			if q.RemainingFraction != nil {
				if summary.ClaudeRemainingFraction == nil || *q.RemainingFraction < *summary.ClaudeRemainingFraction {
					summary.ClaudeRemainingFraction = q.RemainingFraction
					if q.ResetTime != "" {
						summary.ClaudeResetTime = q.ResetTime
					}
				}
			} else if summary.ClaudeResetTime == "" && q.ResetTime != "" {
				summary.ClaudeResetTime = q.ResetTime
			}
		}
	}

	return summary
}

// FormatResetTime formats an RFC3339 reset time string as "15:04 UTC", or returns "Ready" if empty or in the past.
func FormatResetTime(resetTime string, now time.Time) string {
	if resetTime == "" {
		return "Ready"
	}
	t, err := time.Parse(time.RFC3339, resetTime)
	if err != nil {
		t, err = time.Parse(time.RFC3339Nano, resetTime)
		if err != nil {
			return resetTime
		}
	}
	if !t.After(now) {
		return "Ready"
	}
	return t.UTC().Format("15:04 UTC")
}

// FormatQuota formats a quota fraction pointer as a percentage string (e.g. "85%"), or "100%" if nil.
func FormatQuota(fraction *float64) string {
	if fraction == nil {
		return "100%"
	}
	val := max(0.0, min(1.0, *fraction))
	return fmt.Sprintf("%d%%", int(val*100))
}
