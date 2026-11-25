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
	"sync"
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
	// server is the active local proxy Server under test.
	server *Server

	// rotator coordinates multi-account failover for the harness.
	rotator *rotator.Rotator

	// catalog manages model metadata and quota tracking.
	catalog *catalog.ModelCatalog

	// mockUpstream simulates Google Cloud Code Assist endpoints.
	mockUpstream *httptest.Server

	// apiKey is the Bearer key required to authenticate client requests.
	apiKey string

	// accEmail is the primary Google account email associated with the test account.
	accEmail string
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

	// 1. GET /health (loopback authority, as every daemon probe uses)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Host = "127.0.0.1:51400"
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
	if accResp.Strategy != string(harness.server.rotator.Strategy()) {
		t.Errorf("expected accounts strategy %q, got %q", harness.server.rotator.Strategy(), accResp.Strategy)
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

// TestModelByIDEndpoint verifies single model retrieval resolves configured aliases and answers an
// OpenAI-compatible 404 for unknown identifiers.
func TestModelByIDEndpoint(t *testing.T) {
	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-token", "expires_in": 3600})
			return
		}
		if strings.HasSuffix(r.URL.Path, "fetchAvailableModels") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": map[string]any{
					"gemini-pro-agent": map[string]any{
						"displayName":     "Gemini 3.1 Pro",
						"maxTokens":       1048576,
						"maxOutputTokens": 65536,
					},
				},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	handler := harness.server.buildHandler()

	// 1. Direct identifier.
	req := httptest.NewRequest(http.MethodGet, "/v1/models/gemini-pro-agent", nil)
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for known model, got %d: %s", rec.Code, rec.Body.String())
	}
	var item ModelItem
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatalf("decoding model item: %v", err)
	}
	if item.ID != "gemini-pro-agent" || item.Object != "model" {
		t.Errorf("unexpected model payload: %+v", item)
	}
	if item.Meta.MaxOutputTokens != 65536 {
		t.Errorf("expected max output tokens preserved, got %d", item.Meta.MaxOutputTokens)
	}

	// 2. Public alias resolves to the upstream catalog identifier.
	aliasReq := httptest.NewRequest(http.MethodGet, "/v1/models/gemini-3.1-pro-high", nil)
	aliasReq.Header.Set("Authorization", "Bearer "+harness.apiKey)
	aliasRec := httptest.NewRecorder()
	handler.ServeHTTP(aliasRec, aliasReq)
	if aliasRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for aliased model, got %d: %s", aliasRec.Code, aliasRec.Body.String())
	}
	var aliasItem ModelItem
	if err := json.Unmarshal(aliasRec.Body.Bytes(), &aliasItem); err != nil {
		t.Fatalf("decoding aliased model item: %v", err)
	}
	if aliasItem.ID != "gemini-pro-agent" {
		t.Errorf("expected alias resolved to gemini-pro-agent, got %q", aliasItem.ID)
	}

	// 3. Unknown identifier answers an OpenAI-shaped 404.
	missingReq := httptest.NewRequest(http.MethodGet, "/v1/models/does-not-exist", nil)
	missingReq.Header.Set("Authorization", "Bearer "+harness.apiKey)
	missingRec := httptest.NewRecorder()
	handler.ServeHTTP(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown model, got %d", missingRec.Code)
	}
	var errResp APIErrorResponse
	if err := json.Unmarshal(missingRec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decoding 404 payload: %v", err)
	}
	if errResp.Error.Code != http.StatusNotFound || errResp.Error.Message == "" {
		t.Errorf("unexpected 404 payload: %+v", errResp)
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

// TestAnthropicMessagesUnauthorizedReturnsAnthropicFormat verifies that unauthenticated requests to
// /v1/messages receive standard Anthropic error envelopes instead of OpenAI error envelopes.
func TestAnthropicMessagesUnauthorizedReturnsAnthropicFormat(t *testing.T) {
	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := harness.server.buildHandler()

	payload := `{"model": "claude-sonnet-4-6", "messages": [{"role": "user", "content": "hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately omit Authorization header
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %d", rec.Code)
	}

	var parsed AnthropicErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("expected AnthropicErrorResponse JSON, got unmarshal error: %v (body: %s)", err, rec.Body.String())
	}

	if parsed.Type != "error" || parsed.Error.Type != "authentication_error" {
		t.Errorf("unexpected error format: type=%q, error.type=%q", parsed.Type, parsed.Error.Type)
	}
}

// TestHealthRejectsNonLoopbackHost verifies the unauthenticated liveness endpoint rejects
// non-loopback Host headers to defend against DNS rebinding attacks.
func TestHealthRejectsNonLoopbackHost(t *testing.T) {
	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	handler := harness.server.buildHandler()

	rebindReq := httptest.NewRequest(http.MethodGet, "/health", nil)
	rebindReq.Host = "attacker.example.com:51400"
	rebindRec := httptest.NewRecorder()
	handler.ServeHTTP(rebindRec, rebindReq)
	if rebindRec.Code != http.StatusForbidden {
		t.Fatalf("/health with foreign host status = %d; want 403", rebindRec.Code)
	}
}

// TestChatCompletionsPromptRejectionFallback verifies a bare RESOURCE_EXHAUSTED rejection is
// retried once with system instructions moved into the first user turn, without cooling the
// healthy account.
func TestChatCompletionsPromptRejectionFallback(t *testing.T) {
	var attempts atomic.Int64
	var firstHadSystem atomic.Bool
	var retryHadSystem atomic.Bool
	var retrySystemText atomic.Value
	retrySystemText.Store("")

	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
			})
			return
		}
		if !strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.WriteHeader(http.StatusOK)
			return
		}

		var payload struct {
			Request struct {
				Contents []struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"contents"`
				SystemInstruction map[string]any `json:"systemInstruction"`
			} `json:"request"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("failed decoding upstream envelope: %v", err)
		}

		if attempts.Add(1) == 1 {
			firstHadSystem.Store(payload.Request.SystemInstruction != nil)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`))
			return
		}

		retryHadSystem.Store(payload.Request.SystemInstruction != nil)
		if len(payload.Request.Contents) > 0 && len(payload.Request.Contents[0].Parts) > 0 {
			retrySystemText.Store(payload.Request.Contents[0].Parts[0].Text)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"response":{"candidates":[{"content":{"parts":[{"text":"Recovered."}]},"finishReason":"STOP"}]}}` + "\n\n"))
	})

	handler := harness.server.buildHandler()
	payload := `{
		"model": "gemini-2.5-flash",
		"messages": [
			{"role": "system", "content": "You are Claude Code, Anthropic's official CLI for Claude."},
			{"role": "user", "content": "Hi"}
		],
		"stream": false
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200 after prompt fallback retry, got %d: %s", rec.Code, rec.Body.String())
	}
	if attempts.Load() != 2 {
		t.Errorf("expected exactly 2 upstream attempts, got %d", attempts.Load())
	}
	if !firstHadSystem.Load() {
		t.Errorf("expected the first attempt to keep systemInstruction")
	}
	if retryHadSystem.Load() {
		t.Errorf("expected the retry to omit systemInstruction")
	}
	gotSystem, _ := retrySystemText.Load().(string)
	if !strings.Contains(gotSystem, "You are a coding assistant.") || strings.Contains(gotSystem, "Claude Code") {
		t.Errorf("expected scrubbed system text in the first user turn, got %q", gotSystem)
	}

	states := harness.rotator.Pool().States()
	if len(states) != 1 {
		t.Fatalf("expected 1 account state, got %d", len(states))
	}
	if until := states[0].CoolingUntil("gemini-2.5-flash"); !until.IsZero() {
		t.Errorf("expected no cooldown after prompt rejection fallback, got %v", until)
	}
}

// TestChatCompletionsEnvelopeRequestTypeShape verifies outbound envelopes emit requestType
// "agent" only for tool-enabled turns and omit it for plain chat completions.
func TestChatCompletionsEnvelopeRequestTypeShape(t *testing.T) {
	envelopes := make(chan map[string]any, 4)

	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
			})
			return
		}
		if !strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.WriteHeader(http.StatusOK)
			return
		}

		var envelope map[string]any
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Errorf("failed decoding envelope: %v", err)
		}
		envelopes <- envelope

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"response":{"candidates":[{"content":{"parts":[{"text":"Ok."}]},"finishReason":"STOP"}]}}` + "\n\n"))
	})

	handler := harness.server.buildHandler()
	payloads := []string{
		`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"Hi"}]}`,
		`{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"Hi"}],"tools":[{"type":"function","function":{"name":"lookup"}}]}`,
	}

	for _, payload := range payloads {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(payload))
		req.Header.Set("Authorization", "Bearer "+harness.apiKey)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
		}
	}

	plain := <-envelopes
	if _, exists := plain["requestType"]; exists {
		t.Errorf("expected plain chat envelope to omit requestType, got %v", plain["requestType"])
	}

	agentic := <-envelopes
	if got := agentic["requestType"]; got != "agent" {
		t.Errorf("expected tool-enabled envelope requestType 'agent', got %v", got)
	}
}

// TestChatCompletionsLongDecodeContinuation verifies a hop that stops with finishReason CONTINUATION
// is chained into a bounded follow-up request carrying only the continuation token, and that both
// segments reach the client as one streamed answer.
func TestChatCompletionsLongDecodeContinuation(t *testing.T) {
	var calls atomic.Int64
	var mu sync.Mutex
	var hopBodies []string

	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"access_token": "mock-token",
				"expires_in":   3600,
			})
			return
		}
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			hopBodies = append(hopBodies, string(body))
			mu.Unlock()

			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)

			if calls.Add(1) == 1 {
				_, _ = w.Write([]byte(`data: {"response":{"candidates":[{"content":{"parts":[{"text":"segment one "}]},"finishReason":"CONTINUATION","continuationToken":"cont-token-1"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}}` + "\n\n"))
				return
			}
			_, _ = w.Write([]byte(`data: {"response":{"candidates":[{"content":{"parts":[{"text":"segment two"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":4}}}` + "\n\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := harness.server.buildHandler()

	payload := `{
		"model": "gemini-4-hydrogen-high",
		"messages": [{"role": "user", "content": "original prompt"}],
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

	body := rec.Body.String()
	if !strings.Contains(body, "segment one ") || !strings.Contains(body, "segment two") {
		t.Errorf("expected both continuation segments in the stream, got:\n%s", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Errorf("expected streamed response to terminate with [DONE], got:\n%s", body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hopBodies) != 2 {
		t.Fatalf("expected exactly 2 upstream hops, got %d", len(hopBodies))
	}
	if !strings.Contains(hopBodies[1], `"continuationToken":"cont-token-1"`) {
		t.Errorf("expected second hop to carry the continuation token, got:\n%s", hopBodies[1])
	}
	if !strings.Contains(hopBodies[1], "original prompt") {
		t.Errorf("expected second hop to resend the original prompt, got:\n%s", hopBodies[1])
	}
	if strings.Contains(hopBodies[1], "segment one") {
		t.Errorf("expected second hop to omit partial model text, got:\n%s", hopBodies[1])
	}
}

// TestChatCompletionsLongDecodeSkipsContinuationUnderExplicitCap verifies a request that set max_tokens is
// not chained onto a continuation hop, because Cloud Code accumulates maxOutputTokens across hops and would
// reject the resent ceiling. The capped response finishes as length-limited output instead.
func TestChatCompletionsLongDecodeSkipsContinuationUnderExplicitCap(t *testing.T) {
	var calls atomic.Int64

	harness := setupTestHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "mock-token", "expires_in": 3600})
			return
		}
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			calls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`data: {"response":{"candidates":[{"content":{"parts":[{"text":"partial answer"}]},"finishReason":"CONTINUATION","continuationToken":"cont-token-capped"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}}` + "\n\n"))
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	handler := harness.server.buildHandler()

	payload := `{
		"model": "gemini-4-hydrogen-high",
		"messages": [{"role": "user", "content": "original prompt"}],
		"max_tokens": 16
	}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(payload))
	req.Header.Set("Authorization", "Bearer "+harness.apiKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected exactly 1 upstream hop under an explicit cap, got %d", got)
	}

	var completion translate.ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("decoding completion: %v", err)
	}
	if len(completion.Choices) == 0 {
		t.Fatalf("expected a completion choice, got %s", rec.Body.String())
	}
	if completion.Choices[0].FinishReason != "length" {
		t.Errorf("expected finish reason length for a capped continuation, got %q", completion.Choices[0].FinishReason)
	}
	if content, _ := completion.Choices[0].Message.Content.(string); !strings.Contains(content, "partial answer") {
		t.Errorf("expected partial output preserved, got %q", content)
	}
}
