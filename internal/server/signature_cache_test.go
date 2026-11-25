// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/translate"
)

// TestDeriveSessionIDIsStableAcrossTurns verifies the derived identifier depends only on the model and
// the opening user prompt, so later turns of one conversation keep the same session.
func TestDeriveSessionIDIsStableAcrossTurns(t *testing.T) {
	opening := openingChatPrompt([]translate.ChatMessage{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "list the files"},
		{Role: "assistant", Content: "..."},
	})
	if opening == "" {
		t.Fatal("expected the opening user prompt to be captured")
	}

	first := deriveSessionID("gemini-3.8-pro", opening)
	second := deriveSessionID("gemini-3.8-pro", opening)
	if first == "" || first != second {
		t.Fatalf("expected stable session identifier, got %q then %q", first, second)
	}

	// A later turn adds messages but the opening prompt is unchanged, so the identifier must match.
	later := openingChatPrompt([]translate.ChatMessage{
		{Role: "system", Content: "be terse"},
		{Role: "user", Content: "list the files"},
		{Role: "assistant", Content: "a.go"},
		{Role: "user", Content: "now the tests"},
	})
	if got := deriveSessionID("gemini-3.8-pro", later); got != first {
		t.Errorf("expected identical identifier across turns, got %q vs %q", got, first)
	}

	if other := deriveSessionID("claude-sonnet-4-6", opening); other == first {
		t.Error("expected different models to derive different sessions")
	}
	if empty := deriveSessionID("gemini-3.8-pro", ""); empty != "" {
		t.Errorf("expected empty identifier for an empty prompt, got %q", empty)
	}
}

// TestDeriveSessionIDSeparatesSystemPrompts verifies that two agents sharing a generic opening message
// but carrying different system instructions land on different sessions.
func TestDeriveSessionIDSeparatesSystemPrompts(t *testing.T) {
	first := openingChatPrompt([]translate.ChatMessage{
		{Role: "system", Content: "You are agent alpha."},
		{Role: "user", Content: "hello"},
	})
	second := openingChatPrompt([]translate.ChatMessage{
		{Role: "system", Content: "You are agent beta."},
		{Role: "user", Content: "hello"},
	})
	if first == second {
		t.Fatal("expected different system prompts to produce different seeds")
	}
	if deriveSessionID("gemini-3.8-pro", first) == deriveSessionID("gemini-3.8-pro", second) {
		t.Error("expected different session identifiers for differently configured agents")
	}
}

// TestRecipeCacheStoreAndExpire verifies recorded recipes are replayable within the TTL and dropped after.
func TestRecipeCacheStoreAndExpire(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	cache := newRecipeCache(func() time.Time { return now })

	details := []translate.ReasoningDetail{{Type: translate.DetailType, ID: translate.DetailID, Format: translate.RecipeFormat, Data: `{"model":"m","steps":[{"t":"call","id":"c1","sig":"sig"}]}`}}
	cache.Store("session-a", details)

	if got := cache.Get("session-a"); len(got) != 1 {
		t.Fatalf("expected cached recipe, got %d entries", len(got))
	}

	now = now.Add(recipeCacheTTL + time.Minute)
	if got := cache.Get("session-a"); len(got) != 0 {
		t.Errorf("expected expired recipe dropped, got %d entries", len(got))
	}

	cache.Store("", details)
	if got := cache.Get(""); got != nil {
		t.Error("expected empty session identifier to be ignored")
	}
}

// TestRecipeCacheBoundsSessions verifies the cache evicts the entry closest to expiry once full.
func TestRecipeCacheBoundsSessions(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	cache := newRecipeCache(func() time.Time { return now })
	cache.maxEntries = 2

	details := []translate.ReasoningDetail{{Type: translate.DetailType, ID: translate.DetailID, Format: translate.RecipeFormat, Data: "{}"}}
	cache.Store("first", details)
	now = now.Add(time.Minute)
	cache.Store("second", details)
	now = now.Add(time.Minute)
	cache.Store("third", details)

	if len(cache.entries) != 2 {
		t.Fatalf("expected 2 bounded entries, got %d", len(cache.entries))
	}
	if got := cache.Get("first"); len(got) != 0 {
		t.Error("expected the earliest-expiring session evicted")
	}
	if got := cache.Get("third"); len(got) == 0 {
		t.Error("expected the newest session retained")
	}
}

// TestRecipeCacheApplyAttachesMissingDetails verifies a stripped client still replays the recorded
// recipe, while an already present reasoning_details payload is left untouched.
func TestRecipeCacheApplyAttachesMissingDetails(t *testing.T) {
	cache := newRecipeCache(nil)
	details := []translate.ReasoningDetail{{Type: translate.DetailType, ID: translate.DetailID, Format: translate.RecipeFormat, Data: `{"model":"m","steps":[{"t":"call","id":"c1","sig":"sig"}]}`}}
	cache.Store("session-b", details)

	messages := []translate.ChatMessage{
		{Role: "user", Content: "start"},
		{Role: "assistant", Content: "", ToolCalls: []translate.ChatToolCall{{ID: "c1", Type: "function", Function: translate.ChatFunctionCall{Name: "edit", Arguments: `{}`}}}},
		{Role: "tool", ToolCallID: "c1", Content: "done"},
	}
	cache.Apply("session-b", messages)
	if len(messages[1].ReasoningDetails) != 1 {
		t.Fatalf("expected recipe replayed onto the assistant tool turn, got %d", len(messages[1].ReasoningDetails))
	}

	explicit := []translate.ReasoningDetail{{Type: "custom", ID: "client", Format: "client.v1", Data: "{}"}}
	withDetails := []translate.ChatMessage{
		{Role: "user", Content: "start"},
		{Role: "assistant", ReasoningDetails: explicit, ToolCalls: []translate.ChatToolCall{{ID: "c1", Type: "function", Function: translate.ChatFunctionCall{Name: "edit", Arguments: `{}`}}}},
	}
	cache.Apply("session-b", withDetails)
	if len(withDetails[1].ReasoningDetails) != 1 || withDetails[1].ReasoningDetails[0].ID != "client" {
		t.Errorf("expected client-supplied reasoning details preserved, got %+v", withDetails[1].ReasoningDetails)
	}

	// A cached recipe whose tool call IDs do not match the turn must never be replayed, even when the
	// session identifier collides because two conversations opened with the same prompt.
	foreign := []translate.ChatMessage{
		{Role: "assistant", ToolCalls: []translate.ChatToolCall{{ID: "other-call", Type: "function", Function: translate.ChatFunctionCall{Name: "edit", Arguments: `{}`}}}},
	}
	cache.Apply("session-b", foreign)
	if len(foreign[0].ReasoningDetails) != 0 {
		t.Error("expected a foreign recipe to be rejected by tool call ID mismatch")
	}

	// No cache entry means nothing is injected.
	untouched := []translate.ChatMessage{{Role: "assistant", ToolCalls: []translate.ChatToolCall{{ID: "c9"}}}}
	cache.Apply("unknown-session", untouched)
	if len(untouched[0].ReasoningDetails) != 0 {
		t.Error("expected no injection without a cached recipe")
	}
}
