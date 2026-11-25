#!/usr/bin/env bash
# scripts/test-installer.sh
# Automated verification test suite for scripts/install.sh.
# Tests architecture detection, checksum validation, malicious tamper detection,
# unmanaged binary protection, explicit version enforcement, and local directory installs.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
INSTALLER="${ROOT_DIR}/scripts/install.sh"

echo "=== Running Installer Test Suite ==="

TMP_TEST_DIR="$(mktemp -d 2>/dev/null || mktemp -d -t 'agw-install-test')"
cleanup() {
    rm -rf "$TMP_TEST_DIR"
}
trap cleanup EXIT INT TERM

# 1. Test missing/unresolvable version fails with non-zero exit code
echo "[test 1] unresolvable release fails safely"
if "$INSTALLER" --repo "nonexistent-owner/nonexistent-repo-agw-test" >/dev/null 2>&1; then
    echo "FAIL: Expected installer to fail when repository releases cannot be resolved" >&2
    exit 1
fi
echo "PASS: unresolvable release rejected"

# 2. Build mock release archive and SHA256SUMS in a local directory
MOCK_DIST="${TMP_TEST_DIR}/mock-dist"
mkdir -p "$MOCK_DIST"
MOCK_VERSION="9.9.9"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
    x86_64) ARCH="amd64" ;;
    arm64|aarch64) ARCH="arm64" ;;
esac

MOCK_ARCHIVE_NAME="agw_${MOCK_VERSION}_${OS}_${ARCH}.tar.gz"
MOCK_STAGE="${TMP_TEST_DIR}/stage"
mkdir -p "$MOCK_STAGE"

# Create dummy executable
cat << 'EOF' > "${MOCK_STAGE}/agw"
#!/usr/bin/env sh
echo "agw version 9.9.9-mock"
EOF
chmod +x "${MOCK_STAGE}/agw"

# Archive dummy
tar -czf "${MOCK_DIST}/${MOCK_ARCHIVE_NAME}" -C "$MOCK_STAGE" agw

# Compute checksum
if command -v sha256sum >/dev/null 2>&1; then
    (cd "$MOCK_DIST" && sha256sum "$MOCK_ARCHIVE_NAME" > SHA256SUMS)
else
    (cd "$MOCK_DIST" && shasum -a 256 "$MOCK_ARCHIVE_NAME" > SHA256SUMS)
fi

# 3. Test valid local installation via positional argument and --dist-url
echo "[test 2] valid installation with positional version and verified checksum"
TEST_INSTALL_DIR="${TMP_TEST_DIR}/bin"
"$INSTALLER" \
    "$MOCK_VERSION" \
    --dist-url "$MOCK_DIST" \
    --install-dir "$TEST_INSTALL_DIR"

if [ ! -x "${TEST_INSTALL_DIR}/agw" ]; then
    echo "FAIL: Installed binary not found or not executable" >&2
    exit 1
fi

OUTPUT="$("${TEST_INSTALL_DIR}/agw")"
if [ "$OUTPUT" != "agw version 9.9.9-mock" ]; then
    echo "FAIL: Binary execution output mismatch: $OUTPUT" >&2
    exit 1
fi
echo "PASS: valid installation succeeded"

# 4. Test protection against unmanaged overwrite (without --force)
echo "[test 3] refusal to overwrite existing binary without --force"
if "$INSTALLER" \
    --version "$MOCK_VERSION" \
    --dist-url "$MOCK_DIST" \
    --install-dir "$TEST_INSTALL_DIR" >/dev/null 2>&1; then
    echo "FAIL: Installer unexpectedly overwrote existing binary without --force" >&2
    exit 1
fi
echo "PASS: existing binary protected"

# 5. Test overwrite when --force is supplied
echo "[test 4] overwrite succeeds when --force provided"
"$INSTALLER" \
    --version "$MOCK_VERSION" \
    --dist-url "$MOCK_DIST" \
    --install-dir "$TEST_INSTALL_DIR" \
    --force
echo "PASS: overwrite with --force succeeded"

# 6. Test checksum mismatch detection (corrupted/tampered archive)
echo "[test 5] checksum mismatch rejection"
TAMPER_DIST="${TMP_TEST_DIR}/tamper-dist"
mkdir -p "$TAMPER_DIST"
cp "${MOCK_DIST}/SHA256SUMS" "$TAMPER_DIST/"
# Corrupt archive content
echo "malicious or corrupted data" > "${TAMPER_DIST}/${MOCK_ARCHIVE_NAME}"

TAMPER_INSTALL_DIR="${TMP_TEST_DIR}/bin-tamper"
if "$INSTALLER" \
    --version "$MOCK_VERSION" \
    --dist-url "$TAMPER_DIST" \
    --install-dir "$TAMPER_INSTALL_DIR" >/dev/null 2>&1; then
    echo "FAIL: Installer accepted tampered archive with bad checksum!" >&2
    exit 1
fi

if [ -f "${TAMPER_INSTALL_DIR}/agw" ]; then
    echo "FAIL: Binary was installed despite checksum failure!" >&2
    exit 1
fi
echo "PASS: checksum mismatch safely rejected without installing binary"

# 7. Test missing archive from SHA256SUMS
echo "[test 6] archive missing from SHA256SUMS"
EMPTY_SUMS_DIST="${TMP_TEST_DIR}/empty-sums-dist"
mkdir -p "$EMPTY_SUMS_DIST"
echo "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 other_archive.tar.gz" > "${EMPTY_SUMS_DIST}/SHA256SUMS"
cp "${MOCK_DIST}/${MOCK_ARCHIVE_NAME}" "$EMPTY_SUMS_DIST/"

EMPTY_SUMS_INSTALL="${TMP_TEST_DIR}/bin-empty"
if "$INSTALLER" \
    --version "$MOCK_VERSION" \
    --dist-url "$EMPTY_SUMS_DIST" \
    --install-dir "$EMPTY_SUMS_INSTALL" >/dev/null 2>&1; then
    echo "FAIL: Installer accepted unlisted archive!" >&2
    exit 1
fi
echo "PASS: missing SHA256SUMS entry safely rejected"

# 8. Test CRLF line endings in SHA256SUMS
echo "[test 7] CRLF line endings in SHA256SUMS handled properly"
CRLF_DIST="${TMP_TEST_DIR}/crlf-dist"
mkdir -p "$CRLF_DIST"
awk '{printf "%s\r\n", $0}' "${MOCK_DIST}/SHA256SUMS" > "${CRLF_DIST}/SHA256SUMS"
cp "${MOCK_DIST}/${MOCK_ARCHIVE_NAME}" "$CRLF_DIST/"

CRLF_INSTALL_DIR="${TMP_TEST_DIR}/bin-crlf"
"$INSTALLER" \
    "$MOCK_VERSION" \
    --dist-url "$CRLF_DIST" \
    --install-dir "$CRLF_INSTALL_DIR"
if [ ! -x "${CRLF_INSTALL_DIR}/agw" ]; then
    echo "FAIL: Installation with CRLF SHA256SUMS failed" >&2
    exit 1
fi
echo "PASS: CRLF line endings in SHA256SUMS handled successfully"

# 9. Test pipe execution from stdin (simulates curl ... | sh -s -- <args>)
echo "[test 8] stdin piped execution (curl | sh -s --)"
PIPE_INSTALL_DIR="${TMP_TEST_DIR}/bin-pipe"
cat "$INSTALLER" | sh -s -- \
    "$MOCK_VERSION" \
    --dist-url "$MOCK_DIST" \
    --install-dir "$PIPE_INSTALL_DIR"
if [ ! -x "${PIPE_INSTALL_DIR}/agw" ]; then
    echo "FAIL: Piped stdin installation failed" >&2
    exit 1
fi
OUTPUT="$("${PIPE_INSTALL_DIR}/agw")"
if [ "$OUTPUT" != "agw version 9.9.9-mock" ]; then
    echo "FAIL: Piped binary execution output mismatch: $OUTPUT" >&2
    exit 1
fi
echo "PASS: stdin piped execution succeeded"

# 10. Test 'v'-prefixed version acceptance (tag-style input)
echo "[test 9] v-prefixed version accepted"
VPREFIX_INSTALL_DIR="${TMP_TEST_DIR}/bin-vprefix"
"$INSTALLER" \
    "v${MOCK_VERSION}" \
    --dist-url "$MOCK_DIST" \
    --install-dir "$VPREFIX_INSTALL_DIR"
if [ ! -x "${VPREFIX_INSTALL_DIR}/agw" ]; then
    echo "FAIL: v-prefixed version installation failed" >&2
    exit 1
fi
echo "PASS: v-prefixed version accepted"

# 11. Test strict semver validation
echo "[test 10] invalid version strings rejected"
for bad_version in "1.2" "v1.2.3.4" "../1.2.3" "1.2.3/../../evil" "latest"; do
    if "$INSTALLER" \
        --version "$bad_version" \
        --dist-url "$MOCK_DIST" \
        --install-dir "${TMP_TEST_DIR}/bin-bad-version" >/dev/null 2>&1; then
        echo "FAIL: Installer accepted invalid version '$bad_version'" >&2
        exit 1
    fi
done
echo "PASS: invalid versions rejected"

# 12. Test symlink extraction rejection
echo "[test 11] archive symlink instead of binary rejected"
SYMLINK_DIST="${TMP_TEST_DIR}/symlink-dist"
SYMLINK_STAGE="${TMP_TEST_DIR}/symlink-stage"
mkdir -p "$SYMLINK_DIST" "$SYMLINK_STAGE"
ln -s /etc/passwd "${SYMLINK_STAGE}/agw"
tar -czf "${SYMLINK_DIST}/${MOCK_ARCHIVE_NAME}" -C "$SYMLINK_STAGE" agw
if command -v sha256sum >/dev/null 2>&1; then
    (cd "$SYMLINK_DIST" && sha256sum "$MOCK_ARCHIVE_NAME" > SHA256SUMS)
else
    (cd "$SYMLINK_DIST" && shasum -a 256 "$MOCK_ARCHIVE_NAME" > SHA256SUMS)
fi

SYMLINK_INSTALL_DIR="${TMP_TEST_DIR}/bin-symlink"
if "$INSTALLER" \
    --version "$MOCK_VERSION" \
    --dist-url "$SYMLINK_DIST" \
    --install-dir "$SYMLINK_INSTALL_DIR" >/dev/null 2>&1; then
    echo "FAIL: Installer accepted a symlinked binary entry" >&2
    exit 1
fi
if [ -f "${SYMLINK_INSTALL_DIR}/agw" ]; then
    echo "FAIL: Binary installed from a symlinked archive entry" >&2
    exit 1
fi
echo "PASS: symlinked archive entry rejected"

# 13. Test --dry-run performs verification without installing
echo "[test 12] --dry-run verifies without installing"
DRY_RUN_DIR="${TMP_TEST_DIR}/bin-dry-run"
DRY_RUN_OUTPUT="$("$INSTALLER" \
    --version "$MOCK_VERSION" \
    --dist-url "$MOCK_DIST" \
    --install-dir "$DRY_RUN_DIR" \
    --dry-run)"
case "$DRY_RUN_OUTPUT" in
    *"[dry-run]"*) ;;
    *)
        echo "FAIL: --dry-run did not report a dry-run summary: $DRY_RUN_OUTPUT" >&2
        exit 1
        ;;
esac
if [ -e "${DRY_RUN_DIR}/agw" ]; then
    echo "FAIL: --dry-run installed a binary" >&2
    exit 1
fi
echo "PASS: --dry-run verified without installing"

# 14. Test file:// dist URL support
echo "[test 13] file:// dist url supported"
FILE_URL_INSTALL_DIR="${TMP_TEST_DIR}/bin-file-url"
"$INSTALLER" \
    --version "$MOCK_VERSION" \
    --dist-url "file://${MOCK_DIST}" \
    --install-dir "$FILE_URL_INSTALL_DIR"
if [ ! -x "${FILE_URL_INSTALL_DIR}/agw" ]; then
    echo "FAIL: file:// dist url installation failed" >&2
    exit 1
fi
echo "PASS: file:// dist url supported"

# 15. Test unknown flags are rejected
echo "[test 14] unknown flag rejected"
if "$INSTALLER" --bogus-flag >/dev/null 2>&1; then
    echo "FAIL: Installer accepted an unknown flag" >&2
    exit 1
fi
echo "PASS: unknown flag rejected"

echo "=== All Installer Tests Passed Successfully ==="
