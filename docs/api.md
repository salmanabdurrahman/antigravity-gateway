# HTTP API Reference

The gateway exposes OpenAI Chat Completions and Anthropic Messages on local loopback only (`127.0.0.1:51400` by default). Authentication uses the local API key stored in `config.json` (`agw_` plus 64 hexadecimal characters), except for the unauthenticated loopback endpoints listed below.

## 1. Endpoints

| Method & Route              | Purpose                                                                | Authentication                     |
| :-------------------------- | :--------------------------------------------------------------------- | :--------------------------------- |
| `GET /health`               | Liveness probe: gateway name, version, active account count            | None (loopback authority required) |
| `GET /v1/models`            | OpenAI-compatible model listing with context windows and capabilities  | Bearer token                       |
| `GET /v1/models/{id}`       | Single model descriptor by identifier or alias, OpenAI 404 otherwise   | Bearer token                       |
| `POST /v1/chat/completions` | OpenAI-compatible chat completions, streaming or buffered              | Bearer token                       |
| `POST /v1/messages`         | Anthropic Messages API for Claude Code CLI and compatible clients      | `x-api-key` or Bearer              |
| `GET /v1/accounts`          | Redacted account diagnostics, cooldowns, and request metrics           | Bearer token                       |
| `GET /`                     | Health payload, or `302` to `/ui` for HTML clients                     | None (loopback only)               |
| `GET /ui`                   | Embedded dashboard HTML                                                | None (loopback only)               |
| `GET /ui/api/status`        | Dashboard status, catalog, quotas, and rolling quota windows           | None (loopback only)               |
| `POST /ui/api/strategy`     | Same-origin rotation strategy change, persisted to the launched config | Same-origin request signal         |
| `POST /ui/api/refresh`      | Same-origin forced live quota and catalog refresh                      | Same-origin request signal         |

Errors use a single envelope across endpoints:

```json
{ "error": { "message": "…", "type": "invalid_request_error", "code": 400 } }
```

`code` carries the HTTP status code; `type` categorizes the failure (for example `invalid_request_error`, `authentication_error`, `rate_limit_error`).

Dashboard endpoints, framing headers, and same-origin rules are specified in the [Web Dashboard Contract](webui.md).

## 2. Quick tests

```bash
API_KEY=$(grep -o '"api_key": "[^"]*' ~/.agw/config.json | cut -d'"' -f4)

# OpenAI-compatible chat completion
curl -s http://127.0.0.1:51400/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d '{
    "model": "gemini-4-hydrogen-high",
    "messages": [{"role": "user", "content": "Hello!"}],
    "max_tokens": 50
  }' | jq .

# Anthropic Messages
curl -s http://127.0.0.1:51400/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: $API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "claude-sonnet-4-6",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "Hello!"}]
  }' | jq .

# Streaming (OpenAI SSE)
curl -sN http://127.0.0.1:51400/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d '{"model":"gemini-2.5-flash","stream":true,"messages":[{"role":"user","content":"Count to 3"}]}'
```

## 3. Request & response translation

The gateway transparently translates OpenAI Chat Completions requests into Google Cloud Code Assist protocol envelopes and back.

- **Multimodal support**: maps OpenAI base64 `data:` URLs directly into Cloud Code `inlineData`.
- **Tool schema defenses**: converts OpenAI function `parameters` for Gemini (`parametersJsonSchema`) and Claude (`parameters`), stripping disallowed attributes (`$ref`, `$defs`, `$schema`, unsupported unions) and flattening composition keywords. HTTP 400 schema rejections are detected at runtime and retried in legacy `parameters` mode.
- **Tool choice enforcement**: `tool_choice: "none"` (and the Anthropic `{"type": "none"}` form) omits tool declarations entirely, because upstream Gemini can still invoke a declared tool even when `functionCallingConfig` mode is `NONE`.
- **Deterministic output caps**: an explicit `max_tokens` / `max_completion_tokens` (and Anthropic `max_tokens`) is a hard ceiling. The reasoning budget shrinks to fit under it and is disabled when the cap falls below the model's minimum budget (1024 tokens for Claude and for `gpt-oss`), so clients get predictable cost and latency. Cloud Code accumulates the ceiling across long-decode hops, so continuation is skipped for capped requests: they end with OpenAI `finish_reason: "length"` or Anthropic `stop_reason: "max_tokens"`. Long-decode chaining therefore requires omitting the cap on the OpenAI endpoint; the Anthropic schema always carries `max_tokens`, so it does not chain.
- **Thought signature & replay**: encodes upstream thinking tokens, steps, and Gemini/Claude cryptographic `thoughtSignature` data into OpenAI `reasoning_details`. Missing signatures on tool re-execution are replaced with `skip_thought_signature_validator` for Gemini generations that validate them (2.x and newer) to avoid upstream tool validation failures, while authentic client signatures are always preserved.
- **Streaming SSE engine**: streams `v1internal:streamGenerateContent?alt=sse` chunks as OpenAI SSE chunks (`choices[0].delta.content`, `choices[0].delta.reasoning_content`, and `choices[0].delta.tool_calls`), with scanner capacity buffered up to 10 MB to handle large code diffs and thinking blocks.
- **Session affinity**: tool-enabled requests that carry no `X-Session-Affinity` or `session_id` header get a stable session derived from the model, system instructions, and opening user turn. That session marks the envelope as agent traffic (`requestType: "agent"`) and lets the bounded signature cache replay `reasoning_details` after a client strips them. Plain chat requests without tools keep an empty session and a non-agent envelope.

## 4. Behavior changes & migration notes

These behaviors are intentionally stricter than earlier releases. Review them before upgrading scripts or automation that talks to the daemon directly.

- **`/health` requires a loopback authority.** The unauthenticated liveness probe only answers requests whose `Host` header (or bracketed `Host` for IPv6) resolves to `127.0.0.1`, `localhost`, or `::1`; anything else receives `403`. SSH local forwards and `agw`'s own probes keep working because they preserve the loopback authority, while a reverse proxy that rewrites `Host` to a public name no longer passes.
- **Dashboard mutations require a same-origin signal and JSON.** `POST /ui/api/strategy` and `POST /ui/api/refresh` reject requests that carry neither a matching `Origin` nor `Sec-Fetch-Site: same-origin`, and cross-port loopback origins count as cross-origin. `POST /ui/api/strategy` additionally requires `Content-Type: application/json`; form-encoded bodies receive `415`.

  ```bash
  curl -s -X POST http://127.0.0.1:51400/ui/api/strategy \
    -H 'Content-Type: application/json' \
    -H 'Sec-Fetch-Site: same-origin' \
    -d '{"strategy":"sticky"}'
  ```

- **`agw accounts --check` exits non-zero when the configuration cannot be loaded.** It previously printed an `Error (config)` status column and exited `0`, letting broken configuration pass unnoticed in scripts. Live quota inspection now aborts with the configuration error, and `--re-enable` still requires `--check`.
- **Explicit output caps are enforced.** `max_tokens` / `max_completion_tokens` / Anthropic `max_tokens` bound total generation instead of being expanded to leave reasoning headroom. Reasoning adapts to the cap and is disabled when the cap is below the model minimum, and `tool_choice: "none"` omits tool declarations so upstream cannot invoke a disabled tool.
- **Tool-enabled requests derive a session identifier.** See session affinity above; clients that want explicit affinity should send `X-Session-Affinity`.
