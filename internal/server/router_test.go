// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
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
	"github.com/salmanabdurrahman/antigravity-gateway/internal/translate"
)

// testHarness wires a Server instance, mock upstream server, and test account together for integration testing.
type testHarness struct {
	server       *Server
	rotator      *rotator.Rotator
	catalog      *catalog.ModelCatalog
	mockUpstream *httptest.Server
	apiKey       string
	accEmail     string
}

// setupTestHarness initializes an isolated test environment with temporary stores and mocked endpoints.
func setupTestHarness(t *testing.T, upstreamHandler http.HandlerFunc) *testHarness {
	t.Helper()
	tempDir := t.TempDir()

	// Isolate config/account path resolution so handlers that persist settings never
	// touch the operator's real ~/.agw directory.
	t.Setenv(config.EnvHome, tempDir)

	mockUpstream := httptest.NewServer(upstreamHandler)
	t.Cleanup(mockUpstream.Close)

	apiKey := "test-gateway-key-999"
	accEmail := "test-user@gmail.com"

	cfg := &config.Config{
		Host:        "127.0.0.1",
		Port:        51400,
		APIKey:      apiKey,
		Strategy:    "sticky",
		TokenURL:    mockUpstream.URL + "/token",
		Endpoints:   []string{mockUpstream.URL},
		ClientID:    "test-client-id",
		UserInfoURL: mockUpstream.URL + "/userinfo",
	}

	accFile := filepath.Join(tempDir, "accounts.json")
	store := auth.NewStore(accFile)
	_, err := store.Upsert(&auth.Account{
		Email:        accEmail,
		RefreshToken: "test-refresh-token",
		ProjectID:    "companion-test-proj",
		AddedAt:      time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("failed setting up test account: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := rotator.NewPool(store, cfg, time.Now)
	if err != nil {
		t.Fatalf("failed creating pool: %v", err)
	}

	rot := rotator.NewRotator(pool, cfg, logger, time.Now)
	rot.SetHTTPClient(mockUpstream.Client())

	cacheFile := filepath.Join(tempDir, "models-cache.json")
	cat := catalog.NewModelCatalog(cfg, rot, cacheFile, logger)
	cat.SetHTTPClient(mockUpstream.Client())

	srv, err := NewServer(Options{
		Config:  cfg,
		Rotator: rot,
		Catalog: cat,
		Logger:  logger,
	})
	if err != nil {
		t.Fatalf("failed creating server: %v", err)
	}

	return &testHarness{
		server:       srv,
		rotator:      rot,
		catalog:      cat,
		mockUpstream: mockUpstream,
		apiKey:       apiKey,
		accEmail:     accEmail,
	}
}

// TestHealthAndAccountsEndpoints verifies /health and /v1/accounts diagnostic responses.
func TestHealthAndAccountsEndpoints(t *testing.T) {
	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := harness.server.buildHandler()

	// 1. GET /health
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected /health status 200, got %d", rec.Code)
	}

	var health HealthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("failed decoding health response: %v", err)
	}
	if !health.OK || health.Name != ServerName || health.Version != config.Version {
		t.Errorf("unexpected health payload: %+v", health)
	}
	if health.Accounts != 1 {
		t.Errorf("expected 1 usable account, got %d", health.Accounts)
	}

	// 2. GET /v1/accounts
	reqAcc := httptest.NewRequest(http.MethodGet, "/v1/accounts", nil)
	reqAcc.Header.Set("Authorization", "Bearer "+harness.apiKey)
	recAcc := httptest.NewRecorder()
	handler.ServeHTTP(recAcc, reqAcc)

	if recAcc.Code != http.StatusOK {
		t.Fatalf("expected /v1/accounts status 200, got %d", recAcc.Code)
	}

	var accResp AccountsResponse
	if err := json.Unmarshal(recAcc.Body.Bytes(), &accResp); err != nil {
		t.Fatalf("failed decoding accounts response: %v", err)
	}
	if len(accResp.Accounts) != 1 || accResp.Accounts[0].Email != harness.accEmail {
		t.Errorf("unexpected accounts response: %+v", accResp)
	}
}

// TestModelsEndpoint verifies /v1/models listing and integration with catalog cache.
func TestModelsEndpoint(t *testing.T) {
	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "fetchAvailableModels") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": map[string]any{
					"gemini-2.5-flash": map[string]any{
						"displayName":     "Gemini 2.5 Flash",
						"maxTokens":       1048576,
						"maxOutputTokens": 8192,
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := harness.server.buildHandler()

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected /v1/models status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var modelList ModelListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &modelList); err != nil {
		t.Fatalf("failed decoding model list response: %v", err)
	}

	if modelList.Object != "list" || len(modelList.Data) == 0 {
		t.Fatalf("expected non-empty model list, got: %+v", modelList)
	}

	found := false
	for _, m := range modelList.Data {
		if m.ID == "gemini-2.5-flash" {
			found = true
			if m.Meta.DisplayName != "Gemini 2.5 Flash" {
				t.Errorf("expected display name 'Gemini 2.5 Flash', got %q", m.Meta.DisplayName)
			}
			break
		}
	}
	if !found {
		t.Error("expected to find gemini-2.5-flash in model list")
	}
}

// TestChatCompletionsStreaming verifies SSE streaming conversion of Gemini responses to OpenAI chunk deltas.
func TestChatCompletionsStreaming(t *testing.T) {
	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
			})
			return
		}
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)

			event1 := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"Hello from "}]}}]}}` + "\n\n"
			event2 := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"Antigravity!"}]}}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}}` + "\n\n"

			_, _ = w.Write([]byte(event1))
			_, _ = w.Write([]byte(event2))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := harness.server.buildHandler()

	payload := `{
		"model": "gemini-2.5-flash",
		"messages": [{"role": "user", "content": "Hi"}],
		"stream": true
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	contentType := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("expected text/event-stream Content-Type, got %q", contentType)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "Hello from ") || !strings.Contains(body, "Antigravity!") {
		t.Errorf("stream missing expected text chunks. Body:\n%s", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("stream missing [DONE] terminator. Body:\n%s", body)
	}
}

// TestChatCompletionsNonStreaming verifies full aggregated non-streaming chat completions generation.
func TestChatCompletionsNonStreaming(t *testing.T) {
	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
			})
			return
		}
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)

			event := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"Complete answer."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":4,"totalTokenCount":12}}}` + "\n\n"
			_, _ = w.Write([]byte(event))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := harness.server.buildHandler()

	payload := `{
		"model": "gemini-2.5-flash",
		"messages": [{"role": "user", "content": "Hi"}],
		"stream": false
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var completion translate.ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("failed decoding completion response: %v", err)
	}

	if len(completion.Choices) == 0 {
		t.Fatal("expected at least 1 choice in completion")
	}

	contentStr, ok := completion.Choices[0].Message.Content.(string)
	if !ok || contentStr != "Complete answer." {
		t.Errorf("expected content 'Complete answer.', got %v", completion.Choices[0].Message.Content)
	}
	if completion.Choices[0].FinishReason != "stop" {
		t.Errorf("expected finish_reason 'stop', got %q", completion.Choices[0].FinishReason)
	}
}

// TestChatCompletionsSchemaRejectionFallback verifies automatic schema simplification retry upon 400 rejection.
func TestChatCompletionsSchemaRejectionFallback(t *testing.T) {
	var attempts atomic.Int64

	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
			})
			return
		}
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			curr := attempts.Add(1)
			if curr == 1 {
				// Simulate Google rejecting parametersJsonSchema
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Invalid value at parameters_json_schema"}}`))
				return
			}

			// Second attempt succeeds under legacySchemas mode
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			event := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"Success after schema retry"}]},"finishReason":"STOP"}]}}` + "\n\n"
			_, _ = w.Write([]byte(event))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := harness.server.buildHandler()

	payload := `{
		"model": "gemini-2.5-flash",
		"messages": [{"role": "user", "content": "Run tool"}],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "lookup",
					"description": "Lookup entity",
					"parameters": {"type": "object", "properties": {"q": {"type": "string"}}}
				}
			}
		],
		"stream": false
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200 after schema fallback retry, got %d: %s", rec.Code, rec.Body.String())
	}

	if attempts.Load() < 2 {
		t.Errorf("expected at least 2 attempts for schema fallback, got %d", attempts.Load())
	}

	var completion translate.ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("failed decoding completion response: %v", err)
	}

	contentStr, ok := completion.Choices[0].Message.Content.(string)
	if !ok || contentStr != "Success after schema retry" {
		t.Errorf("unexpected content: %v", completion.Choices[0].Message.Content)
	}
}

// TestAnthropicMessagesStreaming verifies streaming translation of Google Cloud Code parts
// into Anthropic SSE events on POST /v1/messages.
func TestAnthropicMessagesStreaming(t *testing.T) {
	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
			})
			return
		}
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)

			event1 := `data: {"response":{"candidates":[{"content":{"parts":[{"thought":true,"text":"Thinking...","thoughtSignature":"sig_1"}]}}]}}` + "\n\n"
			event2 := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"Hello from Claude endpoint!"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":12,"candidatesTokenCount":6}}}` + "\n\n"

			_, _ = w.Write([]byte(event1))
			_, _ = w.Write([]byte(event2))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := harness.server.buildHandler()

	payload := `{
		"model": "claude-sonnet-4-6",
		"messages": [{"role": "user", "content": "Hi"}],
		"stream": true,
		"max_tokens": 1024
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") {
		t.Error("missing event: message_start")
	}
	if !strings.Contains(body, "event: content_block_start") {
		t.Error("missing event: content_block_start")
	}
	if !strings.Contains(body, "event: content_block_delta") {
		t.Error("missing event: content_block_delta")
	}
	if !strings.Contains(body, "event: message_delta") {
		t.Error("missing event: message_delta")
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Error("missing event: message_stop")
	}
	if !strings.Contains(body, "Hello from Claude endpoint!") {
		t.Error("missing generation text in stream response")
	}
}

// TestAnthropicMessagesNonStreaming verifies non-streaming message generation on POST /v1/messages.
func TestAnthropicMessagesNonStreaming(t *testing.T) {
	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
			})
			return
		}
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)

			event := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"Direct answer."}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5}}}` + "\n\n"
			_, _ = w.Write([]byte(event))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := harness.server.buildHandler()

	payload := `{
		"model": "claude-sonnet-4-6",
		"messages": [{"role": "user", "content": "Hi"}],
		"stream": false,
		"max_tokens": 1024
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp translate.AnthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed decoding response: %v", err)
	}

	if resp.Type != "message" {
		t.Errorf("expected type message, got %s", resp.Type)
	}
	if resp.Role != "assistant" {
		t.Errorf("expected role assistant, got %s", resp.Role)
	}
	if len(resp.Content) == 0 || resp.Content[0].Text != "Direct answer." {
		t.Errorf("unexpected content: %+v", resp.Content)
	}
	if resp.StopReason != "end_turn" {
		t.Errorf("expected stop_reason end_turn, got %s", resp.StopReason)
	}
}

// TestAnthropicMessagesValidationError verifies structured 400 error responses on invalid requests.
func TestAnthropicMessagesValidationError(t *testing.T) {
	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {})
	handler := harness.server.buildHandler()

	// Missing model field
	payload := `{"messages": [{"role": "user", "content": "Hi"}]}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", rec.Code)
	}

	var errResp AnthropicErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed decoding error response: %v", err)
	}

	if errResp.Type != "error" || errResp.Error.Type != "invalid_request_error" {
		t.Errorf("unexpected error payload: %+v", errResp)
	}
}

// TestAnthropicMessagesToolCallAndSchemaFallback verifies tool calling and automatic schema fallback retry on /v1/messages.
func TestAnthropicMessagesToolCallAndSchemaFallback(t *testing.T) {
	var attempts atomic.Int64

	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
			})
			return
		}
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			curr := attempts.Add(1)
			if curr == 1 {
				// Simulate Google rejecting parametersJsonSchema
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"code":400,"message":"Invalid value at parameters_json_schema"}}`))
				return
			}

			// Second attempt succeeds under legacySchemas mode
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			event := `data: {"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"lookup","id":"call_123","args":{"q":"test"}}}]},"finishReason":"TOOL_USE"}]}}` + "\n\n"
			_, _ = w.Write([]byte(event))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := harness.server.buildHandler()

	payload := `{
		"model": "gemini-2.5-flash",
		"messages": [{"role": "user", "content": "Run tool"}],
		"tools": [
			{
				"name": "lookup",
				"description": "Lookup entity",
				"input_schema": {"type": "object", "properties": {"q": {"type": "string"}}}
			}
		],
		"stream": false,
		"max_tokens": 512
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200 after schema fallback, got %d: %s", rec.Code, rec.Body.String())
	}

	if attempts.Load() < 2 {
		t.Errorf("expected at least 2 attempts for schema fallback, got %d", attempts.Load())
	}

	var resp translate.AnthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response failed: %v", err)
	}

	if resp.StopReason != "tool_use" {
		t.Errorf("expected stop_reason tool_use, got %s", resp.StopReason)
	}
	if len(resp.Content) == 0 || resp.Content[0].Type != "tool_use" {
		t.Fatalf("expected tool_use block, got %+v", resp.Content)
	}
	if resp.Content[0].Name != "lookup" {
		t.Errorf("expected tool name lookup, got %s", resp.Content[0].Name)
	}
}

// TestModelMappingAliasInRouter verifies that custom model aliases in config.ModelMapping
// are resolved to their target model before dispatching upstream.
func TestModelMappingAliasInRouter(t *testing.T) {
	var requestedModel string

	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
			})
			return
		}
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			var bodyMap map[string]any
			_ = json.NewDecoder(r.Body).Decode(&bodyMap)
			if m, ok := bodyMap["model"].(string); ok {
				requestedModel = m
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			event := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"Model alias resolved"}]},"finishReason":"STOP"}]}}` + "\n\n"
			_, _ = w.Write([]byte(event))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	// Configure custom model alias
	harness.server.setConfig(func(c *config.Config) {
		c.ModelMapping = map[string]string{"gemini-4-argon": "gemini-4-argon-high"}
	})

	handler := harness.server.buildHandler()

	payload := `{
		"model": "gemini-4-argon",
		"messages": [{"role": "user", "content": "Hi"}],
		"stream": false
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if requestedModel != "gemini-4-argon-high" {
		t.Errorf("expected mapped model gemini-4-argon-high, got %s", requestedModel)
	}
}

// TestUIEndpoints verifies serving embedded WebUI dashboard HTML and API status/strategy endpoints.
func TestUIEndpoints(t *testing.T) {
	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := harness.server.buildHandler()

	// 1. GET /ui returns 200 HTML
	reqUI := httptest.NewRequest(http.MethodGet, "/ui", nil)
	reqUI.Host = "127.0.0.1:51400"
	recUI := httptest.NewRecorder()
	handler.ServeHTTP(recUI, reqUI)

	if recUI.Code != http.StatusOK {
		t.Fatalf("expected /ui status 200, got %d", recUI.Code)
	}
	if !strings.Contains(recUI.Header().Get("Content-Type"), "text/html") {
		t.Errorf("expected text/html Content-Type, got %s", recUI.Header().Get("Content-Type"))
	}
	if !strings.Contains(recUI.Body.String(), "Antigravity Gateway Dashboard") {
		t.Errorf("expected dashboard title in HTML, got %s", recUI.Body.String())
	}

	// 2. GET / with Accept: text/html redirects to /ui
	reqRoot := httptest.NewRequest(http.MethodGet, "/", nil)
	reqRoot.Header.Set("Accept", "text/html,application/xhtml+xml")
	recRoot := httptest.NewRecorder()
	handler.ServeHTTP(recRoot, reqRoot)

	if recRoot.Code != http.StatusFound {
		t.Fatalf("expected / to redirect with 302, got %d", recRoot.Code)
	}
	if loc := recRoot.Header().Get("Location"); loc != "/ui" {
		t.Errorf("expected redirect Location /ui, got %s", loc)
	}

	// 3. GET /ui/api/status returns JSON status payload
	reqStatus := httptest.NewRequest(http.MethodGet, "/ui/api/status", nil)
	reqStatus.Host = "127.0.0.1:51400"
	recStatus := httptest.NewRecorder()
	handler.ServeHTTP(recStatus, reqStatus)

	if recStatus.Code != http.StatusOK {
		t.Fatalf("expected /ui/api/status 200, got %d", recStatus.Code)
	}
	var statusResp UIStatusResponse
	if err := json.Unmarshal(recStatus.Body.Bytes(), &statusResp); err != nil {
		t.Fatalf("unmarshaling status failed: %v", err)
	}
	if statusResp.Version != config.Version {
		t.Errorf("expected version %s, got %s", config.Version, statusResp.Version)
	}

	// 4. POST /ui/api/strategy updates active strategy
	// Seed a config file on disk whose values intentionally differ from the in-memory harness
	// config, so the assertion below proves the handler never overwrites the file with process state.
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("resolving config path: %v", err)
	}
	diskCfg := config.DefaultConfig()
	diskCfg.Port = 52400
	diskCfg.APIKey = config.APIKeyPrefix + strings.Repeat("a", config.APIKeyHexLen)
	diskCfg.UserAgent = "disk-agent/1.0"
	if err := config.Save(cfgPath, diskCfg); err != nil {
		t.Fatalf("seeding config file: %v", err)
	}

	reqStrat := httptest.NewRequest(http.MethodPost, "/ui/api/strategy", bytes.NewBufferString(`{"strategy": "hybrid"}`))
	reqStrat.Host = "127.0.0.1:51400"
	reqStrat.Header.Set("Content-Type", "application/json")
	recStrat := httptest.NewRecorder()
	handler.ServeHTTP(recStrat, reqStrat)

	if recStrat.Code != http.StatusOK {
		t.Fatalf("expected /ui/api/strategy 200, got %d: %s", recStrat.Code, recStrat.Body.String())
	}
	if harness.server.rotator.Strategy() != rotator.StrategyHybrid {
		t.Errorf("expected strategy updated to hybrid, got %s", harness.server.rotator.Strategy())
	}

	persisted, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reloading persisted config: %v", err)
	}
	if persisted.Strategy != string(rotator.StrategyHybrid) {
		t.Errorf("persisted strategy = %q, expected %q", persisted.Strategy, rotator.StrategyHybrid)
	}
	if persisted.Port != 52400 {
		t.Errorf("persisted port = %d, expected the on-disk value 52400", persisted.Port)
	}
	if persisted.APIKey != diskCfg.APIKey {
		t.Errorf("persisted api_key was replaced by the in-memory key")
	}
	if persisted.UserAgent != "disk-agent/1.0" {
		t.Errorf("persisted user_agent = %q, expected 'disk-agent/1.0'", persisted.UserAgent)
	}
	if got := harness.server.config().Strategy; got != string(rotator.StrategyHybrid) {
		t.Errorf("in-memory config snapshot strategy = %q, expected %q", got, rotator.StrategyHybrid)
	}

	// 5. POST /ui/api/strategy rejects unsupported strategy names
	reqBad := httptest.NewRequest(http.MethodPost, "/ui/api/strategy", bytes.NewBufferString(`{"strategy": "random"}`))
	reqBad.Host = "127.0.0.1:51400"
	reqBad.Header.Set("Content-Type", "application/json")
	recBad := httptest.NewRecorder()
	handler.ServeHTTP(recBad, reqBad)

	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported strategy, got %d", recBad.Code)
	}
	if harness.server.rotator.Strategy() != rotator.StrategyHybrid {
		t.Errorf("strategy must remain hybrid after rejected update, got %s", harness.server.rotator.Strategy())
	}

	// 6. POST /ui/api/strategy rejects cross-origin mutations
	reqCSRF := httptest.NewRequest(http.MethodPost, "/ui/api/strategy", bytes.NewBufferString(`{"strategy": "sticky"}`))
	reqCSRF.Host = "127.0.0.1:51400"
	reqCSRF.Header.Set("Content-Type", "text/plain")
	reqCSRF.Header.Set("Origin", "http://evil.example.com")
	recCSRF := httptest.NewRecorder()
	handler.ServeHTTP(recCSRF, reqCSRF)

	if recCSRF.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-origin mutation, got %d", recCSRF.Code)
	}
	if harness.server.rotator.Strategy() != rotator.StrategyHybrid {
		t.Errorf("strategy must remain hybrid after rejected CSRF update, got %s", harness.server.rotator.Strategy())
	}

	// 7. GET /ui/api/status and POST /ui/api/strategy reject non-loopback Host (DNS rebinding defense)
	reqRebind := httptest.NewRequest(http.MethodGet, "/ui/api/status", nil)
	reqRebind.Host = "attacker.example.com:51400"
	recRebind := httptest.NewRecorder()
	handler.ServeHTTP(recRebind, reqRebind)
	if recRebind.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for DNS rebinding attempt, got %d", recRebind.Code)
	}
}
