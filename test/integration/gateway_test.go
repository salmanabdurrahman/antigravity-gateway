//go:build integration

// Package integration_test provides end-to-end integration tests for Antigravity Gateway.
package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/server"
)

// TestGatewayMultiAccountFailoverAndStreaming verifies that:
// 1. Account A encounters an upstream HTTP 429 rate limit with RetryInfo resetTime.
// 2. Gateway transparently selects Account B without interrupting client connection.
// 3. Client receives HTTP 200 and a continuous, unbroken SSE stream.
func TestGatewayMultiAccountFailoverAndStreaming(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	var accountACalls atomic.Int64
	var accountBCalls atomic.Int64

	// 1. Spawn Mock Google Cloud Code Assist Server
	mockGoogle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Handle token refresh
		if r.URL.Path == "/token" {
			_ = r.ParseForm()
			refreshToken := r.FormValue("refresh_token")
			accessToken := "token-for-" + refreshToken

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": accessToken,
				"token_type":   "Bearer",
				"expires_in":   3600,
			})
			return
		}

		// Handle streamGenerateContent
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			bodyBytes, _ := io.ReadAll(r.Body)
			var payload struct {
				Project string `json:"project"`
			}
			_ = json.Unmarshal(bodyBytes, &payload)

			if payload.Project == "proj-a" {
				accountACalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{
						"code":    429,
						"status":  "RESOURCE_EXHAUSTED",
						"message": "Quota exceeded for project proj-a",
						"details": []any{
							map[string]any{
								"@type":      "type.googleapis.com/google.rpc.RetryInfo",
								"retryDelay": "60s",
							},
						},
					},
				})
				return
			}

			if payload.Project == "proj-b" {
				accountBCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.WriteHeader(http.StatusOK)

				flusher, _ := w.(http.Flusher)

				// 1. Thinking delta chunk
				fmt.Fprintf(w, "data: %s\n\n", `{"response":{"candidates":[{"content":{"parts":[{"text":"Thinking about failover resolution...","thought":true}]}}]}}`)
				if flusher != nil {
					flusher.Flush()
				}

				// 2. Text delta chunk
				fmt.Fprintf(w, "data: %s\n\n", `{"response":{"candidates":[{"content":{"parts":[{"text":"Hello from Account B after transparent failover!"}]}}]}}`)
				if flusher != nil {
					flusher.Flush()
				}

				// 3. Usage metadata chunk
				fmt.Fprintf(w, "data: %s\n\n", `{"response":{"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":8,"totalTokenCount":20}}}`)
				if flusher != nil {
					flusher.Flush()
				}
				return
			}

			http.Error(w, "Unknown project: "+payload.Project, http.StatusBadRequest)
			return
		}

		http.NotFound(w, r)
	}))
	defer mockGoogle.Close()

	// 2. Set up Gateway Server environment
	tempDir := t.TempDir()
	cfg := &config.Config{
		Host:      "127.0.0.1",
		Port:      0, // Listener will allocate free port
		APIKey:    "agw_test_secret_key",
		Strategy:  "sticky",
		TokenURL:  mockGoogle.URL + "/token",
		Endpoints: []string{mockGoogle.URL},
	}

	accFile := filepath.Join(tempDir, "accounts.json")
	store := auth.NewStore(accFile)

	// Save Account A and Account B
	_, _ = store.Upsert(&auth.Account{
		Email:        "account-a@example.com",
		RefreshToken: "ref-token-a",
		ProjectID:    "proj-a",
		AddedAt:      "2026-01-01T00:00:00Z",
	})
	_, _ = store.Upsert(&auth.Account{
		Email:        "account-b@example.com",
		RefreshToken: "ref-token-b",
		ProjectID:    "proj-b",
		AddedAt:      "2026-01-01T00:00:01Z",
	})

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := rotator.NewPool(store, cfg, time.Now)
	if err != nil {
		t.Fatalf("failed creating account pool: %v", err)
	}

	rot := rotator.NewRotator(pool, cfg, logger, time.Now)
	cacheFile := filepath.Join(tempDir, "models-cache.json")
	cat := catalog.NewModelCatalog(cfg, rot, cacheFile, logger)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed creating tcp listener: %v", err)
	}

	gatewayServer, err := server.NewServer(server.Options{
		Config:   cfg,
		Rotator:  rot,
		Catalog:  cat,
		Logger:   logger,
		Listener: ln,
	})
	if err != nil {
		t.Fatalf("failed creating gateway server: %v", err)
	}

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- gatewayServer.Start()
	}()

	gatewayURL := "http://" + ln.Addr().String()

	// Wait for gateway to answer /health
	waitForHealth(t, ctx, gatewayURL)

	// 3. Dispatch Streaming Chat Completion Request
	chatReqPayload := map[string]any{
		"model": "gemini-3.1-pro-high",
		"messages": []map[string]string{
			{"role": "user", "content": "Demonstrate transparent failover"},
		},
		"stream": true,
	}
	payloadBytes, _ := json.Marshal(chatReqPayload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/chat/completions", bytes.NewReader(payloadBytes))
	if err != nil {
		t.Fatalf("creating chat completion request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("chat completion request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected HTTP 200 from gateway, got HTTP %d: %s", resp.StatusCode, string(body))
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/event-stream") {
		t.Errorf("expected Content-Type text/event-stream, got %s", contentType)
	}

	// 4. Parse SSE Stream
	scanner := bufio.NewScanner(resp.Body)
	var chunks []string
	var reasoningText strings.Builder
	var contentText strings.Builder
	sawDone := false

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			sawDone = true
			break
		}

		chunks = append(chunks, data)

		var chunk struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}

		if err := json.Unmarshal([]byte(data), &chunk); err == nil && len(chunk.Choices) > 0 {
			if chunk.Choices[0].Delta.ReasoningContent != "" {
				reasoningText.WriteString(chunk.Choices[0].Delta.ReasoningContent)
			}
			if chunk.Choices[0].Delta.Content != "" {
				contentText.WriteString(chunk.Choices[0].Delta.Content)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		t.Fatalf("reading SSE stream: %v", err)
	}

	// 5. Assertions on Stream Content and Failover Behavior
	if !sawDone {
		t.Errorf("expected terminal [DONE] marker in SSE stream")
	}

	if reasoningText.String() != "Thinking about failover resolution..." {
		t.Errorf("unexpected reasoning content: %q", reasoningText.String())
	}

	if contentText.String() != "Hello from Account B after transparent failover!" {
		t.Errorf("unexpected content: %q", contentText.String())
	}

	// Verify Account A was called first and returned 429
	if accountACalls.Load() != 1 {
		t.Errorf("expected 1 call to Account A, got %d", accountACalls.Load())
	}

	// Verify Account B handled failover
	if accountBCalls.Load() != 1 {
		t.Errorf("expected 1 call to Account B, got %d", accountBCalls.Load())
	}

	// Verify Account A is now marked cooling
	states := pool.States()
	var stateA *rotator.AccountState
	for _, s := range states {
		if s.Email() == "account-a@example.com" {
			stateA = s
			break
		}
	}
	if stateA == nil {
		t.Fatalf("account-a state not found in pool")
	}
	now := time.Now()
	if !stateA.CoolingUntil("gemini-pro-agent").After(now) && !stateA.CoolingUntil("gemini-3.1-pro-high").After(now) {
		t.Errorf("expected Account A to be cooling after HTTP 429")
	}

	// 6. Non-Streaming Chat Completion Request Failover Test
	chatReqNonStream := map[string]any{
		"model": "gemini-3.1-pro-high",
		"messages": []map[string]string{
			{"role": "user", "content": "Non-streaming failover test"},
		},
		"stream": false,
	}
	payloadBytesNonStream, _ := json.Marshal(chatReqNonStream)

	reqNonStream, err := http.NewRequestWithContext(ctx, http.MethodPost, gatewayURL+"/v1/chat/completions", bytes.NewReader(payloadBytesNonStream))
	if err != nil {
		t.Fatalf("creating non-streaming request: %v", err)
	}
	reqNonStream.Header.Set("Content-Type", "application/json")
	reqNonStream.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	respNonStream, err := client.Do(reqNonStream)
	if err != nil {
		t.Fatalf("non-streaming chat completion request failed: %v", err)
	}
	defer respNonStream.Body.Close()

	if respNonStream.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(respNonStream.Body)
		t.Fatalf("expected HTTP 200 for non-streaming failover, got HTTP %d: %s", respNonStream.StatusCode, string(body))
	}

	var nonStreamResult struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(respNonStream.Body).Decode(&nonStreamResult); err != nil {
		t.Fatalf("decoding non-streaming response: %v", err)
	}

	if len(nonStreamResult.Choices) == 0 || nonStreamResult.Choices[0].Message.Content != "Hello from Account B after transparent failover!" {
		t.Errorf("unexpected non-streaming content: %+v", nonStreamResult)
	}

	// 7. Graceful Server Shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(t.Context()), 2*time.Second)
	defer shutdownCancel()
	_ = gatewayServer.Shutdown(shutdownCtx)
}

// waitForHealth polls the gateway /health endpoint until it answers HTTP 200 OK or context expires.
func waitForHealth(t *testing.T, ctx context.Context, gatewayURL string) {
	t.Helper()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for gateway /health endpoint")
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, gatewayURL+"/health", nil)
			if err != nil {
				continue
			}
			resp, err := http.DefaultClient.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				_ = resp.Body.Close()
				return
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
		}
	}
}
