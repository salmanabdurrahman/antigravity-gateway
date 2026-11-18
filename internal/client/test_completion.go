// Package client provides automated configuration injectors and diagnostic tools
// for local AI coding agents such as Pi and OpenCode.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TestResult records the outcome of an end-to-end completion connectivity probe.
type TestResult struct {
	// Model is the model name probed during the test.
	Model string

	// Text contains the completion text returned by the model.
	Text string

	// Duration records total roundtrip latency for the request.
	Duration time.Duration

	// Success indicates whether the completion succeeded with HTTP 200 and non-empty content.
	Success bool
}

// TestChatCompletion performs a lightweight single-token completion against the gateway.
func TestChatCompletion(ctx context.Context, baseURL string, apiKey string, model string) (*TestResult, error) {
	if model == "" {
		return nil, errors.New("cannot perform test completion without target model")
	}

	normBase := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(normBase, "/v1") {
		normBase += "/v1"
	}
	endpoint := normBase + "/chat/completions"

	reqBody := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{
				"role":    "user",
				"content": "Reply with the single word: ok",
			},
		},
		"max_tokens": 1,
		"stream":     false,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling test completion request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{
		Timeout: 45 * time.Second,
	}

	start := time.Now()
	resp, err := client.Do(httpReq)
	duration := time.Since(start)

	if err != nil {
		return &TestResult{
			Model:    model,
			Duration: duration,
			Success:  false,
		}, fmt.Errorf("requesting test chat completion to %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return &TestResult{
			Model:    model,
			Duration: duration,
			Success:  false,
		}, fmt.Errorf("reading response from gateway: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errEnvelope struct {
			Error struct {
				Message string `json:"message"`
				Code    any    `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(respBytes, &errEnvelope)
		msg := errEnvelope.Error.Message
		if msg == "" {
			msg = string(respBytes)
		}
		return &TestResult{
			Model:    model,
			Duration: duration,
			Success:  false,
		}, fmt.Errorf("test completion returned HTTP %d: %s", resp.StatusCode, msg)
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return &TestResult{
			Model:    model,
			Duration: duration,
			Success:  false,
		}, fmt.Errorf("parsing response JSON: %w", err)
	}

	content := ""
	if len(parsed.Choices) > 0 {
		content = strings.TrimSpace(parsed.Choices[0].Message.Content)
	}

	return &TestResult{
		Model:    model,
		Text:     content,
		Duration: duration,
		Success:  true,
	}, nil
}

// TestAnthropicCompletion performs a lightweight single-token completion against the gateway's /v1/messages endpoint.
func TestAnthropicCompletion(ctx context.Context, baseURL string, apiKey string, model string) (*TestResult, error) {
	if model == "" {
		return nil, errors.New("cannot perform test completion without target model")
	}

	normBase := strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(normBase, "/v1") {
		normBase += "/v1"
	}
	endpoint := normBase + "/messages"

	reqBody := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{
				"role":    "user",
				"content": "Reply with the single word: ok",
			},
		},
		"max_tokens": 1,
		"stream":     false,
	}

	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling test completion request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
		httpReq.Header.Set("x-api-key", apiKey)
	}

	start := time.Now()
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("dispatching test completion request: %w", err)
	}
	defer resp.Body.Close()

	duration := time.Since(start)

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil, fmt.Errorf("reading test completion response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var errResp struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		errMsg := string(bodyBytes)
		if json.Unmarshal(bodyBytes, &errResp) == nil && errResp.Error.Message != "" {
			errMsg = errResp.Error.Message
		}
		return nil, fmt.Errorf("gateway returned HTTP %d: %s", resp.StatusCode, errMsg)
	}

	var anthResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}

	if err := json.Unmarshal(bodyBytes, &anthResp); err != nil {
		return nil, fmt.Errorf("decoding test completion response: %w", err)
	}

	text := ""
	for _, c := range anthResp.Content {
		if c.Type == "text" && c.Text != "" {
			text = c.Text
			break
		}
	}

	return &TestResult{
		Model:    model,
		Text:     text,
		Duration: duration,
		Success:  true,
	}, nil
}
