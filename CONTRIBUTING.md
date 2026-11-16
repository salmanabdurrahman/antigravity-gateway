# Contributing to Antigravity Gateway

Thank you for your interest in contributing to Antigravity Gateway (`agw`)! This document provides guidelines and workflows for contributing to the repository.

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Prerequisites](#prerequisites)
- [Getting Started](#getting-started)
- [Development Workflow](#development-workflow)
- [Engineering Standards](#engineering-standards)
- [Testing & Verification](#testing--verification)
- [Pull Request Process](#pull-request-process)

## Code of Conduct

This project adheres to the Contributor Covenant. By participating, you are expected to uphold this code. Please read [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) for details.

## Prerequisites

- **Go**: Version 1.24 or later.
- **Git**: Version 2.20 or later.
- **Make**: Standard GNU Make or compatible build utility.

## Getting Started

1. **Fork and Clone**:

   Clone the repository directly (or fork on GitHub and clone your fork):

   ```bash
   git clone https://github.com/salmanabdurrahman/antigravity-gateway.git
   cd antigravity-gateway
   ```

2. **Install Git Hooks**:
   Install the native pre-commit hook to verify formatting, linting, and smoke tests automatically before every commit:

   ```bash
   make hooks-install
   ```

3. **Verify Build & Test Suite**:
   ```bash
   make check
   ```

## Development Workflow

1. Create a feature branch from `main`:

   ```bash
   git checkout -b feat/my-new-feature
   ```

2. Implement your changes following the engineering standards below.
3. Run formatting, static analysis, and tests:

   ```bash
   make fmt
   make check
   ```

4. Commit your changes using conventional commit messages (e.g. `feat: ...`, `fix: ...`, `docs: ...`, `test: ...`).

## Engineering Standards

- **Modern Go Idioms**:
  - Target **Go 1.24+**.
  - Use `any` instead of `interface{}`.
  - Use standard library slices and maps packages (`slices.Contains`, `maps.Clone`, etc.) instead of manual loops.
  - Use `net/http` method-aware ServeMux patterns (e.g. `mux.HandleFunc("GET /health", ...)`).
  - Use Go 1.24+ `omitzero` struct tags on boolean, numeric, and struct fields where zero-values should be omitted from JSON.
  - Synchronize shared mutable states using `sync.RWMutex` or typed atomics (`atomic.Bool`, `atomic.Int64`).
- **Documentation Coverage**:
  - All package declarations must begin with a package doc comment (`// Package <pkg> ...`).
  - All exported identifiers (types, structs, methods, functions, constants) must have English doc comments starting with the identifier name.
  - Struct fields in public or state models must be documented.
  - Test functions must include comments documenting scenarios, preconditions, and assertions.
  - Any Google Cloud Code Assist upstream quirks or protocol translations must be thoroughly explained with inline reasoning.
- **Security & Hygiene**:
  - **Loopback Enforcement**: Proxy and callback servers must bind strictly to `127.0.0.1`. Never default to `0.0.0.0`.
  - **POSIX Permissions**: All config and account files must be written with `0600` permissions (`0700` for directory).
  - **Atomic Writes**: Always write files using atomic temp-file rename patterns to prevent corruption.
  - **Zero Secret Exposure**: Never log access tokens, refresh tokens, bearer API keys, or raw authorization headers.
  - **Isolated Tests**: Unit tests must never mutate live user directories (`~/.agw`) or require external network connectivity. Place static fixtures under `testdata/`.

## Testing & Verification

Antigravity Gateway maintains a high standard of automated verification:

| Command                 | Description                                                    |
| :---------------------- | :------------------------------------------------------------- |
| `make fmt`              | Automatically format Go source files via `gofmt`               |
| `make fmt-check`        | Verify all files follow Go formatting conventions              |
| `make lint`             | Run static analysis with `go vet`                              |
| `make test`             | Run unit tests with race detection (`go test -v -race ./...`)  |
| `make test-integration` | Run end-to-end integration tests (multi-account failover)      |
| `make test-installer`   | Run automated verification test suite for `scripts/install.sh` |
| `make test-all`         | Run all unit, integration, and installer tests                 |
| `make check`            | Run preflight verification (fmt-check, lint, test, installer)  |

## Pull Request Process

1. Ensure `make check` passes completely without errors or warnings.
2. Ensure `git diff --check` reports zero whitespace or newline errors.
3. Keep pull requests focused on a single responsibility. Large architectural changes should be preceded by an issue discussion.
4. Fill out the pull request template completely, including testing evidence.
