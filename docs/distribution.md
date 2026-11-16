# Supported Platforms & Binary Installation Contract

This document defines the supported operating systems, hardware architectures, filesystem layout, binary installation contract, and lifecycle management for distributed Antigravity Gateway (`agw`) binaries.

## 1. Supported Platform Matrix

Antigravity Gateway is compiled as a static, zero-dependency binary with native cross-platform build support:

| Operating System | Architecture | Target Tuple    | Support Tier | Requirements / Notes                                                    |
| :--------------- | :----------- | :-------------- | :----------- | :---------------------------------------------------------------------- |
| **macOS**        | ARM64        | `darwin/arm64`  | **Tier 1**   | Apple Silicon (M1, M2, M3, M4); macOS 11.0 (Big Sur) or later.          |
| **macOS**        | x86_64       | `darwin/amd64`  | **Tier 1**   | Intel Macs; macOS 10.15 (Catalina) or later.                            |
| **Linux**        | x86_64       | `linux/amd64`   | **Tier 1**   | Modern 64-bit Linux distributions (glibc 2.17+ or musl; kernel 3.10+).  |
| **Linux**        | ARM64        | `linux/arm64`   | **Tier 1**   | 64-bit ARM Linux (Raspberry Pi 4/5, AWS Graviton, Ampere); kernel 4.x+. |
| **Windows**      | x86_64       | `windows/amd64` | **Tier 1**   | 64-bit Windows 10, Windows 11, or Windows Server 2016+.                 |
| **Windows**      | ARM64        | `windows/arm64` | **Tier 2**   | Windows on ARM devices; built via Go cross-compilation.                 |

### Tier Definitions

- **Tier 1 (Primary)**: Automated compilation, unit and integration test verification, static analysis, and guaranteed pre-built release artifacts.
- **Tier 2 (Secondary)**: Supported via Go cross-compilation and verified for build compatibility; tested on demand.

## 2. Binary Packaging & Release Naming

Official release artifacts follow standard semantic naming conventions:

```text
agw_<version>_<os>_<arch>.tar.gz   (macOS, Linux)
agw_<version>_<os>_<arch>.zip      (Windows)
```

Example artifacts for version `0.1.0`:

- `agw_0.1.0_darwin_arm64.tar.gz`
- `agw_0.1.0_darwin_amd64.tar.gz`
- `agw_0.1.0_linux_amd64.tar.gz`
- `agw_0.1.0_linux_arm64.tar.gz`
- `agw_0.1.0_windows_amd64.zip`
- `agw_0.1.0_windows_arm64.zip`

### Integrity & Verification

Every release includes a companion `SHA256SUMS` file published alongside the binaries containing SHA-256 cryptographic digests for each release archive. Users should always verify package integrity prior to unpacking or executing binaries:

#### On Linux

```bash
# Verify checksums (checks matching files in current directory)
sha256sum --check --ignore-missing SHA256SUMS
```

#### On macOS

```bash
# Verify checksums using shasum (ignoring archives for other platforms)
shasum -a 256 --check --ignore-missing SHA256SUMS
```

#### On Windows (PowerShell)

```powershell
# Compute SHA-256 hash
(Get-FileHash -Algorithm SHA256 .\agw_0.1.0_windows_amd64.zip).Hash.ToLower()

# Compare against the hash listed in SHA256SUMS
Get-Content SHA256SUMS | Select-String "agw_0.1.0_windows_amd64.zip"
```

### Unpacking & Initial Execution Verification

Once checksum verification passes, extract the binary and confirm functionality:

```bash
# Linux / macOS
tar -xzf agw_0.1.0_darwin_arm64.tar.gz
./agw version
./agw --help

# Move binary to local user PATH
mkdir -p ~/.local/bin
mv agw ~/.local/bin/
```

### Building Release Artifacts Locally

Developers can reproduce official release packages and checksums locally:

```bash
# Build all platform archives and generate dist/SHA256SUMS
make release

# Or invoke the release builder directly with a custom version tag
./scripts/build-release.sh 0.1.0
```

## 3. Automated Checksum-Verifying Installer

Antigravity Gateway provides checksum-verifying, architecture-detecting installer scripts for POSIX systems (`scripts/install.sh`) and Windows PowerShell (`scripts/install.ps1`) that install released binaries to user-local directories without requiring elevated/root privileges.

### Security & Invariants

- **Cryptographic Verification**: The installer downloads the release archive and companion `SHA256SUMS`, calculates the local SHA-256 digest, and halts immediately if checksums do not match.
- **Unmanaged Binary Protection**: The installer will not overwrite an existing binary unless `--force` (or `-Force`) is explicitly provided.
- **Zero Elevated Privileges**: Installs to user-local directory (`~/.local/bin` on POSIX, `%LOCALAPPDATA%\Programs\agw` on Windows by default), preventing unwanted mutation of system directories.
- **Flexible Versioning**: Supports automatic resolution of the latest GitHub release or explicit pinned semantic versions (`vX.Y.Z`).

### Quick Start (One-Liner Install)

#### macOS & Linux (curl / sh)

```bash
# Install latest release
curl -fsSL https://raw.githubusercontent.com/salmanabdurrahman/antigravity-gateway/main/scripts/install.sh | sh

# Install a specific pinned release version
curl -fsSL https://raw.githubusercontent.com/salmanabdurrahman/antigravity-gateway/main/scripts/install.sh | sh -s -- v0.1.0
```

#### Windows (PowerShell)

```powershell
# Install latest release
irm https://raw.githubusercontent.com/salmanabdurrahman/antigravity-gateway/main/scripts/install.ps1 | iex

# Install a specific pinned release version
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/salmanabdurrahman/antigravity-gateway/main/scripts/install.ps1))) -Version "0.1.0"
```

### Local Script Options

```bash
# Install latest or pinned version via local script
./scripts/install.sh
./scripts/install.sh 0.1.0
./scripts/install.sh --version 0.1.0

# Install to a custom directory
./scripts/install.sh --version 0.1.0 --install-dir ~/bin

# Overwrite an existing installation
./scripts/install.sh --version 0.1.0 --force

# Dry-run validation (verifies platform and checksums without writing)
./scripts/install.sh --version 0.1.0 --dry-run
```

To run the automated installer test suite:

```bash
make test-installer
```

## 4. Filesystem Installation Contract

### 4.1 Executable Location

The single binary executable should be placed in a directory already present in the user's `$PATH`:

- **User-local installation (recommended, no root required)**:
  - macOS / Linux: `~/.local/bin/agw`
  - Windows: `%LOCALAPPDATA%\Programs\agw\agw.exe`
- **System-wide installation (requires administrative privileges)**:
  - macOS / Linux: `/usr/local/bin/agw`
  - Windows: `C:\Program Files\agw\agw.exe`

### 4.2 Runtime State & Configuration Directory

All runtime states, cached models, logs, and credentials reside in the user's home configuration directory:

- Default location: `~/.agw` (POSIX systems) or `%USERPROFILE%\.agw` (Windows).
- Override via environment variable: `AGW_HOME=/custom/path`.

#### Directory & File Permission Security Contract

| Path                       | Purpose                                      | Permissions           | Management                                                 |
| :------------------------- | :------------------------------------------- | :-------------------- | :--------------------------------------------------------- |
| `~/.agw/`                  | Base configuration directory                 | `0700` (`drwx------`) | Created on first run; restricted to current user.          |
| `~/.agw/config.json`       | Proxy daemon settings, port, local API key   | `0600` (`-rw-------`) | Created on first run with generated API key; atomic write. |
| `~/.agw/accounts.json`     | Encrypted/redacted OAuth tokens and projects | `0600` (`-rw-------`) | Written upon `agw login` / `agw logout`; atomic write.     |
| `~/.agw/models-cache.json` | Cached upstream model catalog                | `0600` (`-rw-------`) | Auto-updated on startup or manual refresh.                 |
| `~/.agw/agw.pid`           | Process ID of background daemon              | `0600` (`-rw-------`) | Written by `agw start --detach`; removed by `agw stop`.    |
| `~/.agw/agw.log`           | Background daemon standard output & error    | `0600` (`-rw-------`) | Appended during detached daemon execution.                 |

## 5. Lifecycle & Upgrade Contract

### 5.1 In-Place Upgrade Flow

Upgrading to a newer binary version preserves all configuration, cached models, and authenticated accounts:

1. **Stop active daemon**:
   ```bash
   agw stop
   ```
2. **Replace executable**:
   Replace the existing binary in `~/.local/bin/agw` or `/usr/local/bin/agw` with the new version.
3. **Verify version**:
   ```bash
   agw version
   ```
4. **Restart daemon**:
   ```bash
   agw start --detach
   ```

### 5.2 Downgrade Compatibility

All configuration and credential files maintain backward compatibility with default fallback values. In the event of a downgrade, prior versions will safely read existing configuration without loss of credentials.

### 5.3 Stale PID Recovery

If a process is killed uncleanly (e.g. system reboot or SIGKILL), the gateway detects whether the PID stored in `agw.pid` is active:

- If inactive or owned by another process, `agw start` automatically clears the stale PID and restarts cleanly.

## 6. Uninstallation Contract

To completely remove Antigravity Gateway and restore agent configurations:

1. **Stop the daemon**:
   ```bash
   agw stop
   ```
2. **Revert client configurations**:
   - For Pi Coding Agent: Revert `~/.pi/agent/models.json` using the automatic backup file (`models.json.bak-<timestamp>`) created during `agw setup pi`.
   - For OpenCode: Remove the `antigravity` provider entry from the OpenCode configuration.
3. **Remove binary executable**:
   ```bash
   rm -f ~/.local/bin/agw
   # or on Windows
   Remove-Item "$env:LOCALAPPDATA\Programs\agw\agw.exe"
   ```
4. **Remove runtime configuration and tokens (optional)**:
   ```bash
   rm -rf ~/.agw
   # or on Windows
   Remove-Item -Recurse -Force "$env:USERPROFILE\.agw"
   ```
