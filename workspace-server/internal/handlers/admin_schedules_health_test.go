package handlers

import (
	"testing"
	"time"
)

// ==================== computeStaleThreshold unit tests ====================

// TestComputeStaleThreshold_FiveMinuteCron verifies that "*/5 * * * *" produces
// a 600 s (2 × 5 min) stale threshold.
func TestComputeStaleThreshold_FiveMinuteCron(t *testing.T) {
	threshold, err := computeStaleThreshold("*/5 * * * *", "UTC", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = 600 * time.Second
	if threshold != want {
		t.Errorf("expected %v, got %v", want, threshold)
	}
}

// TestComputeStaleThreshold_HourlyCron verifies that "0 * * * *" produces
// a 7200 s (2 h) stale threshold.
func TestComputeStaleThreshold_HourlyCron(t *testing.T) {
	threshold, err := computeStaleThreshold("0 * * * *", "UTC", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = 2 * time.Hour
	if threshold != want {
		t.Errorf("expected %v, got %v", want, threshold)
	}
}

// TestComputeStaleThreshold_DailyCron verifies that "0 9 * * *" (09:00 UTC daily)
// produces a 48 h (2 × 24 h) stale threshold.
func TestComputeStaleThreshold_DailyCron(t *testing.T) {
	threshold, err := computeStaleThreshold("0 9 * * *", "UTC", time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	const want = 48 * time.Hour
	if threshold != want {
		t.Errorf("expected %v, got %v", want, threshold)
	}
}

// TestComputeStaleThreshold_InvalidCron verifies that a malformed cron expression
// returns an error rather than silently returning zero.
func TestComputeStaleThreshold_InvalidCron(t *testing.T) {
	_, err := computeStaleThreshold("not-a-cron", "UTC", time.Now())
	if err == nil {
		t.Error("expected error for invalid cron expression, got nil")
	}
}

// TestComputeStaleThreshold_InvalidTimezone verifies that an unknown timezone
// returns an error.
func TestComputeStaleThreshold_InvalidTimezone(t *testing.T) {
	_, err := computeStaleThreshold("*/5 * * * *", "Not/ATimezone", time.Now())
	if err == nil {
		t.Error("expected error for invalid timezone, got nil")
	}
}

// ==================== classifyScheduleStatus unit tests ====================

// TestClassifyScheduleStatus_NeverRun verifies nil last_run_at → "never_run".
func TestClassifyScheduleStatus_NeverRun(t *testing.T) {
	status := classifyScheduleStatus(nil, 10*time.Minute, time.Now())
	if status != "never_run" {
		t.Errorf("expected never_run, got %q", status)
	}
}

// TestClassifyScheduleStatus_Stale verifies that a run older than the threshold
// produces "stale".
func TestClassifyScheduleStatus_Stale(t *testing.T) {
	now := time.Now()
	lastRun := now.Add(-11 * time.Minute) // older than 10-min threshold
	status := classifyScheduleStatus(&lastRun, 10*time.Minute, now)
	if status != "stale" {
		t.Errorf("expected stale, got %q", status)
	}
}

// TestClassifyScheduleStatus_OK verifies that a run within the threshold → "ok".
func TestClassifyScheduleStatus_OK(t *testing.T) {
	now := time.Now()
	lastRun := now.Add(-4 * time.Minute) // within 10-min threshold
	status := classifyScheduleStatus(&lastRun, 10*time.Minute, now)
	if status != "ok" {
		t.Errorf("expected ok, got %q", status)
	}
}

// TestClassifyScheduleStatus_ZeroThreshold_NeverStale verifies that when
// the threshold is 0 (cron parse failed), a run is never classified as stale
// — we degrade gracefully rather than false-alarming.
func TestClassifyScheduleStatus_ZeroThreshold_NeverStale(t *testing.T) {
	now := time.Now()
	lastRun := now.Add(-365 * 24 * time.Hour) // very old run
	status := classifyScheduleStatus(&lastRun, 0, now)
	if status != "ok" {
		t.Errorf("expected ok (zero threshold = no stale detection), got %q", status)
	}
}
