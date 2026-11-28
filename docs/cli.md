# CLI Reference

Complete command reference for the Antigravity Gateway (`agw`) binary. For installation, see the [Binary Installation Contract](distribution.md); for configuration keys, see the [Configuration Reference](configuration.md).

```text
Usage:
  agw [command]

Available Commands:
  accounts    List configured Google Antigravity accounts
  completion  Generate the autocompletion script for the specified shell
  config      Inspect and update gateway configuration
  debug       Inspect the effective upstream wire identity without changing anything
  doctor      Run diagnostics on ports, permissions, accounts, and network connectivity
  help        Help about any command
  login       Authenticate a Google Antigravity account via OAuth PKCE
  logout      Remove a Google account from the local account store
  models      Inspect available chat models and token limits
  quota       Inspect live rolling (5h), weekly, and per-model account quotas
  setup       Configure AI coding agents to use Antigravity Gateway
  start       Start the local OpenAI-compatible gateway reverse proxy
  status      Show gateway daemon status, active strategy, and runtime metrics
  stop        Stop the running background gateway daemon
  update      Update Antigravity Gateway to the latest release
  version     Print the semantic version of agw
```

Global flag:

| Flag              | Purpose                                                                    |
| :---------------- | :------------------------------------------------------------------------- |
| `--config <path>` | Config file path (default `~/.agw/config.json` or `$AGW_HOME/config.json`) |
| `-h`, `--help`    | Help for any command                                                       |

## 1. `agw login`

Runs the Google OAuth PKCE flow, opening your browser and starting an ephemeral loopback callback listener on `http://127.0.0.1:51121/oauth-callback`.

```bash
agw login
```

| Flag           | Purpose                                                                                       |
| :------------- | :-------------------------------------------------------------------------------------------- |
| `--no-browser` | Do not open the default browser; print the authorization URL instead (headless or SSH hosts). |
| `--local`      | Import the session already signed in to the Antigravity desktop app instead of running OAuth. |

If the browser cannot reach the loopback listener, paste the final redirect URL into the terminal prompt — the authorization code is extracted locally.

### Importing a desktop session (`--local`)

`agw login --local` reads `antigravityAuthStatus` from the IDE SQLite state database (`state.vscdb`) or the IDE's local listener on port `9092`, whichever is reachable. Desktop sessions expose only a short-lived bearer token, so the import is stored separately from OAuth refresh tokens (`access_token` plus `credential_source: local_desktop`) and is never sent to the OAuth token endpoint. When that token expires, requests start returning `401`; re-run `agw login --local` while the IDE is signed in. `agw doctor` reports imported sessions and reminds you to re-import. If an account with the same email already holds an OAuth refresh token, the import is skipped so refreshable credentials are never downgraded to a short-lived token.

An Antigravity CLI (`agy`) token file can be imported explicitly through `AGW_AGY_TOKEN_PATH`; `AGW_AGY_KEYRING=1` opts in to a read-only OS keyring lookup. See the [Configuration Reference](configuration.md#3-environment-overrides).

## 2. `agw accounts`

Lists configured accounts with their linked Cloud Code companion projects, normalized subscription tiers, credential sources (`oauth` or `local_desktop`), and status.

```bash
agw accounts
agw accounts --check
agw accounts --check --json
```

| Flag          | Purpose                                                                                                                          |
| :------------ | :------------------------------------------------------------------------------------------------------------------------------- |
| `--check`     | Query live upstream rolling (5h), weekly, Gemini/Claude family quotas, and reset countdowns.                                     |
| `--re-enable` | Re-enable disabled accounts (requires `--check`); for a suspended account it clears the stored verdict without probing upstream. |
| `--json`      | Output redacted account metadata (and live quota details when combined with `--check`) as JSON.                                  |

`--check` queries accounts concurrently in a bounded pool and never re-enables disabled accounts on its own: a disabled account whose credentials still work is reported as disabled with a hint, and clearing that state requires `--check --re-enable`. The only state the inspection records by itself is a revoked refresh token, which is marked disabled so later requests skip it until you log in again. The command exits non-zero when the configuration cannot be loaded, so broken configuration fails scripts instead of printing a status column.

An account Google suspended for a terms violation shows `Suspended (<kind>)` instead of `Disabled (...)` and prints the upstream appeal link underneath the table. Suspended accounts receive **no** upstream traffic — no token refresh, catalog fetch, or quota probe — because probing a refused account is what escalates a first suspension into a permanent one. Reinstating stays explicit: after Google lifts the suspension, `agw accounts --check --re-enable` clears the local verdict without contacting upstream.

## 3. `agw quota`

Inspects live upstream quota windows across all configured accounts, separating the rolling 5-hour allowance from the weekly allowance and highlighting any constrained models (`<100%`).

```bash
agw quota
agw quota --models
agw quota --watch --interval 30s
agw quota --json
```

| Flag             | Purpose                                                                               |
| :--------------- | :------------------------------------------------------------------------------------ |
| `-m`, `--models` | Show the complete per-model quota breakdown under each account (including 100% ones). |
| `--json`         | Output structured quota details as JSON.                                              |
| `-w`, `--watch`  | Continuously refresh quota status in the terminal until interrupted (`Ctrl+C`).       |
| `--interval`     | Refresh interval when `--watch` is enabled (default `30s`, minimum `5s`).             |
| `--re-enable`    | Re-enable disabled accounts whose credentials still work during the check.            |

## 4. `agw status`

Probes the local gateway daemon (`/health` and authenticated `/v1/accounts`) to report whether the daemon is running, its PID, active rotation strategy, cached catalog count, and per-account runtime request/429/cooldown metrics. With `--risk`, it also snapshots the local risk posture (profile, rotation mode, in-flight ceiling, bare-429 guard) plus per-account counters into `~/.agw/risk.json`, using a masked address and an opaque credential digest so the file stays useful without storing identities.

```bash
agw status
agw status --port 51400
agw status --json
```

| Flag           | Purpose                                                                          |
| :------------- | :------------------------------------------------------------------------------- |
| `-p`, `--port` | Gateway HTTP port to inspect (default from `config.json` or `51400`).            |
| `--risk`       | Capture risk telemetry into `~/.agw/risk.json` and print the local risk posture. |
| `--json`       | Output daemon status and runtime metrics as JSON.                                |

## 5. `agw logout`

```bash
agw logout user@example.com
```

Removes the account's credentials and project mapping from `~/.agw/accounts.json`. When the account was suspended upstream, the command also prints the stored verdict, the validated appeal link, and the reminder to revoke third-party access at `https://myaccount.google.com/permissions` before signing in again — background traffic from a still-authorized integration can turn a lifted suspension into a permanent one.

## 6. `agw models`

```bash
agw models list            # instant, served from ~/.agw/models-cache.json
agw models list --quota    # include cached remaining quota per model
agw models list --refresh  # force a live upstream catalog refresh
agw models list --json
```

| Flag            | Purpose                                                                       |
| :-------------- | :---------------------------------------------------------------------------- |
| `--refresh`     | Force a live catalog refresh from upstream Google Cloud Code Assist.          |
| `-q`, `--quota` | Include cached remaining quota and reset countdowns from `models-cache.json`. |
| `--json`        | Output the model catalog, active aliases, and optional cached quotas as JSON. |

`--refresh`, `--quota`, and `--json` are persistent flags on `models`, so `agw models --refresh list` and `agw models --quota` both work. Listings include context window, maximum output tokens, reasoning/multimodal capability flags, and any configured `model_mapping` aliases.

Catalog parsing follows the capability metadata returned by `fetchAvailableModels`: `agentModelSorts` and `tieredModelIds` override name-based heuristics, `apiProvider`/`modelProvider` exclude non-conversational entries, and `<model>-disabled` placeholders are never listed. Gemini 4 Neon reports its documented 512K context window. When setup runs fully offline, `agw` falls back to a bundled manifest covering Gemini 4 (Neon, Carbon, Hydrogen) and Claude 5.5 instead of retired identifiers.

## 7. `agw config`

Inspects redacted settings in `config.json`, resolves the active config file path, and updates the persisted rotation strategy safely.

```bash
agw config show
agw config show --json
agw config path
agw config strategy hybrid
```

| Subcommand                  | Purpose                                                                     |
| :-------------------------- | :-------------------------------------------------------------------------- |
| `agw config show`           | Display redacted settings (API key and OAuth client secret remain masked).  |
| `agw config path`           | Print the resolved `config.json` path (`--config` and `$AGW_HOME` honored). |
| `agw config strategy [val]` | Print or persist `strategy` (`sticky`, `round-robin`, or `hybrid`).         |

`--json` is supported across all `agw config` subcommands. Updating `strategy` rewrites only the `"strategy"` field in `config.json`; if a daemon is running, restart it (`agw stop && agw start -d`) for the new strategy to take effect.

`agw config show` also reports the resolved `Profile`, `Rotation Mode`, and `Catalog Refresh` window, the pacing posture (`Max In Flight`, `Dispatch Budget`, `Prompt Rejection Guard`), and the effective post-suspension protection window as `Suspension Pause`: the resolved number of seconds, or `disabled` when `disable_suspend_pause` is set. See the [Configuration Reference](configuration.md#2-configjson-keys) for the underlying keys.

## 8. `agw start`

Runs the gateway reverse proxy on `127.0.0.1:51400`.

```bash
agw start                     # foreground
agw start --strategy hybrid   # rotation override for this run
agw start --detach            # background daemon
agw start --detach --config /etc/agw/config.json
```

| Flag                | Purpose                                                        |
| :------------------ | :------------------------------------------------------------- |
| `-d`, `--detach`    | Run the daemon in the background; logs go to `~/.agw/agw.log`. |
| `-p`, `--port <n>`  | HTTP port to bind (default `51400`, or `port` from config).    |
| `--strategy <name>` | Rotation strategy: `sticky`, `round-robin`, or `hybrid`.       |
| `--config <path>`   | Config file path; forwarded to the detached child as well.     |

Detached launchers forward every command-line override (`--strategy`, `--port`, `--config`) to the child process, so a custom configuration path is honored in the background exactly as in the foreground. The PID file (`~/.agw/agw.pid`) is written by the daemon once it reaches the listening state.

`--strategy` only governs selection between pooled accounts; under the default `safe` profile the gateway serves a single primary account, so enable `rotation_mode: pool` (or `profile: manual`) first if you want the strategy to spread traffic.

Before spawning, `agw start` probes `/health` on the configured port. If an orphaned daemon answers, the command reports it and restores the missing PID file instead of launching a second process that cannot bind. A listener that does not identify itself as `antigravity-gateway` is never touched.

## 9. `agw stop`

```bash
agw stop
agw stop --port 51401
```

| Flag           | Purpose                                                                      |
| :------------- | :--------------------------------------------------------------------------- |
| `-p`, `--port` | Gateway HTTP port to probe and stop (default from `config.json` or `51400`). |

Terminates the running daemon gracefully via `SIGTERM`. If `~/.agw/agw.pid` is missing or stale, `agw stop` rediscovers the daemon through its `/health` identity probe on the target port (`--port` or `config.json`) and resolves the owning process from the operating system socket table.

## 10. `agw update`

Replaces the current executable with a newer release, atomically, in place.

```bash
agw update                  # latest release
agw update --check          # report availability only
agw update --version 0.2.1  # pin or downgrade
agw update --force          # reinstall even when up to date
agw update --no-restart     # leave a running daemon stopped
```

| Flag            | Purpose                                                                                     |
| :-------------- | :------------------------------------------------------------------------------------------ |
| `--check`       | Query GitHub Releases and print availability without downloading or installing.             |
| `--version <v>` | Target a specific release (`0.2.1` or `v0.2.1`).                                            |
| `--force`       | Reinstall even when already current, or replace the binary when the daemon refused to stop. |
| `--no-restart`  | Do not restart a background daemon after replacement.                                       |

A running daemon is stopped before the executable is replaced, and the restart preserves the `--config` path the daemon was launched with. If the stop request is refused or the daemon keeps answering `/health`, the upgrade aborts instead of swapping the binary underneath a live process; `--force` overrides that guard. Release metadata, archives, and checksums must be served over HTTPS (plain HTTP is accepted only for loopback mirrors), HTTPS-to-HTTP redirect downgrades are rejected, archives must contain `agw` as a regular file (symlinks and directories are refused), and extraction is size-capped. `agw upgrade` is an alias.

## 11. `agw setup`

Configures an AI coding agent to use the local gateway. Every subcommand creates a timestamped atomic backup (`*.bak-<timestamp>`) before writing, and by default performs a 1-token test completion to verify end-to-end connectivity. Other OpenAI-compatible clients (such as Cursor) can point directly at `http://127.0.0.1:51400/v1` with the bearer key from `~/.agw/config.json`.

Shared flags:

| Flag                | Purpose                                                                                        |
| :------------------ | :--------------------------------------------------------------------------------------------- |
| `--url <base-url>`  | Gateway base URL (default from config or `http://127.0.0.1:51400`).                            |
| `--api-key <key>`   | API key override (default from `config.json`); must match `agw_` + 64 hex.                     |
| `--models <filter>` | Filter models by comma-separated patterns, e.g. `"gemini-*,!*-low"` (Pi and OpenCode targets). |
| `--no-verify`       | Skip the 1-token connectivity probe.                                                           |
| `--dry-run`         | Print the generated configuration without writing to disk.                                     |

### `agw setup pi`

Registers the `antigravity` provider in `~/.pi/agent/models.json`.

| Flag              | Purpose                                                                     |
| :---------------- | :-------------------------------------------------------------------------- |
| `--pi-dir <path>` | Pi configuration directory (default `~/.pi/agent` or `$PI_AGENT_DIR`).      |
| `--clean-legacy`  | Remove legacy loopback proxy entries and placeholder tokens (default true). |

### `agw setup opencode`

Registers the `antigravity` provider through `@opencode/ai/providers/openai-compatible` in `~/.config/opencode/opencode.json`.

| Flag                | Purpose                                                                                         |
| :------------------ | :---------------------------------------------------------------------------------------------- |
| `--config-file <p>` | OpenCode configuration file (default `~/.config/opencode/opencode.json` or `$OPENCODE_CONFIG`). |

### `agw setup claude`

Configures Claude Code CLI to talk to the Anthropic Messages endpoint (`/v1/messages`) by updating `~/.claude/settings.json`.

| Flag                 | Purpose                                                                                                           |
| :------------------- | :---------------------------------------------------------------------------------------------------------------- |
| `--model <id>`       | Primary `ANTHROPIC_MODEL` configured for the CLI (auto-resolved from catalog, fallback `claude-sonnet-5-5-high`). |
| `--settings <path>`  | Settings file (default `~/.claude/settings.json` or `$CLAUDE_CONFIG_DIR`).                                        |
| `--experimental-mcp` | Set `ENABLE_EXPERIMENTAL_MCP_CLI=true` in Claude settings (default `true`).                                       |

An `--api-key` value that does not match `agw_` plus 64 hexadecimal characters fails before any client file is written. To keep an intentionally custom key, edit `api_key` in `config.json` and the client files directly; `agw doctor` reports when a stored key no longer matches the generated format.

## 12. `agw doctor`

```bash
agw doctor
```

Runs an end-to-end environment audit:

1. **Network ports**: gateway port (`51400` or configured `port`) and OAuth callback port resolved from `redirect_uri` (default `51121`).
2. **Filesystem permissions**: `0700` directory and `0600` credential files, plus API-key format validation.
3. **Account pool**: configured accounts, refresh tokens, companion project IDs, imported desktop sessions, plus an operating-profile advisory that warns when `profile: manual` would rotate more than one active account and explains idle accounts under the safe profile. Suspended accounts are reported with their appeal link, since they stay out of rotation until the operator appeals and clears them.
4. **Upstream identity**: the advertised `antigravity/hub/<version>` identity with its paired `cl` changelist, warning when a version override has no matching changelist, plus the locally installed `agy` CLI version reported on its own track.
5. **Upstream connectivity**: TLS reachability and latency to `oauth2.googleapis.com`, `daily-cloudcode-pa.googleapis.com`, and `cloudcode-pa.googleapis.com`; IPv4 versus IPv6 reachability per Cloud Code host; configured `HTTPS_PROXY` with credentials masked.
6. **Client integrations**: whether Pi, OpenCode, and Claude Code configurations exist, point at the gateway, and carry the same API key as `config.json`.

## 13. `agw debug`

Read-only diagnostics for the identity the gateway presents to Google Cloud Code Assist. Nothing here contacts Google, writes `config.json`, or prints an API key, OAuth client secret, access token, or refresh token.

### `agw debug wire`

```bash
agw debug wire
agw debug wire --json
```

Prints the values that actually leave the machine, resolved through the same helpers the daemon uses:

| Field                    | Meaning                                                                                               |
| :----------------------- | :---------------------------------------------------------------------------------------------------- |
| `Config File`            | Resolved `config.json` path honoring `--config` and `$AGW_HOME`                                       |
| `Gateway Endpoint`       | Clamped loopback base URL clients connect to                                                          |
| `Strategy`               | Configured account rotation strategy                                                                  |
| `Profile`                | Resolved operating profile (`safe` or `manual`)                                                       |
| `Rotation Mode`          | Resolved dispatch mode (`single` keeps one primary account, `pool` allows failover)                   |
| `Quota Threshold`        | Fractional remaining-quota floor before an account is deprioritized                                   |
| `User-Agent`             | Effective `User-Agent` header, including `AGW_USER_AGENT` or `user_agent` overrides                   |
| `X-Goog-Api-Client`      | Effective `X-Goog-Api-Client` header value                                                            |
| `Client-Metadata`        | Effective `Client-Metadata` JSON header value                                                         |
| `Upstream Transport`     | Active wire shape: `http/1.1` per-credential pools without ALPN, or the `AGW_WIRE_MODE=auto` fallback |
| `Max Cooldown Wait`      | Maximum cooldown wait absorbed before a request fails                                                 |
| `Default 429 Cooldown`   | Base cooldown applied to a `429` without an explicit retry delay                                      |
| `Empty Stream Retries`   | Retry budget for empty upstream streams                                                               |
| `Catalog Refresh`        | Effective catalog freshness window in minutes                                                         |
| `Max In Flight`          | Resolved concurrent-stream ceiling per credential, or `unbounded` under the manual profile            |
| `Dispatch Budget`        | Explicit per-request dispatch budget, or `profile default` when the profile decides                   |
| `Prompt Rejection Guard` | Bare-429 threshold and park duration, or `off` when the guard is disabled                             |
| `Upstream Endpoints`     | Upstream Cloud Code Assist base URLs in dispatch order                                                |

Use it before opening an upstream rejection report, and after an upgrade, to confirm which identity and transport a request really carried. Secrets are never printed, so the report is safe to paste into an issue.

## 14. `agw version`

Prints the semantic version embedded at build time (for example `agw version 0.2.1`).
