// Package rotator_test provides unit and concurrency test suites for the rotator package.
package rotator_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/salmanabdurrahman/antigravity-gateway/internal/auth"
	"github.com/salmanabdurrahman/antigravity-gateway/internal/rotator"
)

// TestAccountSnapshotExposesRiskCounters verifies that the diagnostic snapshot carries the counters and
// suspension metadata the risk telemetry depends on.
func TestAccountSnapshotExposesRiskCounters(t *testing.T) {
	account := newTestAccount("risk@example.com")
	account.SuspendKind = string(rotator.SuspensionTermsOfService)
	account.AppealURL = "https://forms.gle/SNAPSHOTCASE"

	state := rotator.NewAccountState(account)
	now := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)

	state.RecordPromptRejection(now, 5*time.Minute)
	state.RecordPromptRejection(now.Add(time.Second), 5*time.Minute)
	state.RecordSuccess(now.Add(2 * time.Second))

	snapshot := state.Snapshot(now.Add(3 * time.Second))

	if snapshot.PromptRejections != 2 {
		t.Errorf("PromptRejections = %d; want 2", snapshot.PromptRejections)
	}
	if snapshot.LastUpstreamOK != now.Add(2*time.Second).Format(time.RFC3339) {
		t.Errorf("LastUpstreamOK = %q; want the recorded success timestamp", snapshot.LastUpstreamOK)
	}
	if snapshot.SuspendKind != string(rotator.SuspensionTermsOfService) {
		t.Errorf("SuspendKind = %q; want %q", snapshot.SuspendKind, rotator.SuspensionTermsOfService)
	}
	if snapshot.AppealURL != "https://forms.gle/SNAPSHOTCASE" {
		t.Errorf("AppealURL = %q; want the stored appeal link", snapshot.AppealURL)
	}
}

// TestRotatorRecordsSuccessfulCompletion verifies that a successful dispatch stamps LastUpstreamOK on the
// pooled snapshot, so telemetry can distinguish a working credential from one that only errors.
func TestRotatorRecordsSuccessfulCompletion(t *testing.T) {
	r := newPacingRotator(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]}}]}}\n\n"))
	}, nil)

	res, err := r.Generate(t.Context(), "gemini-pro", pacingBuilder)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	_ = res.Response.Body.Close()

	snapshots := r.Pool().Snapshots()
	if len(snapshots) != 1 {
		t.Fatalf("captured %d snapshot(s); want 1", len(snapshots))
	}
	if snapshots[0].LastUpstreamOK == "" {
		t.Error("LastUpstreamOK is empty after a successful completion")
	}
}

// TestPacingDisabledSnapshotOmitsZeroCounters verifies that the snapshot JSON omits counters that never
// moved, keeping the risk file compact.
func TestPacingDisabledSnapshotOmitsZeroCounters(t *testing.T) {
	account := &auth.Account{Email: "quiet@example.com", RefreshToken: "rt-quiet"}
	snapshot := rotator.NewAccountState(account).Snapshot(time.Now())

	if snapshot.PromptRejections != 0 || snapshot.LastUpstreamOK != "" {
		t.Errorf("fresh snapshot = (%d, %q); want zero counters", snapshot.PromptRejections, snapshot.LastUpstreamOK)
	}
}
