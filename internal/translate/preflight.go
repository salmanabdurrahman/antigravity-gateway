// Package translate provides bidirectional translation between OpenAI Chat Completions
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"regexp"
	"strings"
)

// PreflightRuleID names one deterministic instruction rewrite so callers and tests can refer to a rule
// without handling the prompt text that matched it.
type PreflightRuleID string

const (
	// PreflightOpenCodeBanner rewrites the OpenCode harness banner line to a neutral identity sentence.
	// Cloud Code Assist fingerprints that exact banner and answers with a bare 429 RESOURCE_EXHAUSTED.
	PreflightOpenCodeBanner PreflightRuleID = "wrapper-opencode-banner"

	// PreflightClaudeCodePreamble drops the Claude Code preamble paragraph introducing the software
	// engineering workflow, which upstream fingerprints the same way.
	PreflightClaudeCodePreamble PreflightRuleID = "wrapper-claude-code-preamble"

	// PreflightSessionMoveAnchor neutralizes the literal OpenCode `session_move` tool anchor that upstream
	// rejects inside system instructions.
	PreflightSessionMoveAnchor PreflightRuleID = "wrapper-session-move-anchor"
)

// preflightReplacementIdentity is the neutral sentence substituted for a harness banner line, matching the
// identity rewrite already applied to third-party identity lines.
const preflightReplacementIdentity = "You are a coding assistant."

// PreflightRule pairs one anchored pattern with its deterministic replacement.
type PreflightRule struct {
	// ID names the rule for reporting.
	ID PreflightRuleID

	// Pattern matches the fingerprint inside the instruction text. Patterns are anchored per line so
	// ordinary prose that merely mentions a product name is untouched.
	Pattern *regexp.Regexp

	// Replacement is the text substituted for every match; an empty replacement drops the match.
	Replacement string
}

// defaultPreflightRules holds the harness fingerprints observed to trigger content-scoped rejections.
var defaultPreflightRules = []PreflightRule{
	{
		// The trailing newline is captured so a banner that ends the instruction does not grow one.
		ID:          PreflightOpenCodeBanner,
		Pattern:     regexp.MustCompile(`(?mi)^[ \t]*you are opencode,[^\n]*(\n?)`),
		Replacement: preflightReplacementIdentity + "${1}",
	},
	{
		ID:      PreflightClaudeCodePreamble,
		Pattern: regexp.MustCompile(`(?mi)^[ \t]*the user will primarily request you (?:to )?perform software engineering tasks[^\n]*\n?`),
	},
	{
		ID:          PreflightSessionMoveAnchor,
		Pattern:     regexp.MustCompile(`tools\.opencode\.session_move`),
		Replacement: "internal_session_move",
	},
}

// PreflightLimits bounds how much of an instruction a preflight pass may rewrite before it is rejected.
type PreflightLimits struct {
	// MaxRemovedFraction is the largest share of the original instruction that may be removed. A pass that
	// would remove more is discarded, because a caller's instruction that is mostly harness boilerplate is
	// indistinguishable from a caller whose real instruction happens to match a rule.
	MaxRemovedFraction float64
}

// DefaultPreflightLimits permits dropping up to eighty percent of an instruction's bytes. A caller whose
// system prompt is mostly harness boilerplate is normal, while a pass that removes nearly everything is
// indistinguishable from a rule matching a real instruction, so only that extreme is rejected.
var DefaultPreflightLimits = PreflightLimits{MaxRemovedFraction: 0.8}

// PreflightResult reports what a preflight pass did.
type PreflightResult struct {
	// Applied lists the rules that matched, in evaluation order.
	Applied []PreflightRuleID

	// Rejected lists the rules that matched but were discarded because the rewrite exceeded the limits.
	Rejected []PreflightRuleID

	// Changed reports whether the returned text differs from the input.
	Changed bool
}

// ApplyPromptPreflight rewrites known third-party harness fingerprints out of a system instruction.
//
// Protocol Quirk: Cloud Code Assist fingerprints the instruction stack for competing agent banners and
// returns a bare 429 RESOURCE_EXHAUSTED while quota is intact. Rewriting only the matched lines keeps the
// rest of the caller's instruction intact, and every rule is written so a second pass over its own output
// matches nothing (idempotent), which keeps retries and fallback hops from re-introducing a fingerprint.
func ApplyPromptPreflight(text string, disabled bool) (string, PreflightResult) {
	return applyPromptPreflight(text, disabled, DefaultPreflightLimits)
}

// applyPromptPreflight runs the rules with explicit limits so callers and tests can bound the pass without
// mutating package state.
func applyPromptPreflight(text string, disabled bool, limits PreflightLimits) (string, PreflightResult) {
	var result PreflightResult
	if text == "" || disabled {
		return text, result
	}

	rewritten := text
	var applied []PreflightRuleID
	for _, rule := range defaultPreflightRules {
		if !rule.Pattern.MatchString(rewritten) {
			continue
		}
		rewritten = rule.Pattern.ReplaceAllString(rewritten, rule.Replacement)
		applied = append(applied, rule.ID)
	}

	if len(applied) == 0 {
		return text, result
	}

	if exceedsPreflightLimits(text, rewritten, limits) {
		result.Rejected = applied
		return text, result
	}

	result.Applied = applied
	result.Changed = true
	return rewritten, result
}

// exceedsPreflightLimits reports whether a rewrite removed more of the instruction than the configured
// budget allows, or emptied an instruction that was not empty before.
func exceedsPreflightLimits(original, rewritten string, limits PreflightLimits) bool {
	if strings.TrimSpace(rewritten) == "" {
		return true
	}
	originalLen := len(strings.TrimSpace(original))
	if originalLen == 0 {
		return false
	}
	removed := originalLen - len(strings.TrimSpace(rewritten))
	if removed <= 0 {
		return false
	}
	return float64(removed)/float64(originalLen) > limits.MaxRemovedFraction
}

// ScrubSystemInstruction applies the full instruction preflight in a stable order: harness fingerprint
// rewrites first, then billing pseudo-headers and third-party identity lines. Running the fingerprint pass
// first keeps its rule IDs meaningful, because the identity rewrite would otherwise mask the banner line.
func ScrubSystemInstruction(text string, disabled bool) (string, PreflightResult) {
	rewritten, result := ApplyPromptPreflight(text, disabled)
	return ScrubAgentMarkers(rewritten), result
}
