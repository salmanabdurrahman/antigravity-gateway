# Antigravity Gateway (`agw`)

[![CI](https://github.com/salmanabdurrahman/antigravity-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/salmanabdurrahman/antigravity-gateway/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/go-1.24%2B-00ADD8?logo=go&logoColor=white)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/salmanabdurrahman/antigravity-gateway?color=brightgreen)](https://github.com/salmanabdurrahman/antigravity-gateway/releases)

Antigravity Gateway (`agw`) is a zero-dependency, single-binary Go reverse proxy and CLI daemon. It connects modern AI coding agents (such as Pi Coding Agent, OpenCode, and Cursor) to personal Google Antigravity / Google AI subscriptions over a local OpenAI-compatible HTTP interface (`http://127.0.0.1:51400/v1`).

## Key Features

- **Dual OpenAI & Anthropic Compatible Local Endpoints**: Exposes `/v1/chat/completions` (OpenAI format) and `/v1/messages` (Anthropic format) bounded strictly to local loopback (`127.0.0.1:51400`).
- **Multi-Account Failover & Rotation**: Manages multiple Google Antigravity accounts locally with configurable strategies (`sticky`, `round-robin`, or `hybrid` with health scores, token bucket burst throttling, and proactive quota protection).
- **Protocol Fidelity**: Handles thought-signature preservation and replay for Gemini 3 and Claude models, including required interleaved thinking headers, multi-phase tool schema sanitization ($ref hints, union flattening, array repair), thinking budget clamping, interrupted tool-loop recovery, proactive `cache_control` stripping, and billing header sanitization.
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
| **Gemini 3 Thought-Signatures** | **Lossless Preservation (`pga.v1` format)**                      | Stripped / normalized                       |
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

Fork and launch the daemon in the background. The process ID is recorded in `~/.agw/agw.pid` and logs are written to `~/.agw/agw.log`:

```bash
agw start --detach
```

#### Stop Gateway Daemon

Terminate the running daemon gracefully via SIGTERM:

```bash
agw stop
```

The PID file is written by the daemon itself as soon as it reaches the listening state (foreground and detached alike) and removed on graceful shutdown. If `~/.agw/agw.pid` is missing or stale, `agw stop` rediscovers the daemon through its `/health` identity probe on the port recorded in `config.json` and resolves the owning process from the operating system socket table; a listener that does not identify itself as `antigravity-gateway` is never signalled. `agw start` performs the same probe before spawning a process, so an orphaned daemon is reported (and its PID file restored) instead of launching a second process that cannot bind. Because discovery uses the configured port, keep `port` in `config.json` in sync when running the daemon on a custom `--port`.

### Web Management Dashboard

Launch your default browser to open the embedded zero-dependency dashboard hosted by the running gateway daemon:

```bash
agw ui
```

Or navigate directly to `http://127.0.0.1:51400/ui` (automatic redirect from root when visited in web browsers). Features visual account health progress bars, token bucket balances, live model quota tracking, and dynamic strategy switching. Strategy changes take effect immediately in memory and are persisted by rewriting only the `strategy` field of `config.json`, leaving every other stored setting untouched.

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
- `--config`: Custom OpenCode configuration file path

#### Configure Claude Code CLI

Configures Claude Code CLI to communicate with Antigravity Gateway's Anthropic Messages API (`/v1/messages`) by updating `~/.claude/settings.json`:

```bash
agw setup claude
```

Optional flags:

- `--model`: Primary model configured for Claude CLI (default: `claude-sonnet-4-6`)
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
4. **Upstream Connectivity**: Tests TLS reachability and latency to Google OAuth (`oauth2.googleapis.com`) and Cloud Code PA (`cloudcode-pa.googleapis.com`).
5. **Client Integrations**: Checks whether Pi Coding Agent, OpenCode, and Claude Code configurations are installed, configured, and carrying the same gateway API key as `config.json`.

### HTTP API Endpoints

The gateway listens exclusively on local loopback (`127.0.0.1:51400`):

| Method & Route              | Purpose                                                               | Authentication               |
| :-------------------------- | :-------------------------------------------------------------------- | :--------------------------- |
| `GET /health`               | Liveness probe, gateway name, version, and active account count       | None (public)                |
| `GET /v1/models`            | OpenAI-compatible model listing with context windows and capabilities | Bearer token (`config.json`) |
| `POST /v1/chat/completions` | OpenAI-compatible streaming SSE or non-streaming chat completions     | Bearer token (`config.json`) |
| `GET /v1/accounts`          | Redacted diagnostic snapshots, cooldown statuses, and request metrics | Bearer token (`config.json`) |

#### Quick API Test with `curl`

Send a chat completion request to the running gateway:

```bash
API_KEY=$(grep -o '"api_key": "[^"]*' ~/.agw/config.json | cut -d'"' -f4)

curl -s http://127.0.0.1:51400/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $API_KEY" \
  -d '{
    "model": "gemini-3.8-flash-high",
    "messages": [{"role": "user", "content": "Hello!"}],
    "max_tokens": 50
  }' | jq .
```

### Request & Response Translation

The gateway transparently translates between OpenAI Chat Completions requests and Google Cloud Code Assist protocol envelopes:

- **Multimodal Support**: Maps OpenAI base64 `data:` URLs directly into Cloud Code `inlineData`.
- **Tool Schema Defenses**: Converts OpenAI function parameters for Gemini (`parametersJsonSchema`) and Claude (`parameters`), while stripping disallowed schema attributes (`$ref`, `$defs`, `$schema`, unsupported unions). Dynamically detects HTTP 400 schema rejections to fall back to legacy `parameters` mode.
- **Thought Signature & Replay**: Encodes upstream thinking tokens, steps, and Gemini 3/Claude cryptographic `thoughtSignature` data into OpenAI `reasoning_details`. Replaces missing signatures on tool re-execution with `skip_thought_signature_validator` to avoid upstream tool validation failures.
- **Streaming SSE Engine**: Streams `v1internal:streamGenerateContent?alt=sse` chunks to OpenAI SSE chunks (`choices[0].delta.content`, `choices[0].delta.reasoning_content`, and `choices[0].delta.tool_calls`), with buffered scanner capacity up to 10 MB to safely handle massive code diffs and thinking blocks.

## Configuration & Environment

The gateway configuration directory defaults to `~/.agw` and can be overridden via the `AGW_HOME` environment variable.

| File                       | Purpose                                                                                      | File Permissions |
| :------------------------- | :------------------------------------------------------------------------------------------- | :--------------- |
| `~/.agw/config.json`       | Daemon settings, host/port, local API key, rotation strategy, quota threshold, model mapping | `0600`           |
| `~/.agw/accounts.json`     | Stored Google account tokens and companion project mappings                                  | `0600`           |
| `~/.agw/models-cache.json` | Cached model catalog and token limits                                                        | `0600`           |
| `~/.agw/agw.pid`           | Running daemon process ID (written once listening)                                           | `0600`           |

`config.json` is the single source of truth for gateway settings. Runtime mutations rewrite only the field being changed after re-reading the file first, so a running daemon never persists stale or partial state over your API key, endpoints, or model mapping. The daemon logs its resolved configuration path at startup, and `agw doctor` verifies that the stored key still matches the generated format and the key embedded in each configured client.

### Environment Overrides

- `AGW_HOME`: Root path for local configuration and credential storage (default: `~/.agw`).
- `AGW_USER_AGENT` (legacy fallback: `PGA_USER_AGENT`): Custom User-Agent header override.
- `AGW_API_CLIENT` (legacy fallback: `PGA_API_CLIENT`): Custom `X-Goog-Api-Client` header override.
- `AGW_CLIENT_METADATA` (legacy fallback: `PGA_CLIENT_METADATA`): Custom `Client-Metadata` JSON header override.
- `AGW_ANTIGRAVITY_VERSION` (legacy fallback: `PGA_ANTIGRAVITY_VERSION`): Version string used in default User-Agent construction (default: `2.11.0`).
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
    "claude-custom": "claude-sonnet-4-6"
  }
}
```

When rate limits occur:

- Upstream `resetTime` (RFC3339) or `retryDelay` is parsed to calculate exact cooldown duration (defaults to 60s).
- **Per-Model Cooldown Isolation**: An account rate-limited on `gemini-3.8-flash` remains fully available to serve other non-cooling models (e.g. `claude-sonnet-4-6` or `gpt-oss-120b`).
- The account is put on cooldown for that model and requests immediately fail over to the next available account.
- If all accounts are cooling and the wait duration is $\le 20$s, `agw` absorbs the wait transparently before serving the request.
- If the wait duration exceeds 20s, `agw` returns HTTP 429 with the exact seconds in the `Retry-After` header.
- Secondary endpoint and healthy account fallbacks are automatically exercised upon upstream 5xx server errors.

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
