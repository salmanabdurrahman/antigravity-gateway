#!/usr/bin/env sh
# scripts/install.sh
# Checksum-verifying, architecture-detecting, user-local installer for Antigravity Gateway (agw).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/salmanabdurrahman/antigravity-gateway/main/scripts/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/salmanabdurrahman/antigravity-gateway/main/scripts/install.sh | sh -s -- v0.1.0
#   ./scripts/install.sh [vX.Y.Z] [--version <version>] [--install-dir <dir>] [--repo <owner/repo>] [--force]
#
# Invariants:
#   - Pinned semantic version or automatic latest release resolution.
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
  install.sh [version] [options]
  install.sh --version <vX.Y.Z|X.Y.Z> [options]

Arguments:
  version                   Semantic version to install (optional, defaults to latest release)

Options:
  --version <version>       Exact semantic version to install (e.g. 0.1.0 or v0.1.0)
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
    -*)
      echo "Error: Unknown argument: $1" >&2
      usage 1
      ;;
    *)
      if [ -z "$VERSION" ]; then
        VERSION="$1"
        shift
      else
        echo "Error: Unexpected argument: $1" >&2
        usage 1
      fi
      ;;
  esac
done

# 1. Check downloader utility early
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
    # Remote downloads are pinned to HTTPS so a redirected or spoofed URL cannot downgrade the
    # checksum-signing transport. Local directory and file:// sources bypass curl entirely above.
    case "$_src" in
      https://*)
        curl -fsSL --proto '=https' --retry 2 "$_src" -o "$_dst"
        ;;
      http://*)
        echo "Error: Refusing insecure HTTP download from $_src" >&2
        return 1
        ;;
      *)
        curl -fsSL --retry 2 "$_src" -o "$_dst"
        ;;
    esac
  elif [ "$DOWNLOADER" = "wget" ]; then
    case "$_src" in
      http://*)
        echo "Error: Refusing insecure HTTP download from $_src" >&2
        return 1
        ;;
      *)
        wget -q --tries=2 "$_src" -O "$_dst"
        ;;
    esac
  else
    echo "Error: No utility to fetch $_src" >&2
    return 1
  fi
}

# 2. Resolve version (if not specified, auto-detect latest release)
if [ -z "$VERSION" ]; then
  echo "--> No version specified. Resolving latest release for ${REPO}..."
  TAG_NAME=""

  # Try HTTP redirect resolution first (immune to unauthenticated GitHub API rate limits)
  if [ "$DOWNLOADER" = "curl" ]; then
    LATEST_REDIRECT="$(curl -fsSLI --proto '=https' -o /dev/null -w "%{url_effective}" "https://github.com/${REPO}/releases/latest" 2>/dev/null || true)"
    if [ -n "$LATEST_REDIRECT" ]; then
      CANDIDATE="${LATEST_REDIRECT##*/}"
      if [ -n "$CANDIDATE" ] && [ "$CANDIDATE" != "latest" ]; then
        TAG_NAME="$CANDIDATE"
      fi
    fi
  fi

  # Fallback to GitHub REST API if redirect did not resolve to a tag
  if [ -z "$TAG_NAME" ]; then
    if [ "$DOWNLOADER" = "curl" ]; then
      TAG_NAME="$(curl -fsSL --proto '=https' "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | grep '"tag_name":' | head -n1 | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' || true)"
    elif [ "$DOWNLOADER" = "wget" ]; then
      TAG_NAME="$(wget -qO- "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null | grep '"tag_name":' | head -n1 | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' || true)"
    fi
  fi

  if [ -n "$TAG_NAME" ] && [ "$TAG_NAME" != "latest" ]; then
    VERSION="$TAG_NAME"
    echo "--> Detected latest release: ${VERSION}"
  else
    echo "Error: Could not determine latest release version for ${REPO}." >&2
    echo "Specify version explicitly: --version <version> (e.g. --version 0.1.0)." >&2
    exit 1
  fi
fi

# Clean version (strip leading 'v' if present)
CLEAN_VERSION="${VERSION#v}"

# Reject anything that is not a strict semantic version. The value is interpolated into archive
# filenames and download paths, so an unvalidated tag containing '/', '..', or whitespace could escape
# the staging directory or fetch an unrelated asset.
case "$CLEAN_VERSION" in
  *[!0-9A-Za-z.+-]*|[!0-9]*)
    echo "Error: Invalid version '$VERSION': expected a semantic version such as 0.1.0 or v0.1.0." >&2
    exit 1
    ;;
esac
if ! printf '%s' "$CLEAN_VERSION" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$'; then
  echo "Error: Invalid version '$VERSION': expected a semantic version such as 0.1.0 or v0.1.0." >&2
  exit 1
fi

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
INSTALL_TMP=""
cleanup() {
  rm -rf "$TMP_DIR"
  if [ -n "$INSTALL_TMP" ]; then
    rm -f "$INSTALL_TMP" 2>/dev/null || true
  fi
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

EXPECTED_HASH="$(tr -d '\r' < "$CHECKSUMS_PATH" | awk -v arch="$ARCHIVE_NAME" '$2 == arch || $2 == "*"arch {print $1}' | head -n1 || true)"
if [ -z "$EXPECTED_HASH" ]; then
  # Fallback matching with grep
  EXPECTED_HASH="$(tr -d '\r' < "$CHECKSUMS_PATH" | grep " ${ARCHIVE_NAME}\$" | awk '{print $1}' | head -n1 || true)"
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
if [ -L "$EXTRACTED_BIN" ]; then
  echo "Error: Extracted '${BIN_NAME}' is a symbolic link; refusing to install a link target." >&2
  exit 1
fi

# 10. Install binary through an exclusively created staging file in the destination directory, so a
# pre-planted symlink at a predictable temp name can never be followed.
mkdir -p "$INSTALL_DIR"
INSTALL_TMP="$(mktemp "${INSTALL_DIR}/.agw.tmp.XXXXXX")"
cp "$EXTRACTED_BIN" "$INSTALL_TMP"
chmod +x "$INSTALL_TMP"
mv -f "$INSTALL_TMP" "$TARGET_BIN"

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
