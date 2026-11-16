# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/salmanabdurrahman/antigravity-gateway/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/salmanabdurrahman/antigravity-gateway/releases/tag/v0.1.0
