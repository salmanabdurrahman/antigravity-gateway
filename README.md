# Antigravity Gateway (`agw`)

[![CI](https://github.com/salmanabdurrahman/antigravity-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/salmanabdurrahman/antigravity-gateway/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/go-1.24%2B-00ADD8?logo=go&logoColor=white)](https://golang.org)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/salmanabdurrahman/antigravity-gateway?color=brightgreen)](https://github.com/salmanabdurrahman/antigravity-gateway/releases)

Zero-dependency, single-binary Go reverse proxy and CLI that connects AI coding agents (Pi, OpenCode, Claude Code, Cursor) to your own Google Antigravity / Google AI subscription through a local, OpenAI- and Anthropic-compatible HTTP interface on `http://127.0.0.1:51400`.

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

## Quick Start

```bash
# 1. Install the checksum-verified binary (macOS / Linux)
curl -fsSL https://raw.githubusercontent.com/salmanabdurrahman/antigravity-gateway/main/scripts/install.sh | sh

# Windows (PowerShell)
# irm https://raw.githubusercontent.com/salmanabdurrahman/antigravity-gateway/main/scripts/install.ps1 | iex

# 2. Authenticate your Google Antigravity subscription
agw login                 # browser OAuth (PKCE); use --no-browser on headless hosts
# agw login --local       # or reuse an existing Antigravity desktop session

# 3. Start the daemon
agw start --detach        # logs: ~/.agw/agw.log

# 4. Point your agent at the gateway
agw setup pi              # or: agw setup opencode | agw setup claude

# 5. Verify
agw doctor
curl -s http://127.0.0.1:51400/health | jq .
```

Everything else — platform archives, manual checksum verification, source builds, upgrades — is documented in the [Binary Installation Contract](docs/distribution.md).

## Highlights

- **Dual protocol surface**: OpenAI Chat Completions (`/v1/chat/completions`, `/v1/models`) and Anthropic Messages (`/v1/messages`) on the same loopback daemon.
- **Lossless reasoning fidelity**: Gemini and Claude thought signatures are preserved and replayed across tool turns, so multi-turn agentic calls do not hit upstream HTTP 400.
- **Multi-account failover**: `sticky`, `round-robin`, and `hybrid` rotation with per-model cooldowns, transparent 429 absorption, and quota-aware parking.
- **Hard output caps**: an explicit `max_tokens` is a real ceiling — reasoning budgets adapt, and long-decode chaining is skipped so clients get predictable cost and latency.
- **~20 MB RSS, instant startup, zero runtime dependencies**: one static binary, loopback-only, credentials confined to `~/.agw`.

Deeper design notes: [Architecture Overview](docs/architecture.md).

## Commands at a Glance

| Command                          | Purpose                                                        |
| :------------------------------- | :------------------------------------------------------------- |
| `agw login`                      | Google OAuth PKCE login (`--no-browser`, `--local`)            |
| `agw accounts [--check]`         | List accounts, tiers, sources, live quota (`--json`)           |
| `agw quota [--models] [--watch]` | Inspect rolling (5h), weekly, and per-model quota usage        |
| `agw logout <email>`             | Remove an account from the local store                         |
| `agw models list [--refresh]`    | Inspect catalog models, context windows, limits (`--quota`)    |
| `agw config [show\|strategy]`    | Inspect redacted config or update rotation strategy            |
| `agw start [--detach]`           | Run the gateway (`--strategy`, `--port`, `--config`)           |
| `agw status`                     | Show daemon state, active strategy, and runtime account stats  |
| `agw stop`                       | Stop the daemon, rediscovering it from `/health` if needed     |
| `agw setup <pi/opencode/claude>` | Transactional client configuration with backups + verification |
| `agw doctor`                     | Ports, permissions, accounts, upstream reachability, clients   |
| `agw update [--check]`           | Verify-and-replace the binary in place (alias: `upgrade`)      |
| `agw version`                    | Print the build version                                        |

Full flag reference: [CLI Reference](docs/cli.md).

## Endpoints at a Glance

| Route                       | Purpose                                            |
| :-------------------------- | :------------------------------------------------- |
| `GET /health`               | Liveness probe (loopback authority required)       |
| `GET /v1/models`            | OpenAI-style model listing (`/v1/models/{id}` too) |
| `POST /v1/chat/completions` | OpenAI chat completions, buffered or SSE streaming |
| `POST /v1/messages`         | Anthropic Messages API                             |
| `GET /v1/accounts`          | Redacted account diagnostics and metrics           |

`/v1/*` requires the generated bearer key from `~/.agw/config.json`; Anthropic clients may use `x-api-key`. Payload translation, session affinity, and migration notes: [HTTP API Reference](docs/api.md).

## Configuration Essentials

| Path                       | Purpose                                     |
| :------------------------- | :------------------------------------------ |
| `~/.agw/config.json`       | Host/port, API key, strategy, model mapping |
| `~/.agw/accounts.json`     | OAuth tokens and companion project mappings |
| `~/.agw/models-cache.json` | Cached catalog, token limits, quota windows |
| `~/.agw/agw.log`           | Detached daemon log (`agw.pid` for the PID) |

Common overrides: `AGW_HOME` (state directory), `AGW_CLIENT_ID` / `AGW_CLIENT_SECRET` (BYOC), `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY`, `AGW_PREFER_IPV4=1`, `PI_AGENT_DIR`, `OPENCODE_CONFIG`, `CLAUDE_CONFIG_DIR`.

Every key, threshold, rotation rule, and alias is documented in the [Configuration Reference](docs/configuration.md).

## Documentation

| Document                                             | Contents                                                         |
| :--------------------------------------------------- | :--------------------------------------------------------------- |
| [Binary Installation Contract](docs/distribution.md) | Platform matrix, archives, checksums, install/upgrade/uninstall  |
| [CLI Reference](docs/cli.md)                         | Every command and flag, daemon lifecycle, self-upgrade           |
| [HTTP API Reference](docs/api.md)                    | Endpoints, curl examples, translation internals, migration notes |
| [Configuration Reference](docs/configuration.md)     | Files, `config.json` keys, env vars, aliases, rotation rules     |
| [Architecture Overview](docs/architecture.md)        | Request pipeline, package map, protocol fidelity, benchmarks     |
| [OAuth 2.0 Client Strategy](docs/oauth-strategy.md)  | PKCE design, bundled client, BYOC setup                          |
| [Security Policy](SECURITY.md)                       | Supported versions, disclosure process, security invariants      |
| [Changelog](CHANGELOG.md)                            | Release history                                                  |

## Development

```bash
git clone https://github.com/salmanabdurrahman/antigravity-gateway.git
cd antigravity-gateway
make hooks-install   # gofmt + vet + smoke tests before every commit
make check           # fmt-check, vet, race tests, installer suite
make build           # ./bin/agw
```

Go 1.24+ is required. Contribution workflow, engineering standards, and the full verification matrix live in [CONTRIBUTING.md](CONTRIBUTING.md).

## Acknowledgments

- **[pi-gemini-auth](https://github.com/fzn0x/pi-gemini-auth)**: reference architecture for Cloud Code Assist (`v1internal`) reverse-engineering, PKCE, and the thought-signature recipe format for Gemini 3 and Claude.
- **[9router](https://github.com/decolua/9router)**: inspiration for local AI reverse proxying and coding-agent workflow integration.

## Contributing & License

Contributions are welcome — read [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md) first. Security issues follow [SECURITY.md](SECURITY.md).

Licensed under the [MIT License](LICENSE).
