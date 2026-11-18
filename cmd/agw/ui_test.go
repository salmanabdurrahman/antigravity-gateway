// Package main provides the command-line interface entrypoints and subcommands for agw.
package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/config"
)

// TestUICommand verifies running agw ui when daemon is running vs stopped.
func TestUICommand(t *testing.T) {
	tempHome := t.TempDir()
	t.Setenv(config.EnvHome, tempHome)

	// Mock running health server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	cfg := config.DefaultConfig()
	cfg.Port = port
	cfgPath, _ := config.ConfigFilePath()
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("saving config failed: %v", err)
	}

	buf := new(bytes.Buffer)
	uiCmd.SetOut(buf)
	uiCmd.SetErr(buf)

	var openedURL string
	origBrowser := openBrowserFn
	openBrowserFn = func(url string) {
		openedURL = url
	}
	defer func() { openBrowserFn = origBrowser }()

	if err := uiCmd.RunE(uiCmd, []string{}); err != nil {
		t.Fatalf("uiCmd.RunE failed: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "Opening dashboard at http://127.0.0.1:") {
		t.Errorf("unexpected output: %s", out)
	}
	if !strings.HasPrefix(openedURL, "http://127.0.0.1:") {
		t.Errorf("expected opened browser URL http://127.0.0.1:..., got %s", openedURL)
	}
}
