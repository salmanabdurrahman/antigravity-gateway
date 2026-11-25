<#
.SYNOPSIS
    Checksum-verifying, architecture-detecting installer for Antigravity Gateway (agw) on Windows.

.DESCRIPTION
    Downloads the appropriate release zip and SHA256SUMS from GitHub Releases,
    verifies cryptographic integrity, and installs agw.exe to a user-local directory.

.EXAMPLE
    irm https://raw.githubusercontent.com/salmanabdurrahman/antigravity-gateway/main/scripts/install.ps1 | iex
    powershell -ExecutionPolicy Bypass -File .\scripts\install.ps1 -Version "0.1.0"
#>

[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [string]$Version = "",

    [string]$InstallDir = "",
    [string]$Repo = "",
    [string]$DistUrl = "",
    [switch]$Force,
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"

# Ensure TLS 1.2 is enabled for all network calls in legacy PowerShell environments
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
} catch {
    # Ignore on platforms where TLS 1.2 is default or not directly configurable
}

# 1. Resolve Install Directory and Repository Defaults
if (-not $InstallDir) {
    if ($env:AGW_INSTALL_DIR) {
        $InstallDir = $env:AGW_INSTALL_DIR
    } elseif ($env:LOCALAPPDATA) {
        $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\agw"
    } else {
        $InstallDir = Join-Path $HOME ".local\bin"
    }
}

if (-not $Repo) {
    if ($env:AGW_GITHUB_REPO) {
        $Repo = $env:AGW_GITHUB_REPO
    } else {
        $Repo = "salmanabdurrahman/antigravity-gateway"
    }
}
if (-not $DistUrl -and $env:AGW_DIST_URL) {
    $DistUrl = $env:AGW_DIST_URL
}

# 2. Detect Architecture
$arch = "amd64"
$rawArch = $env:PROCESSOR_ARCHITECTURE
if ($rawArch -eq "ARM64") {
    $arch = "arm64"
} elseif ($rawArch -eq "AMD64") {
    $arch = "amd64"
} else {
    Write-Error "Unsupported processor architecture: $rawArch. Supported: AMD64, ARM64."
    exit 1
}

# 3. Resolve Version
if (-not $Version) {
    Write-Host "--> No version specified. Resolving latest release for $Repo..."
    try {
        $releaseUrl = "https://api.github.com/repos/$Repo/releases/latest"
        $releaseJson = Invoke-RestMethod -Uri $releaseUrl -Headers @{ "User-Agent" = "agw-installer" } -UseBasicParsing
        $Version = $releaseJson.tag_name
    } catch {
        Write-Error "Could not resolve latest release for $Repo: $_`nPlease specify version explicitly via -Version <vX.Y.Z>."
        exit 1
    }
}

$cleanVersion = $Version.Trim().TrimStart('v')
if ($cleanVersion -notmatch '^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$') {
    Write-Error "Invalid version '$Version': expected a semantic version such as 0.1.0 or v0.1.0."
    exit 1
}

$archiveName = "agw_${cleanVersion}_windows_${arch}.zip"
$targetBin = Join-Path $InstallDir "agw.exe"

# 4. Check Existing Binary
if ((Test-Path -Path $targetBin) -and (-not $Force)) {
    Write-Error "Destination binary already exists at $targetBin. Use -Force to overwrite."
    exit 1
}

# 5. Base URLs
if ($DistUrl) {
    $baseUrl = $DistUrl.TrimEnd('/')
} else {
    $baseUrl = "https://github.com/$Repo/releases/download/v$cleanVersion"
}

$archiveUrl = "$baseUrl/$archiveName"
$sumsUrl = "$baseUrl/SHA256SUMS"

# 6. Temporary Workspace
$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("agw-install-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tempDir -Force | Out-Null

try {
    $archivePath = Join-Path $tempDir $archiveName
    $sumsPath = Join-Path $tempDir "SHA256SUMS"

    Write-Host "--> Preparing to install Antigravity Gateway (agw) v$cleanVersion for windows/$arch"
    Write-Host "--> Fetching checksums from $sumsUrl..."
    Invoke-WebRequest -Uri $sumsUrl -OutFile $sumsPath -UseBasicParsing

    # Parse expected hash. sha256sum -b emits a binary-mode marker (`*`) before the filename, and the
    # separator may be a tab or repeated spaces, so both the marker and whitespace are normalized.
    $expectedHash = ""
    Get-Content -Path $sumsPath | ForEach-Object {
        $parts = $_.Trim() -split '\s+'
        if ($parts.Length -ge 2 -and $parts[1].TrimStart('*') -eq $archiveName) {
            $expectedHash = $parts[0].Trim().ToLower()
        }
    }

    if (-not $expectedHash) {
        Write-Error "Archive '$archiveName' not found in SHA256SUMS."
        exit 1
    }

    Write-Host "--> Downloading release archive $archiveName..."
    Invoke-WebRequest -Uri $archiveUrl -OutFile $archivePath -UseBasicParsing

    Write-Host "--> Verifying SHA-256 cryptographic digest..."
    $actualHash = (Get-FileHash -Path $archivePath -Algorithm SHA256).Hash.ToLower()

    if ($actualHash -ne $expectedHash) {
        Write-Error "Cryptographic checksum verification failed!`n  Expected: $expectedHash`n  Actual:   $actualHash"
        exit 1
    }

    Write-Host "--> Checksum verified: $actualHash"

    if ($DryRun) {
        Write-Host "[dry-run] Verification succeeded for $archiveName."
        Write-Host "[dry-run] Binary would be installed to: $targetBin"
        exit 0
    }

    # 7. Extract & Install
    $stageDir = Join-Path $tempDir "extracted"
    Expand-Archive -Path $archivePath -DestinationPath $stageDir -Force

    $extractedBin = Join-Path $stageDir "agw.exe"
    if (-not (Test-Path -Path $extractedBin -PathType Leaf)) {
        Write-Error "Extracted archive did not contain a regular 'agw.exe' file."
        exit 1
    }
    $extractedItem = Get-Item -Path $extractedBin -Force
    if ($extractedItem.LinkType) {
        Write-Error "Extracted 'agw.exe' is a link ($($extractedItem.LinkType)); refusing to install a link target."
        exit 1
    }

    if (-not (Test-Path -Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }

    Copy-Item -Path $extractedBin -Destination $targetBin -Force

    Write-Host ""
    Write-Host "================================================================================"
    Write-Host "  Antigravity Gateway (agw) v$cleanVersion installed successfully!"
    Write-Host "================================================================================"
    Write-Host "Binary location: $targetBin"

    # PATH check
    $pathTokens = $env:PATH -split ';'
    if ($pathTokens -notcontains $InstallDir) {
        Write-Host ""
        Write-Host "Note: '$InstallDir' is not currently in your system PATH."
        Write-Host "To add it permanently in PowerShell:"
        Write-Host "  [Environment]::SetEnvironmentVariable('Path', `"`$([Environment]::GetEnvironmentVariable('Path', 'User'));$InstallDir`", 'User')"
    }

    Write-Host ""
    Write-Host "Next steps:"
    Write-Host "  1. Authenticate with Google:  agw login"
    Write-Host "  2. Start local proxy daemon:   agw start --detach"
    Write-Host "  3. Configure coding agent:     agw setup pi   (or agw setup opencode)"
    Write-Host ""
    Write-Host "To uninstall:"
    Write-Host "  1. Stop daemon:                agw stop"
    Write-Host "  2. Remove binary:              Remove-Item `"$targetBin`""
    Write-Host "  3. Remove local state/tokens:  Remove-Item -Recurse -Force `"`$env:USERPROFILE\.agw`""
    Write-Host "================================================================================"

} finally {
    if (Test-Path -Path $tempDir) {
        Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
