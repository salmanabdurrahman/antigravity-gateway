// Package server implements the local OpenAI-compatible reverse proxy daemon,
// routing incoming completions requests to Google Cloud Code Assist upstream endpoints.
package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/catalog"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/translate"
)

// maxContinuationHops bounds Gemini 4 long-decode chaining so a misbehaving upstream cannot keep a
// single client request alive indefinitely across continuation segments.
const maxContinuationHops = 16

// streamTranslator is the shared surface required to chain long-decode continuation hops across the
// OpenAI and Anthropic stream translators.
type streamTranslator interface {
	// Push ingests one upstream SSE event map.
	Push(event map[string]any)

	// ContinuationToken returns the long-decode token captured from the current hop, if any.
	ContinuationToken() string

	// ClearContinuationToken resets the token before the next hop is read.
	ClearContinuationToken()
}

// readUpstreamHops consumes the upstream SSE body of the current attempt and, while Gemini reports a
// long-decode continuation token, dispatches bounded follow-up hops that reuse the original payload.
//
// Protocol Quirk: a Gemini 4 long decode stops a candidate with finishReason "CONTINUATION" plus an
// opaque continuationToken once it reaches the per-request output boundary. Only the token may be
// added to the follow-up request; the original contents, system instruction, tools, and
// maxOutputTokens must be resent unchanged, and the partial model text must not be appended.
//
// allowContinuation is false when the client supplied an explicit output cap: Cloud Code accumulates
// maxOutputTokens across continuation hops, so a capped request cannot resend the same ceiling and is
// finished as a length-limited response instead of failing the follow-up with HTTP 400.
func (s *Server) readUpstreamHops(
	ctx context.Context,
	upstream string,
	first *rotator.AttemptResult,
	continuation func(st *rotator.AccountState, token string) (any, error),
	translator streamTranslator,
	allowContinuation bool,
) error {
	current := first
	// The final hop body is closed on return; intermediate hops are closed as soon as they are drained
	// so a long continuation chain never accumulates open response bodies.
	defer func() { _ = current.Response.Body.Close() }()
	for hop := range maxContinuationHops + 1 {
		translator.ClearContinuationToken()
		if err := translate.ReadSSEEvents(current.Response.Body, func(event map[string]any) error {
			translator.Push(event)
			return nil
		}); err != nil {
			return err
		}

		token := translator.ContinuationToken()
		if token == "" || continuation == nil {
			return nil
		}
		if !allowContinuation {
			s.logger.Debug("long-decode continuation skipped because the request set an explicit output cap",
				"model", upstream, "hop", hop)
			return nil
		}
		if hop >= maxContinuationHops {
			s.logger.Warn("long-decode continuation hop limit reached", "model", upstream, "hops", hop)
			return nil
		}

		next, err := s.rotator.Generate(ctx, upstream, func(st *rotator.AccountState) (any, error) {
			return continuation(st, token)
		})
		if err != nil {
			return fmt.Errorf("continuing long decode after %d hop(s): %w", hop+1, err)
		}
		// A continuation token may be bound to the account that produced it, so a switch is surfaced
		// explicitly instead of failing silently mid-stream. Only account emails are logged.
		if firstAccount := first.State.Email(); next.State.Email() != firstAccount {
			s.logger.Warn("long-decode continuation switched account",
				"model", upstream, "from", firstAccount, "to", next.State.Email(), "hop", hop+1)
		}
		// Closing the previous hop body after its stream is drained is safe and keeps descriptors
		// bounded across long continuation chains.
		_ = current.Response.Body.Close()
		current = next
	}
	return nil
}

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
	// Strategy is the currently active rotation strategy in the running daemon.
	Strategy string `json:"strategy,omitempty"`

	// Accounts enumerates non-sensitive operational states for configured accounts.
	Accounts []rotator.AccountSnapshot `json:"accounts"`
}

// buildHandler constructs the HTTP ServeMux and attaches logging, auth, and recovery middleware.
func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1/models/{id}", s.handleModelByID)
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

// handleHealth answers liveness probes and reports gateway version and usable account counts. The
// endpoint is unauthenticated, so it stays restricted to loopback authorities.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackHost(r.Host) {
		sendJSONError(w, http.StatusForbidden, "Forbidden: loopback access only", 0)
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
		items[i] = newModelItem(m)
	}

	sendJSON(w, http.StatusOK, ModelListResponse{
		Object: "list",
		Data:   items,
	})
}

// newModelItem converts a catalog model into the OpenAI model descriptor returned by the models API.
func newModelItem(m catalog.CatalogModel) ModelItem {
	return ModelItem{
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

// handleModelByID retrieves a single model descriptor, resolving configured and built-in aliases the
// same way a completions request would, and answers an OpenAI-compatible 404 for unknown models.
func (s *Server) handleModelByID(w http.ResponseWriter, r *http.Request) {
	requested := strings.TrimSpace(r.PathValue("id"))
	if requested == "" {
		sendJSONError(w, http.StatusNotFound, "Model identifier is required.", 0)
		return
	}

	if err := s.catalog.Ensure(r.Context()); err != nil {
		if snap, _ := s.catalog.Snapshot(); len(snap.Models) == 0 {
			status := http.StatusBadGateway
			var rotErr *rotator.RotatorError
			if errors.As(err, &rotErr) {
				status = rotErr.StatusCode
			}
			sendJSONError(w, status, fmt.Sprintf("Could not resolve model '%s': %v", requested, err), 0)
			return
		}
	}

	resolved := catalog.ResolveModelAlias(requested, s.config().ModelMapping)
	upstream := catalog.UpstreamModel(resolved)

	snap, _ := s.catalog.Snapshot()
	for _, m := range snap.Models {
		if strings.EqualFold(m.ID, requested) || strings.EqualFold(m.ID, resolved) || strings.EqualFold(m.ID, upstream) {
			sendJSON(w, http.StatusOK, newModelItem(m))
			return
		}
	}

	sendJSONError(w, http.StatusNotFound, fmt.Sprintf("The model '%s' does not exist", requested), 0)
}

// handleAccounts returns non-secret account diagnostics, active strategy, request metrics, and cooldown states.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	snapshots := s.rotator.Pool().Snapshots()
	sendJSON(w, http.StatusOK, AccountsResponse{
		Strategy: string(s.rotator.Strategy()),
		Accounts: snapshots,
	})
}

// isLoopbackHost verifies whether the host header represents a local loopback interface.
// This prevents DNS rebinding attacks against unauthenticated local liveness endpoints.
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
	// Clients that send no affinity header still need a stable session so sticky rotation and the
	// thought-signature cache survive multi-turn tool loops. Derivation stays limited to tool-enabled
	// turns because a session identifier marks the envelope as agent traffic.
	if sessionID == "" && len(chatReq.Tools) > 0 {
		sessionID = deriveSessionID(resolvedModel, openingChatPrompt(chatReq.Messages))
	}

	// Replay the last recorded recipe when the client stripped reasoning_details between turns.
	s.recipeCache.Apply(sessionID, chatReq.Messages)

	randID := make([]byte, 12)
	_, _ = rand.Read(randID)
	id := fmt.Sprintf("chatcmpl-%s", hex.EncodeToString(randID))
	created := time.Now().Unix()

	// Protocol Quirk: a prompt rejected by upstream content filtering is retried exactly once with
	// the system instruction moved into the first user turn. The extra loop iteration keeps the
	// retry independent from the empty-stream retry budget.
	promptFallback := false
	for attempt := range s.emptyStreamRetryCount() + 2 {
		opts := translate.BuildOptions{
			Info:          info,
			LegacySchemas: s.legacySchemas.Load(),
			SessionID:     sessionID,
			InlineSystem:  promptFallback,
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
				if rotErr.PromptRejected && !promptFallback {
					promptFallback = true
					s.logger.Warn("upstream rejected prompt; retrying with system instructions moved into the first user turn", "model", upstream)
					continue
				}
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

		// Long-decode continuations resend the identical payload with only the token added.
		continuation := func(st *rotator.AccountState, token string) (any, error) {
			hopReq := *geminiReq
			hopReq.ContinuationToken = token
			return translate.WrapEnvelope(&hopReq, upstream, st.ProjectID()), nil
		}

		chatReq.Model = originalModel
		done, retry := s.processCompletionsResponse(w, r, &chatReq, attemptResult, continuation, id, created, upstream, attempt, sessionID)
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
	continuation func(st *rotator.AccountState, token string) (any, error),
	id string,
	created int64,
	upstream string,
	attempt int,
	sessionID string,
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

		streamErr := s.readUpstreamHops(r.Context(), upstream, result, continuation, translator, chatReq.MaxTokens == nil && chatReq.MaxCompletionTokens == nil)

		if !translator.SawOutput() && !started && attempt < s.emptyStreamRetryCount() {
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
			s.recipeCache.Store(sessionID, details)
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
	streamErr := s.readUpstreamHops(r.Context(), upstream, result, continuation, translator, chatReq.MaxTokens == nil && chatReq.MaxCompletionTokens == nil)

	if !translator.SawOutput() && attempt < s.emptyStreamRetryCount() {
		s.logger.Warn("empty response from upstream; retrying", "account", result.State.Email(), "model", upstream)
		return false, true
	}

	if streamErr != nil && !translator.SawOutput() {
		sendJSONError(w, http.StatusBadGateway, fmt.Sprintf("Upstream stream failed: %v", streamErr), 0)
		return true, false
	}

	if details := translator.ReasoningDetails(); len(details) > 0 {
		s.recipeCache.Store(sessionID, details)
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
	// Anthropic clients rarely send an affinity header, so a tool-enabled conversation derives a stable
	// session from its opening prompt while plain chat keeps an empty (non-agent) session.
	if sessionID == "" && len(anthReq.Tools) > 0 {
		sessionID = deriveSessionID(resolvedModel, openingAnthropicPrompt(anthReq.Messages))
	}

	// The reasoning-recipe cache is intentionally OpenAI-only: Anthropic clients replay signed `thinking`
	// blocks inside message content themselves, so there is no stripped reasoning_details field to restore.
	// The derived session identifier still stabilizes upstream session affinity for tool loops.

	// Protocol Quirk: a prompt rejected by upstream content filtering is retried exactly once with
	// the system instruction moved into the first user turn before the error is surfaced.
	promptFallback := false
	for attempt := range s.emptyStreamRetryCount() + 2 {
		opts := translate.BuildOptions{
			Info:          info,
			LegacySchemas: s.legacySchemas.Load(),
			SessionID:     sessionID,
			InlineSystem:  promptFallback,
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
				if rotErr.PromptRejected && !promptFallback {
					promptFallback = true
					s.logger.Warn("upstream rejected prompt; retrying with system instructions moved into the first user turn", "model", upstream)
					continue
				}
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

		// Long-decode continuations resend the identical payload with only the token added.
		continuation := func(st *rotator.AccountState, token string) (any, error) {
			hopReq := *geminiReq
			hopReq.ContinuationToken = token
			return translate.WrapEnvelope(&hopReq, upstream, st.ProjectID()), nil
		}

		anthReq.Model = originalModel
		done, retry := s.processAnthropicResponse(w, r, &anthReq, attemptResult, continuation, upstream, attempt)
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
	continuation func(st *rotator.AccountState, token string) (any, error),
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

		streamErr := s.readUpstreamHops(r.Context(), upstream, result, continuation, translator, anthReq.MaxTokens <= 0)

		if !translator.SawOutput() && !started && attempt < s.emptyStreamRetryCount() {
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
	streamErr := s.readUpstreamHops(r.Context(), upstream, result, continuation, translator, anthReq.MaxTokens <= 0)

	if !translator.SawOutput() && attempt < s.emptyStreamRetryCount() {
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
