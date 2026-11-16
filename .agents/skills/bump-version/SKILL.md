---
name: bump-version
description: Bump semantic version, update CHANGELOG.md, synchronize version strings across codebase, and verify release artifacts for Antigravity Gateway.
---

# Bump Version Skill

This skill guides the release process for Antigravity Gateway (`agw`): updating version numbers, logging changes in `CHANGELOG.md`, and verifying the build.

## When to Use

- Bumping patch, minor, or major versions (e.g. `0.1.0` -> `0.1.1` or `0.2.0`).
- Preparing a release and generating changelog notes.

## Version Decision Guide

Follow Semantic Versioning (`MAJOR.MINOR.PATCH`) to decide the next version:

| Release Type | Version Target         | Criteria & Conditions                                                                                                                | Examples in `agw`                                                                                                                                                                                                                                                                     |
| :----------- | :--------------------- | :----------------------------------------------------------------------------------------------------------------------------------- | :------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **Patch**    | `0.1.0` &rarr; `0.1.1` | **Backward-compatible bug fixes** and maintenance.<br>No new CLI commands, flags, or configuration fields.                           | - Fix upstream SSE parser edge-cases or chunk boundaries.<br>- Fix 429 backoff / reset delay calculation.<br>- Fix thought signature validation quirks.<br>- Patching installer scripts (`install.sh`, `install.ps1`) or documentation.                                               |
| **Minor**    | `0.1.0` &rarr; `0.2.0` | **New backward-compatible features**.<br>Existing configs and API calls continue working without changes.                            | - New CLI command or flag (e.g. `agw accounts export`, `--timeout`).<br>- New client auto-config injector (e.g. `agw setup cursor`).<br>- New rotation strategy (e.g. priority-weighted pool).<br>- Support for new upstream capabilities (e.g. search grounding, multimodal inputs). |
| **Major**    | `0.1.0` &rarr; `1.0.0` | **Breaking changes** or reaching **production-ready maturity**.<br>Old configs, CLI flags, or API requests are no longer compatible. | - Breaking format change in `~/.agw/accounts.json` requiring migration.<br>- Incompatible changes to `/v1/chat/completions` or `/v1/models` contracts.<br>- Removing deprecated flags or subcommands.<br>- First production-stable milestone declaration (`v1.0.0`).                  |

## Files Touched

| File                            | Purpose                                          |
| :------------------------------ | :----------------------------------------------- |
| `internal/config/config.go`     | Runtime version constant (`var Version = "..."`) |
| `CHANGELOG.md`                  | Release history following Keep a Changelog       |
| `scripts/build-release.sh`      | Fallback default version for packaging           |
| `.github/workflows/release.yml` | Default input for GitHub Actions workflow        |

## Step-by-Step Workflow

### 1. Collect Changes & Update CHANGELOG.md

Review git history since the previous release:

```bash
git log $(git describe --tags --abbrev=0 2>/dev/null || echo "HEAD~10")..HEAD --oneline
```

In `CHANGELOG.md`, add notable items under standard categories:

- `### Added` (new features, commands)
- `### Changed` (changes to existing behavior)
- `### Fixed` (bug fixes)
- `### Security` (auth/token/permission fixes)

### 2. Synchronize Version Strings

Run the automated bump script via `make`:

```bash
make bump VERSION=<target-version>
# Example:
make bump VERSION=0.1.1
```

The script automatically:

- Updates `internal/config/config.go`.
- Updates `scripts/build-release.sh` and `.github/workflows/release.yml`.
- Moves unreleased notes to `## [<target-version>] - YYYY-MM-DD` in `CHANGELOG.md` and updates GitHub compare links.
- Compiles `bin/agw` and verifies that `./bin/agw version` matches the new version.

### 3. Verify

Run the full preflight check to ensure nothing is broken:

```bash
make check
```

For major changes or protocol updates, also run integration tests:

```bash
make test-integration
```

### 4. Commit & Tag

Stage the updated files and commit:

```bash
git add internal/config/config.go scripts/build-release.sh .github/workflows/release.yml CHANGELOG.md
git commit -m "chore(release): bump version to <target-version>"
```

Always ask the user before creating or pushing git tags:

```bash
git tag -a "v<target-version>" -m "release: v<target-version>"
git push origin main
git push origin "v<target-version>"
```
