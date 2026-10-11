package logging

import (
	"strings"
	"testing"
	"time"
)

func TestRepeatTrackerSurfacesFirstFailureImmediately(t *testing.T) {
	var r RepeatTracker
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)

	if got := r.Observe("pci probe failed", now); got != "pci probe failed" {
		t.Errorf("first observation returned %q, want the message itself", got)
	}
}

func TestRepeatTrackerSuppressesUntilSummary(t *testing.T) {
	var r RepeatTracker
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	r.Observe("pci probe failed", now)

	for i := 1; i < summaryEvery; i++ {
		now = now.Add(time.Minute)
		if got := r.Observe("pci probe failed", now); got != "" {
			t.Fatalf("observation %d returned %q, want suppression", i, got)
		}
	}

	now = now.Add(time.Minute)
	got := r.Observe("pci probe failed", now)
	if got == "" {
		t.Fatal("expected a summary line after the suppression window")
	}
	if !strings.Contains(got, "still failing") {
		t.Errorf("summary = %q, want it to mention that it is still failing", got)
	}
	if !strings.Contains(got, "11 attempts") {
		t.Errorf("summary = %q, want an attempt count of 11", got)
	}
}

func TestRepeatTrackerSurfacesChangedMessage(t *testing.T) {
	var r RepeatTracker
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	r.Observe("pci probe failed", now)

	now = now.Add(time.Minute)
	if got := r.Observe("pci probe timed out", now); got != "pci probe timed out" {
		t.Errorf("changed message returned %q, want it surfaced immediately", got)
	}
}

func TestRepeatTrackerReportsRecoveryOnce(t *testing.T) {
	var r RepeatTracker
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	r.Observe("pci probe failed", now)
	now = now.Add(2 * time.Minute)

	got := r.Recovered(now)
	if !strings.Contains(got, "recovered") {
		t.Fatalf("Recovered = %q, want a recovery line", got)
	}
	if again := r.Recovered(now); again != "" {
		t.Errorf("second Recovered = %q, want silence", again)
	}
}

func TestRepeatTrackerStaysSilentWhenNothingFailed(t *testing.T) {
	var r RepeatTracker
	if got := r.Recovered(time.Now()); got != "" {
		t.Errorf("Recovered on a healthy tracker = %q, want silence", got)
	}
}

func TestRepeatTrackerRecoveryReportsTotalAttempts(t *testing.T) {
	var r RepeatTracker
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	r.Observe("pci probe failed", now)

	now = now.Add(time.Minute)
	r.Observe("pci probe failed", now)

	now = now.Add(time.Minute)
	r.Observe("pci probe failed", now)

	now = now.Add(time.Minute)
	got := r.Recovered(now)
	if !strings.Contains(got, "3 failed attempts") {
		t.Errorf("Recovered = %q, want it to contain '3 failed attempts'", got)
	}
}
