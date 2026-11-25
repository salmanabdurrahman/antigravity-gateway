// Package catalog discovers upstream model availability, limits, and live quota tracking.
package catalog

import (
	"encoding/json"
	"fmt"
	"maps"
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

	// Weekly holds the separate weekly allowance window when upstream reports one.
	Weekly *QuotaWindow `json:"weekly,omitzero"`
}

// QuotaWindow describes a single upstream quota window such as the rolling five-hour or weekly allowance.
type QuotaWindow struct {
	// RemainingFraction is the remaining capacity fraction (0.0 to 1.0) for this window.
	RemainingFraction *float64 `json:"remainingFraction,omitzero"`

	// ResetTime is the RFC 3339 timestamp when the window resets.
	ResetTime string `json:"resetTime,omitempty"`
}

// Untouched reports whether the window still holds full capacity and carries no reset pressure.
// Untouched windows are filtered out of summaries so a brand-new weekly allowance does not
// drown out a partially consumed rolling window.
func (w QuotaWindow) Untouched() bool {
	return w.RemainingFraction != nil && *w.RemainingFraction >= 1.0 && w.ResetTime == ""
}

// AccountQuotaSummary separates rolling five-hour usage from the weekly allowance reported by
// `v1internal:retrieveUserQuotaSummary`.
type AccountQuotaSummary struct {
	// FiveHour is the rolling five-hour window, when reported.
	FiveHour *QuotaWindow `json:"fiveHour,omitzero"`

	// Weekly is the weekly allowance window, when reported.
	Weekly *QuotaWindow `json:"weekly,omitzero"`
}

// CatalogSnapshot holds a collection of chat models and their current quota states.
type CatalogSnapshot struct {
	// Models lists all active chat completion models discovered from upstream.
	Models []CatalogModel `json:"models"`

	// Quotas maps model identifiers to their live remaining quota metrics.
	Quotas map[string]ModelQuota `json:"quotas,omitempty"`

	// DefaultModel holds the default agent model identifier designated by Google.
	DefaultModel string `json:"defaultModel,omitempty"`

	// AccountQuota separates the rolling five-hour window from the weekly allowance aggregated
	// across the accounts that served the refresh (merged to the most constrained window). It is nil
	// when upstream reports neither window.
	AccountQuota *AccountQuotaSummary `json:"accountQuota,omitzero"`
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

// isSafeModelID reports whether an upstream identifier is syntactically safe to surface to clients.
func isSafeModelID(id string) bool {
	return len(id) > 0 && len(id) <= 128 && safeIDRegex.MatchString(id) && !strings.Contains(id, "..")
}

// IsListedChatModel determines if an upstream identifier represents a valid conversational chat model,
// filtering out internal IDE tools, image generation models, embeddings, tab completion variants, and
// `*-disabled` placeholders that upstream advertises for retired models.
func IsListedChatModel(id string) bool {
	if !isSafeModelID(id) {
		return false
	}
	lower := strings.ToLower(id)
	if lower == "gemini" || lower == "claude" {
		return false
	}
	// Upstream keeps retired models in fetchAvailableModels as `<model>-disabled` placeholders so old
	// clients receive an explicit identifier, but they can never serve a completion.
	if strings.HasSuffix(lower, "-disabled") {
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
	if isInternalGemini35Model(lower) {
		return false
	}
	return true
}

// isInternalGemini35Model reports whether an identifier belongs to the Gemini 3.5 IDE-internal family
// that is not a conversational chat model, except for the public flash-lite variant.
func isInternalGemini35Model(lower string) bool {
	return strings.HasPrefix(lower, "gemini-3.5-") && lower != "gemini-3.5-flash-lite"
}

// familyContextWindow returns the documented context window ceiling for a model family, and whether
// a ceiling applies. Gemini 4 Neon exposes 512K instead of the 1M window inherited by other Gemini IDs.
func familyContextWindow(lower string) (int, bool) {
	if strings.HasPrefix(lower, "gemini-4-neon") {
		return 524288, true
	}
	return 0, false
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
		contextWindow := 1048576
		if ceiling, ok := familyContextWindow(lower); ok {
			contextWindow = ceiling
		}
		return CatalogModel{
			ID:              id,
			ContextWindow:   contextWindow,
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

	// AgentModelSorts carries the ordered agent model groups advertised by upstream. It is kept as a
	// raw message because Google has used both string arrays and object arrays for this field, and a
	// strict struct decode would reject the whole catalog when the shape changes.
	AgentModelSorts json.RawMessage `json:"agentModelSorts"`

	// TieredModelIDs advertises models that support categorical reasoning levels regardless of suffix.
	TieredModelIDs json.RawMessage `json:"tieredModelIds"`
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

	// APIProvider names the serving API provider when upstream reports it. The value shape varies
	// between Google releases, so it is decoded generically and inspected as text.
	APIProvider any `json:"apiProvider"`

	// ModelProvider names the model-serving provider family when upstream reports it.
	ModelProvider any `json:"modelProvider"`

	// QuotaInfo describes the remaining quota and reset timestamp.
	QuotaInfo *rawQuotaInfo `json:"quotaInfo"`
}

// rawQuotaInfo extracts fractional quota availability from upstream.
type rawQuotaInfo struct {
	// RemainingFraction is a float between 0.0 and 1.0 indicating remaining capacity.
	RemainingFraction *float64 `json:"remainingFraction"`

	// ResetTime is the ISO-8601/RFC 3339 reset timestamp string.
	ResetTime string `json:"resetTime"`

	// WeeklyQuotaInfo carries the weekly quota window when upstream splits rolling and weekly limits.
	WeeklyQuotaInfo *rawQuotaWindow `json:"weeklyQuotaInfo"`
}

// rawQuotaWindow describes a single quota window (for example the weekly allowance).
type rawQuotaWindow struct {
	// RemainingFraction is the remaining capacity fraction for the window.
	RemainingFraction *float64 `json:"remainingFraction"`

	// ResetTime is the RFC 3339 timestamp when the window resets.
	ResetTime string `json:"resetTime"`
}

// advertisedModelIDs collects model identifiers from agentModelSorts and tieredModelIds.
// The identifiers are used as an authoritative inclusion signal: a model the API explicitly
// advertises for agent use must survive name-based heuristics such as the Gemini 3.5 filter.
func advertisedModelIDs(resp rawCatalogResponse) map[string]struct{} {
	ids := make(map[string]struct{})
	collectRawModelIDs(resp.AgentModelSorts, ids)
	collectRawModelIDs(resp.TieredModelIDs, ids)
	return ids
}

// collectRawModelIDs decodes a raw JSON fragment and recursively gathers identifier-like strings.
func collectRawModelIDs(raw json.RawMessage, ids map[string]struct{}) {
	if len(raw) == 0 {
		return
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return
	}
	collectModelIDValues(decoded, ids, "")
}

// collectModelIDValues walks a decoded JSON value and records model identifiers found under known
// identifier keys. Walking generically keeps catalog parsing resilient to upstream schema changes.
func collectModelIDValues(value any, ids map[string]struct{}, key string) {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if isIdentifierKey(key) && isSafeModelID(trimmed) && strings.Contains(trimmed, "-") {
			ids[strings.ToLower(trimmed)] = struct{}{}
		}
	case []any:
		for _, item := range v {
			collectModelIDValues(item, ids, key)
		}
	case map[string]any:
		for k, item := range v {
			collectModelIDValues(item, ids, k)
		}
	}
}

// isIdentifierKey reports whether a JSON key conventionally carries a model identifier.
func isIdentifierKey(key string) bool {
	switch strings.ToLower(key) {
	case "id", "modelid", "modelids", "models", "model":
		return true
	default:
		return false
	}
}

// providerImpliesNonChat reports whether upstream provider metadata marks a model as non-conversational.
func providerImpliesNonChat(info rawModelInfo) bool {
	provider := strings.ToLower(providerText(info.APIProvider) + " " + providerText(info.ModelProvider))
	if provider == " " {
		return false
	}
	for _, marker := range []string{"image", "imagen", "embedding", "tts", "speech", "veo"} {
		if strings.Contains(provider, marker) {
			return true
		}
	}
	return false
}

// providerText flattens a generically decoded provider value into searchable text.
func providerText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, providerText(item))
		}
		return strings.Join(parts, " ")
	case map[string]any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, providerText(item))
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}

// modelIDsInOrder returns advertised identifiers in a stable order so default-model selection is deterministic.
func modelIDsInOrder(raw json.RawMessage) []string {
	ids := make(map[string]struct{})
	collectRawModelIDs(raw, ids)
	ordered := slices.Sorted(maps.Keys(ids))
	return ordered
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
	advertised := advertisedModelIDs(resp)

	for rawID, info := range resp.Models {
		id := strings.TrimSpace(rawID)
		if !isSafeModelID(id) {
			continue
		}
		lower := strings.ToLower(id)
		// `*-disabled` placeholders and non-conversational providers can never serve completions.
		if strings.HasSuffix(lower, "-disabled") || providerImpliesNonChat(info) {
			continue
		}
		// Explicit API advertisement (agentModelSorts / tieredModelIds) may override only the narrow
		// Gemini 3.5 name heuristic. Internal chat_/tab_ tools, image models, and embedding models
		// stay filtered even if upstream lists them in agent metadata.
		if !IsListedChatModel(id) {
			if _, ok := advertised[lower]; !ok || !isInternalGemini35Model(lower) {
				continue
			}
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
		if ceiling, ok := familyContextWindow(lower); ok {
			model.ContextWindow = min(model.ContextWindow, ceiling)
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
			if info.QuotaInfo.WeeklyQuotaInfo != nil {
				weekly := QuotaWindow{}
				if wf := info.QuotaInfo.WeeklyQuotaInfo.RemainingFraction; wf != nil && *wf >= 0 && *wf <= 1 {
					fraction := *wf
					weekly.RemainingFraction = &fraction
				}
				weekly.ResetTime = strings.TrimSpace(info.QuotaInfo.WeeklyQuotaInfo.ResetTime)
				if !weekly.Untouched() {
					q.Weekly = &weekly
				}
			}
			if q.RemainingFraction != nil || q.ResetTime != "" || q.Weekly != nil {
				snapshot.Quotas[strings.ToLower(id)] = q
			}
		}
	}

	slices.SortFunc(snapshot.Models, func(a, b CatalogModel) int {
		return strings.Compare(a.ID, b.ID)
	})

	snapshot.DefaultModel = resolveDefaultModel(snapshot.Models, resp)

	return snapshot, nil
}

// resolveDefaultModel chooses a default agent model that is actually present in the parsed catalog.
// Upstream reports it directly, but it can point at a filtered or retired identifier, so
// agentModelSorts / tieredModelIds order is used as the next source of truth.
func resolveDefaultModel(models []CatalogModel, resp rawCatalogResponse) string {
	present := make(map[string]struct{}, len(models))
	for _, model := range models {
		present[strings.ToLower(model.ID)] = struct{}{}
	}

	candidate := strings.TrimSpace(resp.DefaultAgentModelID)
	if candidate != "" {
		if _, ok := present[strings.ToLower(candidate)]; ok {
			return candidate
		}
	}

	for _, id := range append(modelIDsInOrder(resp.AgentModelSorts), modelIDsInOrder(resp.TieredModelIDs)...) {
		for _, model := range models {
			if strings.EqualFold(model.ID, id) {
				return model.ID
			}
		}
	}

	if len(models) > 0 {
		return models[0].ID
	}
	return ""
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

	// GeminiWeeklyRemainingFraction holds the minimum remaining weekly allowance across Gemini models.
	GeminiWeeklyRemainingFraction *float64

	// ClaudeWeeklyRemainingFraction holds the minimum remaining weekly allowance across Claude models.
	ClaudeWeeklyRemainingFraction *float64

	// GeminiWeeklyResetTime indicates when the Gemini weekly allowance resets.
	GeminiWeeklyResetTime string

	// ClaudeWeeklyResetTime indicates when the Claude weekly allowance resets.
	ClaudeWeeklyResetTime string
}

// SummarizeQuotas aggregates per-model quotas into Gemini and Claude family remaining fractions and reset times.
// Rolling and weekly windows are tracked separately so operators can tell a 5-hour throttle apart from
// weekly exhaustion. Untouched 100% weekly windows are ignored.
func SummarizeQuotas(quotas map[string]ModelQuota) QuotaSummary {
	var summary QuotaSummary

	// Sort model IDs to guarantee deterministic iteration order
	keys := slices.Sorted(maps.Keys(quotas))

	for _, id := range keys {
		q := quotas[id]
		if q.RemainingFraction == nil && q.ResetTime == "" && q.Weekly == nil {
			continue
		}

		lower := strings.ToLower(id)
		if strings.HasPrefix(lower, "gemini") {
			summary.GeminiRemainingFraction, summary.GeminiResetTime = foldQuota(
				summary.GeminiRemainingFraction, summary.GeminiResetTime, q.RemainingFraction, q.ResetTime)
			if q.Weekly != nil && !q.Weekly.Untouched() {
				summary.GeminiWeeklyRemainingFraction, summary.GeminiWeeklyResetTime = foldQuota(
					summary.GeminiWeeklyRemainingFraction, summary.GeminiWeeklyResetTime, q.Weekly.RemainingFraction, q.Weekly.ResetTime)
			}
		} else if config.IsClaude(lower) || strings.HasPrefix(lower, "gpt-oss") {
			summary.ClaudeRemainingFraction, summary.ClaudeResetTime = foldQuota(
				summary.ClaudeRemainingFraction, summary.ClaudeResetTime, q.RemainingFraction, q.ResetTime)
			if q.Weekly != nil && !q.Weekly.Untouched() {
				summary.ClaudeWeeklyRemainingFraction, summary.ClaudeWeeklyResetTime = foldQuota(
					summary.ClaudeWeeklyRemainingFraction, summary.ClaudeWeeklyResetTime, q.Weekly.RemainingFraction, q.Weekly.ResetTime)
			}
		}
	}

	return summary
}

// foldQuota merges a quota fraction/reset pair into a running minimum, preferring the reset time that
// belongs to the most constrained fraction.
func foldQuota(current *float64, currentReset string, fraction *float64, reset string) (*float64, string) {
	if fraction != nil {
		if current == nil || *fraction < *current {
			if reset != "" {
				return fraction, reset
			}
			return fraction, currentReset
		}
		return current, currentReset
	}
	if currentReset == "" && reset != "" {
		return current, reset
	}
	return current, currentReset
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
