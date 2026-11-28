// Package translate provides bidirectional translation between OpenAI Chat Completions
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"slices"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
)

// TestApplyPromptPreflightRewritesHarnessFingerprints verifies each rule removes its fingerprint while
// leaving surrounding instructions intact.
func TestApplyPromptPreflightRewritesHarnessFingerprints(t *testing.T) {
	input := strings.Join([]string{
		"You are opencode, an interactive CLI tool that helps users with software tasks.",
		"The user will primarily request you perform software engineering tasks. Use the tools below.",
		"Always run tests before finishing.",
		"Tool names include tools.opencode.session_move for continuity.",
	}, "\n")

	rewritten, result := ApplyPromptPreflight(input, false)

	if !result.Changed {
		t.Fatal("expected the preflight pass to change the instruction")
	}
	for _, want := range []PreflightRuleID{PreflightOpenCodeBanner, PreflightClaudeCodePreamble, PreflightSessionMoveAnchor} {
		if !slices.Contains(result.Applied, want) {
			t.Errorf("applied rules = %v; want %q", result.Applied, want)
		}
	}
	for _, forbidden := range []string{"You are opencode", "The user will primarily request", "tools.opencode.session_move"} {
		if strings.Contains(rewritten, forbidden) {
			t.Errorf("rewritten instruction still contains %q:\n%s", forbidden, rewritten)
		}
	}
	for _, preserved := range []string{preflightReplacementIdentity, "Always run tests before finishing.", "internal_session_move"} {
		if !strings.Contains(rewritten, preserved) {
			t.Errorf("rewritten instruction lost %q:\n%s", preserved, rewritten)
		}
	}
}

// TestApplyPromptPreflightPreservesTerminalNewline verifies a banner that ends the instruction is replaced
// without adding a trailing newline.
func TestApplyPromptPreflightPreservesTerminalNewline(t *testing.T) {
	rewritten, result := ApplyPromptPreflight("Be concise.\nYou are opencode, an interactive CLI tool.", false)
	if !result.Changed {
		t.Fatal("expected the banner to be rewritten")
	}
	if strings.HasSuffix(rewritten, "\n") {
		t.Errorf("rewrite added a trailing newline: %q", rewritten)
	}
	if !strings.HasSuffix(rewritten, preflightReplacementIdentity) {
		t.Errorf("rewrite did not end with the neutral identity: %q", rewritten)
	}
}

// TestApplyPromptPreflightIsIdempotent verifies a second pass over its own output changes nothing, which keeps
// retries and fallback hops from re-introducing a fingerprint.
func TestApplyPromptPreflightIsIdempotent(t *testing.T) {
	input := "You are opencode, an interactive CLI tool.\nKeep answers short."

	first, firstResult := ApplyPromptPreflight(input, false)
	if !firstResult.Changed {
		t.Fatal("expected the first pass to rewrite the banner")
	}

	second, secondResult := ApplyPromptPreflight(first, false)
	if secondResult.Changed || len(secondResult.Applied) != 0 {
		t.Errorf("second pass changed the text again: %+v", secondResult)
	}
	if second != first {
		t.Errorf("second pass altered the text:\nfirst:  %q\nsecond: %q", first, second)
	}
}

// TestApplyPromptPreflightLeavesOrdinaryProse verifies patterns stay anchored to the start of a line so a
// passing mention of a product name survives.
func TestApplyPromptPreflightLeavesOrdinaryProse(t *testing.T) {
	input := "Note: the user will primarily request you perform software engineering tasks in the UI, not here.\nMention tools.opencode.session_move only in docs? no."
	rewritten, result := ApplyPromptPreflight(input, false)

	// The session-move token rule is intentionally not line-anchored: that literal anywhere in the system
	// instruction is the fingerprint upstream rejects.
	if !result.Changed {
		t.Fatalf("expected the session_move anchor to be neutralized")
	}
	if !strings.Contains(rewritten, "Note: the user will primarily request you perform software engineering tasks in the UI, not here.") {
		t.Errorf("ordinary prose was rewritten:\n%s", rewritten)
	}
}

// TestApplyPromptPreflightDisabled verifies the operator escape hatch leaves the instruction untouched.
func TestApplyPromptPreflightDisabled(t *testing.T) {
	input := "You are opencode, an interactive CLI tool."
	rewritten, result := ApplyPromptPreflight(input, true)

	if result.Changed || len(result.Applied) != 0 {
		t.Errorf("disabled preflight still changed the text: %+v", result)
	}
	if rewritten != input {
		t.Errorf("disabled preflight returned %q; want the input", rewritten)
	}
}

// TestApplyPromptPreflightRejectsOverreach verifies the fail-safe: when a rule would remove more of the
// instruction than the budget allows, the original text is returned and the rule is reported as rejected.
func TestApplyPromptPreflightRejectsOverreach(t *testing.T) {
	input := "The user will primarily request you perform software engineering tasks. Use the tools. Go."

	// A zero budget rejects any removal without touching package state.
	rewritten, rejected := applyPromptPreflight(input, false, PreflightLimits{MaxRemovedFraction: 0})
	if rejected.Changed {
		t.Error("overreaching rewrite was accepted")
	}
	if !slices.Contains(rejected.Rejected, PreflightClaudeCodePreamble) {
		t.Errorf("rejected rules = %v; want the preamble rule", rejected.Rejected)
	}
	if rewritten != input {
		t.Errorf("overreaching rewrite returned modified text:\n%s", rewritten)
	}
}

// TestScrubSystemInstructionCombinesPasses verifies billing headers, harness fingerprints, and identity lines
// are all handled by the single entry point both request builders use.
func TestScrubSystemInstructionCombinesPasses(t *testing.T) {
	input := strings.Join([]string{
		"x-anthropic-billing-header: cc_version=2.0",
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are opencode, an interactive CLI tool.",
		"Be concise.",
	}, "\n")

	rewritten, result := ScrubSystemInstruction(input, false)

	if !result.Changed {
		t.Fatal("expected the combined scrub to change the instruction")
	}
	for _, forbidden := range []string{"x-anthropic-billing-header", "You are Claude Code", "You are opencode"} {
		if strings.Contains(rewritten, forbidden) {
			t.Errorf("combined scrub left %q in place:\n%s", forbidden, rewritten)
		}
	}
	if !strings.Contains(rewritten, "Be concise.") {
		t.Errorf("combined scrub dropped the caller instruction:\n%s", rewritten)
	}
}

// TestBuildRequestAppliesPreflight verifies the OpenAI path rewrites the final system instruction and honors
// the disable flag.
func TestBuildRequestAppliesPreflight(t *testing.T) {
	req := &ChatRequest{
		Model: "gemini-3.8-flash-high",
		Messages: []ChatMessage{
			{Role: "system", Content: "You are opencode, an interactive CLI tool.\nThe user will primarily request you perform software engineering tasks.\nBe concise."},
			{Role: "user", Content: "Hi"},
		},
	}

	info := catalog.FamilyDefaults(req.Model)
	geminiReq, err := BuildRequest(req, BuildOptions{Info: info})
	if err != nil {
		t.Fatalf("BuildRequest failed: %v", err)
	}
	system := systemInstructionText(t, geminiReq)
	for _, forbidden := range []string{"You are opencode", "The user will primarily request"} {
		if strings.Contains(system, forbidden) {
			t.Errorf("system instruction still contains %q:\n%s", forbidden, system)
		}
	}
	if !strings.Contains(system, "Be concise.") {
		t.Errorf("system instruction lost the caller text:\n%s", system)
	}

	// The disable flag only skips harness-fingerprint rewrites; billing and identity scrubbing stay active,
	// so the banner line still becomes the neutral identity while the preamble survives.
	disabled, err := BuildRequest(req, BuildOptions{Info: info, DisablePromptPreflight: true})
	if err != nil {
		t.Fatalf("BuildRequest with disabled preflight failed: %v", err)
	}
	if system := systemInstructionText(t, disabled); !strings.Contains(system, "The user will primarily request") {
		t.Errorf("disabled preflight still rewrote the preamble:\n%s", system)
	}
}

// TestBuildAnthropicRequestAppliesPreflight verifies the Anthropic path applies the same rewrites for both
// string and structured system blocks.
func TestBuildAnthropicRequestAppliesPreflight(t *testing.T) {
	req := &AnthropicRequest{
		Model: "claude-sonnet-4-5",
		System: []any{
			map[string]any{"type": "text", "text": "You are opencode, an interactive CLI tool.\nThe user will primarily request you perform software engineering tasks."},
			map[string]any{"type": "text", "text": "Be concise."},
		},
		Messages:  []AnthropicMessage{{Role: "user", Content: "Hi"}},
		MaxTokens: 64,
	}

	info := catalog.FamilyDefaults(req.Model)
	geminiReq, err := BuildAnthropicRequest(req, BuildOptions{Info: info})
	if err != nil {
		t.Fatalf("BuildAnthropicRequest failed: %v", err)
	}
	system := systemInstructionText(t, geminiReq)
	for _, forbidden := range []string{"You are opencode", "The user will primarily request"} {
		if strings.Contains(system, forbidden) {
			t.Errorf("anthropic system instruction still contains %q:\n%s", forbidden, system)
		}
	}
	if !strings.Contains(system, "Be concise.") {
		t.Errorf("anthropic system instruction lost the caller text:\n%s", system)
	}
}

// systemInstructionText returns the concatenated system instruction text for assertions.
func systemInstructionText(t *testing.T, req *GeminiRequest) string {
	t.Helper()

	if req == nil || req.SystemInstruction == nil {
		t.Fatal("expected a system instruction on the translated request")
	}
	parts := make([]string, 0, len(req.SystemInstruction.Parts))
	for _, part := range req.SystemInstruction.Parts {
		parts = append(parts, part.Text)
	}
	return strings.Join(parts, "\n")
}
