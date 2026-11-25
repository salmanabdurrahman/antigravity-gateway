# Antigravity Gateway (`agw`)

[![CI](https://github.com/salmanabdurrahman/antigravity-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/salmanabdurrahman/antigravity-gateway/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/go-1.24%2B-00ADD8?logo=go&logoColor=white)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/salmanabdurrahman/antigravity-gateway?color=brightgreen)](https://github.com/salmanabdurrahman/antigravity-gateway/releases)

Antigravity Gateway (`agw`) is a zero-dependency, single-binary Go reverse proxy and CLI daemon. It connects modern AI coding agents (such as Pi Coding Agent, OpenCode, and Cursor) to personal Google Antigravity / Google AI subscriptions over a local OpenAI-compatible HTTP interface (`http://127.0.0.1:51400/v1`).

## ⚠️ Unofficial Tool & Account Risk

> **`agw` is an independent community project. It is not affiliated with, endorsed by, sponsored by, or supported by Google LLC.**
> It reaches Cloud Code Assist through the same undocumented internal endpoints the Antigravity IDE uses, on the strength of your own consumer subscription. That is a reverse-engineered integration, not a public Google product surface.

**By using this software you acknowledge:**

- **It may violate the provider's terms.** Review the [Google Terms of Service](https://policies.google.com/terms) and the [Generative AI Additional Terms of Service](https://policies.google.com/terms/generative-ai) yourself; judging whether proxying a subscription counts as permitted use is your responsibility.
- **Your Google account can be restricted or disabled.** Community proxies built on the same endpoints report accounts being suspended, shadow-restricted, or permanently disabled for terms-of-service violations. Prefer a secondary account you can afford to lose over the account you depend on.
- **Nothing here is guaranteed.** Upstream endpoints, quota rules, model availability, and authentication can change without notice and break the gateway.
- **No warranty.** The software is provided "as is" under the [MIT license](LICENSE); the authors accept no liability for lost access, lost data, or quota spend.
- **Credentials stay local.** Tokens are stored only in `~/.agw/` with `0600` permissions behind a loopback-only daemon, and nothing is reported back to the project. See [SECURITY.md](SECURITY.md) for the security invariants and the public-client OAuth disclosure.

If those terms are unacceptable to you, do not use this project.

## Key Features

- **Dual OpenAI & Anthropic Compatible Local Endpoints**: Exposes `/v1/chat/completions` (OpenAI format), `/v1/messages` (Anthropic format), and OpenAI-style model discovery (`/v1/models`, `/v1/models/{id}`) bounded strictly to local loopback (`127.0.0.1:51400`).
- **Multi-Account Failover & Rotation**: Manages multiple Google Antigravity accounts locally with configurable strategies (`sticky`, `round-robin`, or `hybrid` with health scores, token bucket burst throttling, and proactive quota protection).
- **Protocol Fidelity**: Handles thought-signature preservation and replay for Gemini 2.x and newer plus Claude models, including a bounded per-session signature cache with tool-call-ID matching, deterministic session affinity for clients that send no header, required interleaved thinking headers, multi-phase tool schema sanitization ($ref hints, union flattening, array repair across every JSON Schema composition keyword), categorical thinking levels, thinking budget clamping, interrupted tool-loop recovery, proactive `cache_control` stripping, and billing header plus third-party agent identity sanitization.
- **Corporate Proxy & VPN Support**: Automatically honors standard `HTTP_PROXY`, `HTTPS_PROXY`, and `NO_PROXY` environment variables for outbound API communication.
- **Secure Local Configuration**: Stores sensitive credentials in `~/.agw/` with strict POSIX permissions (`0700` directories, `0600` files) and atomic write guarantees.
- **Zero Runtime Dependencies**: Compiled into a single self-contained binary built on Go 1.24+ standard library and Cobra CLI.

## Performance & Architecture Comparison (vs 9router)

A benchmark using `gemini-3.8-flash` on the same machine highlights key architectural and resource trade-offs between Antigravity Gateway (`agw`) and multi-provider Node.js proxies like `9router`:

| Metric / Capability             | Antigravity Gateway (`agw`)                                      | 9router                                     |
| :------------------------------ | :--------------------------------------------------------------- | :------------------------------------------ |
| **Runtime Architecture**        | Single compiled Go binary (zero dependencies)                    | Node.js + Next.js + SQLite                  |
| **Memory Footprint (RSS)**      | **~19 MB**                                                       | **~135 MB** (7x higher RAM footprint)       |
| **Startup Latency**             | Instant (< 5 ms)                                                 | ~2 – 4 seconds                              |
| **Network Security**            | **Strict Loopback (`127.0.0.1:51400`)**                          | `0.0.0.0` (Exposed to local LAN by default) |
| **Gemini Thought-Signatures**   | **Lossless Preservation (`pga.v1` format)**                      | Stripped / normalized                       |
| **Multi-Turn Tool Calling**     | **100% Reliable** (`skip_thought_signature_validator`)           | Prone to Google HTTP 400 on turn 2+         |
| **Claude Interleaved Thinking** | Automatic `anthropic-beta` header injection                      | Adapter-dependent                           |
| **Client Auto-Setup**           | Transactional CLI setup (`agw setup pi` / `opencode` / `claude`) | Manual client configuration                 |

## Supported Platforms

Antigravity Gateway is compiled as a static, zero-dependency binary across modern desktop and server platforms:

| Platform    | Architecture                 | Target          | Status     |
| :---------- | :--------------------------- | :-------------- | :--------- |
| **macOS**   | ARM64 (Apple Silicon: M1–M4) | `darwin/arm64`  | **Tier 1** |
| **macOS**   | x86_64 (Intel)               | `darwin/amd64`  | **Tier 1** |
| **Linux**   | x86_64 (AMD64)               | `linux/amd64`   | **Tier 1** |
| **Linux**   | ARM64 (AArch64)              | `linux/arm64`   | **Tier 1** |
| **Windows** | x86_64 (AMD64)               | `windows/amd64` | **Tier 1** |
| **Windows** | ARM64                        | `windows/arm64` | **Tier 2** |

Tier 1 platforms are exercised by the native continuous-integration test matrix: the race-enabled unit suite and a CLI `version` smoke run on Linux, macOS, and Windows runners, alongside the cross-compilation build matrix. Platform-specific daemon, port-owner, and binary-applier paths therefore run natively instead of only compiling.

For details on packaging, filesystem layout, upgrade flow, and uninstallation, see the [Binary Installation Contract](docs/distribution.md).

## Installation & Build

### One-Liner Install (Recommended)

Install the pre-compiled, checksum-verified binary directly to your local user directory without manual cloning:

```bash
# macOS & Linux (curl)
curl -fsSL https://raw.githubusercontent.com/salmanabdurrahman/antigravity-gateway/main/scripts/install.sh | sh

# macOS & Linux (pinned version)
curl -fsSL https://raw.githubusercontent.com/salmanabdurrahman/antigravity-gateway/main/scripts/install.sh | sh -s -- v0.1.0

# Windows (PowerShell)
irm https://raw.githubusercontent.com/salmanabdurrahman/antigravity-gateway/main/scripts/install.ps1 | iex
```

### Manual Download Pre-built Release & Verify Checksums

Pre-compiled standalone binaries for macOS, Linux, and Windows are available for each release. Every release provides a companion `SHA256SUMS` file for cryptographic integrity verification.

1. **Download archive and checksum file**:
   Download the archive matching your platform (e.g. `agw_0.1.0_darwin_arm64.tar.gz`) along with `SHA256SUMS`.

2. **Verify SHA-256 checksum**:

   ```bash
   # On macOS
   shasum -a 256 --check --ignore-missing SHA256SUMS

   # On Linux
   sha256sum --check --ignore-missing SHA256SUMS
   ```

3. **Extract and test binary**:
   ```bash
   tar -xzf agw_0.1.0_darwin_arm64.tar.gz
   ./agw version
   ./agw --help

   # Move binary to user PATH
   mkdir -p ~/.local/bin
   mv agw ~/.local/bin/
   ```

Alternatively, invoke the local automated installer script:

```bash
# Downloads binary and SHA256SUMS, verifies digest, and installs to ~/.local/bin
./scripts/install.sh
./scripts/install.sh 0.1.0
```

For Windows PowerShell verification, unattended installs, and system installation paths, see the [Binary Installation Contract](docs/distribution.md).

### Prerequisites (Source Build)

- Go 1.24 or higher

### Build from Source

```bash
git clone https://github.com/salmanabdurrahman/antigravity-gateway.git
cd antigravity-gateway
go build -o bin/agw ./cmd/agw

# Verify local binary
./bin/agw version

# Optional: move binary to local PATH for global usage
mkdir -p ~/.local/bin
mv bin/agw ~/.local/bin/
```

## CLI Usage

```text
Antigravity Gateway (agw) is a lightweight, zero-dependency local OpenAI-compatible
reverse proxy connecting AI coding agents directly to Google Antigravity subscriptions.

Usage:
  agw [command]

Available Commands:
  accounts    List configured Google Antigravity accounts
  completion  Generate the autocompletion script for the specified shell
  doctor      Run diagnostics on ports, permissions, accounts, and network connectivity
  help        Help about any command
  login       Authenticate a Google Antigravity account via OAuth PKCE
  logout      Remove a Google account from the local account store
  models      Inspect available chat models and token limits
  setup       Configure AI coding agents (Pi Coding Agent or OpenCode)
  start       Start the local OpenAI-compatible gateway reverse proxy
  stop        Stop the running background gateway daemon
  version     Print the semantic version of agw

Flags:
      --config string   config file path (default is ~/.agw/config.json or $AGW_HOME/config.json)
  -h, --help            help for agw
```

### Account Management

For OAuth architecture details, authorized scopes, PKCE compliance, and custom client configuration (BYOC), see the [OAuth 2.0 Client Strategy & Distribution Architecture](docs/oauth-strategy.md).

#### Authenticate an Account

Launch the interactive Google OAuth PKCE login flow. Automatically opens your browser and starts an ephemeral loopback callback listener on `http://127.0.0.1:51121/oauth-callback`:

```bash
agw login
```

For headless environments or remote SSH sessions:

```bash
agw login --no-browser
```

_(If port binding fails or browser redirect does not hit the loopback server, paste the final redirect URL into the terminal prompt to complete authentication)_

#### Import a Local Desktop Session

Reuse the session already signed in to the Antigravity IDE instead of running a browser OAuth flow:

```bash
agw login --local
```

The command reads `antigravityAuthStatus` from the IDE SQLite state database (`state.vscdb`) or the IDE's local listener on port `9092`, whichever is reachable. Desktop sessions expose only a short-lived bearer token, so the import is stored separately from OAuth refresh tokens (`access_token` plus `credential_source: local_desktop`) and is never sent to the OAuth token endpoint. When that token expires, requests start returning `401`; re-run `agw login --local` while the IDE is signed in. `agw doctor` reports imported sessions and reminds you to re-import. If an account with the same email already holds an OAuth refresh token, the import is skipped so refreshable credentials are never downgraded to a short-lived token.

#### List Accounts

Inspect configured accounts and their linked Cloud Code companion projects:

```bash
agw accounts
```

#### Check Account Quotas

Query live upstream quota remaining (Gemini, Claude, and reset time) per account:

```bash
agw accounts --check
```

`--check` never re-enables accounts: a disabled account whose credentials still work is reported as disabled with a hint. Clearing that state requires the explicit opt-in flag, which is only accepted together with `--check`. The only state the inspection still records is a revoked refresh token, which is marked disabled so later requests skip it until you log in again:

```bash
agw accounts --check --re-enable
```

#### Remove an Account

Remove credentials for an account from the local store:

```bash
agw logout user@example.com
```

### Model Catalog & Token Limits

#### List Chat Models

Inspect all available chat completion models along with their context window, maximum output tokens, and reasoning/multimodal capabilities:

```bash
agw models list
```

_(Loads from `~/.agw/models-cache.json` for instant offline responses. Pass `--refresh` to force a live upstream catalog refresh)._

Catalog parsing follows the capability metadata returned by `fetchAvailableModels`: `agentModelSorts` and `tieredModelIds` override name-based heuristics, `apiProvider`/`modelProvider` exclude non-conversational entries, and `<model>-disabled` placeholders are never listed. Gemini 4 Neon reports its documented 512K context window. When setup runs fully offline, `agw` falls back to a bundled manifest covering Gemini 4 (Neon, Carbon, Hydrogen) and Claude 5.5 instead of retired identifiers.

#### Upstream Model Aliases

The gateway maps user-friendly public model aliases to Google's internal Cloud Code Assist identifiers automatically:

| Public Model Name     | Upstream Model Identifier | Family   |
| :-------------------- | :------------------------ | :------- |
| `gemini-3.1-pro-high` | `gemini-pro-agent`        | Gemini 3 |
| `gpt-oss-120b`        | `gpt-oss-120b-medium`     | GPT-OSS  |

### Gateway Daemon & Server Lifecycle

#### Start Gateway in Foreground

Run the gateway reverse proxy in the foreground on `127.0.0.1:51400`:

```bash
agw start
```

With rotation strategy override (`sticky`, `round-robin`, or `hybrid`):

```bash
agw start --strategy hybrid
```

#### Start Gateway in Background (Detached)

Fork and launch the daemon in the background. The process ID is recorded in `~/.agw/agw.pid` and logs are written to `~/.agw/agw.log`. Any launch override supplied on the command line (`--strategy`, `--port`, `--config`) is forwarded to the detached child, so a custom configuration path is honored in the background exactly as it is in the foreground:

```bash
agw start --detach
agw start --detach --config /etc/agw/config.json
```

#### Stop Gateway Daemon

Terminate the running daemon gracefully via SIGTERM:

```bash
agw stop
```

The PID file is written by the daemon itself as soon as it reaches the listening state (foreground and detached alike) and removed on graceful shutdown. If `~/.agw/agw.pid` is missing or stale, `agw stop` rediscovers the daemon through its `/health` identity probe on the port recorded in `config.json` and resolves the owning process from the operating system socket table; a listener that does not identify itself as `antigravity-gateway` is never signalled. `agw start` performs the same probe before spawning a process, so an orphaned daemon is reported (and its PID file restored) instead of launching a second process that cannot bind. Because discovery uses the configured port, keep `port` in `config.json` in sync when running the daemon on a custom `--port`.

#### In-Place Gateway Self-Upgrade (`update` / `upgrade`)

Update the gateway executable to the latest release in-place atomically without manually running install scripts:

```bash
agw update
```

Optional flags:

- `--check`: Query GitHub Releases and display update availability without downloading or installing
- `--version <v>`: Pin or downgrade to a specific version (e.g. `agw update --version 0.1.0`)
- `--force`: Force download and reinstallation even when already on the latest version, or continue replacing the binary when the running daemon reports it could not be stopped
- `--no-restart`: Skip automatically restarting a running background daemon after binary replacement

A running daemon is stopped before the executable is replaced, and the restart preserves the configuration path the daemon was launched with. If the stop request is refused or the daemon keeps answering `/health`, the upgrade aborts instead of swapping the binary underneath a live process; `--force` overrides that guard. Release metadata, archives, and checksum files must be served over HTTPS (plain HTTP is accepted only for loopback), and HTTPS-to-HTTP redirect downgrades are rejected.

Alias `agw upgrade` is also supported.

### Web Management Dashboard

Launch your default browser to open the embedded zero-dependency dashboard hosted by the running gateway daemon:

```bash
agw ui
```

Or navigate directly to `http://127.0.0.1:51400/ui` (automatic redirect from root when visited in web browsers). Features visual account health progress bars, per-account request/429 counters with the last recorded error, rolling five-hour versus weekly quota windows, a manual quota refresh control, live model quota tracking, and dynamic strategy switching. Strategy changes take effect immediately in memory and are persisted by rewriting only the `strategy` field of the configuration file the daemon was started with, leaving every other stored setting untouched. That file is the one supplied through `--config` when present, otherwise `~/.agw/config.json`.

The dashboard and its JSON endpoints are documented in the [Web Dashboard Contract](docs/webui.md): read endpoints stay unauthenticated on loopback, strategy mutation is same-origin restricted, and no token material is ever serialized.

### Client Configuration & Agent Setup

Antigravity Gateway automatically configures AI coding agents to use the local reverse proxy as an OpenAI-compatible provider. Before modifying existing configurations, `agw` creates a timestamped atomic backup (`*.bak-<timestamp>`) and performs a 1-token test chat completion probe to verify end-to-end connectivity.

#### Configure Pi Coding Agent

Registers the `"antigravity"` provider in `~/.pi/agent/models.json`:

```bash
agw setup pi
```

Optional flags:

- `--models`: Filter models by pattern, e.g. `--models "gemini-*,!*-low"`
- `--no-verify`: Skip 1-token connectivity test probe
- `--dry-run`: Preview generated JSON without writing to disk
- `--pi-dir`: Custom Pi agent directory (defaults to `~/.pi/agent` or `$PI_AGENT_DIR`)

#### Configure OpenCode

Registers the `"antigravity"` provider using `@opencode/ai/providers/openai-compatible` in `~/.config/opencode/opencode.json`:

```bash
agw setup opencode
```

Optional flags:

- `--models`: Filter models by pattern
- `--no-verify`: Skip 1-token connectivity test probe
- `--dry-run`: Preview generated JSON without writing to disk
- `--config-file`: Custom OpenCode configuration file path (defaults to `~/.config/opencode/opencode.json` or `$OPENCODE_CONFIG`)

#### Configure Claude Code CLI

Configures Claude Code CLI to communicate with Antigravity Gateway's Anthropic Messages API (`/v1/messages`) by updating `~/.claude/settings.json`:

```bash
agw setup claude
```

Optional flags:

- `--model`: Primary model configured for Claude CLI (default: `claude-sonnet-5-5-high`)
- `--no-verify`: Skip 1-token connectivity test probe
- `--dry-run`: Preview generated JSON without writing to disk
- `--settings`: Custom settings file path (defaults to `~/.claude/settings.json` or `$CLAUDE_CONFIG_DIR`)

Once configured, simply launch `claude` in your terminal to route through Antigravity Gateway.

### Environment Diagnostics (`doctor`)

Run an end-to-end sanity check on your gateway environment:

```bash
agw doctor
```

Audits and reports:

1. **Network Ports**: Verifies gateway port (`51400`) and OAuth callback port (`51121`).
2. **File System Permissions**: Confirms `0700` directory and `0600` credential file security, and validates that the stored gateway API key still matches the generated `agw_` format.
3. **Account Pool**: Validates configured Google accounts, refresh tokens, and companion project IDs.
4. **Upstream Connectivity**: Tests TLS reachability and latency to Google OAuth (`oauth2.googleapis.com`), the daily Cloud Code host (`daily-cloudcode-pa.googleapis.com`), and Cloud Code PA (`cloudcode-pa.googleapis.com`); reports IPv4 versus IPv6 reachability per Cloud Code host and surfaces the configured `HTTPS_PROXY` with credentials masked.
5. **Client Integrations**: Checks whether Pi Coding Agent, OpenCode, and Claude Code configurations are installed, configured, and carrying the same gateway API key as `config.json`.

### HTTP API Endpoints

The gateway listens exclusively on local loopback (`127.0.0.1:51400`):

| Method & Route              | Purpose                                                               | Authentication               |
| :-------------------------- | :-------------------------------------------------------------------- | :--------------------------- |
| `GET /health`               | Liveness probe, gateway name, version, and active account count       | None (loopback only)         |
| `GET /v1/models`            | OpenAI-compatible model listing with context windows and capabilities | Bearer token (`config.json`) |
| `GET /v1/models/{id}`       | Single model descriptor by identifier or alias, OpenAI 404 otherwise  | Bearer token (`config.json`) |
| `POST /v1/chat/completions` | OpenAI-compatible streaming SSE or non-streaming chat completions     | Bearer token (`config.json`) |
| `POST /v1/messages`         | Anthropic Messages API for Claude Code CLI and OpenClaw               | `x-api-key` or Bearer        |
| `GET /v1/accounts`          | Redacted diagnostic snapshots, cooldown statuses, and request metrics | Bearer token (`config.json`) |
| `GET /ui`                   | Embedded WebUI dashboard (loopback only, framing denied)              | None (loopback only)         |
| `GET /ui/api/status`        | Dashboard status, model catalog, quotas, and 5h/weekly windows        | None (loopback only)         |
| `POST /ui/api/strategy`     | Same-origin rotation strategy change, persisted to `config.json`      | Same-origin request signal   |
| `POST /ui/api/refresh`      | Same-origin forced live quota and catalog refresh                     | Same-origin request signal   |

#### Quick API Test with `curl`

Send a chat completion request to the running gateway:

```bash
API_KEY=$(grep -o '"api_key": "[^"]*' ~/.agw/config.json | cut -d'"' -f4)

curl -s http://127.0.0.1:51400/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d '{
    "model": "gemini-4-hydrogen-high",
    "messages": [{"role": "user", "content": "Hello!"}],
    "max_tokens": 50
  }' | jq .
```

### Request & Response Translation

The gateway transparently translates between OpenAI Chat Completions requests and Google Cloud Code Assist protocol envelopes:

- **Multimodal Support**: Maps OpenAI base64 `data:` URLs directly into Cloud Code `inlineData`.
- **Tool Schema Defenses**: Converts OpenAI function parameters for Gemini (`parametersJsonSchema`) and Claude (`parameters`), while stripping disallowed schema attributes (`$ref`, `$defs`, `$schema`, unsupported unions). Dynamically detects HTTP 400 schema rejections to fall back to legacy `parameters` mode.
- **Tool Choice Enforcement**: `tool_choice: "none"` (and the Anthropic `{"type": "none"}` form) omits tool declarations entirely, because upstream Gemini can still invoke a declared tool even when `functionCallingConfig` mode is `NONE`.
- **Deterministic Output Caps**: An explicit `max_tokens` / `max_completion_tokens` (and Anthropic `max_tokens`) is a hard ceiling. The reasoning budget shrinks to fit under it and is disabled when the cap falls below the model's minimum budget (1024 tokens for Claude and for `gpt-oss`), so clients get predictable cost and latency. Cloud Code accumulates the ceiling across long-decode hops, so continuation is skipped for capped requests: they end with OpenAI `finish_reason: "length"` or Anthropic `stop_reason: "max_tokens"`. Long-decode chaining therefore requires omitting the cap on the OpenAI endpoint; the Anthropic schema always carries `max_tokens`, so it does not chain.
- **Thought Signature & Replay**: Encodes upstream thinking tokens, steps, and Gemini/Claude cryptographic `thoughtSignature` data into OpenAI `reasoning_details`. Replaces missing signatures on tool re-execution with `skip_thought_signature_validator` for every Gemini generation that validates them (2.x and newer) to avoid upstream tool validation failures, while authentic client signatures are always preserved.
- **Streaming SSE Engine**: Streams `v1internal:streamGenerateContent?alt=sse` chunks to OpenAI SSE chunks (`choices[0].delta.content`, `choices[0].delta.reasoning_content`, and `choices[0].delta.tool_calls`), with buffered scanner capacity up to 10 MB to safely handle massive code diffs and thinking blocks.

### Behavior Changes & Migration Notes

These behaviors are intentionally stricter than earlier releases. Review them before upgrading scripts or automation that talks to the daemon directly.

- **`/health` requires a loopback authority.** The liveness probe is unauthenticated, so it only answers requests whose `Host` header (or bracketed `Host` for IPv6) resolves to `127.0.0.1`, `localhost`, or `::1`; anything else receives `403`. SSH local forwards and `agw`'s own probes keep working because they preserve the loopback authority, while a reverse proxy that rewrites `Host` to a public name no longer passes the probe.
- **Dashboard mutations require a same-origin signal and JSON.** `POST /ui/api/strategy` and `POST /ui/api/refresh` reject requests that carry neither a matching `Origin` nor `Sec-Fetch-Site: same-origin`, and cross-port loopback origins are treated as cross-origin. `POST /ui/api/strategy` additionally requires `Content-Type: application/json`; form-encoded bodies receive `415`. Scripts must send the header explicitly:

  ```bash
  curl -s -X POST http://127.0.0.1:51400/ui/api/strategy \
    -H 'Content-Type: application/json' \
    -H 'Sec-Fetch-Site: same-origin' \
    -d '{"strategy":"sticky"}'
  ```

  Details: [Web Dashboard Contract](docs/webui.md).

- **`agw accounts --check` exits non-zero when the configuration cannot be loaded.** It previously printed an `Error (config)` status column and exited `0`, which let broken configuration pass unnoticed in scripts. Live quota inspection now aborts with the configuration error, and `--re-enable` still requires `--check`.
- **`agw setup --api-key` validates the generated key format.** A value that does not match `agw_` plus 64 hexadecimal characters now fails before any client file is written, instead of being injected into every agent configuration. To keep an intentionally custom key, edit `api_key` in `config.json` and the client files directly; `agw doctor` reports when a stored key no longer matches the generated format.
- **Explicit output caps are enforced.** `max_tokens` / `max_completion_tokens` / Anthropic `max_tokens` now bound total generation instead of being expanded to leave reasoning headroom. Reasoning adapts to the cap and is disabled when the cap is below the model minimum, and `tool_choice: "none"` omits tool declarations so upstream cannot invoke a disabled tool.
- **Tool-enabled requests derive a session identifier.** When a tool-enabled request carries no `X-Session-Affinity`/`session_id` header, the gateway derives a stable session from the model, system instructions, and opening user turn. That session marks the upstream envelope as agent traffic (`requestType: "agent"`) and lets the bounded signature cache replay `reasoning_details` after a client strips them. Plain chat requests without tools keep an empty session and a non-agent envelope.

## Configuration & Environment

The gateway configuration directory defaults to `~/.agw` and can be overridden via the `AGW_HOME` environment variable.

| File                       | Purpose                                                                                      | File Permissions |
| :------------------------- | :------------------------------------------------------------------------------------------- | :--------------- |
| `~/.agw/config.json`       | Daemon settings, host/port, local API key, rotation strategy, quota threshold, model mapping | `0600`           |
| `~/.agw/accounts.json`     | Stored Google account tokens and companion project mappings                                  | `0600`           |
| `~/.agw/models-cache.json` | Cached model catalog, token limits, and rolling/weekly quota windows                         | `0600`           |
| `~/.agw/agw.pid`           | Running daemon process ID (written once listening)                                           | `0600`           |

`config.json` is the single source of truth for gateway settings. Runtime mutations rewrite only the field being changed after re-reading the file first, so a running daemon never persists stale or partial state over your API key, endpoints, or model mapping. The daemon logs its resolved configuration path at startup, and `agw doctor` verifies that the stored key still matches the generated format and the key embedded in each configured client.

Operational thresholds are tunable without code changes:

| Key                        | Default | Range    | Purpose                                                                                         |
| :------------------------- | :------ | :------- | :---------------------------------------------------------------------------------------------- |
| `max_wait_seconds`         | `20`    | `1-120`  | Longest cooldown window the rotator absorbs before returning HTTP 429 with `Retry-After`.       |
| `default_cooldown_seconds` | `60`    | `1-3600` | Base 429 cooldown when upstream omits retry metadata; repeated 429s escalate 1x, 5x, 30x, 120x. |
| `empty_stream_retries`     | `2`     | `1-5`    | Extra attempts when Google closes a stream before emitting any content.                         |

### Environment Overrides

- `AGW_HOME`: Root path for local configuration and credential storage (default: `~/.agw`).
- `AGW_CLIENT_ID`: Custom Google OAuth 2.0 Client ID for Bring-Your-Own-Client (BYOC) setups.
- `AGW_CLIENT_SECRET`: Custom Google OAuth 2.0 Client Secret for Bring-Your-Own-Client (BYOC) setups. The bundled default client is a desktop **public client**, so its embedded credential is public by design (RFC 8252 §8.5); use these overrides when you need a private client identity.
- `AGW_USER_AGENT` (legacy fallback: `PGA_USER_AGENT`): Custom User-Agent header override.
- `AGW_API_CLIENT` (legacy fallback: `PGA_API_CLIENT`): Custom `X-Goog-Api-Client` header override.
- `AGW_CLIENT_METADATA` (legacy fallback: `PGA_CLIENT_METADATA`): Custom `Client-Metadata` JSON header override.
- `AGW_ANTIGRAVITY_VERSION` (legacy fallback: `PGA_ANTIGRAVITY_VERSION`): Antigravity Hub version advertised in the default User-Agent (default: `2.33.0`).
- `AGW_ANTIGRAVITY_CHANGELIST`: Piper changelist (`cl`) token paired with the Hub version in the default User-Agent. Known Hub releases resolve their own changelist automatically; set this to pin an unmapped build (default: `1023160026`).

Default upstream requests identify as `antigravity/hub/<version> (aidev_client; os_type=<os>; arch=<arch>; cl=<changelist>)`. Override the whole header with `AGW_USER_AGENT` when proxying a different client identity.

- `AGW_PREFER_IPV4`: Set to `1` to dial IPv4 addresses before IPv6 addresses. Use it when the local network publishes an IPv6 route that blackholes traffic and every upstream call stalls before falling back.
- `AGW_AGY_TOKEN_PATH`: Explicit Antigravity CLI (`agy`) token file to import through `agw login --local` (for example `~/.config/antigravity/acp_token.json`). Auto-discovery probes the common CLI locations when unset.
- `AGW_AGY_KEYRING`: Set to `1` to opt in to a read-only OS keyring lookup for the `agy` session token. Disabled by default.
- `PI_AGENT_DIR`: Custom Pi Coding Agent configuration directory (default: `~/.pi/agent`).
- `OPENCODE_CONFIG`: Custom OpenCode configuration file path (default: `~/.config/opencode/opencode.json`).

### Account Rotation & Failover Strategy

The rotator engine supports three selection strategies configured via `config.json` (`strategy` field) or `--strategy`:

- `sticky` (default): Uses the current active account until rate limits (HTTP 429) occur, preserving upstream prompt caching.
- `round-robin`: Cycles sequentially through all non-cooling accounts on every request.
- `hybrid`: Smart multi-signal load balancing combining rolling operational health scores, token bucket burst rate limiting, proactive quota protection (parking accounts when remaining quota drops below `quota_threshold`, default 5%), and LRU freshness.

### Model Aliasing & Mapping

You can define custom model aliases in `~/.agw/config.json` via the `model_mapping` dictionary. This enables transparent translation of friendly names or preview codenames into full backend model IDs:

```json
{
  "model_mapping": {
    "gemini-4-argon": "gemini-4-argon-high",
    "claude-custom": "claude-sonnet-5-5-high"
  }
}
```

When rate limits occur:

- Upstream `resetTime` (RFC3339) or `retryDelay` is parsed to calculate exact cooldown duration (defaults to 60s).
- A bare `429 RESOURCE_EXHAUSTED` without `details[]`, retry metadata, or `Retry-After` is treated as a request-scoped prompt rejection: healthy accounts are never cooled, third-party agent identity lines are scrubbed from system instructions, and the request is retried once with the system instruction moved into the first user turn before the HTTP 400 is surfaced.
- Repeated 429 responses without retry metadata escalate progressively (`60s -> 5m -> 30m -> 2h`) and zero the affected model's quota fraction so hybrid scoring stops preferring the throttled account.
- **Per-Model Cooldown Isolation**: An account rate-limited on one model remains fully available to serve other non-cooling models (e.g. Claude or GPT-OSS models).
- The account is put on cooldown for that model and requests immediately fail over to the next available account.
- If all accounts are cooling and the wait duration is $\le$ `max_wait_seconds`, `agw` absorbs the wait transparently before serving the request.
- If the wait duration exceeds the configured limit, `agw` returns HTTP 429 with the exact seconds in the `Retry-After` header.
- Secondary endpoint and healthy account fallbacks are automatically exercised upon upstream 5xx server errors.
- HTTP 403 bodies that indicate a permanent terms-of-service suspension (`has been disabled`, `violation of terms of service`, `VALIDATION_REQUIRED`) disable the account persistently with the upstream reason instead of looping it back through 30-minute cooldowns. Transient 403s still cool down and fail over.

Account subscription tiers select the endpoint order: personal (free/Plus/Pro) accounts try `daily-cloudcode-pa.googleapis.com` first, while business and enterprise accounts try `cloudcode-pa.googleapis.com` first. A custom `endpoints` array in `config.json` keeps its configured order; the standard default pair is the only list that gets tier-optimized.

Long Gemini 4 outputs are chained transparently: when upstream stops a candidate with `finishReason: CONTINUATION` and a `continuationToken`, `agw` re-sends the identical request payload with only the token added, for a bounded number of hops, and merges every segment into one client response. Continuation hops are dispatched through the account rotator, so the `sticky` strategy keeps them on the original account while `round-robin`/`hybrid` may switch; a switch is logged as a warning because an upstream token can be bound to the account that produced it. Per-model output ceilings are also respected, so `max_tokens`/`max_completion_tokens` requests above the model limit are clamped instead of failing upstream validation.

`host` accepts IPv4 and IPv6 loopback addresses; non-loopback values are coerced back to `127.0.0.1`. IPv6 literals are bracketed everywhere the daemon binds or builds a URL, so `host: "::1"` yields `[::1]:51400`.

## Development & Verification

### Makefile Commands

AGW includes a `Makefile` for streamlined development, formatting, testing, and hook installation:

```bash
make help             # View all available make targets
make build            # Build agw CLI binary (bin/agw)
make fmt              # Format all Go source files with gofmt
make fmt-check        # Verify formatting without modifying files
make lint             # Run static analysis (go vet)
make test             # Run unit tests with race detection
make test-short       # Run fast unit tests
make test-integration # Run end-to-end integration tests
make test-installer   # Run automated verification for install.sh
make test-all         # Run unit, integration, and installer tests
make check            # Run full preflight verification (fmt-check, lint, test, test-installer)
make release          # Package cross-platform release archives with checksums (dist/)
make hooks-install    # Install git pre-commit hooks (.githooks/pre-commit)
make pre-commit       # Run pre-commit checks manually
```

### Git Pre-Commit Hook

Install pre-commit hook (zero external dependencies, using `.githooks`):

```bash
make hooks-install
```

The hook automatically validates staged files on every `git commit`:

1. Checks git diff whitespace and conflict markers (`git diff --cached --check`).
2. Checks Go code formatting (`gofmt -l`).
3. Runs static analysis (`go vet ./...`).
4. Runs fast smoke unit tests (`go test -short ./...`).

To bypass hooks during emergency commits:

```bash
git commit --no-verify
# or
SKIP_HOOKS=1 git commit -m "..."
```

### Manual Verification Commands

Run all unit tests with race detection:

```bash
go test -v -race ./...
```

Run end-to-end integration tests (multi-account failover and streaming):

```bash
go test -v -race -tags=integration ./test/integration/...
```

Run installer test suite:

```bash
./scripts/test-installer.sh
```

Run static analysis:

```bash
go vet ./...
```

Check code formatting and diff hygiene:

```bash
git diff --check
```

## Acknowledgments & Inspiration

Antigravity Gateway (`agw`) is inspired by and builds upon ideas from the local AI proxy ecosystem:

- **[pi-gemini-auth](https://github.com/fzn0x/pi-gemini-auth)**: Foundational reference architecture for Google Cloud Code Assist (`v1internal`) protocol reverse-engineering, PKCE authentication, and the cryptographic thought-signature recipe format (`pi-gemini-auth.v1`) for Gemini 3 and Claude.
- **[9router](https://github.com/decolua/9router)**: Inspiration for local AI reverse proxying and coding agent workflow integration.

### Key Architectural Evolutions in `agw`

While drawing inspiration from these predecessors, `agw` introduces significant engineering improvements:

- **Zero-Dependency Native Go Binary**: Rewritten entirely in Go 1.24+ without Node.js, Next.js, Electron, or external runtime dependencies.
- **Ultra-Low Memory Footprint**: Operates at **~19 MB RSS memory**—over **7x to 8.5x lighter** than equivalent Node.js stacks (~135 MB – ~162 MB).
- **Strict Loopback Security**: Enforces `127.0.0.1:51400` loopback isolation with POSIX `0700`/`0600` atomic file permissions, avoiding inadvertent LAN exposure (`0.0.0.0`).
- **Lossless Thought Signature Protocol**: Retains authentic reasoning signatures across multi-turn tool invocations, ensuring coding agents (Pi, OpenCode, Cursor) never encounter upstream HTTP 400 schema rejections.
- **Transactional Client Injector**: Single-command automated configuration (`agw setup pi`, `agw setup opencode`) with timestamped atomic backups and live completion verification probes.

## Contributing

Contributions, bug reports, and feature requests are welcome! Please review our [Contributing Guidelines](CONTRIBUTING.md) and [Code of Conduct](CODE_OF_CONDUCT.md) before submitting pull requests. For release history and detailed notes on each version, consult the [Changelog](CHANGELOG.md).

If you discover a security vulnerability, please follow our responsible disclosure instructions in [SECURITY.md](SECURITY.md).

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.
