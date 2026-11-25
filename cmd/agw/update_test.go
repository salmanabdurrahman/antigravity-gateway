// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/updater"
)

// TestUpdateCommandCheckOnly verifies running agw update --check with a mock release server.
func TestUpdateCommandCheckOnly(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			release := updater.GitHubRelease{
				TagName:     "v0.2.0",
				PublishedAt: "2026-11-26T12:00:00Z",
				Assets: []updater.ReleaseAsset{
					{
						Name:               updater.FormatArchiveName("0.2.0", runtime.GOOS, runtime.GOARCH),
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

	buf := new(bytes.Buffer)
	updateCmd.SetOut(buf)
	updateCmd.SetErr(buf)

	// Save original flags & client state
	origCheck := updateCheckOnly
	origTarget := updateTargetVersion
	origForce := updateForce
	origNoRestart := updateNoRestart
	origClient := updateHTTPClient
	defer func() {
		updateCheckOnly = origCheck
		updateTargetVersion = origTarget
		updateForce = origForce
		updateNoRestart = origNoRestart
		updateHTTPClient = origClient
	}()

	updateCheckOnly = true
	updateTargetVersion = ""
	updateForce = false
	updateNoRestart = true
	updateHTTPClient = ts.Client()
	updateRepoURL = ts.URL + "/releases/latest"
	defer func() { updateRepoURL = "" }()

	// Direct call via RunE
	if err := updateCmd.RunE(updateCmd, []string{}); err != nil {
		t.Fatalf("updateCmd.RunE failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Checking for updates") {
		t.Errorf("expected checking for updates message, got: %s", out)
	}
	if !strings.Contains(out, "Latest version:  v0.2.0") {
		t.Errorf("expected latest version v0.2.0, got: %s", out)
	}
	if !strings.Contains(out, "A new version is available!") {
		t.Errorf("expected new version notification, got: %s", out)
	}
}

// TestUpdateCommandAlreadyUpToDate verifies that agw update reports already up-to-date when versions match.
func TestUpdateCommandAlreadyUpToDate(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			release := updater.GitHubRelease{
				TagName:     "v" + config.Version,
				PublishedAt: "2026-11-25T12:00:00Z",
			}
			_ = json.NewEncoder(w).Encode(release)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	buf := new(bytes.Buffer)
	updateCmd.SetOut(buf)
	updateCmd.SetErr(buf)

	origCheck := updateCheckOnly
	origTarget := updateTargetVersion
	origForce := updateForce
	origNoRestart := updateNoRestart
	origClient := updateHTTPClient
	defer func() {
		updateCheckOnly = origCheck
		updateTargetVersion = origTarget
		updateForce = origForce
		updateNoRestart = origNoRestart
		updateHTTPClient = origClient
	}()

	updateCheckOnly = false
	updateTargetVersion = ""
	updateForce = false
	updateNoRestart = true
	updateHTTPClient = ts.Client()
	updateRepoURL = ts.URL + "/releases/latest"
	defer func() { updateRepoURL = "" }()

	if err := updateCmd.RunE(updateCmd, []string{}); err != nil {
		t.Fatalf("updateCmd.RunE failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "You are already running the latest version") {
		t.Errorf("expected already up to date message, got: %s", out)
	}
}

// TestUpdateCommandExecutionWithMock verifies end-to-end download, checksum verification, and atomic application.
func TestUpdateCommandExecutionWithMock(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	fakeNewBinary := []byte("#!/bin/sh\necho 'new agw v9.9.9'\n")
	var tarGzBuf bytes.Buffer
	gw := gzip.NewWriter(&tarGzBuf)
	tw := tar.NewWriter(gw)
	_ = tw.WriteHeader(&tar.Header{Name: "agw", Size: int64(len(fakeNewBinary)), Mode: 0o755})
	_, _ = tw.Write(fakeNewBinary)
	_ = tw.Close()
	_ = gw.Close()

	archiveBytes := tarGzBuf.Bytes()
	hasher := sha256.New()
	hasher.Write(archiveBytes)
	validHash := hex.EncodeToString(hasher.Sum(nil))

	archiveName := updater.FormatArchiveName("9.9.9", runtime.GOOS, runtime.GOARCH)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			w.Header().Set("Content-Type", "application/json")
			release := updater.GitHubRelease{
				TagName:     "v9.9.9",
				PublishedAt: "2026-11-26T12:00:00Z",
				Assets: []updater.ReleaseAsset{
					{
						Name:               archiveName,
						BrowserDownloadURL: "https://example.com/" + archiveName,
					},
					{
						Name:               "SHA256SUMS",
						BrowserDownloadURL: "https://example.com/SHA256SUMS",
					},
				},
			}
			_ = json.NewEncoder(w).Encode(release)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/SHA256SUMS") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(fmt.Sprintf("%s  %s\n", validHash, archiveName)))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/"+archiveName) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(archiveBytes)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	// Direct download & verify integration test
	ctx := t.Context()
	rel, err := updater.CheckUpdate(ctx, ts.URL+"/releases/latest", config.Version, "", ts.Client())
	if err != nil {
		t.Fatalf("CheckUpdate failed: %v", err)
	}

	rel.ArchiveURL = ts.URL + "/" + archiveName
	rel.ChecksumsURL = ts.URL + "/SHA256SUMS"

	extracted, err := updater.DownloadAndVerify(ctx, rel, ts.Client())
	if err != nil {
		t.Fatalf("DownloadAndVerify failed: %v", err)
	}
	if !bytes.Equal(extracted, fakeNewBinary) {
		t.Errorf("binary content mismatch: got %q, want %q", string(extracted), string(fakeNewBinary))
	}

	// Apply replacement to a dummy binary file in temp dir
	targetBin := filepath.Join(tempHome, "agw")
	if err := os.WriteFile(targetBin, []byte("old binary"), 0o755); err != nil {
		t.Fatalf("writing old binary: %v", err)
	}

	if err := updater.ApplyBinary(targetBin, extracted); err != nil {
		t.Fatalf("ApplyBinary failed: %v", err)
	}

	upgradedBytes, err := os.ReadFile(targetBin)
	if err != nil {
		t.Fatalf("reading upgraded binary: %v", err)
	}
	if !bytes.Equal(upgradedBytes, fakeNewBinary) {
		t.Errorf("expected target binary to be updated with new content")
	}
}
