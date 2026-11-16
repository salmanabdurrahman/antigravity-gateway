#!/usr/bin/env sh
# scripts/bump-version.sh
# Synchronously bumps version strings across Antigravity Gateway (agw) files,
# updates CHANGELOG.md, and optionally commits the release preparation.
#
# Usage:
#   ./scripts/bump-version.sh <version> [--commit] [--dry-run]
#
# Examples:
#   ./scripts/bump-version.sh 0.1.1
#   ./scripts/bump-version.sh v0.2.0 --commit

set -eu

DO_COMMIT=0
DRY_RUN=0
TARGET_VERSION=""

while [ $# -gt 0 ]; do
  case "$1" in
    --commit)
      DO_COMMIT=1
      shift
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      echo "Usage: $0 <new-version> [--commit] [--dry-run]"
      exit 0
      ;;
    -*)
      echo "ERROR: Unknown option '$1'" >&2
      exit 1
      ;;
    *)
      if [ -z "$TARGET_VERSION" ]; then
        TARGET_VERSION="$1"
      else
        echo "ERROR: Unexpected argument '$1'" >&2
        exit 1
      fi
      shift
      ;;
  esac
done

if [ -z "$TARGET_VERSION" ]; then
  echo "ERROR: Target version is required (e.g. $0 0.1.1)" >&2
  exit 1
fi

# Strip optional leading 'v'
NEW_VER="${TARGET_VERSION#v}"

# Validate semantic version format (e.g. 1.2.3 or 1.2.3-rc1)
case "$NEW_VER" in
  *[!0-9A-Za-z.-]*)
    echo "ERROR: Version '$NEW_VER' contains invalid characters." >&2
    exit 1
    ;;
esac

if ! echo "$NEW_VER" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
  echo "ERROR: Version '$NEW_VER' does not follow semantic versioning (X.Y.Z)." >&2
  exit 1
fi

# Detect current version from internal/config/config.go
CONFIG_FILE="internal/config/config.go"
if [ ! -f "$CONFIG_FILE" ]; then
  echo "ERROR: Cannot find $CONFIG_FILE. Must run from repository root." >&2
  exit 1
fi

CURRENT_VER=$(grep -E '^var Version = "[^"]+"' "$CONFIG_FILE" | sed -E 's/var Version = "([^"]+)"/\1/')
if [ -z "$CURRENT_VER" ]; then
  echo "ERROR: Failed to extract current version from $CONFIG_FILE." >&2
  exit 1
fi

if [ "$NEW_VER" = "$CURRENT_VER" ]; then
  echo "ERROR: Target version ($NEW_VER) is identical to current version ($CURRENT_VER)." >&2
  exit 1
fi

echo "--> Bumping version: $CURRENT_VER -> $NEW_VER"

if [ "$DRY_RUN" -eq 1 ]; then
  echo "--> DRY-RUN enabled. The following files would be updated:"
  echo "  - $CONFIG_FILE"
  echo "  - scripts/build-release.sh"
  echo "  - .github/workflows/release.yml"
  echo "  - CHANGELOG.md"
  exit 0
fi

# 1. Update internal/config/config.go
sed -i.bak -E "s/var Version = \"$CURRENT_VER\"/var Version = \"$NEW_VER\"/" "$CONFIG_FILE"
rm -f "${CONFIG_FILE}.bak"

# 2. Update scripts/build-release.sh fallback
BUILD_SCRIPT="scripts/build-release.sh"
if [ -f "$BUILD_SCRIPT" ]; then
  sed -i.bak -E "s/echo \"$CURRENT_VER\"/echo \"$NEW_VER\"/g" "$BUILD_SCRIPT"
  sed -i.bak -E "s/VERSION_INPUT=\"$CURRENT_VER\"/VERSION_INPUT=\"$NEW_VER\"/g" "$BUILD_SCRIPT"
  sed -i.bak -E "s/fallback to $CURRENT_VER/fallback to $NEW_VER/g" "$BUILD_SCRIPT"
  rm -f "${BUILD_SCRIPT}.bak"
fi

# 3. Update .github/workflows/release.yml default version
WORKFLOW_FILE=".github/workflows/release.yml"
if [ -f "$WORKFLOW_FILE" ]; then
  sed -i.bak -E "s/default: 'v$CURRENT_VER'/default: 'v$NEW_VER'/" "$WORKFLOW_FILE"
  rm -f "${WORKFLOW_FILE}.bak"
fi

# 4. Update CHANGELOG.md
CHANGELOG_FILE="CHANGELOG.md"
if [ -f "$CHANGELOG_FILE" ]; then
  RELEASE_DATE=$(date +%Y-%m-%d)
  # Check if section for new version already exists
  if ! grep -Fq "## [$NEW_VER]" "$CHANGELOG_FILE"; then
    # Insert new version heading right under ## [Unreleased]
    TMP_CHANGELOG=$(mktemp)
    awk -v ver="$NEW_VER" -v rdate="$RELEASE_DATE" '
      /## \[Unreleased\]/ {
        print $0;
        print "";
        print "## [" ver "] - " rdate;
        next;
      }
      { print }
    ' "$CHANGELOG_FILE" > "$TMP_CHANGELOG"
    mv "$TMP_CHANGELOG" "$CHANGELOG_FILE"

    # Update compare URLs at bottom of CHANGELOG
    REPO_URL="https://github.com/salmanabdurrahman/antigravity-gateway"
    sed -i.bak -E "s|\[Unreleased\]: $REPO_URL/compare/v$CURRENT_VER\.\.\.HEAD|\[Unreleased\]: $REPO_URL/compare/v$NEW_VER\.\.\.HEAD\n\[$NEW_VER\]: $REPO_URL/compare/v$CURRENT_VER\.\.\.v$NEW_VER|" "$CHANGELOG_FILE"
    rm -f "${CHANGELOG_FILE}.bak"
  fi
fi

# 5. Build and verify version
echo "--> Verifying compiled binary output..."
go build -o bin/agw ./cmd/agw
BIN_VER=$(./bin/agw version)
if [ "$BIN_VER" != "agw version $NEW_VER" ]; then
  echo "ERROR: Binary version mismatch! Expected 'agw version $NEW_VER', got '$BIN_VER'" >&2
  exit 1
fi
echo "--> Successfully verified binary: $BIN_VER"

# 6. Commit if requested
if [ "$DO_COMMIT" -eq 1 ]; then
  echo "--> Staging changes and creating git commit..."
  git add "$CONFIG_FILE" "$BUILD_SCRIPT" "$WORKFLOW_FILE" "$CHANGELOG_FILE"
  git commit -m "chore(release): bump version to $NEW_VER"
  echo "--> Release commit created for v$NEW_VER"
fi

echo "================================================================================"
echo "  Version bump to $NEW_VER completed successfully!"
echo "================================================================================"
