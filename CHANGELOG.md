# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.0] - 2026-11-16

### Added

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

[Unreleased]: https://github.com/salmanabdurrahman/antigravity-gateway/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/salmanabdurrahman/antigravity-gateway/releases/tag/v0.1.0
