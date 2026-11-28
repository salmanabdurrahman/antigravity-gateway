#!/usr/bin/env bash
# scripts/build-release.sh
# Builds cross-platform release archives and generates companion SHA256 checksums.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
DIST_DIR="${ROOT_DIR}/dist"

# Validate required build utilities upfront
for tool in go tar zip; do
    if ! command -v "$tool" > /dev/null 2>&1; then
        echo "Error: Required utility '$tool' is not installed." >&2
        exit 1
    fi
done

if ! command -v sha256sum > /dev/null 2>&1 && ! command -v shasum > /dev/null 2>&1; then
    echo "Error: Neither 'sha256sum' nor 'shasum' is available for checksum generation." >&2
    exit 1
fi

# Determine release version
VERSION_INPUT="${1:-${VERSION:-}}"
if [ -z "$VERSION_INPUT" ]; then
    # Fallback to git tag if present, else fallback to 0.4.0
    if git rev-parse --git-dir > /dev/null 2>&1; then
        VERSION_INPUT=$(git describe --tags --exact-match 2>/dev/null || echo "0.4.0")
    else
        VERSION_INPUT="0.4.0"
    fi
fi

# Clean version for filenames (strip leading 'v' if present)
CLEAN_VERSION="${VERSION_INPUT#v}"

echo "=================================================="
echo " Building Antigravity Gateway (agw) v${CLEAN_VERSION}"
echo "=================================================="

# Ensure clean dist directory
rm -rf "${DIST_DIR}"
mkdir -p "${DIST_DIR}"

PLATFORMS=(
    "darwin/arm64"
    "darwin/amd64"
    "linux/amd64"
    "linux/arm64"
    "windows/amd64"
    "windows/arm64"
)

LDFLAGS="-s -w -X github.com/salmanabdurrahman/antigravity-gateway/internal/config.Version=${CLEAN_VERSION}"

for target in "${PLATFORMS[@]}"; do
    OS="${target%/*}"
    ARCH="${target#*/}"

    echo "--> Compiling for ${OS}/${ARCH}..."

    STAGE_DIR="${DIST_DIR}/stage_${OS}_${ARCH}"
    rm -rf "${STAGE_DIR}"
    mkdir -p "${STAGE_DIR}"

    BIN_NAME="agw"
    if [ "${OS}" = "windows" ]; then
        BIN_NAME="agw.exe"
    fi

    # Build static binary
    (
        cd "${ROOT_DIR}"
        CGO_ENABLED=0 GOOS="${OS}" GOARCH="${ARCH}" go build \
            -trimpath \
            -ldflags "${LDFLAGS}" \
            -o "${STAGE_DIR}/${BIN_NAME}" \
            ./cmd/agw
    )

    # Include user documentation and license if available
    cp "${ROOT_DIR}/README.md" "${STAGE_DIR}/"
    if [ -f "${ROOT_DIR}/LICENSE" ]; then
        cp "${ROOT_DIR}/LICENSE" "${STAGE_DIR}/"
    fi

    # Package archive
    if [ "${OS}" = "windows" ]; then
        ARCHIVE_NAME="agw_${CLEAN_VERSION}_${OS}_${ARCH}.zip"
        (
            cd "${STAGE_DIR}"
            zip -q -9 "${DIST_DIR}/${ARCHIVE_NAME}" *
        )
    else
        ARCHIVE_NAME="agw_${CLEAN_VERSION}_${OS}_${ARCH}.tar.gz"
        (
            cd "${STAGE_DIR}"
            tar -czf "${DIST_DIR}/${ARCHIVE_NAME}" *
        )
    fi

    rm -rf "${STAGE_DIR}"
    echo "    Created ${ARCHIVE_NAME}"
done

# Generate SHA256 checksums
echo "--> Generating SHA256SUMS..."
(
    cd "${DIST_DIR}"
    if command -v sha256sum > /dev/null 2>&1; then
        sha256sum agw_* > SHA256SUMS
    elif command -v shasum > /dev/null 2>&1; then
        shasum -a 256 agw_* > SHA256SUMS
    else
        echo "Error: Neither sha256sum nor shasum found." >&2
        exit 1
    fi
)

echo "=================================================="
echo " Artifacts built successfully in dist/:"
echo "=================================================="
ls -lh "${DIST_DIR}"
echo ""
echo "Checksums (dist/SHA256SUMS):"
cat "${DIST_DIR}/SHA256SUMS"
