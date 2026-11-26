# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.3.0] - 2026-11-26

### Added

- `agw quota` command for live rolling five-hour, weekly, and per-model quota inspection (`--models`, `--watch`, `--interval`, `--json`, `--re-enable`).
- `agw status` command probing `/health` and authenticated `/v1/accounts` for daemon state, port-matched PID, active rotation strategy, cached catalog age, and per-account runtime metrics (`--port`, `--json`).
- `agw config` command (`show`, `path`, `strategy`) to inspect redacted configuration values and update the persisted rotation strategy (`--json`).
- Parallel bounded `agw accounts --check` inspection reporting rolling five-hour and weekly windows, `TIER` and `SOURCE` columns, relative reset countdowns, and `--json` output.
- `agw models list --quota` and `--json` flags displaying cached quota remaining, cache age hints, and configured `model_mapping` aliases.
- `agw stop --port <n>` for port-scoped daemon shutdown, `--experimental-mcp` opt-out flag on `agw setup claude`, and Cobra `Examples:` with next-step hints across commands.

### Changed

- Removed the embedded WebUI dashboard (`internal/server/webui`), `agw ui` command, `/ui*` routes, HTML root redirect, and `docs/webui.md` to focus strictly on the CLI and authenticated `/v1/*` APIs.
- Included the daemon's active `strategy` in `GET /v1/accounts` responses.
- Dynamic Sonnet, Opus, and Haiku model selection in `agw setup claude` using numeric version and effort ranking instead of a hardcoded Opus identifier.
- Restructured `README.md` into focused guides under `docs/` (`cli.md`, `api.md`, `configuration.md`, `architecture.md`) and added unofficial-tool and public OAuth client disclosures.

### Fixed

- Applied the 1024-token numeric reasoning floor to `gpt-oss` models under tight explicit `max_tokens` caps.
- Resolved the OAuth callback port from `redirect_uri` in `agw doctor` and surfaced warnings for malformed URIs instead of hardcoding port `51121`.
- Reused pooled `sync.OnceValue` HTTP transports in `config.DefaultHTTPClient` across standard and `AGW_PREFER_IPV4=1` modes to avoid per-call connection pool churn.
- Preserved an unrelated primary daemon's `agw.pid` when stopping a custom port via `agw stop --port` and when `agw status --port` probes a separate instance.

### Security

- Restricted unauthenticated loopback endpoints strictly to `/` and `/health` by removing all `/ui` and `/ui/api/*` routes and same-origin mutation surfaces.
- Enforced secret redaction for gateway API keys and OAuth client secrets across `agw config show` and `agw config show --json`.

## [0.2.1] - 2026-11-25

### Fixed

- Explicit `max_tokens` / `max_completion_tokens` and Anthropic `max_tokens` now bound total generation instead of being expanded to leave reasoning headroom.
- Reasoning budgets shrink to fit an explicit output cap and thinking is disabled when the cap falls below the model minimum (1024 tokens for Claude), avoiding upstream HTTP 400 budget validation errors.
- `tool_choice: "none"` omits tool declarations, because upstream models could still invoke a declared tool even with `functionCallingConfig` mode `NONE`.
- Long-decode continuation is skipped for explicitly capped requests, which previously failed the follow-up hop with HTTP 400 because `maxOutputTokens` accumulates across continuation requests.

### Changed

- Capped OpenAI requests finish with `finish_reason: "length"`, and capped Anthropic requests finish with `stop_reason: "max_tokens"`. Long-decode chaining therefore requires omitting the cap on `/v1/chat/completions` and does not apply to `/v1/messages`.
- Documented hard output caps, tool choice enforcement, and the migration note in the README.

## [0.2.0] - 2026-11-25

### Added

- OpenAI-style single-model retrieval at `GET /v1/models/{id}` with alias resolution and an OpenAI-compatible 404 for unknown identifiers.
- Same-origin `POST /ui/api/refresh` for an explicit live catalog and quota refresh without leaving the dashboard.
- Dashboard account metrics (request count, 429 count, sanitized last error), rolling five-hour versus weekly quota windows, and a manual quota refresh control.
- Deterministic session affinity for clients that send no affinity header, plus a bounded two-hour thought-signature cache that only replays a recipe when its tool-call IDs match the turn.
- Antigravity CLI (`agy`) session discovery (`acp_token.json`, `antigravity-oauth-token`), with `AGW_AGY_TOKEN_PATH` override and opt-in read-only keyring lookup via `AGW_AGY_KEYRING`.
- Immutable Google user identity (`user_id`) for account keying, duplicate detection, and legacy email-keyed migration.
- `AGW_PREFER_IPV4` opt-in IPv4-first dialing for networks whose IPv6 route blackholes traffic.
- `agw doctor` now probes the daily Cloud Code host, reports IPv4 versus IPv6 reachability per Cloud Code host, and surfaces `HTTPS_PROXY` with credentials masked.
- Native macOS and Windows test jobs in CI, plus pre-publish artifact and installer verification gates in the release workflow.

### Changed

- Upstream identity now advertises the modern Antigravity Hub User-Agent (`antigravity/hub/<version>` with changelist) instead of the legacy IDE fingerprint.
- Reasoning translation supports Anthropic `adaptive` thinking, Gemini 4 `xhigh`, and Claude 5.x `thinkingLevel`, while legacy Claude 4.x keeps `thinking_budget`.
- Model catalog defaults migrate to the Gemini 4 and Claude 5.5 lineup, filter `*-disabled` placeholders, clamp Gemini 4 Neon to 512K context, and ship an embedded offline fallback manifest.
- Upstream endpoint order follows account tier (personal prefers the daily host, business and enterprise prefer production), and rotator absorption limits are configurable through `max_wait_seconds`, `default_cooldown_seconds`, and `empty_stream_retries`.
- Schema normalization now traverses every JSON Schema composition keyword: `additionalProperties`, `prefixItems`, `dependentSchemas`, `not`, and `if`/`then`/`else`.
- Tool-enabled requests without an affinity header derive a stable session and are marked as agent traffic upstream.
- `/health` answers only loopback authorities, dashboard mutations require a same-origin signal (and JSON for strategy changes), `agw accounts --check` fails non-zero when configuration cannot be loaded, and `agw setup --api-key` validates the generated key format. See the README "Behavior Changes & Migration Notes" section.

### Fixed

- `quotaResetDelay` millisecond values such as `"500ms"` no longer parse as minutes, preventing multi-hour cooldowns from sub-second upstream delays.
- Cooldown pruning follows the injected clock, and accounts holding only a static desktop bearer token cool down immediately instead of retrying the same 401 token.
- Account hot reloads serialize under a reload mutex, and backward wall-clock steps trigger a clean resync instead of freezing reloads.
- Rotator dispatch budget no longer consumes cooldown absorption, backoff sleeps honor context cancellation, and token bucket accounting stays balanced across retries.
- Anthropic streaming usage no longer double-counts cumulative thoughts tokens, and `tool_result` images, empty-text signatures, safety stop reasons, and tool identifiers are preserved.
- Output ceilings are enforced per model, and Gemini 4 long decodes chain `CONTINUATION` hops with a bounded guard while resending the original payload.
- IPv6 loopback (`::1`) addresses format correctly across the daemon, `doctor`, `start`, `setup`, `update`, and `ui`, and `agw start --detach` plus `agw update` preserve `--config`.
- Self-updater rejects non-regular archive entries, caps decompression, requires HTTPS release metadata, and refuses HTTPS-to-HTTP redirect downgrades.
- Clients that strip `reasoning_details` between turns still replay signed thinking parts, and a text signature is no longer dropped when a recipe lists a tool call before the text step.
- Windows bind failures report the owning PID and the `agw stop` hint by recognizing the `WSAEADDRINUSE` errno.

### Security

- Bare `RESOURCE_EXHAUSTED` prompt rejections no longer cool healthy accounts, and third-party agent identity markers are scrubbed from system instructions.
- Permanent terms-of-service 403 responses disable the account persistently instead of cycling thirty-minute cooldowns.
- Daemon shutdown verifies port ownership before signalling a PID, request paths reject parent-directory traversal, and the dashboard sends framing-denial headers.
- Setup and completion probes no longer forward bearer or `x-api-key` credentials across HTTP redirects.
- Installers validate strict semantic versions, reject symlinked archive entries, stage installs through `mktemp`, and require HTTPS for remote downloads.

## [0.1.1] - 2026-11-25

### Changed

- Implemented stale-while-revalidate for `ModelCatalog.Ensure` with non-blocking background refreshes and lowercase $O(1)$ model indexing.
- Configured dedicated HTTP transports across upstream API and auth clients (`MaxIdleConns: 100`, `MaxIdleConnsPerHost: 32`, `ResponseHeaderTimeout: 60s`, HTTP/2 multiplexing) to eliminate connection churn.
- Cached static upstream headers and eliminated redundant format allocations on the request hot path.
- Replaced pool filesystem stat locks with lockless atomic nanosecond throttling and eliminated mutex overhead in token bucket retrieval.
- Snapshotted candidate account metrics once per hybrid selection cycle to remove nested lock acquisitions.
- Hoisted request translation outside rotator account failover retry loops and added a zero-allocation fast-path to cache control cleaning.
- Added buffer pooling (`sync.Pool`) and single-line payload fast-paths for SSE stream parsing.
- Upgraded JSON struct tags to Go 1.24+ `omitzero` on boolean, numeric, and pointer struct fields across translation and server packages.
- Modernized map and slice operations with standard library `maps.Clone`, `maps.Copy`, and `slices.Sorted(maps.Keys(...))`.
- Completed 100% English doc comment coverage across exported constants, test helpers, and diagnostic status types.

### Fixed

- Connected upstream quota fractions to active account states during catalog refresh and persisted quota snapshots to `models-cache.json` for live `hybrid` rotation.
- Honored `thinking: {"type": "disabled"}` in Anthropic message translation, suppressed thinking when tools are forced or unverified in Claude tool loops, and translated `tool_choice` into upstream tool configurations.
- Guaranteed terminal `message_start` precedes `message_delta` and `message_stop` on empty Anthropic streams.
- Deep-cloned message content turns in interrupted tool recovery to eliminate caller slice mutation.
- Protected token refresh persistence with value-copied account states and context timeout isolation.
- Disambiguated global `--config` flag across subcommands and renamed local OpenCode setup flag to `--config-file`.
- Corrected OpenCode API key inspection in `agw doctor` to check provider settings rather than options.
- Isolated unit tests from host browser launches and live internet TCP dialing.
- Added ephemeral `/health` probe mock server to foreground collision tests, eliminating runner timeout deadlocks.

### Security

- Enforced loopback host validation and cross-site request checks on WebUI dashboard endpoints (`/ui`, `/ui/api/status`, `/ui/api/strategy`) to defend against DNS rebinding attacks.
- Validated OAuth state parameter in manual redirect URL pastes and prioritized state checks before error callbacks to prevent OAuth CSRF and unauthorized authentication aborts.
- Redacted API keys and bearer tokens in `agw setup --dry-run` output, and added masked `String()` representations for `Config` and `LocalSession`.
- Added verified health checking before signalling background daemon processes in `StopDetached`, preventing termination of recycled OS PIDs.
- Enforced bounded memory reads via `io.LimitReader` on upstream catalog fetching, health probes, test completions, database fallback scans, and SSE stream buffers.

## [0.1.0] - 2026-11-16

### Added

- Embedded zero-dependency WebUI dashboard (`internal/server/webui`) served at `http://127.0.0.1:51400/ui` featuring real-time account health bars, token bucket status, quota monitors, and dynamic strategy switching.
- Dedicated CLI command `agw ui` to inspect daemon status and open the web management dashboard in the default system browser.
- Local desktop session auto-import via `agw login --local`, extracting active tokens from Antigravity IDE SQLite state databases (`state.vscdb`) or local listener port 9092.
- Environment diagnostics auto-detection in `agw doctor` suggesting `agw login --local` when unimported desktop credentials exist.
- Multi-strategy account load balancing introducing `hybrid` selection alongside `sticky` and `round-robin`, combining operational health scoring, token bucket burst rate limiting, quota threshold awareness, and LRU freshness.
- Quota-aware protection with proactive account parking when remaining fractional quota drops below threshold (`quota_threshold`, default 5%).
- Dynamic model aliasing and custom mapping (`model_mapping` in `config.json`) resolving friendly or codename IDs to upstream model identifiers.
- Multi-phase tool schema sanitization pipeline converting `$ref` paths to description hints, merging `allOf`, flattening `anyOf`/`oneOf` with heuristic scoring, preserving constraints as description text, and ensuring valid `items` on array nodes.
- Centralized thinking budget clamping per model family (Gemini 2.5 Flash/Pro 24,576 token cap) with automatic `maxOutputTokens` headroom expansion.
- Interrupted tool-loop synthetic recovery resolving pending tool invocations when turns are aborted mid-call without tool results.
- Native Anthropic Messages API endpoint (`POST /v1/messages`) supporting both real-time SSE streaming and non-streaming modes for Claude Code CLI and OpenClaw.
- Transactional Claude Code CLI configuration injector (`agw setup claude`) updating `~/.claude/settings.json` with timestamped atomic backup and live completion probe.
- Integration diagnostics for Claude Code CLI in `agw doctor`.
- Outbound corporate proxy support honoring standard `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` environment variables across all HTTP clients.
- Proactive `cache_control` attribute stripping across all message blocks to eliminate Google Cloud Code HTTP 400 "Extra inputs are not permitted" rejections.
- Claude Code `x-anthropic-billing-header` line removal preventing false HTTP 429 quota exhaustion errors.
- Local reverse proxy on `http://127.0.0.1:51400/v1` exposing OpenAI-compatible `/v1/chat/completions` and `/v1/models` endpoints.
- Multi-account management with sticky prompt cache affinity and round-robin balancing across stored Google Antigravity subscriptions.
- Transparent failover upon encountering HTTP 429 rate limits, parsing upstream `resetTime` / `RetryInfo` and absorbing short waits ($\le 20$s).
- Live model catalog discovery (`v1internal:fetchAvailableModels`) and remaining quota inspection via `agw models`.
- Bidirectional translation between OpenAI request/response schemas and Cloud Code Assist SSE streaming protocols.
- Gemini 3 and Claude thought-signature preservation and replay across multi-turn tool invocations (`pi-gemini-auth.v1` format).
- Tool JSON schema sanitization defending against upstream `$ref` collisions.
- Transactional client configuration injectors for Pi Coding Agent (`agw setup pi`) and OpenCode (`agw setup opencode`) with timestamped atomic backups.
- Cross-platform background daemon lifecycle management (`agw start --detach`, `agw stop`, and system diagnostics via `agw doctor`).
- Checksum-verifying user-local installers for POSIX systems (`install.sh`) and Windows PowerShell (`install.ps1`).
- Automated CI verification workflows, Git pre-commit hooks, and cross-platform release packaging matrix (`make release`).
- Open-source governance including MIT license (`LICENSE`), security policy (`SECURITY.md`), contributing guide (`CONTRIBUTING.md`), code of conduct (`CODE_OF_CONDUCT.md`), and GitHub issue/PR templates.

### Fixed

- WebUI strategy switching now re-reads `config.json` and rewrites only the `strategy` field, so a stale in-memory configuration can no longer overwrite the stored API key, port, endpoints, or model mapping.
- Gateway API key format validated at daemon startup and by `agw doctor`, warning when the stored key no longer matches the `agw_` prefix plus 64 hexadecimal characters that clients were configured with.
- `agw doctor` flags configured clients (Pi, OpenCode, Claude Code) whose stored API key differs from `config.json`, surfacing the root cause of unexpected HTTP 401 responses.
- Daemon startup logs the active configuration file path so operators can confirm which `config.json` the reverse proxy actually loaded.
- `agw stop` rediscovers the daemon through its `/health` identity probe and the operating system socket table when `agw.pid` is missing or stale, so an orphaned daemon can still be terminated.
- The PID file is written by the daemon itself once the listener is bound (foreground and detached) and removed on graceful shutdown.
- Bind failures now name the process holding the port and point at `agw stop`, and starting a second daemon reports the running instance and its version.
- `agw start` probes the configured port before spawning, so an orphaned daemon is reported and its `agw.pid` restored instead of launching a process that cannot bind.
- Socket table lookups used for daemon discovery are bounded by a timeout and their `lsof`/`netstat` output parsing is unit tested on every platform.
- Server configuration is published as an immutable copy-on-write snapshot, so handlers never observe a partially mutated struct and the caller's configuration is left untouched.
- Package-level test isolation pins `AGW_HOME` to a temporary directory for the whole test binary, so no test can read or write the operator's real `~/.agw` directory.
- Dashboard feedback uses a non-blocking inline toast with a connection state badge, and polling pauses while the tab is hidden.
- Added the authoritative [Web Dashboard Contract](docs/webui.md) covering endpoints, loopback trust model, and the same-origin mutation rule.
- Imported desktop sessions store their bearer token in a dedicated `access_token` field with `credential_source: local_desktop`, so it is never sent to the OAuth token endpoint as a refresh token.
- Accounts without a refresh token resolve their token through `auth.AccessTokenFor`, and an expired desktop session reports an actionable re-import message instead of a misleading OAuth failure.
- `agw accounts` and `agw doctor` surface imported desktop sessions and remind operators to re-run `agw login --local` once the token expires.
- Hot reload compares both credential kinds, so re-importing a desktop session replaces the stale bearer token in a running daemon.
- Credential database discovery only executes `sqlite3` from absolute trusted paths, never from `PATH`.
- Imported desktop payloads are validated for token length and control characters, and local listener reads are bounded.
- Re-importing a desktop session never downgrades an existing OAuth account: accounts that already hold a refresh token keep their refreshable credentials.

[Unreleased]: https://github.com/salmanabdurrahman/antigravity-gateway/compare/v0.3.0...HEAD
[0.3.0]: https://github.com/salmanabdurrahman/antigravity-gateway/compare/v0.2.1...v0.3.0
[0.2.1]: https://github.com/salmanabdurrahman/antigravity-gateway/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/salmanabdurrahman/antigravity-gateway/compare/v0.1.1...v0.2.0
[0.1.1]: https://github.com/salmanabdurrahman/antigravity-gateway/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/salmanabdurrahman/antigravity-gateway/releases/tag/v0.1.0
