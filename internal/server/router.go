// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/server/webui"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/translate"
)

// HealthResponse describes the health and identity status returned by /health.
type HealthResponse struct {
	// OK indicates daemon operational health.
	OK bool `json:"ok"`

	// Name is the service identifier (e.g. "antigravity-gateway").
	Name string `json:"name"`

	// Version reports the compiled semantic release version.
	Version string `json:"version"`

	// Accounts reports the count of usable accounts in the pool.
	Accounts int `json:"accounts"`
}

// ModelMetadata represents supplemental model properties formatted for OpenAI client inspection.
type ModelMetadata struct {
	// DisplayName is the human-readable model label.
	DisplayName string `json:"display_name"`

	// ContextLength specifies input token context capacity.
	ContextLength int `json:"context_length"`

	// MaxOutputTokens specifies generation output ceiling.
	MaxOutputTokens int `json:"max_output_tokens"`

	// Thinking indicates chain-of-thought support.
	Thinking bool `json:"thinking"`

	// ThinkingBudget holds default reasoning budget if configured.
	ThinkingBudget *int `json:"thinking_budget,omitzero"`

	// Multimodal indicates image input support.
	Multimodal bool `json:"multimodal"`

	// ToolCalling indicates function calling support.
	ToolCalling bool `json:"tool_calling"`
}

// ModelItem represents a single model in the OpenAI model listing response.
type ModelItem struct {
	// ID is the unique model identifier.
	ID string `json:"id"`

	// Object is always "model".
	Object string `json:"object"`

	// Created is the timestamp when the model was cataloged.
	Created int64 `json:"created"`

	// OwnedBy designates model ownership, typically "antigravity-gateway".
	OwnedBy string `json:"owned_by"`

	// ContextWindow reflects input context capacity.
	ContextWindow int `json:"context_window"`

	// Meta holds extended model limits and capabilities.
	Meta ModelMetadata `json:"meta"`
}

// ModelListResponse represents the OpenAI list models response envelope.
type ModelListResponse struct {
	// Object is always "list".
	Object string `json:"object"`

	// Data contains the array of available model items.
	Data []ModelItem `json:"data"`
}

// AccountsResponse represents the response containing safe account snapshots.
type AccountsResponse struct {
	// Accounts enumerates non-sensitive operational states for configured accounts.
	Accounts []rotator.AccountSnapshot `json:"accounts"`
}

// buildHandler constructs the HTTP ServeMux and attaches logging, auth, and recovery middleware.
func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleHealth)
	mux.HandleFunc("GET /ui", s.handleUI)
	mux.HandleFunc("GET /ui/", s.handleUI)
	mux.HandleFunc("GET /ui/api/status", s.handleUIStatus)
	mux.HandleFunc("POST /ui/api/strategy", s.handleUIStrategy)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("POST /v1/messages", s.handleAnthropicMessages)
	mux.HandleFunc("GET /v1/accounts", s.handleAccounts)

	// Chain middlewares: Logging -> Recovery -> Auth -> ServeMux
	handler := LoggingMiddleware(s.logger)(
		RecoveryMiddleware(s.logger)(
			AuthMiddleware(s.config().APIKey, s.logger)(mux),
		),
	)

	return handler
}

// handleHealth answers liveness probes and reports gateway version and usable account counts.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if (r.URL.Path == "/" || r.URL.Path == "") && strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/ui", http.StatusFound)
		return
	}

	states := s.rotator.Pool().States()
	usableCount := 0
	for _, st := range states {
		if st.Usable() {
			usableCount++
		}
	}

	sendJSON(w, http.StatusOK, HealthResponse{
		OK:       true,
		Name:     ServerName,
		Version:  config.Version,
		Accounts: usableCount,
	})
}

// handleModels lists available chat completion models with their token boundaries and capabilities.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if err := s.catalog.Ensure(r.Context()); err != nil {
		snap, _ := s.catalog.Snapshot()
		if len(snap.Models) == 0 {
			var rotErr *rotator.RotatorError
			status := http.StatusBadGateway
			if errors.As(err, &rotErr) {
				status = rotErr.StatusCode
			}
			sendJSONError(w, status, fmt.Sprintf("Could not list models: %v", err), 0)
			return
		}
	}

	snap, _ := s.catalog.Snapshot()
	items := make([]ModelItem, len(snap.Models))
	for i, m := range snap.Models {
		items[i] = ModelItem{
			ID:            m.ID,
			Object:        "model",
			Created:       0,
			OwnedBy:       ServerName,
			ContextWindow: m.ContextWindow,
			Meta: ModelMetadata{
				DisplayName:     m.DisplayName,
				ContextLength:   m.ContextWindow,
				MaxOutputTokens: m.MaxOutputTokens,
				Thinking:        m.Thinking,
				ThinkingBudget:  m.ThinkingBudget,
				Multimodal:      m.Images,
				ToolCalling:     true,
			},
		}
	}

	sendJSON(w, http.StatusOK, ModelListResponse{
		Object: "list",
		Data:   items,
	})
}

// handleAccounts returns non-secret account diagnostics, request metrics, and cooldown states.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	snapshots := s.rotator.Pool().Snapshots()
	sendJSON(w, http.StatusOK, AccountsResponse{
		Accounts: snapshots,
	})
}

// UIStatusResponse represents the comprehensive status payload consumed by the local WebUI dashboard.
type UIStatusResponse struct {
	// Version is the compiled gateway version.
	Version string `json:"version"`

	// Strategy is the currently active rotation strategy.
	Strategy string `json:"strategy"`

	// Accounts lists all registered account states.
	Accounts []rotator.AccountSnapshot `json:"accounts"`

	// Models lists available chat models.
	Models []catalog.CatalogModel `json:"models"`

	// Quotas maps model identifiers to their live remaining quota metrics.
	Quotas map[string]catalog.ModelQuota `json:"quotas,omitempty"`
}

// UIStrategyRequest represents the incoming JSON payload to dynamically update the rotation strategy.
type UIStrategyRequest struct {
	// Strategy is the requested rotation strategy ("sticky", "round-robin", or "hybrid").
	Strategy string `json:"strategy"`
}

// isLoopbackHost verifies whether the host header represents a local loopback interface.
// This prevents DNS rebinding attacks against unauthenticated WebUI and local dashboard endpoints.
func isLoopbackHost(hostHeader string) bool {
	host := strings.TrimSpace(hostHeader)
	if host == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if strings.EqualFold(host, "localhost") || host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleUI serves the embedded static WebUI dashboard HTML page.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackHost(r.Host) {
		sendJSONError(w, http.StatusForbidden, "Forbidden: loopback access only", 0)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(webui.IndexHTML))
}

// handleUIStatus aggregates accounts, strategy, models, and quota metrics for the WebUI dashboard.
func (s *Server) handleUIStatus(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackHost(r.Host) {
		sendJSONError(w, http.StatusForbidden, "Forbidden: loopback access only", 0)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		sendJSONError(w, http.StatusForbidden, "Forbidden: cross-site requests not allowed", 0)
		return
	}

	_ = s.catalog.Ensure(r.Context())
	snap, _ := s.catalog.Snapshot()
	snapshots := s.rotator.Pool().Snapshots()

	sendJSON(w, http.StatusOK, UIStatusResponse{
		Version:  config.Version,
		Strategy: string(s.rotator.Strategy()),
		Accounts: snapshots,
		Models:   snap.Models,
		Quotas:   snap.Quotas,
	})
}

// handleUIStrategy updates the active account rotation strategy dynamically via the WebUI dashboard.
func (s *Server) handleUIStrategy(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackHost(r.Host) {
		sendJSONError(w, http.StatusForbidden, "Forbidden: loopback access only", 0)
		return
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		sendJSONError(w, http.StatusForbidden, "Forbidden: cross-site requests not allowed", 0)
		return
	}

	// Protocol Quirk: the dashboard is unauthenticated on loopback, so reject cross-site
	// mutations that a malicious page could issue as a "simple" request without preflight.
	if origin := r.Header.Get("Origin"); origin != "" {
		if host, err := url.Parse(origin); err != nil || !strings.EqualFold(host.Host, r.Host) || !isLoopbackHost(host.Host) {
			sendJSONError(w, http.StatusForbidden, "Cross-origin strategy mutation is not permitted.", 0)
			return
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, 8*1024)
	var req UIStrategyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONError(w, http.StatusBadRequest, "Invalid JSON payload", 0)
		return
	}

	strat := rotator.Strategy(req.Strategy)
	if strat != rotator.StrategySticky && strat != rotator.StrategyRoundRobin && strat != rotator.StrategyHybrid {
		sendJSONError(w, http.StatusBadRequest, fmt.Sprintf("Unsupported strategy %q: must be 'sticky', 'round-robin', or 'hybrid'", req.Strategy), 0)
		return
	}

	s.strategyMu.Lock()
	defer s.strategyMu.Unlock()

	s.rotator.SetStrategy(strat)
	s.setConfig(func(c *config.Config) { c.Strategy = string(strat) })

	// Persist best-effort: the in-memory strategy is already active, so a write failure only
	// affects the next daemon restart and must not fail the UI request. Only the strategy field
	// is written back; the config file on disk remains the source of truth for every other value.
	if cfgPath, err := config.ConfigFilePath(); err == nil {
		if saveErr := config.SaveStrategy(cfgPath, string(strat)); saveErr != nil {
			s.logger.Warn("failed persisting strategy to config file", "error", saveErr)
		}
	}

	sendJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleChatCompletions processes OpenAI chat completions requests, coordinating schema translation,
// transparent multi-account 429 rotation, thinking preservation, and SSE streaming.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		sendJSONError(w, http.StatusBadRequest, fmt.Sprintf("Reading request body: %v", err), 0)
		return
	}

	var chatReq translate.ChatRequest
	if err := json.Unmarshal(bodyBytes, &chatReq); err != nil {
		sendJSONError(w, http.StatusBadRequest, "Request body is not valid JSON.", 0)
		return
	}

	if err := translate.ValidateChatRequest(&chatReq); err != nil {
		sendJSONError(w, http.StatusBadRequest, err.Error(), 0)
		return
	}

	originalModel := chatReq.Model
	resolvedModel := catalog.ResolveModelAlias(chatReq.Model, s.config().ModelMapping)
	_ = s.catalog.Ensure(r.Context())
	info := s.catalog.Info(resolvedModel)
	upstream := catalog.UpstreamModel(resolvedModel)
	chatReq.Model = resolvedModel

	sessionID := cmp.Or(
		r.Header.Get("X-Session-Affinity"),
		r.Header.Get("session_id"),
		r.Header.Get("X-Session-Id"),
	)

	randID := make([]byte, 12)
	_, _ = rand.Read(randID)
	id := fmt.Sprintf("chatcmpl-%s", hex.EncodeToString(randID))
	created := time.Now().Unix()

	for attempt := range EmptyStreamRetries + 1 {
		opts := translate.BuildOptions{
			Info:          info,
			LegacySchemas: s.legacySchemas.Load(),
			SessionID:     sessionID,
		}

		geminiReq, reqErr := translate.BuildRequest(&chatReq, opts)
		if reqErr != nil {
			sendJSONError(w, http.StatusBadRequest, reqErr.Error(), 0)
			return
		}

		attemptResult, err := s.rotator.Generate(r.Context(), upstream, func(st *rotator.AccountState) (any, error) {
			return translate.WrapEnvelope(geminiReq, upstream, st.ProjectID()), nil
		})
		if err != nil {
			var rotErr *rotator.RotatorError
			if errors.As(err, &rotErr) {
				// Protocol Quirk: If upstream rejects parametersJsonSchema, remember legacy mode and retry
				if !s.legacySchemas.Load() && translate.IsSchemaRejection(rotErr.StatusCode, rotErr.ErrorMessage) {
					s.legacySchemas.Store(true)
					s.logger.Info("Google refused parametersJsonSchema; switching tool schemas to legacy parameters field")
					continue
				}
				sendJSONError(w, rotErr.StatusCode, rotErr.ErrorMessage, rotErr.RetryAfter)
				return
			}
			sendJSONError(w, http.StatusBadGateway, err.Error(), 0)
			return
		}

		chatReq.Model = originalModel
		done, retry := s.processCompletionsResponse(w, r, &chatReq, attemptResult, id, created, upstream, attempt)
		if retry {
			chatReq.Model = resolvedModel
			continue
		}
		if done {
			return
		}
	}
}

// processCompletionsResponse handles the upstream HTTP response, returning whether the request
// finished successfully or requires a retry due to an empty upstream stream.
func (s *Server) processCompletionsResponse(
	w http.ResponseWriter,
	r *http.Request,
	chatReq *translate.ChatRequest,
	result *rotator.AttemptResult,
	id string,
	created int64,
	upstream string,
	attempt int,
) (done bool, retry bool) {
	defer result.Response.Body.Close()

	flusher, _ := w.(http.Flusher)

	if chatReq.Stream {
		var started bool
		writeChunk := func(chunk translate.ChatCompletionChunk) {
			b, err := translate.FormatSSEChunk(&chunk)
			if err == nil {
				_, _ = w.Write(b)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}

		translator := translate.NewResponseTranslator(chatReq.Model, func(delta translate.Delta) {
			if !started {
				started = true
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				w.WriteHeader(http.StatusOK)

				writeChunk(translate.ChatCompletionChunk{
					ID:      id,
					Object:  "chat.completion.chunk",
					Created: created,
					Model:   chatReq.Model,
					Choices: []translate.ChunkChoice{
						{
							Index: 0,
							Delta: translate.Delta{Role: "assistant"},
						},
					},
				})
			}

			writeChunk(translate.ChatCompletionChunk{
				ID:      id,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   chatReq.Model,
				Choices: []translate.ChunkChoice{
					{
						Index: 0,
						Delta: delta,
					},
				},
			})
		})

		streamErr := translate.ReadSSEEvents(result.Response.Body, func(event map[string]any) error {
			translator.Push(event)
			return nil
		})

		if !translator.SawOutput() && !started && attempt < EmptyStreamRetries {
			s.logger.Warn("empty response from upstream; retrying", "account", result.State.Email(), "model", upstream)
			return false, true
		}

		if !started {
			if streamErr != nil || !translator.SawOutput() {
				errMsg := "Google returned an empty response."
				if streamErr != nil {
					errMsg = fmt.Sprintf("Upstream stream failed: %v", streamErr)
				}
				sendJSONError(w, http.StatusBadGateway, errMsg, 0)
				return true, false
			}
			started = true
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.WriteHeader(http.StatusOK)

			writeChunk(translate.ChatCompletionChunk{
				ID:      id,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   chatReq.Model,
				Choices: []translate.ChunkChoice{
					{
						Index: 0,
						Delta: translate.Delta{Role: "assistant"},
					},
				},
			})
		}

		if streamErr != nil {
			s.logger.Error("upstream stream interrupted", "error", streamErr)
			return true, false
		}

		if details := translator.ReasoningDetails(); len(details) > 0 {
			writeChunk(translate.ChatCompletionChunk{
				ID:      id,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   chatReq.Model,
				Choices: []translate.ChunkChoice{
					{
						Index: 0,
						Delta: translate.Delta{ReasoningDetails: details},
					},
				},
			})
		}

		writeChunk(translate.ChatCompletionChunk{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   chatReq.Model,
			Choices: []translate.ChunkChoice{
				{
					Index:        0,
					Delta:        translate.Delta{},
					FinishReason: translator.Finish(),
				},
			},
		})

		if usage := translator.Usage(); usage != nil {
			writeChunk(translate.ChatCompletionChunk{
				ID:      id,
				Object:  "chat.completion.chunk",
				Created: created,
				Model:   chatReq.Model,
				Choices: []translate.ChunkChoice{},
				Usage:   usage,
			})
		}

		_, _ = w.Write(translate.FormatSSEDone())
		if flusher != nil {
			flusher.Flush()
		}
		return true, false
	}

	// Non-streaming completion
	translator := translate.NewResponseTranslator(chatReq.Model, func(delta translate.Delta) {})
	streamErr := translate.ReadSSEEvents(result.Response.Body, func(event map[string]any) error {
		translator.Push(event)
		return nil
	})

	if !translator.SawOutput() && attempt < EmptyStreamRetries {
		s.logger.Warn("empty response from upstream; retrying", "account", result.State.Email(), "model", upstream)
		return false, true
	}

	if streamErr != nil && !translator.SawOutput() {
		sendJSONError(w, http.StatusBadGateway, fmt.Sprintf("Upstream stream failed: %v", streamErr), 0)
		return true, false
	}

	completion := translator.Completion(id, created)
	sendJSON(w, http.StatusOK, completion)
	return true, false
}

// handleAnthropicMessages processes Anthropic Messages API requests (used by Claude Code CLI and OpenClaw),
// coordinating schema translation, transparent multi-account 429 rotation, thinking preservation, and SSE streaming.
func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		sendAnthropicJSONError(w, http.StatusBadRequest, fmt.Sprintf("Reading request body: %v", err), 0)
		return
	}

	var anthReq translate.AnthropicRequest
	if err := json.Unmarshal(bodyBytes, &anthReq); err != nil {
		sendAnthropicJSONError(w, http.StatusBadRequest, "Request body is not valid JSON.", 0)
		return
	}

	if err := translate.ValidateAnthropicRequest(&anthReq); err != nil {
		sendAnthropicJSONError(w, http.StatusBadRequest, err.Error(), 0)
		return
	}

	originalModel := anthReq.Model
	resolvedModel := catalog.ResolveModelAlias(anthReq.Model, s.config().ModelMapping)
	_ = s.catalog.Ensure(r.Context())
	info := s.catalog.Info(resolvedModel)
	upstream := catalog.UpstreamModel(resolvedModel)
	anthReq.Model = resolvedModel

	sessionID := cmp.Or(
		r.Header.Get("X-Session-Affinity"),
		r.Header.Get("session_id"),
		r.Header.Get("X-Session-Id"),
	)

	for attempt := range EmptyStreamRetries + 1 {
		opts := translate.BuildOptions{
			Info:          info,
			LegacySchemas: s.legacySchemas.Load(),
			SessionID:     sessionID,
		}

		geminiReq, reqErr := translate.BuildAnthropicRequest(&anthReq, opts)
		if reqErr != nil {
			sendAnthropicJSONError(w, http.StatusBadRequest, reqErr.Error(), 0)
			return
		}

		attemptResult, err := s.rotator.Generate(r.Context(), upstream, func(st *rotator.AccountState) (any, error) {
			return translate.WrapEnvelope(geminiReq, upstream, st.ProjectID()), nil
		})
		if err != nil {
			var rotErr *rotator.RotatorError
			if errors.As(err, &rotErr) {
				// Protocol Quirk: If upstream rejects parametersJsonSchema, remember legacy mode and retry
				if !s.legacySchemas.Load() && translate.IsSchemaRejection(rotErr.StatusCode, rotErr.ErrorMessage) {
					s.legacySchemas.Store(true)
					s.logger.Info("Google refused parametersJsonSchema; switching tool schemas to legacy parameters field")
					continue
				}
				sendAnthropicJSONError(w, rotErr.StatusCode, rotErr.ErrorMessage, rotErr.RetryAfter)
				return
			}
			sendAnthropicJSONError(w, http.StatusBadGateway, err.Error(), 0)
			return
		}

		anthReq.Model = originalModel
		done, retry := s.processAnthropicResponse(w, r, &anthReq, attemptResult, upstream, attempt)
		if retry {
			anthReq.Model = resolvedModel
			continue
		}
		if done {
			return
		}
	}
}

// processAnthropicResponse handles the upstream HTTP response for Anthropic Messages requests.
func (s *Server) processAnthropicResponse(
	w http.ResponseWriter,
	r *http.Request,
	anthReq *translate.AnthropicRequest,
	result *rotator.AttemptResult,
	upstream string,
	attempt int,
) (done bool, retry bool) {
	defer result.Response.Body.Close()

	flusher, _ := w.(http.Flusher)

	if anthReq.Stream {
		var started bool
		writeChunk := func(eventType string, data any) {
			b, err := translate.FormatAnthropicSSEEvent(eventType, data)
			if err == nil {
				if !started {
					started = true
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Cache-Control", "no-cache")
					w.Header().Set("Connection", "keep-alive")
					w.WriteHeader(http.StatusOK)
				}
				_, _ = w.Write(b)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}

		translator := translate.NewAnthropicStreamTranslator(anthReq.Model, writeChunk)

		streamErr := translate.ReadSSEEvents(result.Response.Body, func(event map[string]any) error {
			translator.Push(event)
			return nil
		})

		if !translator.SawOutput() && !started && attempt < EmptyStreamRetries {
			s.logger.Warn("empty response from upstream; retrying", "account", result.State.Email(), "model", upstream)
			return false, true
		}

		if !started {
			if streamErr != nil || !translator.SawOutput() {
				errMsg := "Google returned an empty response."
				if streamErr != nil {
					errMsg = fmt.Sprintf("Upstream stream failed: %v", streamErr)
				}
				sendAnthropicJSONError(w, http.StatusBadGateway, errMsg, 0)
				return true, false
			}
		}

		if streamErr != nil {
			s.logger.Error("upstream stream interrupted", "error", streamErr)
			return true, false
		}

		translator.Finish()
		if flusher != nil {
			flusher.Flush()
		}
		return true, false
	}

	// Non-streaming completion
	translator := translate.NewAnthropicStreamTranslator(anthReq.Model, nil)
	streamErr := translate.ReadSSEEvents(result.Response.Body, func(event map[string]any) error {
		translator.Push(event)
		return nil
	})

	if !translator.SawOutput() && attempt < EmptyStreamRetries {
		s.logger.Warn("empty response from upstream; retrying", "account", result.State.Email(), "model", upstream)
		return false, true
	}

	if streamErr != nil && !translator.SawOutput() {
		sendAnthropicJSONError(w, http.StatusBadGateway, fmt.Sprintf("Upstream stream failed: %v", streamErr), 0)
		return true, false
	}

	translator.Finish()
	resp := translator.Response()
	sendJSON(w, http.StatusOK, resp)
	return true, false
}
