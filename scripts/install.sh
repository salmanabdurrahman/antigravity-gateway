#!/usr/bin/env sh
# scripts/install.sh
# Checksum-verifying, architecture-detecting, user-local installer for Antigravity Gateway (agw).
#
# Usage:
#   ./scripts/install.sh --version <version> [--install-dir <dir>] [--repo <owner/repo>] [--force]
#
# Invariants:
#   - Explicit version required (no unpinned 'latest' / silent tag updates).
#   - Validates platform (OS/Arch) before fetching.
#   - Fetches and verifies SHA-256 checksums from companion SHA256SUMS file before extracting.
#   - Installs to user-local directory (default: ~/.local/bin) without requiring root/sudo.
#   - Does not overwrite an existing unmanaged binary unless --force is provided.
#   - Reversible / safe uninstall instructions printed upon completion.

set -eu

VERSION=""
INSTALL_DIR="${AGW_INSTALL_DIR:-${HOME}/.local/bin}"
REPO="${AGW_GITHUB_REPO:-salmanabdurrahman/antigravity-gateway}"
FORCE=0
CUSTOM_DIST_URL="${AGW_DIST_URL:-}"
DRY_RUN=0

usage() {
  cat <<EOF
Antigravity Gateway (agw) Installer

Usage:
  install.sh --version <vX.Y.Z|X.Y.Z> [options]

Required:
  --version <version>       Exact semantic version to install (e.g. 0.1.0 or v0.1.0)

Options:
  --install-dir <path>      Directory to place the agw binary (default: ~/.local/bin or \$AGW_INSTALL_DIR)
  --repo <owner/repo>       GitHub repository hosting releases (default: salmanabdurrahman/antigravity-gateway)
  --dist-url <url_or_dir>   Custom base URL or local directory containing release archives and SHA256SUMS
  --force                   Overwrite existing binary if present
  --dry-run                 Simulate installation checks without writing binary to disk
  -h, --help                Show this help message

Environment Variables:
  AGW_INSTALL_DIR           Override destination install directory
  AGW_GITHUB_REPO           Override GitHub repository
  AGW_DIST_URL              Override base URL or local path for downloads
EOF
  exit "${1:-0}"
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version)
      if [ -z "${2:-}" ]; then
        echo "Error: --version requires an argument." >&2
        usage 1
      fi
      VERSION="$2"
      shift 2
      ;;
    --install-dir)
      if [ -z "${2:-}" ]; then
        echo "Error: --install-dir requires an argument." >&2
        usage 1
      fi
      INSTALL_DIR="$2"
      shift 2
      ;;
    --repo)
      if [ -z "${2:-}" ]; then
        echo "Error: --repo requires an argument." >&2
        usage 1
      fi
      REPO="$2"
      shift 2
      ;;
    --dist-url)
      if [ -z "${2:-}" ]; then
        echo "Error: --dist-url requires an argument." >&2
        usage 1
      fi
      CUSTOM_DIST_URL="$2"
      shift 2
      ;;
    --force)
      FORCE=1
      shift
      ;;
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    -h|--help)
      usage 0
      ;;
    *)
      echo "Error: Unknown argument: $1" >&2
      usage 1
      ;;
  esac
done

if [ -z "$VERSION" ]; then
  echo "Error: Explicit version is required. Unpinned 'latest' is forbidden." >&2
  echo "Specify --version <version> (e.g. --version 0.1.0)." >&2
  exit 1
fi

# Clean version (strip leading 'v' if present)
CLEAN_VERSION="${VERSION#v}"

# 1. Detect OS
RAW_OS="$(uname -s)"
case "$RAW_OS" in
  Darwin)
    OS="darwin"
    EXT="tar.gz"
    ;;
  Linux)
    OS="linux"
    EXT="tar.gz"
    ;;
  MSYS*|MINGW*|CYGWIN*)
    OS="windows"
    EXT="zip"
    ;;
  *)
    echo "Error: Unsupported operating system '$RAW_OS'." >&2
    echo "Supported operating systems: macOS (Darwin), Linux, Windows." >&2
    exit 1
    ;;
esac

# 2. Detect Architecture
RAW_ARCH="$(uname -m)"
case "$RAW_ARCH" in
  x86_64|amd64)
    ARCH="amd64"
    ;;
  arm64|aarch64)
    ARCH="arm64"
    ;;
  *)
    echo "Error: Unsupported hardware architecture '$RAW_ARCH'." >&2
    echo "Supported architectures: amd64 (x86_64), arm64 (Apple Silicon / aarch64)." >&2
    exit 1
    ;;
esac

ARCHIVE_NAME="agw_${CLEAN_VERSION}_${OS}_${ARCH}.${EXT}"
BIN_NAME="agw"
if [ "$OS" = "windows" ]; then
  BIN_NAME="agw.exe"
fi
TARGET_BIN="${INSTALL_DIR}/${BIN_NAME}"

# 3. Check existing binary and safety
if [ -f "$TARGET_BIN" ] && [ "$FORCE" -ne 1 ]; then
  echo "Stop: Destination binary already exists at ${TARGET_BIN}." >&2
  echo "To overwrite, pass the --force flag." >&2
  exit 1
fi

# 4. Check downloader utility
DOWNLOADER=""
if command -v curl >/dev/null 2>&1; then
  DOWNLOADER="curl"
elif command -v wget >/dev/null 2>&1; then
  DOWNLOADER="wget"
elif [ -z "$CUSTOM_DIST_URL" ] || [ ! -d "$CUSTOM_DIST_URL" ]; then
  echo "Error: Neither 'curl' nor 'wget' is available for downloading release artifacts." >&2
  exit 1
fi

download_file() {
  _src="$1"
  _dst="$2"

  # Support local directory or file:// URL
  case "$_src" in
    file://*)
      _local_path="${_src#file://}"
      cp "$_local_path" "$_dst"
      return 0
      ;;
    *)
      if [ -f "$_src" ]; then
        cp "$_src" "$_dst"
        return 0
      fi
      ;;
  esac

  if [ "$DOWNLOADER" = "curl" ]; then
    curl -fsSL --retry 2 "$_src" -o "$_dst"
  elif [ "$DOWNLOADER" = "wget" ]; then
    wget -q --tries=2 "$_src" -O "$_dst"
  else
    echo "Error: No utility to fetch $_src" >&2
    return 1
  fi
}

# 5. Check SHA-256 utility
compute_sha256() {
  _file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$_file" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$_file" | awk '{print $1}'
  else
    echo "Error: Neither 'sha256sum' nor 'shasum' is available for checksum verification." >&2
    exit 1
  fi
}

# Prepare workspace
TMP_DIR="$(mktemp -d 2>/dev/null || mktemp -d -t 'agw-install')"
cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT INT TERM

ARCHIVE_PATH="${TMP_DIR}/${ARCHIVE_NAME}"
CHECKSUMS_PATH="${TMP_DIR}/SHA256SUMS"

# Determine download URLs
if [ -n "$CUSTOM_DIST_URL" ]; then
  BASE_URL="${CUSTOM_DIST_URL%/}"
else
  BASE_URL="https://github.com/${REPO}/releases/download/v${CLEAN_VERSION}"
fi

ARCHIVE_URL="${BASE_URL}/${ARCHIVE_NAME}"
CHECKSUMS_URL="${BASE_URL}/SHA256SUMS"

echo "--> Preparing to install Antigravity Gateway (agw) v${CLEAN_VERSION} for ${OS}/${ARCH}"

# 6. Fetch SHA256SUMS
echo "--> Fetching checksums from ${CHECKSUMS_URL}..."
if ! download_file "$CHECKSUMS_URL" "$CHECKSUMS_PATH"; then
  echo "Error: Failed to download SHA256SUMS from ${CHECKSUMS_URL}." >&2
  exit 1
fi

EXPECTED_HASH="$(grep " ${ARCHIVE_NAME}\$" "$CHECKSUMS_PATH" | awk '{print $1}' | head -n1 || true)"
if [ -z "$EXPECTED_HASH" ]; then
  # Try matching without leading space or tab
  EXPECTED_HASH="$(awk -v arch="$ARCHIVE_NAME" '$2 == arch {print $1}' "$CHECKSUMS_PATH" | head -n1 || true)"
fi

if [ -z "$EXPECTED_HASH" ]; then
  echo "Error: Archive '${ARCHIVE_NAME}' not found in SHA256SUMS." >&2
  exit 1
fi

# 7. Fetch Archive
echo "--> Downloading release archive ${ARCHIVE_NAME}..."
if ! download_file "$ARCHIVE_URL" "$ARCHIVE_PATH"; then
  echo "Error: Failed to download archive from ${ARCHIVE_URL}." >&2
  exit 1
fi

# 8. Verify Checksum
echo "--> Verifying SHA-256 cryptographic digest..."
ACTUAL_HASH="$(compute_sha256 "$ARCHIVE_PATH")"

if [ "$ACTUAL_HASH" != "$EXPECTED_HASH" ]; then
  echo "Error: Cryptographic checksum verification failed!" >&2
  echo "  Expected: ${EXPECTED_HASH}" >&2
  echo "  Actual:   ${ACTUAL_HASH}" >&2
  echo "Installation aborted. The downloaded archive may be corrupted or tampered with." >&2
  exit 1
fi

echo "--> Checksum verified: ${ACTUAL_HASH}"

if [ "$DRY_RUN" -eq 1 ]; then
  echo "[dry-run] Verification succeeded for ${ARCHIVE_NAME}."
  echo "[dry-run] Binary would be installed to: ${TARGET_BIN}"
  exit 0
fi

# 9. Extract binary
STAGE_DIR="${TMP_DIR}/extracted"
mkdir -p "$STAGE_DIR"

echo "--> Extracting ${ARCHIVE_NAME}..."
if [ "$EXT" = "zip" ]; then
  if ! command -v unzip >/dev/null 2>&1; then
    echo "Error: 'unzip' utility is required for Windows release archives." >&2
    exit 1
  fi
  unzip -q -o "$ARCHIVE_PATH" -d "$STAGE_DIR"
else
  tar -xzf "$ARCHIVE_PATH" -C "$STAGE_DIR"
fi

EXTRACTED_BIN="${STAGE_DIR}/${BIN_NAME}"
if [ ! -f "$EXTRACTED_BIN" ]; then
  echo "Error: Extracted archive did not contain binary '${BIN_NAME}'." >&2
  exit 1
fi

# 10. Install binary
mkdir -p "$INSTALL_DIR"
cp "$EXTRACTED_BIN" "${TARGET_BIN}.tmp.$$"
chmod +x "${TARGET_BIN}.tmp.$$"
mv -f "${TARGET_BIN}.tmp.$$" "$TARGET_BIN"

echo ""
echo "================================================================================"
echo "  Antigravity Gateway (agw) v${CLEAN_VERSION} installed successfully!"
echo "================================================================================"
echo "Binary location: ${TARGET_BIN}"

# Check if INSTALL_DIR is in PATH
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*)
    ;;
  *)
    echo ""
    echo "Note: '${INSTALL_DIR}' is not currently in your system PATH."
    echo "Add it to your shell configuration file (~/.bashrc, ~/.zshrc):"
    echo "  export PATH=\"${INSTALL_DIR}:\$PATH\""
    ;;
esac

echo ""
echo "Next steps:"
echo "  1. Authenticate with Google:  agw login"
echo "  2. Start local proxy daemon:   agw start --detach"
echo "  3. Configure coding agent:     agw setup pi   (or agw setup opencode)"
echo ""
echo "To uninstall:"
echo "  1. Stop daemon:                agw stop"
echo "  2. Remove binary:              rm -f \"${TARGET_BIN}\""
echo "  3. Remove local state/tokens:  rm -rf \"\${AGW_HOME:-\$HOME/.agw}\""
echo "================================================================================"
