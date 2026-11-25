// Package updater handles automated release discovery, cryptographic verification,
// archive extraction, and atomic in-place binary upgrades for Antigravity Gateway.
package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

const (
	// DefaultGitHubRepo specifies the official open-source repository hosting releases.
	DefaultGitHubRepo = "salmanabdurrahman/antigravity-gateway"

	// DefaultFetchTimeout is the network timeout applied to GitHub release queries and downloads.
	DefaultFetchTimeout = 45 * time.Second

	// maxArchiveSizeBytes bounds downloaded release archives to 64 MB to prevent denial of service.
	maxArchiveSizeBytes = 64 * 1024 * 1024

	// maxChecksumSizeBytes bounds downloaded SHA256SUMS files to 1 MB.
	maxChecksumSizeBytes = 1024 * 1024

	// maxBinarySizeBytes bounds the extracted executable so a decompression bomb inside an otherwise
	// small archive cannot exhaust process memory.
	maxBinarySizeBytes = 128 * 1024 * 1024
)

var (
	// ErrNoUpdateAvailable indicates that the current binary is already at or ahead of the latest release.
	ErrNoUpdateAvailable = errors.New("no update available: current version is up to date")

	// ErrChecksumMismatch indicates that the computed SHA-256 digest differs from SHA256SUMS.
	ErrChecksumMismatch = errors.New("cryptographic checksum verification failed")

	// ErrBinaryNotFoundInArchive indicates that the agw executable was missing from the release archive.
	ErrBinaryNotFoundInArchive = errors.New("agw executable not found inside release archive")

	// ErrUnsafeArchiveEntry indicates that the archive entry named agw is not a regular file or grows
	// beyond the extraction ceiling, so extracting it would corrupt or exhaust the replacement binary.
	ErrUnsafeArchiveEntry = errors.New("release archive contains an unsafe agw entry")

	// ErrArchiveTooLarge indicates that the downloaded release archive exceeded the accepted size.
	ErrArchiveTooLarge = errors.New("release archive exceeds the maximum accepted size")

	// ErrInsecureURL indicates that a release URL would be fetched over an insecure transport.
	ErrInsecureURL = errors.New("refusing to fetch release artifacts over an insecure URL")

	// ErrUnsupportedPlatform indicates that the current OS or architecture has no compiled release assets.
	ErrUnsupportedPlatform = errors.New("unsupported operating system or CPU architecture")
)

// ReleaseAsset represents an individual downloadable artifact attached to a GitHub release.
type ReleaseAsset struct {
	// Name is the asset file name (e.g. "agw_0.1.1_darwin_arm64.tar.gz").
	Name string `json:"name"`

	// BrowserDownloadURL is the direct HTTP download location for the asset.
	BrowserDownloadURL string `json:"browser_download_url"`

	// Size is the artifact size in bytes.
	Size int64 `json:"size"`
}

// GitHubRelease represents release metadata returned by the GitHub Releases API.
type GitHubRelease struct {
	// TagName is the Git tag identifier (e.g. "v0.1.1").
	TagName string `json:"tag_name"`

	// Name is the descriptive release headline.
	Name string `json:"name"`

	// Draft indicates whether the release is unpublished.
	Draft bool `json:"draft"`

	// Prerelease indicates whether the release is marked as pre-release.
	Prerelease bool `json:"prerelease"`

	// PublishedAt is the RFC 3339 timestamp when the release was made public.
	PublishedAt string `json:"published_at"`

	// Assets lists all attached archives and checksum files.
	Assets []ReleaseAsset `json:"assets"`
}

// ReleaseInfo encapsulates processed version metadata ready for upgrade evaluation.
type ReleaseInfo struct {
	// CurrentVersion is the active runtime version string.
	CurrentVersion string

	// TargetVersion is the release version to be installed (clean without 'v' prefix).
	TargetVersion string

	// TagName is the release Git tag name (e.g. "v0.1.1").
	TagName string

	// PublishedAt is the human-readable or RFC 3339 publication timestamp.
	PublishedAt string

	// ArchiveName is the target archive filename for this platform.
	ArchiveName string

	// ArchiveURL is the direct download URL for the target archive.
	ArchiveURL string

	// ChecksumsURL is the download URL for the companion SHA256SUMS file.
	ChecksumsURL string

	// UpdateAvailable indicates whether TargetVersion is strictly greater than CurrentVersion.
	UpdateAvailable bool
}

// isHTTPSOrLoopback reports whether rawURL is safe to fetch: HTTPS always, and plain HTTP only for
// loopback hosts so local mirrors and httptest servers keep working without weakening transport
// security for real downloads.
func isHTTPSOrLoopback(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	switch parsed.Scheme {
	case "https":
		return true
	case "http":
		host := parsed.Hostname()
		if strings.EqualFold(host, "localhost") {
			return true
		}
		ip := net.ParseIP(host)
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}

// guardRedirects returns a copy of client whose redirect policy refuses any hop that would downgrade
// the transport or leave HTTPS for a non-loopback host.
//
// Protocol Quirk: the release API is queried with "Authorization: Bearer $GITHUB_TOKEN", and Go only
// strips that header on cross-host redirects. A redirect chain ending on plain HTTP would therefore
// leak the token, so every redirect target is validated before it is followed.
func guardRedirects(client *http.Client) *http.Client {
	if client == nil {
		return nil
	}
	guarded := *client
	previous := client.CheckRedirect
	guarded.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if !isHTTPSOrLoopback(req.URL.String()) {
			return fmt.Errorf("%w: redirect to %s", ErrInsecureURL, req.URL.Redacted())
		}
		if previous != nil {
			return previous(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &guarded
}

// CheckUpdate queries the GitHub Releases API and evaluates if an update is available for the current host.
func CheckUpdate(ctx context.Context, repo, currentVersion, requestedVersion string, httpClient *http.Client) (*ReleaseInfo, error) {
	if repo == "" {
		repo = DefaultGitHubRepo
	}
	if httpClient == nil {
		httpClient = config.DefaultHTTPClient(DefaultFetchTimeout)
	}
	httpClient = guardRedirects(httpClient)

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	if strings.HasPrefix(repo, "http://") || strings.HasPrefix(repo, "https://") {
		apiURL = repo
	} else if requestedVersion != "" {
		clean := strings.TrimPrefix(requestedVersion, "v")
		apiURL = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/v%s", repo, clean)
	}
	if !isHTTPSOrLoopback(apiURL) {
		return nil, fmt.Errorf("%w: release endpoint %s must use HTTPS", ErrInsecureURL, apiURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building update query request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "antigravity-gateway-updater/"+currentVersion)

	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying release metadata from %s: %w", apiURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("release not found at %s", apiURL)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("github api returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var ghRelease GitHubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&ghRelease); err != nil {
		return nil, fmt.Errorf("parsing release metadata JSON: %w", err)
	}

	cleanTarget := strings.TrimPrefix(ghRelease.TagName, "v")
	cleanCurrent := strings.TrimPrefix(currentVersion, "v")

	archiveName := FormatArchiveName(cleanTarget, runtime.GOOS, runtime.GOARCH)
	var archiveURL, checksumsURL string

	for _, asset := range ghRelease.Assets {
		if asset.Name == archiveName {
			archiveURL = asset.BrowserDownloadURL
		} else if asset.Name == "SHA256SUMS" {
			checksumsURL = asset.BrowserDownloadURL
		}
	}

	if archiveURL == "" {
		// Fallback to standard GitHub direct asset download URL scheme
		archiveURL = fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repo, ghRelease.TagName, archiveName)
	}
	if checksumsURL == "" {
		checksumsURL = fmt.Sprintf("https://github.com/%s/releases/download/%s/SHA256SUMS", repo, ghRelease.TagName)
	}

	// Assets come from the release JSON and must not be able to downgrade the transport used for the
	// checksum and archive downloads.
	for _, assetURL := range []string{archiveURL, checksumsURL} {
		if !isHTTPSOrLoopback(assetURL) {
			return nil, fmt.Errorf("%w: release asset %s must use HTTPS", ErrInsecureURL, assetURL)
		}
	}

	isNewer := CompareSemver(cleanTarget, cleanCurrent) > 0

	return &ReleaseInfo{
		CurrentVersion:  cleanCurrent,
		TargetVersion:   cleanTarget,
		TagName:         ghRelease.TagName,
		PublishedAt:     ghRelease.PublishedAt,
		ArchiveName:     archiveName,
		ArchiveURL:      archiveURL,
		ChecksumsURL:    checksumsURL,
		UpdateAvailable: isNewer,
	}, nil
}

// FormatArchiveName returns the expected release archive file name for the given platform.
func FormatArchiveName(version, goos, goarch string) string {
	ext := "tar.gz"
	if goos == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("agw_%s_%s_%s.%s", version, goos, goarch, ext)
}

// CompareSemver compares two semantic version strings (e.g. "0.1.1" vs "0.1.0").
// Returns 1 if a > b, -1 if a < b, and 0 if equal.
func CompareSemver(a, b string) int {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")

	partsA := strings.Split(a, ".")
	partsB := strings.Split(b, ".")

	for i := 0; i < max(len(partsA), len(partsB)); i++ {
		numA := 0
		if i < len(partsA) {
			numA, _ = strconv.Atoi(partsA[i])
		}
		numB := 0
		if i < len(partsB) {
			numB, _ = strconv.Atoi(partsB[i])
		}

		if numA > numB {
			return 1
		}
		if numA < numB {
			return -1
		}
	}
	return 0
}

// DownloadAndVerify downloads the release archive, verifies its SHA-256 digest against SHA256SUMS,
// and extracts the replacement executable bytes.
func DownloadAndVerify(ctx context.Context, release *ReleaseInfo, httpClient *http.Client) ([]byte, error) {
	if httpClient == nil {
		httpClient = config.DefaultHTTPClient(DefaultFetchTimeout)
	}
	httpClient = guardRedirects(httpClient)

	for _, targetURL := range []string{release.ChecksumsURL, release.ArchiveURL} {
		if !isHTTPSOrLoopback(targetURL) {
			return nil, fmt.Errorf("%w: %s must use HTTPS", ErrInsecureURL, targetURL)
		}
	}

	// Step 1: Download SHA256SUMS
	checksumReq, err := http.NewRequestWithContext(ctx, http.MethodGet, release.ChecksumsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building checksums request: %w", err)
	}
	checksumResp, err := httpClient.Do(checksumReq)
	if err != nil {
		return nil, fmt.Errorf("downloading checksums from %s: %w", release.ChecksumsURL, err)
	}
	defer checksumResp.Body.Close()

	if checksumResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading checksums failed (HTTP %d)", checksumResp.StatusCode)
	}

	checksumBytes, err := io.ReadAll(io.LimitReader(checksumResp.Body, maxChecksumSizeBytes))
	if err != nil {
		return nil, fmt.Errorf("reading checksums file: %w", err)
	}

	expectedHash, err := ParseChecksumForArchive(string(checksumBytes), release.ArchiveName)
	if err != nil {
		return nil, fmt.Errorf("locating archive in checksums: %w", err)
	}

	// Step 2: Download Release Archive
	archiveReq, err := http.NewRequestWithContext(ctx, http.MethodGet, release.ArchiveURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building archive request: %w", err)
	}
	archiveResp, err := httpClient.Do(archiveReq)
	if err != nil {
		return nil, fmt.Errorf("downloading archive from %s: %w", release.ArchiveURL, err)
	}
	defer archiveResp.Body.Close()

	if archiveResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading archive failed (HTTP %d)", archiveResp.StatusCode)
	}

	archiveBytes, err := io.ReadAll(io.LimitReader(archiveResp.Body, maxArchiveSizeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading archive payload: %w", err)
	}
	// Read one byte past the limit so oversized payloads fail loudly instead of being silently
	// truncated into a corrupt archive that only fails later during extraction.
	if int64(len(archiveBytes)) > maxArchiveSizeBytes {
		return nil, fmt.Errorf("%w: expected at most %d bytes", ErrArchiveTooLarge, maxArchiveSizeBytes)
	}

	// Step 3: Verify Cryptographic Digest
	hasher := sha256.New()
	hasher.Write(archiveBytes)
	actualHash := hex.EncodeToString(hasher.Sum(nil))

	if subtle.ConstantTimeCompare([]byte(actualHash), []byte(expectedHash)) != 1 {
		return nil, fmt.Errorf("%w: expected %s, got %s", ErrChecksumMismatch, expectedHash, actualHash)
	}

	// Step 4: Extract Executable
	isZip := strings.HasSuffix(release.ArchiveName, ".zip")
	return ExtractBinaryFromArchive(archiveBytes, isZip)
}

// ParseChecksumForArchive parses a standard SHA256SUMS file and extracts the expected hash for the given archive name.
func ParseChecksumForArchive(checksumsContent, archiveName string) (string, error) {
	lines := strings.Split(checksumsContent, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			hash := fields[0]
			file := strings.TrimPrefix(fields[1], "*")
			if file == archiveName || filepath.Base(file) == archiveName {
				return strings.ToLower(hash), nil
			}
		}
	}
	return "", fmt.Errorf("archive %q not found in checksums file", archiveName)
}

// readCappedBinary reads an extracted executable while enforcing an explicit size ceiling so a
// decompression bomb inside an otherwise small archive cannot exhaust process memory.
func readCappedBinary(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBinarySizeBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading extracted executable: %w", err)
	}
	if len(data) > maxBinarySizeBytes {
		return nil, fmt.Errorf("%w: extracted executable exceeds %d bytes", ErrUnsafeArchiveEntry, maxBinarySizeBytes)
	}
	return data, nil
}

// ExtractBinaryFromArchive extracts the compiled executable ("agw" or "agw.exe") from a tar.gz or zip buffer.
//
// Only regular files are accepted: a directory or symlink entry named agw would otherwise be extracted
// as empty (or foreign) bytes and corrupt the installed executable during ApplyBinary.
func ExtractBinaryFromArchive(archiveBytes []byte, isZip bool) ([]byte, error) {
	if isZip {
		zipReader, err := zip.NewReader(bytes.NewReader(archiveBytes), int64(len(archiveBytes)))
		if err != nil {
			return nil, fmt.Errorf("reading zip archive: %w", err)
		}

		for _, file := range zipReader.File {
			baseName := filepath.Base(file.Name)
			if baseName != "agw.exe" && baseName != "agw" {
				continue
			}
			if !file.Mode().IsRegular() {
				return nil, fmt.Errorf("%w: zip entry %q has mode %s", ErrUnsafeArchiveEntry, file.Name, file.Mode())
			}
			rc, err := file.Open()
			if err != nil {
				return nil, fmt.Errorf("opening file inside zip: %w", err)
			}
			defer rc.Close()
			return readCappedBinary(rc)
		}
		return nil, ErrBinaryNotFoundInArchive
	}

	// tar.gz handling
	gzReader, err := gzip.NewReader(bytes.NewReader(archiveBytes))
	if err != nil {
		return nil, fmt.Errorf("reading gzip header: %w", err)
	}
	defer gzReader.Close()

	tarReader := tar.NewReader(gzReader)
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading tar entry: %w", err)
		}

		baseName := filepath.Base(header.Name)
		if baseName != "agw" && baseName != "agw.exe" {
			continue
		}
		// Symlink, hardlink, and directory entries named agw must never be extracted: TypeReg covers the
		// modern regular-file marker and TypeRegA the historical NUL/"0" variant emitted by older tar tools.
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("%w: tar entry %q has typeflag %q", ErrUnsafeArchiveEntry, header.Name, header.Typeflag)
		}
		return readCappedBinary(tarReader)
	}

	return nil, ErrBinaryNotFoundInArchive
}
