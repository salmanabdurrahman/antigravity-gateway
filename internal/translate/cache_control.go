// Package translate provides bidirectional translation between OpenAI/Anthropic
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"maps"
	"strings"
)

// CleanCacheControl recursively traverses data structures (maps, slices) and strips
// all "cache_control" keys. It includes a zero-allocation fast-path when no "cache_control" keys exist.
//
// Protocol Quirk: Claude Code CLI and modern coding agents inject ephemeral cache control
// markers (e.g. {"cache_control": {"type": "ephemeral"}}). Google Cloud Code Assist rejects
// any unexpected keys in message blocks with HTTP 400 "Extra inputs are not permitted".
// Cleaning cache_control proactively eliminates these failures.
func CleanCacheControl(data any) any {
	if !hasCacheControl(data) {
		return data
	}
	return cleanCacheControlRecursive(data)
}

// hasCacheControl inspects maps and slices without allocating to detect cache_control keys.
func hasCacheControl(data any) bool {
	switch v := data.(type) {
	case map[string]any:
		for k, val := range v {
			if k == "cache_control" || hasCacheControl(val) {
				return true
			}
		}
	case []any:
		for _, val := range v {
			if hasCacheControl(val) {
				return true
			}
		}
	}
	return false
}

// cleanCacheControlRecursive strips cache_control keys recursively from maps and slices.
func cleanCacheControlRecursive(data any) any {
	switch v := data.(type) {
	case map[string]any:
		cleaned := make(map[string]any, len(v))
		for k, val := range v {
			if k == "cache_control" {
				continue
			}
			cleaned[k] = cleanCacheControlRecursive(val)
		}
		return cleaned
	case []any:
		cleaned := make([]any, len(v))
		for i, val := range v {
			cleaned[i] = cleanCacheControlRecursive(val)
		}
		return cleaned
	default:
		return data
	}
}

// StripBillingHeader removes Claude Code's billing pseudo-header from text prompts.
//
// Protocol Quirk: Claude Code injects lines such as
// "x-anthropic-billing-header: cc_version=...; cc_entrypoint=...".
// Cloud Code API can reject requests containing this line with 429 RESOURCE_EXHAUSTED
// even when quota remains.
func StripBillingHeader(text string) string {
	if text == "" {
		return ""
	}

	// Split by newline and omit any line starting with x-anthropic-billing-header:
	var out []string
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			line := text[start:i]
			if !isBillingHeaderLine(line) {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	if start < len(text) {
		line := text[start:]
		if !isBillingHeaderLine(line) {
			out = append(out, line)
		}
	}

	return strings.Join(out, "\n")
}

// isBillingHeaderLine checks if a single line matches the x-anthropic-billing-header pattern.
func isBillingHeaderLine(line string) bool {
	trimmed := line
	for len(trimmed) > 0 && (trimmed[0] == ' ' || trimmed[0] == '\t' || trimmed[0] == '\r') {
		trimmed = trimmed[1:]
	}
	if len(trimmed) < 27 { // len("x-anthropic-billing-header:") == 27
		return false
	}
	prefix := trimmed[:27]
	for i := 0; i < len(prefix); i++ {
		c1 := prefix[i]
		c2 := "x-anthropic-billing-header:"[i]
		if c1 >= 'A' && c1 <= 'Z' {
			c1 += 'a' - 'A'
		}
		if c1 != c2 {
			return false
		}
	}
	return true
}

// neutralAgentIdentityLine replaces third-party agent identity lines with a neutral
// instruction so the system prompt keeps an identity sentence upstream.
const neutralAgentIdentityLine = "You are a coding assistant."

// thirdPartyAgentIdentityPrefixes lists lowercase line prefixes identifying caller agent
// fingerprints that Cloud Code Assist rejects with a content-scoped 429 RESOURCE_EXHAUSTED.
var thirdPartyAgentIdentityPrefixes = []string{
	"you are claude code",
	"you are a claude agent",
	"you are hermes",
	"you are cline",
	"you are opencode",
	"you are openclaw",
}

// ScrubAgentMarkers strips Claude Code billing pseudo-headers and replaces third-party agent
// identity lines with a neutral identity sentence.
//
// Protocol Quirk: Antigravity rejects system instructions that fingerprint competing agent
// products (Claude Code, Anthropic, Hermes, Cline, OpenCode, OpenClaw) with a bare
// 429 RESOURCE_EXHAUSTED even when account quota is full. Rewriting just the identity line keeps
// the rest of the caller's system prompt intact while avoiding the content-scoped rejection.
func ScrubAgentMarkers(text string) string {
	if text == "" {
		return ""
	}

	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if isBillingHeaderLine(line) {
			continue
		}
		if isThirdPartyAgentIdentityLine(line) {
			out = append(out, neutralAgentIdentityLine)
			continue
		}
		out = append(out, line)
	}

	return strings.Join(out, "\n")
}

// isThirdPartyAgentIdentityLine reports whether a single line is a known third-party agent
// identity sentence. Matching is restricted to the beginning of the trimmed line so ordinary
// prose that merely mentions a product name is left untouched.
func isThirdPartyAgentIdentityLine(line string) bool {
	trimmed := strings.ToLower(strings.TrimLeft(line, " \t\r"))
	for _, prefix := range thirdPartyAgentIdentityPrefixes {
		if strings.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// CleanCacheControlMessages deep-cleans cache_control keys from a slice of message maps.
func CleanCacheControlMessages(messages []any) []any {
	if len(messages) == 0 {
		return nil
	}
	result := make([]any, len(messages))
	for i, m := range messages {
		result[i] = CleanCacheControl(m)
	}
	return result
}

// ShallowCopyWithoutCacheControl returns a copy of a map with cache_control removed.
func ShallowCopyWithoutCacheControl(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	cp := maps.Clone(m)
	delete(cp, "cache_control")
	return cp
}
