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

// TestWebUIUsesNonBlockingFeedback verifies the dashboard reports strategy results through the inline
// toast and pauses polling in background tabs instead of blocking on modal alert dialogs.
func TestWebUIUsesNonBlockingFeedback(t *testing.T) {
	if strings.Contains(IndexHTML, "alert(") {
		t.Error("dashboard must not use blocking alert() dialogs")
	}

	requiredSnippets := []string{
		"id=\"toast\"",
		"function showToast(",
		"visibilitychange",
		"function setConnectionState(",
	}
	for _, snippet := range requiredSnippets {
		if !strings.Contains(IndexHTML, snippet) {
			t.Errorf("expected dashboard to contain %q", snippet)
		}
	}
}

// TestWebUIModelFieldsMatchAPI verifies the dashboard reads the same JSON field names the daemon
// serializes for the model catalog, so token limits are never rendered as placeholders.
func TestWebUIModelFieldsMatchAPI(t *testing.T) {
	requiredFields := []string{"m.displayName", "m.contextWindow", "m.maxOutputTokens"}
	for _, field := range requiredFields {
		if !strings.Contains(IndexHTML, field) {
			t.Errorf("expected dashboard to read %q from the model payload", field)
		}
	}

	staleFields := []string{"m.display_name", "m.context_window", "m.max_output_tokens"}
	for _, field := range staleFields {
		if strings.Contains(IndexHTML, field) {
			t.Errorf("dashboard still reads %q which the API does not serialize", field)
		}
	}
}

// TestWebUIAccountMetricsAndQuotaWindows verifies the dashboard renders the account request counters,
// sanitized error text, manual quota refresh control, and rolling five-hour vs weekly quota windows.
func TestWebUIAccountMetricsAndQuotaWindows(t *testing.T) {
	requiredSnippets := []string{
		"a.requests",
		"a.rate_limits",
		"a.last_error",
		"data.account_quota",
		"fiveHour",
		"weekly",
		"id=\"quota-refresh\"",
		"function refreshQuota(",
		"/ui/api/refresh",
		".replace(/'/g, '&#39;')",
	}
	for _, snippet := range requiredSnippets {
		if !strings.Contains(IndexHTML, snippet) {
			t.Errorf("expected dashboard to contain %q", snippet)
		}
	}
}
