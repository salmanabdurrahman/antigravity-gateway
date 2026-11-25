# Architecture Overview

Antigravity Gateway (`agw`) is a single Go binary with two responsibilities: a CLI that manages credentials and client configuration, and a loopback HTTP daemon that translates agent requests into Google Cloud Code Assist envelopes.

## 1. Request pipeline

```text
AI coding agent (Pi / OpenCode / Claude Code / Cursor)
        │  OpenAI /v1/chat/completions  or  Anthropic /v1/messages
        ▼
┌──────────────────────────────────────────────────────────────┐
│ internal/server      loopback listener, bearer auth, routes   │
│ internal/translate   OpenAI/Anthropic → Cloud Code envelopes   │
│ internal/rotator     account pick, cooldowns, 429 failover     │
│ internal/catalog     model catalog, token limits, quotas       │
└──────────────────────────────────────────────────────────────┘
        │  POST v1internal:generateContent / streamGenerateContent
        ▼
Google Cloud Code Assist (daily-cloudcode-pa / cloudcode-pa)
```

Responses travel the same path backwards: Cloud Code SSE chunks and JSON payloads are translated into OpenAI Chat Completions or Anthropic Messages shapes, including reasoning tokens typed as `reasoning_details`, `reasoning_content`, or Anthropic `thinking` blocks.

## 2. Package map

| Path                     | Responsibility                                                       |
| :----------------------- | :------------------------------------------------------------------- |
| `cmd/agw/`               | Cobra CLI commands; parsing and delegation only.                     |
| `internal/config/`       | `~/.agw` resolution, defaults, env overrides, header builders.       |
| `internal/auth/`         | OAuth PKCE, token refresh, companion project discovery, storage.     |
| `internal/rotator/`      | Account pool, `sticky`/`round-robin`/`hybrid` pickers, cooldowns.    |
| `internal/catalog/`      | Model discovery, token limits, quota tracking, disk cache.           |
| `internal/translate/`    | Bidirectional schema mapping, thought signatures, SSE streaming.     |
| `internal/server/`       | `net/http` daemon, middleware, routes, signature cache.              |
| `internal/client/`       | Pi, OpenCode, and Claude Code config injectors with backups.         |
| `internal/updater/`      | Release discovery, SHA-256 verification, atomic self-upgrade.        |

## 3. Protocol fidelity

Cloud Code Assist is an internal surface, so correctness depends on reproducing its expectations rather than only its shapes.

- **Thought signatures**: Gemini 2.x and newer plus Claude models validate cryptographic `thoughtSignature` values across tool turns. The gateway preserves authentic signatures, replays them from a bounded per-session cache keyed by tool-call IDs, and substitutes `skip_thought_signature_validator` only when a client stripped a signature that the upstream expects.
- **Schema sanitization**: tool JSON schemas pass through a multi-phase pipeline that hoists `$ref` collisions, flattens unions, repairs malformed array declarations across every composition keyword, and falls back to legacy `parameters` mode when upstream rejects a schema.
- **Thinking budgets**: categorical thinking levels and numeric budgets are clamped per model family; explicit output caps shrink reasoning, and a cap below the family floor (1024 tokens for Claude and `gpt-oss`) disables reasoning instead of emitting an invalid envelope.
- **Claude specifics**: the mandatory `anthropic-beta: interleaved-thinking-2025-05-14` header, `cache_control` stripping, and third-party agent identity line scrubbing keep Claude requests acceptable to upstream validation.
- **Streaming**: SSE deltas preserve `index: 0` on tool-call chunks, keep reasoning separate from content, and use an expanded scanner buffer (10 MB) so large diffs and thinking blocks are never truncated.

## 4. Security posture

- **Loopback only**: the daemon and the OAuth callback listener bind strictly to `127.0.0.1`; configured non-loopback hosts are coerced back to loopback.
- **Local credentials**: tokens, the generated API key, and cached catalog data live in `~/.agw` with `0700`/`0600` permissions and atomic writes; nothing is reported to the project.
- **No secret logging**: access tokens, refresh tokens, bearer keys, and authorization headers are never printed or serialized.
- **Authenticated API surface**: all `/v1/*` endpoints require the local Bearer key (`x-api-key` also accepted on `/v1/messages`), leaving only the loopback-restricted liveness probe (`/health` and `/`) unauthenticated.

Full invariants and the public-client OAuth disclosure are in [SECURITY.md](../SECURITY.md).

## 5. Performance characteristics

Benchmark on one machine with `gemini-3.8-flash`, comparing `agw` with a multi-provider Node.js proxy (`9router`):

| Metric / Capability             | Antigravity Gateway (`agw`)                                      | 9router                             |
| :------------------------------ | :--------------------------------------------------------------- | :---------------------------------- |
| **Runtime architecture**        | Single compiled Go binary (zero dependencies)                    | Node.js + Next.js + SQLite          |
| **Memory footprint (RSS)**      | **~20 MB** (19–22 MB measured)                                   | **~135 MB**                         |
| **Startup latency**             | Instant (< 5 ms)                                                 | ~2 – 4 seconds                      |
| **Network security**            | **Strict loopback (`127.0.0.1:51400`)**                          | `0.0.0.0` (LAN-exposed by default)  |
| **Gemini thought signatures**   | **Lossless preservation**                                        | Stripped / normalized               |
| **Multi-turn tool calling**     | **Signature replay + validator bypass**                          | Prone to Google HTTP 400 on turn 2+ |
| **Claude interleaved thinking** | Automatic `anthropic-beta` header injection                      | Adapter-dependent                   |
| **Client auto-setup**           | Transactional CLI setup (`agw setup pi` / `opencode` / `claude`) | Manual client configuration         |

End-to-end measurements against live accounts (macOS arm64, `gemini-2.5-flash`): sequential requests p50 ≈ 0.9 s, six concurrent requests completed in ≈ 1.0 s wall time, local guard endpoints answer in < 5 ms, and `/v1/models` costs ≈ 2.5 s on a cold cache.

## 6. Design decisions

- **Zero dependencies**: only the Go standard library plus Cobra — no Node, Electron, or database engine to install or audit.
- **Single-binary distribution**: cross-compiled archives for six platform/architecture targets with per-release checksums, plus an in-place updater that verifies digests before replacing the executable.
- **Transactional client injection**: `agw setup` writes through atomic temp-file renames, keeps timestamped backups, and verifies connectivity with a 1-token completion before declaring success.
- **Fail fast on policy**: permanent 403 suspensions disable the account with the upstream reason rather than retrying into the same wall, and 429 handling distinguishes prompt-scoped rejections from account-scoped throttles.

## 7. Related documents

- [CLI Reference](cli.md)
- [HTTP API Reference](api.md)
- [Configuration Reference](configuration.md)
- [Binary Installation Contract](distribution.md)
- [OAuth 2.0 Client Strategy](oauth-strategy.md)
