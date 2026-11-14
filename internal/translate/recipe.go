// Package translate provides bidirectional translation between OpenAI Chat Completions
// payloads and Google Cloud Code Assist (Antigravity) envelope structures.
package translate

import (
	"encoding/json"
	"slices"
)

// RecipeFormat defines the serialization format version tag for recorded thought signatures.
const RecipeFormat = "pi-gemini-auth.v1"

// DetailType defines the encrypted reasoning detail type identifier for agent messages.
const DetailType = "reasoning.encrypted"

// DetailID identifies Antigravity Gateway's reasoning detail container.
const DetailID = "pga"

// RecipeStep represents a single step in a model response part sequence.
type RecipeStep struct {
	// Type indicates the step category ("thought", "text", or "call").
	Type string `json:"t"`

	// Text contains reasoning thoughts or output text chunks.
	Text string `json:"text,omitempty"`

	// ID identifies the tool call if Type is "call".
	ID string `json:"id,omitempty"`

	// Sig is the cryptographic thoughtSignature associated with this step.
	Sig string `json:"sig,omitempty"`
}

// Recipe holds recorded response steps and model metadata for lossless conversation replay.
type Recipe struct {
	// Model records the original model identifier that produced these steps.
	Model string `json:"model"`

	// Steps lists the ordered sequence of thoughts, text, and tool calls.
	Steps []RecipeStep `json:"steps"`
}

// ReasoningDetail represents an encrypted or serialized reasoning payload matching OpenAI reasoning_details.
type ReasoningDetail struct {
	// Type is typically "reasoning.encrypted".
	Type string `json:"type"`

	// ID identifies the container (e.g. "pga").
	ID string `json:"id"`

	// Format specifies the serialization version (e.g. "pi-gemini-auth.v1").
	Format string `json:"format"`

	// Data holds the JSON-serialized Recipe payload.
	Data string `json:"data"`
}

// RecipeDetail packs a Recipe into an OpenAI reasoning_details item.
func RecipeDetail(recipe Recipe) ReasoningDetail {
	dataBytes, _ := json.Marshal(recipe)
	return ReasoningDetail{
		Type:   DetailType,
		ID:     DetailID,
		Format: RecipeFormat,
		Data:   string(dataBytes),
	}
}

// ReadRecipe extracts and parses an AGW Recipe from an assistant message's reasoning_details.
//
// Protocol Quirk: Gemini 3 rejects tool execution turns unless each functionCall is accompanied
// by its exact originating thoughtSignature. Claude similarly requires signed thinking blocks
// restored before tool invocations. Because OpenAI standard schemas lack signature fields,
// AGW serializes the complete turn layout into reasoning_details and unpacks it here.
func ReadRecipe(details []ReasoningDetail) *Recipe {
	for _, d := range details {
		if d.Type != DetailType || d.Format != RecipeFormat || d.Data == "" {
			continue
		}
		var r Recipe
		if err := json.Unmarshal([]byte(d.Data), &r); err != nil {
			continue
		}
		if r.Model == "" || len(r.Steps) == 0 {
			continue
		}
		return &r
	}
	return nil
}

// RecipeBuilder accumulates stream parts and thought signatures during SSE generation.
type RecipeBuilder struct {
	// steps collects the linear sequence of recorded response steps.
	steps []RecipeStep

	// textIndex tracks the slice index of the consolidated text step.
	textIndex int

	// hasText records whether visible output text has been encountered.
	hasText bool
}

// NewRecipeBuilder creates an initialized RecipeBuilder.
func NewRecipeBuilder() *RecipeBuilder {
	return &RecipeBuilder{
		textIndex: -1,
	}
}

// Thought records a reasoning block or thinking token chunk with optional cryptographic signature.
func (b *RecipeBuilder) Thought(text string, sig string) {
	if len(b.steps) > 0 {
		lastIdx := len(b.steps) - 1
		last := &b.steps[lastIdx]
		// Gemini streams thoughts across multiple parts; merge into current block if signatures align.
		if last.Type == "thought" && (last.Sig == "" || sig == "" || last.Sig == sig) {
			last.Text += text
			if sig != "" {
				last.Sig = sig
			}
			return
		}
	}
	b.steps = append(b.steps, RecipeStep{
		Type: "thought",
		Text: text,
		Sig:  sig,
	})
}

// Text records visible output text parts and associates trailing signatures.
func (b *RecipeBuilder) Text(sig string) {
	if !b.hasText {
		b.hasText = true
		b.textIndex = len(b.steps)
		b.steps = append(b.steps, RecipeStep{
			Type: "text",
			Sig:  sig,
		})
		return
	}
	if sig != "" && b.textIndex >= 0 && b.textIndex < len(b.steps) {
		b.steps[b.textIndex].Sig = sig
	}
}

// Call records an outgoing tool execution call with optional signature.
func (b *RecipeBuilder) Call(id string, sig string) {
	b.steps = append(b.steps, RecipeStep{
		Type: "call",
		ID:   id,
		Sig:  sig,
	})
}

// Steps returns a clone of the recorded sequence steps.
func (b *RecipeBuilder) Steps() []RecipeStep {
	return slices.Clone(b.steps)
}

// Useful reports whether the recorded steps contain signatures or thoughts worth replaying.
func (b *RecipeBuilder) Useful() bool {
	for _, s := range b.steps {
		if s.Sig != "" || (s.Type == "thought" && len(s.Text) > 0) {
			return true
		}
	}
	return false
}
