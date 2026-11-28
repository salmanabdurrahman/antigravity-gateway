# Configuration Reference

All gateway state lives under `~/.agw` (POSIX) or `%USERPROFILE%\.agw` (Windows). The directory is created on first run with `0700` permissions; every file inside it is written with `0600` through atomic temp-file renames.

Override the root with `AGW_HOME=/custom/path`, or point a single run at another config file with `agw --config /path/config.json`.

## 1. Files

| Path                       | Purpose                                                                                      | Permissions |
| :------------------------- | :------------------------------------------------------------------------------------------- | :---------- |
| `~/.agw/config.json`       | Daemon settings, host/port, local API key, rotation strategy, quota threshold, model mapping | `0600`      |
| `~/.agw/accounts.json`     | Stored Google account tokens, imported desktop tokens, companion projects, tiers             | `0600`      |
| `~/.agw/models-cache.json` | Cached model catalog, token limits, quota windows                                            | `0600`      |
| `~/.agw/agw.pid`           | Running daemon process ID (written once listening)                                           | `0600`      |
| `~/.agw/agw.log`           | Background daemon stdout/stderr                                                              | `0600`      |

`config.json` is the single source of truth for gateway settings, and a running daemon treats it as read-only rather than overwriting your API key, endpoints, or model mapping. Inspect redacted settings with `agw config show`, resolve the active file path with `agw config path`, and update `strategy` safely with `agw config strategy <mode>`. The daemon logs its resolved configuration path at startup, and `agw doctor` verifies that the stored key still matches the generated format and the key embedded in each configured client.

## 2. `config.json` keys

| Key                        | Type              | Default                                 | Purpose                                                                                                                    |
| :------------------------- | :---------------- | :-------------------------------------- | :------------------------------------------------------------------------------------------------------------------------- |
| `host`                     | string            | `127.0.0.1`                             | Loopback interface to bind; non-loopback values are coerced to `127.0.0.1`.                                                |
| `port`                     | integer           | `51400`                                 | Local TCP port for the gateway.                                                                                            |
| `api_key`                  | string            | generated `agw_` + 64 hex               | Bearer token required by `/v1/*` endpoints.                                                                                |
| `strategy`                 | string            | `sticky`                                | Rotation strategy: `sticky`, `round-robin`, or `hybrid`.                                                                   |
| `client_id`                | string            | bundled desktop client                  | Google OAuth client identifier (BYOC override).                                                                            |
| `client_secret`            | string            | bundled desktop client                  | Google OAuth client secret (BYOC override; the bundled value is a public client credential).                               |
| `auth_url`                 | string            | Google OAuth endpoint                   | Authorization endpoint override.                                                                                           |
| `token_url`                | string            | Google OAuth endpoint                   | Token exchange endpoint override.                                                                                          |
| `user_info_url`            | string            | Google userinfo endpoint                | Account identity endpoint override.                                                                                        |
| `redirect_uri`             | string            | `http://127.0.0.1:51121/oauth-callback` | Local OAuth callback listener.                                                                                             |
| `endpoints`                | array of strings  | tier-ordered default pair               | Upstream Cloud Code Assist endpoints, tried in order.                                                                      |
| `user_agent`               | string (optional) | derived Antigravity Hub UA              | Overrides the upstream `User-Agent`.                                                                                       |
| `api_client`               | string (optional) | derived                                 | Overrides the `X-Goog-Api-Client` header.                                                                                  |
| `client_metadata`          | string (optional) | derived                                 | Overrides the `Client-Metadata` JSON header.                                                                               |
| `quota_threshold`          | float (0.0–1.0)   | `0.05` (5%)                             | Remaining-quota floor before `hybrid` deprioritizes an account.                                                            |
| `model_mapping`            | object            | empty                                   | Custom alias → backend model ID map.                                                                                       |
| `max_wait_seconds`         | integer           | `20` (range 1–120)                      | Longest cooldown window the rotator absorbs before returning HTTP 429 with `Retry-After`.                                  |
| `default_cooldown_seconds` | integer           | `60` (range 1–3600)                     | Base 429 cooldown when upstream omits retry metadata; repeated 429s escalate 1x, 5x, 30x, 120x.                            |
| `empty_stream_retries`     | integer           | `2` (range 1–5)                         | Extra attempts when Google returns no visible text, reasoning text, or tool calls (including signature-only empty frames). |

`host` accepts IPv4 and IPv6 loopback addresses; non-loopback values are coerced back to `127.0.0.1`. IPv6 literals are bracketed everywhere the daemon binds or builds a URL, so `host: "::1"` yields `[::1]:51400`.

## 3. Environment overrides

| Variable                                                     | Purpose                                                                                                                                                                    |
| :----------------------------------------------------------- | :------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `AGW_HOME`                                                   | Root path for configuration and credential storage (default `~/.agw`).                                                                                                     |
| `AGW_CLIENT_ID`                                              | Custom Google OAuth 2.0 client ID for BYOC setups.                                                                                                                         |
| `AGW_CLIENT_SECRET`                                          | Custom Google OAuth 2.0 client secret for BYOC setups. The bundled default is a desktop **public client**, so its embedded credential is public by design (RFC 8252 §8.5). |
| `AGW_USER_AGENT` (legacy `PGA_USER_AGENT`)                   | Custom `User-Agent` header.                                                                                                                                                |
| `AGW_API_CLIENT` (legacy `PGA_API_CLIENT`)                   | Custom `X-Goog-Api-Client` header.                                                                                                                                         |
| `AGW_CLIENT_METADATA` (legacy `PGA_CLIENT_METADATA`)         | Custom `Client-Metadata` JSON header.                                                                                                                                      |
| `AGW_ANTIGRAVITY_VERSION` (legacy `PGA_ANTIGRAVITY_VERSION`) | Antigravity Hub version advertised in the default user agent (default `2.33.0`).                                                                                           |
| `AGW_ANTIGRAVITY_CHANGELIST`                                 | Piper changelist (`cl`) token paired with the Hub version; known releases resolve their own changelist.                                                                    |
| `AGW_PREFER_IPV4`                                            | Set to `1` to dial IPv4 before IPv6 when a published IPv6 route blackholes traffic.                                                                                        |
| `AGW_AGY_TOKEN_PATH`                                         | Explicit Antigravity CLI (`agy`) token file for `agw login --local`.                                                                                                       |
| `AGW_AGY_KEYRING`                                            | Set to `1` to opt in to a read-only OS keyring lookup for the `agy` session token.                                                                                         |
| `AGW_BIN`                                                    | Overrides the executable path a detached daemon reports and re-executes; intended for test harnesses and relocated builds.                                                 |
| `PI_AGENT_DIR`                                               | Pi Coding Agent configuration directory (default `~/.pi/agent`).                                                                                                           |
| `OPENCODE_CONFIG`                                            | OpenCode configuration file (default `~/.config/opencode/opencode.json`).                                                                                                  |
| `CLAUDE_CONFIG_DIR`                                          | Claude Code CLI configuration directory (default `~/.claude`).                                                                                                             |
| `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`                    | Standard outbound proxy variables, honored for all upstream API calls.                                                                                                     |

Default upstream requests identify as `antigravity/hub/<version> (aidev_client; os_type=<os>; arch=<arch>; cl=<changelist>)`. Override the whole header with `AGW_USER_AGENT` when proxying a different client identity.

## 4. Model aliasing

Two public identifiers are mapped automatically to Google's internal Cloud Code Assist identifiers:

| Public model name     | Upstream model identifier | Family   |
| :-------------------- | :------------------------ | :------- |
| `gemini-3.1-pro-high` | `gemini-pro-agent`        | Gemini 3 |
| `gpt-oss-120b`        | `gpt-oss-120b-medium`     | GPT-OSS  |

Custom aliases are defined in `config.json` through `model_mapping`, which takes precedence over the built-in table:

```json
{
  "model_mapping": {
    "gemini-4-argon": "gemini-4-argon-high",
    "claude-custom": "claude-sonnet-5-5-high"
  }
}
```

Aliases apply to chat completions, Anthropic messages, and `/v1/models/{id}` lookups.

## 5. Account rotation & failover

The rotator supports three strategies, configured through `strategy` in `config.json`, `agw config strategy <mode>`, or `agw start --strategy`:

- `sticky` (default): keeps the current active account until rate limits (HTTP 429) occur, preserving upstream prompt caching.
- `round-robin`: cycles sequentially through all non-cooling accounts on every request.
- `hybrid`: multi-signal load balancing combining rolling operational health scores, token-bucket burst rate limiting, proactive quota protection (parking accounts whose remaining quota drops below `quota_threshold`, default 5%), and LRU freshness.

### Rate-limit handling

- Upstream `resetTime` (RFC3339) or `retryDelay` is parsed to compute the exact cooldown duration (default 60s).
- A bare `429 RESOURCE_EXHAUSTED` without `details[]`, retry metadata, or `Retry-After` is treated as a request-scoped prompt rejection: healthy accounts are never cooled, third-party agent identity lines are scrubbed from system instructions, and the request is retried once with the system instruction moved into the first user turn before the HTTP 400 is surfaced.
- Repeated 429 responses without retry metadata escalate progressively (`60s → 5m → 30m → 2h`) and zero the affected model's quota fraction so hybrid scoring stops preferring the throttled account.
- **Per-model cooldown isolation**: an account rate-limited on one model stays available for other, non-cooling models (for example Claude or GPT-OSS).
- The account is cooled down for that model and the request immediately fails over to the next available account.
- When every account is cooling and the wait is ≤ `max_wait_seconds`, the gateway absorbs the wait transparently; beyond that it returns HTTP 429 with the exact seconds in `Retry-After`.
- Secondary endpoints and healthy-account fallbacks are exercised on upstream 5xx errors.
- Empty upstream streams (including frames carrying only a `thoughtSignature` or `finishReason: STOP` without visible text, reasoning text, or tool calls) are retried up to `empty_stream_retries` times independently of prompt/schema fallbacks, with linear backoff (`250ms × attempt`) and soft rotation onto another ready account before returning HTTP `502`. Explicit safety/policy stops (`SAFETY`, `RECITATION`, etc.) bypass empty-stream retries and surface `content_filter` / `refusal`.
- HTTP 403 bodies that indicate a permanent suspension (`has been disabled`, `violation of terms of service`, `VALIDATION_REQUIRED`) disable the account persistently with the upstream reason instead of looping it through 30-minute cooldowns. Transient 403s still cool down and fail over.

### Endpoint ordering by subscription tier

Personal (free/Plus/Pro) accounts try `daily-cloudcode-pa.googleapis.com` first, while business and enterprise accounts try `cloudcode-pa.googleapis.com` first. A custom `endpoints` array keeps its configured order; only the standard default pair is tier-optimized.

### Long-decode continuation

Long Gemini 4 outputs are chained transparently: when upstream stops a candidate with `finishReason: CONTINUATION` and a `continuationToken`, the identical payload is re-sent with only the token added, for a bounded number of hops, and every segment is merged into one client response. Continuation hops are dispatched through the account rotator, so `sticky` keeps them on the original account while `round-robin`/`hybrid` may switch; a switch is logged as a warning because an upstream token can be bound to the account that produced it. Per-model output ceilings are respected, so `max_tokens`/`max_completion_tokens` requests above the model limit are clamped instead of failing upstream validation.

An explicit `max_tokens` cap disables continuation entirely; see [HTTP API Reference](api.md#3-request--response-translation).
