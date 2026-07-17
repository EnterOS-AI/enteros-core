package provisioner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// recordingEmitter captures emitBootStep calls thread-safely (the build
// heartbeat emits from a goroutine).
type recordingEmitter struct {
	mu    sync.Mutex
	lines []string // "<status>|<message>"
}

func (r *recordingEmitter) fn(_ string, status, message string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, status+"|"+message)
}

func (r *recordingEmitter) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// TestEmitBootStepNilSafe pins that an unwired provisioner (no emitter, or a
// nil receiver) never panics — production constructs the emitter only on the
// self-hosted Docker path.
func TestEmitBootStepNilSafe(t *testing.T) {
	var nilP *Provisioner
	nilP.emitBootStep("ws", "running", "msg") // must not panic
	(&Provisioner{}).emitBootStep("ws", "running", "msg")
}

// TestBuildTelemetrySlowBuild pins the telemetry contract around a slow local
// build: an immediate "building" line, at least one heartbeat while the build
// runs, and a completion line — the sequence that keeps the canvas watchdog
// alive during a multi-minute first-boot image build.
func TestBuildTelemetrySlowBuild(t *testing.T) {
	origHook, origInterval := ensureLocalImageHook, buildHeartbeatInterval
	t.Cleanup(func() { ensureLocalImageHook, buildHeartbeatInterval = origHook, origInterval })

	buildHeartbeatInterval = 5 * time.Millisecond
	ensureLocalImageHook = func(ctx context.Context, runtime string) (string, error) {
		time.Sleep(40 * time.Millisecond) // several heartbeat intervals
		return "molecule-local/workspace-template-" + runtime + ":test", nil
	}

	rec := &recordingEmitter{}
	p := &Provisioner{bootStep: rec.fn}
	tag, err := p.buildLocalImageWithTelemetry(context.Background(), "ws-test", "hermes")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if tag != "molecule-local/workspace-template-hermes:test" {
		t.Fatalf("unexpected tag %q", tag)
	}

	lines := rec.snapshot()
	if len(lines) < 3 {
		t.Fatalf("want >=3 telemetry lines (building, heartbeat(s), done), got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "building hermes runtime image") {
		t.Fatalf("first line must announce the build, got %q", lines[0])
	}
	sawHeartbeat := false
	for _, l := range lines[1 : len(lines)-1] {
		if strings.Contains(l, "still building") {
			sawHeartbeat = true
		}
	}
	if !sawHeartbeat {
		t.Fatalf("no heartbeat line during a slow build: %v", lines)
	}
	last := lines[len(lines)-1]
	// A 40ms "build" is under the reuse threshold, so the completion line is
	// the reuse variant; both variants are terminal "the image exists" lines.
	if !strings.Contains(last, "already built") && !strings.Contains(last, "ready in") {
		t.Fatalf("last line must report the image exists, got %q", last)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "running|") {
			t.Fatalf("provisioning telemetry must stay status=running (runtime owns ok), got %q", l)
		}
	}
}

// TestBuildTelemetryFailure pins that a failed build stops the heartbeat and
// emits no completion line — the workspace-status failure path owns the
// user-facing error.
func TestBuildTelemetryFailure(t *testing.T) {
	origHook, origInterval := ensureLocalImageHook, buildHeartbeatInterval
	t.Cleanup(func() { ensureLocalImageHook, buildHeartbeatInterval = origHook, origInterval })

	buildHeartbeatInterval = time.Hour // no heartbeats in a fast failure
	ensureLocalImageHook = func(ctx context.Context, runtime string) (string, error) {
		return "", errors.New("boom")
	}

	rec := &recordingEmitter{}
	p := &Provisioner{bootStep: rec.fn}
	if _, err := p.buildLocalImageWithTelemetry(context.Background(), "ws-test", "hermes"); err == nil {
		t.Fatal("want error")
	}
	lines := rec.snapshot()
	if len(lines) != 1 || !strings.Contains(lines[0], "building hermes") {
		t.Fatalf("failed build must emit only the initial building line, got %v", lines)
	}
}
