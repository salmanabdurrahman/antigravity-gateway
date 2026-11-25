// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/translate"
)

const (
	// recipeCacheTTL bounds how long a recorded thought-signature recipe may be replayed upstream. The
	// window is short because a stale signature belongs to a conversation turn the model no longer has
	// in context.
	recipeCacheTTL = 2 * time.Hour

	// recipeCacheMaxSessions bounds the number of tracked conversations so a long-running daemon cannot
	// grow the cache without limit.
	recipeCacheMaxSessions = 512
)

// sessionIDPrefix marks identifiers derived by the gateway rather than supplied by the client.
const sessionIDPrefix = "agw-"

// maxSessionIDLength bounds an accepted session identifier. Client-supplied affinity headers reach the
// cache verbatim, so an oversized value must never become a cache key.
const maxSessionIDLength = 128

// recipeEntry holds one session's replayable reasoning details and its expiry.
type recipeEntry struct {
	// details are the reasoning_details payloads recorded from the last upstream response.
	details []translate.ReasoningDetail

	// expires is the instant after which the entry must not be replayed.
	expires time.Time
}

// recipeCache keeps the most recent reasoning recipe per session.
//
// Protocol Quirk: Gemini and Claude both reject tool-execution turns whose functionCall is not
// accompanied by its originating thoughtSignature, and the signature only reaches the client through
// reasoning_details. Clients that strip that field between turns would break every tool loop, so the
// gateway replays the last recorded recipe for the session from this bounded, short-lived cache.
type recipeCache struct {
	// mu guards concurrent access to entries.
	mu sync.Mutex

	// entries maps a session identifier to its recorded recipe.
	entries map[string]recipeEntry

	// ttl bounds entry lifetime.
	ttl time.Duration

	// maxEntries bounds the tracked session count.
	maxEntries int

	// nowFn provides the current time, injectable for deterministic tests.
	nowFn func() time.Time
}

// newRecipeCache constructs a bounded, TTL-expiring reasoning recipe cache.
func newRecipeCache(nowFn func() time.Time) *recipeCache {
	if nowFn == nil {
		nowFn = time.Now
	}
	return &recipeCache{
		entries:    make(map[string]recipeEntry),
		ttl:        recipeCacheTTL,
		maxEntries: recipeCacheMaxSessions,
		nowFn:      nowFn,
	}
}

// Store records the reasoning details of the latest upstream response for a session, dropping expired
// entries first so the cache stays bounded.
func (c *recipeCache) Store(sessionID string, details []translate.ReasoningDetail) {
	if c == nil || sessionID == "" || len(sessionID) > maxSessionIDLength || len(details) == 0 {
		return
	}

	now := c.nowFn()
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = make(map[string]recipeEntry)
	}
	for id, entry := range c.entries {
		if !entry.expires.After(now) {
			delete(c.entries, id)
		}
	}
	if len(c.entries) >= c.maxEntries {
		c.evictEarliestLocked()
	}

	c.entries[sessionID] = recipeEntry{
		details: slices.Clone(details),
		expires: now.Add(c.ttl),
	}
}

// Get returns a copy of the recorded reasoning details for a session, or nil when absent or expired.
func (c *recipeCache) Get(sessionID string) []translate.ReasoningDetail {
	if c == nil || sessionID == "" || len(sessionID) > maxSessionIDLength {
		return nil
	}

	now := c.nowFn()
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[sessionID]
	if !ok {
		return nil
	}
	if !entry.expires.After(now) {
		delete(c.entries, sessionID)
		return nil
	}
	return slices.Clone(entry.details)
}

// Apply attaches the cached reasoning details to the newest assistant turn when that turn is awaiting tool
// results but lost its reasoning_details, so a stripped client still replays signed thinking parts upstream.
// Only the newest assistant turn is considered: older turns already have their tool results, and the cache
// holds just the latest recipe. A recipe is only replayed when its recorded tool call IDs match the turn's
// calls, so a session identifier collision caused by an identical opening prompt can never inject another
// conversation's signatures.
func (c *recipeCache) Apply(sessionID string, messages []translate.ChatMessage) {
	details := c.Get(sessionID)
	if len(details) == 0 {
		return
	}

	for i := len(messages) - 1; i >= 0; i-- {
		msg := &messages[i]
		if msg.Role != "assistant" {
			continue
		}
		if len(msg.ReasoningDetails) == 0 && len(msg.ToolCalls) > 0 && recipeMatchesToolCalls(details, msg.ToolCalls) {
			msg.ReasoningDetails = details
		}
		return
	}
}

// recipeMatchesToolCalls reports whether the recorded recipe describes exactly the tool calls of the
// assistant turn being replayed. Requiring an exact call-ID set keeps replay safe across conversations
// that share an opening prompt.
func recipeMatchesToolCalls(details []translate.ReasoningDetail, calls []translate.ChatToolCall) bool {
	recipe := translate.ReadRecipe(details)
	if recipe == nil || len(calls) == 0 {
		return false
	}

	wanted := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if call.ID == "" {
			return false
		}
		wanted[call.ID] = struct{}{}
	}

	matched := make(map[string]struct{}, len(wanted))
	for _, step := range recipe.Steps {
		if step.Type != "call" {
			continue
		}
		if _, ok := wanted[step.ID]; !ok {
			return false
		}
		matched[step.ID] = struct{}{}
	}
	return len(matched) == len(wanted)
}

// evictEarliestLocked removes the entry closest to expiry. Callers must hold c.mu.
func (c *recipeCache) evictEarliestLocked() {
	var oldestID string
	var oldestExpiry time.Time
	for id, entry := range c.entries {
		if oldestID == "" || entry.expires.Before(oldestExpiry) {
			oldestID = id
			oldestExpiry = entry.expires
		}
	}
	if oldestID != "" {
		delete(c.entries, oldestID)
	}
}

// deriveSessionID hashes the model and the opening user prompt into a stable, opaque session
// identifier. Hashing only the conversation start keeps every later turn of the same conversation on
// the same identifier even when the client sends no affinity header.
func deriveSessionID(model, openingPrompt string) string {
	if openingPrompt == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(model + "\x00" + openingPrompt))
	return sessionIDPrefix + hex.EncodeToString(sum[:12])
}

// openingChatPrompt renders the system instructions plus the first user turn of an OpenAI conversation
// into a stable seed. Including the system prompt keeps two differently-configured agents that share a
// generic opening message on distinct sessions.
func openingChatPrompt(messages []translate.ChatMessage) string {
	var system, opening string
	for _, msg := range messages {
		switch {
		case msg.Role == "system" && system == "":
			system = renderContent(msg.Content)
		case msg.Role == "user" && opening == "":
			opening = renderContent(msg.Content)
		}
		if system != "" && opening != "" {
			break
		}
	}
	return joinSessionSeed(system, opening)
}

// openingAnthropicPrompt renders the first user turn of an Anthropic conversation into a stable seed.
// Anthropic carries the system prompt outside the message list, so only the opening turn is mixed in.
func openingAnthropicPrompt(messages []translate.AnthropicMessage) string {
	for _, msg := range messages {
		if msg.Role != "user" {
			continue
		}
		return joinSessionSeed("", renderContent(msg.Content))
	}
	return ""
}

// renderContent renders a message payload as stable JSON for hashing.
func renderContent(content any) string {
	encoded, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// joinSessionSeed concatenates the non-empty seed parts. An empty opening user turn yields no seed at all,
// so a request without a conversation start keeps an empty session identifier.
func joinSessionSeed(system, opening string) string {
	if opening == "" {
		return ""
	}
	return system + "\x00" + opening
}
