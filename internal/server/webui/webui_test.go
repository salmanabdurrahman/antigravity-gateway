// Package webui contains the embedded static dashboard assets for local gateway monitoring.
package webui

import (
	"strings"
	"testing"
)

// TestEmbeddedWebUIAssets verifies that the embedded dashboard HTML is present and contains required UI markup.
func TestEmbeddedWebUIAssets(t *testing.T) {
	if len(IndexHTML) == 0 {
		t.Fatal("expected embedded IndexHTML to be non-empty")
	}

	expectedSnippets := []string{
		"<!DOCTYPE html>",
		"<title>Antigravity Gateway Dashboard</title>",
		"id=\"strategy-select\"",
		"id=\"accounts-body\"",
		"id=\"models-body\"",
		"agw setup claude",
	}

	for _, snippet := range expectedSnippets {
		if !strings.Contains(IndexHTML, snippet) {
			t.Errorf("expected embedded HTML to contain snippet %q", snippet)
		}
	}
}
