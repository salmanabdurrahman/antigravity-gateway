//go:build integration

// Package integration_test provides end-to-end integration tests for Antigravity Gateway.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// upstreamSystemCapture records the system instruction the gateway sent upstream.
type upstreamSystemCapture struct {
	// mu guards the captured text.
	mu sync.Mutex
	// text is the concatenated systemInstruction text of the last generation request.
	text string
}

// record stores the captured system instruction text.
func (c *upstreamSystemCapture) record(text string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.text = text
}

// snapshot returns the last captured system instruction text.
func (c *upstreamSystemCapture) snapshot() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.text
}

// TestGatewayPreflightStripsHarnessFingerprints verifies end to end that a caller system prompt carrying
// known harness fingerprints reaches upstream rewritten: the banner is neutralized, the wrapper preamble is
// dropped, and the tool anchor is renamed, while the real instruction survives.
func TestGatewayPreflightStripsHarnessFingerprints(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	capture := &upstreamSystemCapture{}

	gateway := newPacingGateway(t, nil, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "reading body", http.StatusBadRequest)
			return
		}
		capture.record(extractSystemInstruction(t, raw))

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}]}}\n\n"))
	})

	systemPrompt := strings.Join([]string{
		"You are opencode, an interactive CLI tool that helps users with software tasks.",
		"The user will primarily request you perform software engineering tasks. Use the tools below.",
		"Tool names include tools.opencode.session_move for continuity.",
		"Be concise and always run the test suite.",
	}, "\n")

	payload, err := json.Marshal(map[string]any{
		"model": "gemini-3.1-pro-high",
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": "ship it"},
		},
		"stream": false,
	})
	if err != nil {
		t.Fatalf("marshaling chat request: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.url+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("building chat request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+gateway.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dispatching chat request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200, body: %s", resp.StatusCode, body)
	}

	upstream := capture.snapshot()
	if upstream == "" {
		t.Fatal("upstream received no system instruction")
	}
	for _, forbidden := range []string{"You are opencode", "The user will primarily request", "tools.opencode.session_move"} {
		if strings.Contains(upstream, forbidden) {
			t.Errorf("upstream system instruction still contains %q:\n%s", forbidden, upstream)
		}
	}
	for _, preserved := range []string{"You are a coding assistant.", "internal_session_move", "Be concise and always run the test suite."} {
		if !strings.Contains(upstream, preserved) {
			t.Errorf("upstream system instruction lost %q:\n%s", preserved, upstream)
		}
	}
}

// extractSystemInstruction pulls the systemInstruction text out of a Cloud Code Assist envelope, accepting
// both the direct and nested request shapes the gateway can produce.
func extractSystemInstruction(t *testing.T, raw []byte) string {
	t.Helper()

	var envelope struct {
		SystemInstruction *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"systemInstruction"`
		Request struct {
			SystemInstruction *struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"systemInstruction"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decoding upstream envelope: %v", err)
	}

	instruction := envelope.SystemInstruction
	if instruction == nil {
		instruction = envelope.Request.SystemInstruction
	}
	if instruction == nil {
		return ""
	}

	parts := make([]string, 0, len(instruction.Parts))
	for _, part := range instruction.Parts {
		parts = append(parts, part.Text)
	}
	return strings.Join(parts, "\n")
}
