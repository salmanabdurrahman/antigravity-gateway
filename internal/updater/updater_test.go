// Package updater handles automated release discovery, cryptographic verification,
// archive extraction, and atomic in-place binary upgrades for Antigravity Gateway.
package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildPlatformArchive builds the release archive the updater expects for the current platform: a zip
// holding agw.exe on Windows, or a tar.gz holding agw elsewhere. The fixture must match
// FormatArchiveName, because archive type is selected from the published file extension.
func buildPlatformArchive(t *testing.T, content []byte) []byte {
	t.Helper()

	if runtime.GOOS == "windows" {
		var buf bytes.Buffer
		zipWriter := zip.NewWriter(&buf)
		header := &zip.FileHeader{Name: "agw.exe", Method: zip.Deflate}
		header.SetMode(0o755)
		entry, err := zipWriter.CreateHeader(header)
		if err != nil {
			t.Fatalf("creating zip entry: %v", err)
		}
		if _, err := entry.Write(content); err != nil {
			t.Fatalf("writing zip entry: %v", err)
		}
		if err := zipWriter.Close(); err != nil {
			t.Fatalf("closing zip archive: %v", err)
		}
		return buf.Bytes()
	}

	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "agw", Size: int64(len(content)), Mode: 0o755}); err != nil {
		t.Fatalf("writing tar header: %v", err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatalf("writing tar entry: %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("closing tar writer: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("closing gzip writer: %v", err)
	}
	return buf.Bytes()
}

// TestCompareSemver verifies semantic version ordering across major, minor, patch, and equivalent versions.
func TestCompareSemver(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"0.1.1", "0.1.0", 1},
		{"v0.2.0", "v0.1.9", 1},
		{"1.0.0", "0.9.9", 1},
		{"0.1.0", "0.1.1", -1},
		{"0.1.0", "0.1.0", 0},
		{"v0.1.0", "0.1.0", 0},
		{"0.1.0.1", "0.1.0", 1},
	}

	for _, tc := range tests {
		if got := CompareSemver(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareSemver(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestFormatArchiveName verifies naming patterns for tar.gz on POSIX and zip on Windows.
func TestFormatArchiveName(t *testing.T) {
	darwinArchive := FormatArchiveName("0.1.1", "darwin", "arm64")
	if darwinArchive != "agw_0.1.1_darwin_arm64.tar.gz" {
		t.Errorf("unexpected darwin archive: %s", darwinArchive)
	}

	windowsArchive := FormatArchiveName("0.1.1", "windows", "amd64")
	if windowsArchive != "agw_0.1.1_windows_amd64.zip" {
		t.Errorf("unexpected windows archive: %s", windowsArchive)
	}
}

// TestParseChecksumForArchive verifies parsing standard sha256sum output with single and double space separators.
func TestParseChecksumForArchive(t *testing.T) {
	content := `
# Checksums file
e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  agw_0.1.1_darwin_arm64.tar.gz
8c0391fa7d1e242cbf2cea6f5058579e02415500ffafeb40e9f63ae279be47bc *agw_0.1.1_linux_amd64.tar.gz
`
	hash1, err := ParseChecksumForArchive(content, "agw_0.1.1_darwin_arm64.tar.gz")
	if err != nil {
		t.Fatalf("parsing darwin checksum: %v", err)
	}
	if hash1 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("unexpected hash: %s", hash1)
	}

	hash2, err := ParseChecksumForArchive(content, "agw_0.1.1_linux_amd64.tar.gz")
	if err != nil {
		t.Fatalf("parsing linux checksum: %v", err)
	}
	if hash2 != "8c0391fa7d1e242cbf2cea6f5058579e02415500ffafeb40e9f63ae279be47bc" {
		t.Errorf("unexpected hash: %s", hash2)
	}

	if _, err := ParseChecksumForArchive(content, "non_existent.tar.gz"); err == nil {
		t.Error("expected error for non-existent archive in checksums")
	}
}

// TestExtractBinaryFromArchiveTarGz verifies extracting the agw executable from a tar.gz archive in memory.
func TestExtractBinaryFromArchiveTarGz(t *testing.T) {
	binaryContent := []byte("#!/bin/sh\necho 'agw test binary'\n")

	var tarGzBuf bytes.Buffer
	gw := gzip.NewWriter(&tarGzBuf)
	tw := tar.NewWriter(gw)

	// Add dummy files
	_ = tw.WriteHeader(&tar.Header{Name: "README.md", Size: 4, Mode: 0o644})
	_, _ = tw.Write([]byte("docs"))

	_ = tw.WriteHeader(&tar.Header{Name: "agw", Size: int64(len(binaryContent)), Mode: 0o755})
	_, _ = tw.Write(binaryContent)

	_ = tw.Close()
	_ = gw.Close()

	extracted, err := ExtractBinaryFromArchive(tarGzBuf.Bytes(), false)
	if err != nil {
		t.Fatalf("extracting from tar.gz: %v", err)
	}
	if !bytes.Equal(extracted, binaryContent) {
		t.Errorf("extracted content mismatch: got %q, want %q", string(extracted), string(binaryContent))
	}
}

// TestExtractBinaryFromArchiveZip verifies extracting the agw.exe executable from a zip archive in memory.
func TestExtractBinaryFromArchiveZip(t *testing.T) {
	binaryContent := []byte("MZ binary test content")

	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)

	f, _ := zw.Create("agw.exe")
	_, _ = f.Write(binaryContent)
	_ = zw.Close()

	extracted, err := ExtractBinaryFromArchive(zipBuf.Bytes(), true)
	if err != nil {
		t.Fatalf("extracting from zip: %v", err)
	}
	if !bytes.Equal(extracted, binaryContent) {
		t.Errorf("extracted content mismatch: got %q, want %q", string(extracted), string(binaryContent))
	}
}

// TestCheckUpdateEndToEnd verifies discovering releases via mock GitHub API server.
func TestCheckUpdateEndToEnd(t *testing.T) {
	ctx := t.Context()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			release := GitHubRelease{
				TagName:     "v0.2.0",
				Name:        "Release v0.2.0",
				PublishedAt: "2026-11-26T00:00:00Z",
				Assets: []ReleaseAsset{
					{
						Name:               FormatArchiveName("0.2.0", runtime.GOOS, runtime.GOARCH),
						BrowserDownloadURL: "https://example.com/download/agw.tar.gz",
					},
					{
						Name:               "SHA256SUMS",
						BrowserDownloadURL: "https://example.com/download/SHA256SUMS",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(release)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	// Direct query with CheckUpdate using mock server URL
	client := ts.Client()
	rel, err := CheckUpdate(ctx, ts.URL+"/releases/latest", "0.1.1", "", client)
	if err != nil {
		t.Fatalf("CheckUpdate failed: %v", err)
	}

	if rel.TargetVersion != "0.2.0" {
		t.Errorf("expected TargetVersion 0.2.0, got %s", rel.TargetVersion)
	}
	if !rel.UpdateAvailable {
		t.Errorf("expected UpdateAvailable true for 0.2.0 > 0.1.1")
	}
	if rel.ArchiveURL != "https://example.com/download/agw.tar.gz" {
		t.Errorf("unexpected ArchiveURL: %s", rel.ArchiveURL)
	}
}

// TestDownloadAndVerify verifies archive hash verification and tampered payload rejection.
func TestDownloadAndVerify(t *testing.T) {
	ctx := t.Context()

	binaryContent := []byte("compiled-agw-v0.2.0-binary-content")
	archiveBytes := buildPlatformArchive(t, binaryContent)
	hasher := sha256.New()
	hasher.Write(archiveBytes)
	validHash := hex.EncodeToString(hasher.Sum(nil))

	archiveName := FormatArchiveName("0.2.0", runtime.GOOS, runtime.GOARCH)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/SHA256SUMS") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fmt.Sprintf("%s  %s\n", validHash, archiveName)))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/archive") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(archiveBytes)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	release := &ReleaseInfo{
		CurrentVersion: "0.1.1",
		TargetVersion:  "0.2.0",
		ArchiveName:    archiveName,
		ArchiveURL:     ts.URL + "/archive",
		ChecksumsURL:   ts.URL + "/SHA256SUMS",
	}

	extracted, err := DownloadAndVerify(ctx, release, ts.Client())
	if err != nil {
		t.Fatalf("DownloadAndVerify failed: %v", err)
	}
	if !bytes.Equal(extracted, binaryContent) {
		t.Errorf("extracted content mismatch: got %q, want %q", string(extracted), string(binaryContent))
	}

	// Test Tampered Checksum Mismatch
	releaseTampered := &ReleaseInfo{
		CurrentVersion: "0.1.1",
		TargetVersion:  "0.2.0",
		ArchiveName:    archiveName,
		ArchiveURL:     ts.URL + "/archive",
		ChecksumsURL:   ts.URL + "/invalid-checksums",
	}
	tsTampered := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fmt.Sprintf("0000000000000000000000000000000000000000000000000000000000000000  %s\n", archiveName)))
	}))
	defer tsTampered.Close()
	releaseTampered.ChecksumsURL = tsTampered.URL

	if _, err := DownloadAndVerify(ctx, releaseTampered, ts.Client()); err == nil {
		t.Error("expected DownloadAndVerify to fail on checksum mismatch, but it succeeded")
	}
}

// TestApplyBinaryAtomicUpgrade verifies in-place atomic replacement of an executable on disk.
func TestApplyBinaryAtomicUpgrade(t *testing.T) {
	tempDir := t.TempDir()
	targetBin := filepath.Join(tempDir, "test-agw")

	initialContent := []byte("initial binary version 1")
	if err := os.WriteFile(targetBin, initialContent, 0o755); err != nil {
		t.Fatalf("writing initial binary: %v", err)
	}

	newContent := []byte("upgraded binary version 2")
	if err := ApplyBinary(targetBin, newContent); err != nil {
		t.Fatalf("ApplyBinary failed: %v", err)
	}

	readBack, err := os.ReadFile(targetBin)
	if err != nil {
		t.Fatalf("reading upgraded binary: %v", err)
	}
	if !bytes.Equal(readBack, newContent) {
		t.Errorf("upgraded binary mismatch: got %q, want %q", string(readBack), string(newContent))
	}

	info, err := os.Stat(targetBin)
	if err != nil {
		t.Fatalf("stat upgraded binary: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o755 {
		t.Errorf("expected 0755 permissions, got %o", info.Mode().Perm())
	}
}

// TestExtractBinaryFromArchiveRejectsNonRegularEntries verifies directory and symlink entries named
// agw are rejected instead of being extracted as empty or foreign bytes.
func TestExtractBinaryFromArchiveRejectsNonRegularEntries(t *testing.T) {
	t.Run("tar symlink", func(t *testing.T) {
		var tarGzBuf bytes.Buffer
		gw := gzip.NewWriter(&tarGzBuf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{Name: "agw", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd", Mode: 0o755})
		_ = tw.Close()
		_ = gw.Close()

		if _, err := ExtractBinaryFromArchive(tarGzBuf.Bytes(), false); !errors.Is(err, ErrUnsafeArchiveEntry) {
			t.Fatalf("expected ErrUnsafeArchiveEntry for tar symlink, got %v", err)
		}
	})

	t.Run("tar directory", func(t *testing.T) {
		var tarGzBuf bytes.Buffer
		gw := gzip.NewWriter(&tarGzBuf)
		tw := tar.NewWriter(gw)
		_ = tw.WriteHeader(&tar.Header{Name: "agw", Typeflag: tar.TypeDir, Mode: 0o755})
		_ = tw.Close()
		_ = gw.Close()

		if _, err := ExtractBinaryFromArchive(tarGzBuf.Bytes(), false); !errors.Is(err, ErrUnsafeArchiveEntry) {
			t.Fatalf("expected ErrUnsafeArchiveEntry for tar directory, got %v", err)
		}
	})

	t.Run("zip directory", func(t *testing.T) {
		var zipBuf bytes.Buffer
		zw := zip.NewWriter(&zipBuf)
		if _, err := zw.Create("agw/"); err != nil {
			t.Fatalf("creating zip directory entry: %v", err)
		}
		if _, err := zw.Create("agw"); err != nil {
			t.Fatalf("creating zip file entry: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("closing zip writer: %v", err)
		}

		// The directory entry named "agw" must be rejected rather than skipped silently.
		if _, err := ExtractBinaryFromArchive(zipBuf.Bytes(), true); !errors.Is(err, ErrUnsafeArchiveEntry) {
			t.Fatalf("expected ErrUnsafeArchiveEntry for zip directory, got %v", err)
		}
	})
}

// TestDownloadAndVerifyRejectsOversizeArchive verifies a payload larger than the accepted archive
// ceiling fails loudly instead of being silently truncated into a corrupt archive.
func TestDownloadAndVerifyRejectsOversizeArchive(t *testing.T) {
	ctx := t.Context()

	oversize := bytes.Repeat([]byte("A"), maxArchiveSizeBytes+16)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			_, _ = w.Write([]byte("deadbeef  agw_test.tar.gz\n"))
		default:
			_, _ = w.Write(oversize)
		}
	}))
	defer ts.Close()

	release := &ReleaseInfo{
		ArchiveName:  "agw_test.tar.gz",
		ArchiveURL:   ts.URL + "/agw_test.tar.gz",
		ChecksumsURL: ts.URL + "/SHA256SUMS",
	}

	if _, err := DownloadAndVerify(ctx, release, ts.Client()); !errors.Is(err, ErrArchiveTooLarge) {
		t.Fatalf("expected ErrArchiveTooLarge, got %v", err)
	}
}

// TestCheckUpdateRejectsInsecureURLs verifies release endpoints and asset URLs must use HTTPS
// unless they point at loopback, so the GitHub token can never be sent over plain HTTP.
func TestCheckUpdateRejectsInsecureURLs(t *testing.T) {
	ctx := t.Context()

	if _, err := CheckUpdate(ctx, "http://example.com/releases/latest", "0.1.1", "", nil); !errors.Is(err, ErrInsecureURL) {
		t.Fatalf("expected ErrInsecureURL for plain HTTP release endpoint, got %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(GitHubRelease{
			TagName: "v0.2.0",
			Assets: []ReleaseAsset{{
				Name:               FormatArchiveName("0.2.0", runtime.GOOS, runtime.GOARCH),
				BrowserDownloadURL: "http://example.com/download/agw.tar.gz",
			}},
		})
	}))
	defer ts.Close()

	if _, err := CheckUpdate(ctx, ts.URL, "0.1.1", "", ts.Client()); !errors.Is(err, ErrInsecureURL) {
		t.Fatalf("expected ErrInsecureURL for plain HTTP asset URL, got %v", err)
	}
}

// TestCheckUpdateRejectsRedirectDowngrade verifies an HTTPS release endpoint that redirects to plain
// HTTP is refused before the authorization header can be forwarded.
func TestCheckUpdateRejectsRedirectDowngrade(t *testing.T) {
	ctx := t.Context()

	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.com/releases/latest", http.StatusFound)
	}))
	defer ts.Close()

	if _, err := CheckUpdate(ctx, ts.URL, "0.1.1", "", ts.Client()); !errors.Is(err, ErrInsecureURL) {
		t.Fatalf("expected ErrInsecureURL for HTTPS to HTTP redirect downgrade, got %v", err)
	}
}
