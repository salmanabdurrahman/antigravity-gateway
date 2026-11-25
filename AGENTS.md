# AGENTS.md — Antigravity Gateway (AGW)

Project-level agent instruction contract for `antigravity-gateway`.
CLI binary: `agw`

> Global system instructions, runtime safety protocols, and developer commands remain higher priority. This file establishes repository-specific engineering standards, package boundaries, Go coding rules, and verification criteria.

## 1. Identity, Purpose, & Scope

Antigravity Gateway (`agw`) is a zero-dependency, single-binary Go daemon and CLI. It operates as a local OpenAI-compatible reverse proxy (`http://127.0.0.1:51400/v1`) connecting AI coding agents (Pi, OpenCode, Cursor) directly to Google Antigravity / Google AI consumer subscriptions.

Key capabilities:

- Local OAuth PKCE authentication and Cloud Code companion project discovery.
- Multi-account storage with sticky/round-robin rotation.
- Transparent failover upon encountering HTTP 429 rate limits, parsing Google `resetTime` and absorbing short waits ($\le 20$s).
- Live model catalog parsing (`v1internal:fetchAvailableModels`) and remaining quota inspection.
- Bidirectional OpenAI $\leftrightarrow$ Cloud Code Assist SSE translation.
- Gemini 2.x and newer plus Claude thought-signature preservation and replay.
- Transactional client configuration (`agw setup pi`, `agw setup opencode`) with automatic timestamped backups.

## 2. Authority & Source of Truth

- **Global Runtime Instructions**: System/developer instructions and global runtime safety protocols.
- **Project Architecture & Contracts**: `README.md`, `docs/oauth-strategy.md`, and `docs/distribution.md` (own gateway contracts, platform support, and distribution design).
- **Upstream Protocol Specifications**: Google Cloud Code Assist (`v1internal`) and OpenAI Chat Completions API protocol envelopes.
- **Modern Go Standards**: `.agents/skills/use-modern-go/SKILL.md` (mandatory Go guidelines).

## 3. Mandatory Skill Gate: Modern Go Guidelines

**CRITICAL RULE**: Every agent MUST consult `.agents/skills/use-modern-go/SKILL.md` before writing, modifying, fixing, or refactoring any Go code.

1. **Version Resolution**: The project targets **Go 1.24+** (toolchain: Go 1.27+).
2. **Consult Modern Idioms CLI**:
   Run the Modern Go Guidelines tool prior to editing or adding Go files:
   ```bash
   sh .agents/skills/use-modern-go/scripts/run-tool.sh list --go-version 1.24
   ```
3. **Mandatory Modern Idioms**:
   - Use `any` instead of `interface{}`.
   - Use `net/http` method-aware ServeMux patterns (e.g. `mux.HandleFunc("GET /health", ...)`).
   - Use `cmp.Or` for fallback resolution chains.
   - Use `slices.*` (`slices.Contains`, `slices.Clone`, `slices.Sort`) and `maps.*` (`maps.Clone`, `maps.Copy`) instead of manual loops.
   - Use built-in `min` and `max` instead of handwritten condition blocks.
   - Use `clear(m)` / `clear(s)` for wiping maps and slices.
   - Use `sync.OnceFunc` / `sync.OnceValue` for memoization.
   - Use typed atomics (`atomic.Bool`, `atomic.Int64`, `atomic.Pointer[T]`).
   - Use `errors.Is` and `errors.As` instead of direct equality checks.
   - Use `t.Context()` in testing functions, and `context.WithoutCancel(t.Context())` when background or shutdown routines need to outlive test timeout cancellation.
   - Use Go 1.24+ `omitzero` struct tags on boolean, numeric, and struct fields whose zero-value should be omitted from JSON (e.g. `json:"stream,omitzero"`, `json:"disabled,omitzero"`).

## 4. Go Coding Standards & Agent Guidelines

### 4.1 Comprehensive Documentation & English Comments

All documentation and comments in this repository MUST be written in clear technical English. Every single `.go` file must achieve 100% documentation coverage across the following tiers:

1. **Package Doc Comments**:
   Every Go source file (`.go`) MUST begin with a formal package documentation block describing the package's domain responsibility and the file's architectural role:
   ```go
   // Package translate provides bidirectional translation between OpenAI Chat Completions
   // payloads and Google Cloud Code Assist (Antigravity) envelope structures.
   package translate
   ```
2. **Exported Identifiers**:
   Every exported type, struct, interface, function, method, constant, and variable MUST have a doc comment starting with the identifier name:
   ```go
   // AccountPool manages an in-memory collection of Google accounts and their cooldown states.
   type AccountPool struct { ... }
   ```
3. **Struct Fields Documentation**:
   Every field inside a struct (especially in configuration, request/response models, option structs, and in-memory states) MUST have an explanatory comment detailing its role, format, units, or constraints:
   ```go
   type Config struct {
       // Host defines the local loopback network interface to bind the HTTP reverse proxy daemon to.
       Host string `json:"host"`
       // Port defines the local TCP port number where the gateway listens.
       Port int `json:"port"`
   }
   ```
4. **Internal Helper Functions**:
   All unexported helper functions, unexported types, and private methods MUST have clear doc comments explaining their purpose, parameters, return values, and algorithms or edge-case handling.
5. **Test Function Documentation**:
   Every test function (`TestXxx(t *testing.T)`) MUST have a top-level doc comment explaining the specific scenario under test, preconditions, execution flow, and expected assertions:
   ```go
   // TestRotatorStickyAndFailover429 verifies sticky account selection and immediate failover
   // to an alternative account when upstream returns HTTP 429 rate limiting.
   func TestRotatorStickyAndFailover429(t *testing.T) { ... }
   ```
6. **Upstream Protocol Quirks**:
   Any code handling Google Cloud Code Assist quirks MUST be documented with explanatory English comments detailing _why_ the logic is required:
   - Explain why `"skip_thought_signature_validator"` is injected when `thoughtSignature` is absent.
   - Explain why tool JSON schemas must be guarded against `$ref` collisions (prevents upstream HTTP 400 validation errors).
   - Explain why the `anthropic-beta: interleaved-thinking-2025-05-14` header is mandatory for Claude models.
   - Explain why tool delta chunks must explicitly preserve `index: 0`.
   - Explain why scanner capacity is expanded to 10 MB in `ReadSSEEvents`.
7. **Code Clarity**:
   Comments must explain the _reasoning_ (the why) and domain mechanics, not merely restate the obvious Go syntax (the what).

### 4.2 Package Architecture & Seams

Follow the standard Go project layout (`golang-standards/project-layout`):

- `cmd/agw/`: Minimal entrypoints and Cobra CLI commands (`root.go`, `start.go`, `stop.go`, `login.go`, `logout.go`, `accounts.go`, `models.go`, `setup.go`, `doctor.go`, `update.go`, `version.go`). No business logic.
- `internal/`: All core logic belongs under `internal/` packages:
  - `internal/config`: Paths (`paths.go`), defaults and env overrides (`config.go`), loopback address helpers (`addr.go`), upstream headers (`headers.go`), HTTP transport (`transport.go`).
  - `internal/auth`: OAuth PKCE (`oauth.go`, `pkce.go`, `callback.go`), token refresh (`token.go`), companion project discovery (`discovery.go`), IDE/CLI session detection (`local_detect.go`), account persistence (`store.go`).
  - `internal/rotator`: Account pool (`pool.go`), strategy pickers (`picker.go`), cooldown timers (`tracker.go`), error classification and retry delays (`retry.go`), dispatch loop (`rotator.go`).
  - `internal/catalog`: Dynamic model fetcher and quota parsing (`catalog.go`, `models.go`, `quota.go`), embedded offline manifest (`fallback.go`), disk cache (`cache.go`).
  - `internal/translate`: OpenAI/Anthropic request builders (`request.go`, `anthropic.go`), thinking budgets (`thinking.go`), thought signature recipes (`recipe.go`), cache-control markers (`cache_control.go`), schema normalization (`schema.go`, `schema_pipeline.go`), SSE and response translation (`response.go`).
  - `internal/server`: `net/http` daemon (`server.go`, `daemon.go`), router and handlers (`router.go`), middleware (`middleware.go`), bounded thought-signature cache (`signature_cache.go`), PID/port ownership helpers (`portowner*.go`).
  - `internal/client`: Client auto-configurators (`pi.go`, `opencode.go`, `claude.go`), atomic file helpers (`fs.go`), connectivity probes (`test_completion.go`).
- **Internal Encapsulation**: Never leak private structs or raw internal states across package boundaries. Keep interfaces small and focused.

### 4.3 Error Handling Discipline

- Always wrap errors with contextual messages using `%w`:
  ```go
  if err := store.Save(accounts); err != nil {
      return fmt.Errorf("saving accounts to %s: %w", store.path, err)
  }
  ```
- Use sentinel errors (`var ErrNoUsableAccounts = errors.New(...)`) or typed error structs (`type UpstreamError struct { StatusCode int; ... }`).
- Never discard errors (`_ = ...`) without an explicit inline comment explaining why the error is safely ignorable.

### 4.4 Concurrency, Goroutines, & Context

- Synchronize shared mutable states (`AccountPool`, `cooldownMap`, `tokenCache`) using `sync.RWMutex`.
- Always pass `context.Context` as the first argument in network, I/O, or long-running calls.
- Honor cancellation and timeouts:
  ```go
  ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
  defer cancel()
  ```
- Never leak goroutines. Background tasks must listen to `ctx.Done()` or handle graceful shutdown signals.

### 4.5 Logging & Observability

- Use standard library structured logging (`log/slog`).
- Never use `fmt.Println` or `log.Printf` inside `internal/` library code.
- Pass `*slog.Logger` or use package-level configured loggers.
- **Zero Secret Exposure**: Never log raw access tokens, refresh tokens, Bearer API keys, or full authorization headers. Always redact or log only account email addresses and project IDs.

### 4.6 File System & Security Hygiene

- **POSIX Permissions**: All files in `~/.agw/` (`accounts.json`, `config.json`, `models-cache.json`) MUST be written with `0600` permissions (`0700` for directory).
- **Atomic Writes**: Always write files using the atomic rename pattern (write to temp file in same directory, flush/sync to disk, rename over destination) to prevent file corruption on unexpected power loss or process kill.
- **Loopback Enforcement**: Gateway server must bind strictly to `127.0.0.1`. Never default to `0.0.0.0`.

### 4.7 Test Fixtures & Environment Hygiene

- **Tracked Test Data**: All static test fixtures MUST be placed within the corresponding package's `testdata/` directory and tracked in Git.
- **No External Test Dependencies**: Unit and integration tests MUST NEVER depend on untracked local directories, external internet connections, or machine-specific absolute paths.
- **Environment Isolation**: Tests mutating environment variables MUST use `t.Setenv(...)` or cleanup functions to ensure zero leakage across parallel test runs.

### 4.8 Documentation Layout

- `README.md` stays short: project overview, risk notice, quick start, command/endpoint summaries, documentation index.
- Depth belongs in `docs/`: `cli.md` (commands and flags), `api.md` (endpoints, translation, migration notes), `configuration.md` (files, keys, environment, rotation), `architecture.md` (pipeline, package map, protocol fidelity), plus the existing contract documents.
- Never duplicate a fact across documents; link to the deepest authoritative page and update it at the source.
- Behavior changes must update the affected `docs/` page and the README summary that points at it; `CHANGELOG.md` remains release-only.

## 5. Repository Map & Boundaries

| Path                    | Responsibility                                    | Invariants & Rules                                                                                                                                          |
| :---------------------- | :------------------------------------------------ | :---------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `cmd/agw/`              | CLI commands via Cobra                            | Thin CLI layer; parses flags and invokes `internal/` packages; includes native `update` self-upgrade                                                        |
| `docs/`                 | User-facing documentation & contracts             | Installation contract, CLI reference, HTTP API reference, configuration reference, architecture notes, OAuth strategy                                       |
| `internal/config/`      | Config, paths, address helpers, headers           | Handles `~/.agw` resolution, env overrides, and IPv6-safe loopback host/port formatting                                                                     |
| `internal/auth/`        | OAuth PKCE, tokens, discovery, storage            | Manages `accounts.json` with `0600` permissions                                                                                                             |
| `internal/rotator/`     | Account pool, failover, cooldowns                 | Implements `sticky`, `round-robin` & `hybrid`; transparent 429 retry                                                                                        |
| `internal/catalog/`     | Upstream model discovery & quota                  | Filters chat models; caches to `models-cache.json`                                                                                                          |
| `internal/translate/`   | Bidirectional OpenAI $\leftrightarrow$ Cloud Code | Thought signatures, `$ref` schema defense, SSE parser                                                                                                       |
| `internal/server/`      | `net/http` daemon & router                        | Strict `127.0.0.1:51400` binding; Bearer key auth; config snapshot copy-on-write; bounded signature cache                                                   |
| `internal/client/`      | Agent configuration injectors                     | Pi, OpenCode, and Claude Code injectors; safe atomic backup (`*.bak-<timestamp>`) before modifying client configs                                           |
| `internal/updater/`     | Release discovery, SHA-256 verification, upgrade  | In-place atomic replacement (`os.Executable()`); checksum verification against `SHA256SUMS`                                                                 |
| `.github/`              | CI/CD automation workflows                        | Read-only repository permissions (`contents: read`); native Linux/macOS/Windows test jobs; release publishes only after artifact and installer verification |
| `.githooks/`            | Native git hooks repository                       | Zero-dependency pre-commit script enforcing format, vet, smoke tests                                                                                        |
| `scripts/`              | Tooling & operational scripts                     | Release packager, checksum installer, test runner, and git hooks installer                                                                                  |
| `Makefile`              | Standard task runner                              | Canonical build, format, test, and verification commands                                                                                                    |
| `.agents/skills/`       | Project-scoped skill definitions                  | Consult `use-modern-go` before writing Go; use `bump-version` for releases                                                                                  |
| `CHANGELOG.md` / `*.md` | Open source governance, changelog, and guidance   | Keep a Changelog (`CHANGELOG.md`), MIT license, security policy, contributor guide; strict release-only edit rule                                           |

### 5.1 Strict Changelog Policy: Release-Only Mutations

**MANDATORY RULE**: Agents MUST NOT modify `CHANGELOG.md` during ordinary bug fixes, refactoring, feature development, or maintenance tasks.

- `CHANGELOG.md` is strictly reserved for the release preparation workflow via `.agents/skills/bump-version/SKILL.md` (or `make bump VERSION=<target>`).
- Do not create unreleased notes or append incidental task bullet points during routine code modifications.
- Modifications to `CHANGELOG.md` are permitted ONLY when explicitly executing a version bump/release preparation task requested by the operator.

## 6. Request Workflow

When assigned an implementation or maintenance task:

1. **Pre-Check**: Run `git status --short` to verify clean working tree.
2. **Consult Guidelines**: Inspect `.agents/skills/use-modern-go/SKILL.md` and run the CLI wrapper for the file/version being worked on.
3. **Review Requirements**: Review acceptance criteria, architecture contracts, and scope constraints before making changes.
4. **Implement Small & Focused**: Make targeted, minimal changes. Include necessary English docstrings on exported types and protocol quirks.
5. **Run Checks**: Execute unit tests with race detection (`go test -v -race ./...`) and linting.
6. **Verify Diff & Evidence**: Inspect `git diff --check` and verify execution evidence against quality gates.

## 7. Testing & Verification Matrix

| Change Surface                                                       | Minimum Safe Check                                                                                                                                 |
| :------------------------------------------------------------------- | :------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/config`                                                    | `go test -v -race ./internal/config/...`                                                                                                           |
| `internal/auth`                                                      | `go test -v -race ./internal/auth/...`                                                                                                             |
| `internal/rotator`                                                   | `go test -v -race ./internal/rotator/...`                                                                                                          |
| `internal/catalog`                                                   | `go test -v -race ./internal/catalog/...`                                                                                                          |
| `internal/translate`                                                 | `go test -v -race ./internal/translate/...`                                                                                                        |
| `internal/server`                                                    | `go test -v -race ./internal/server/...`                                                                                                           |
| `internal/client`                                                    | `go test -v -race ./internal/client/...`                                                                                                           |
| `internal/updater`                                                   | `go test -v -race ./internal/updater/...`                                                                                                          |
| `test/integration`                                                   | `go test -v -tags=integration ./test/integration/...`                                                                                              |
| `scripts/install.sh`                                                 | `make test-installer`                                                                                                                              |
| Platform-specific code (`*_windows.go`, `*_unix.go`, platform tests) | `GOOS=windows GOARCH=amd64 go vet ./...` and `GOOS=windows GOARCH=amd64 go test -c` for touched packages, so POSIX-only assumptions never reach CI |
| Full Repository                                                      | `go test -v -race ./... && go vet ./...`                                                                                                           |
| Full Preflight (Make)                                                | `make check` (or `make test-all`)                                                                                                                  |
| Release Packaging                                                    | `make release` (cross-platform matrix + SHA256SUMS)                                                                                                |
| CLI Binary Build                                                     | `go build -o bin/agw ./cmd/agw && ./bin/agw version`                                                                                               |

## 8. Stop Conditions & Escalation

Stop and consult the operator immediately if:

- A change requires modifying files outside repository root (e.g. mutating live user configurations or `~/.agw` during automated test runs without using isolated test directories).
- Upstream Google endpoint schemas change drastically, breaking core envelope structures.
- Tests encounter unexpected race conditions in concurrency primitives.
- An action risks exposing raw credentials or secrets in logs, terminal output, or commits.

## 9. Completion Checklist

Before reporting completion on any task:

- [ ] Read and applied `.agents/skills/use-modern-go/SKILL.md` guidelines.
- [ ] Every Go source file has a formal package doc comment (`// Package <pkg> ...`).
- [ ] 100% of exported symbols (types, structs, interfaces, methods, functions, consts, vars) have English doc comments starting with the identifier name.
- [ ] 100% of struct fields (in config, request/response models, states, options) are documented with explanatory comments.
- [ ] 100% of unexported internal helper functions have descriptive comments explaining purpose, parameters, and algorithms.
- [ ] 100% of test functions have top-level comments documenting scenarios, preconditions, and assertions.
- [ ] Upstream protocol quirks and workarounds are thoroughly explained in English doc comments.
- [ ] `CHANGELOG.md` was NOT modified (unless this task is an explicit version bump / release preparation).
- [ ] Test fixtures are placed under `testdata/` and tracked in Git (no dependency on untracked external directories).
- [ ] No raw tokens, keys, or credentials leaked into logs, code, or outputs.
- [ ] All targeted unit/integration tests pass with `-race` enabled.
- [ ] `go vet ./...` reports zero issues.
- [ ] `git diff --check` shows no whitespace or formatting errors.
- [ ] README/docs links resolve, and no documented command, flag, endpoint, or default contradicts the code.
- [ ] Acceptance criteria satisfied and verified with truthful execution evidence.
