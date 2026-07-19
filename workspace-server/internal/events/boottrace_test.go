package events

// boottrace_test.go — unit coverage for the per-Broadcaster boot-telemetry
// trace (boottrace.go). Exercises BootTrace.Observe directly: it is the
// exact surface both Broadcaster taps call, so these tests pin the append /
// reset / drop lifecycle without standing up the WS hub, Postgres or Redis.

import (
	"fmt"
	"testing"
)

func bootPayload(step, total int, key, label, status, message string) map[string]interface{} {
	return map[string]interface{}{
		"workspace_id": "ws-1",
		"step":         step,
		"total":        total,
		"key":          key,
		"label":        label,
		"status":       status,
		"message":      message,
	}
}

func TestBootTrace_AppendsAndReplays(t *testing.T) {
	tr := NewBootTrace()

	tr.Observe(string(EventBootStep), "ws-1",
		bootPayload(1, 8, "PWR", "Provision compute", "running", "building image"))
	tr.Observe(string(EventBootStep), "ws-1",
		bootPayload(1, 8, "PWR", "Provision compute", "running", "still building — 20s elapsed"))
	tr.Observe(string(EventBootStep), "ws-2",
		bootPayload(2, 8, "RT", "Start runtime", "ok", ""))

	got := tr.StepsFor("ws-1")
	if len(got) != 2 {
		t.Fatalf("ws-1 history length = %d, want 2", len(got))
	}
	if got[0].Message != "building image" || got[1].Message != "still building — 20s elapsed" {
		t.Errorf("history out of order: %+v", got)
	}
	if got[0].At == 0 {
		t.Errorf("At not stamped: %+v", got[0])
	}
	if n := len(tr.StepsFor("ws-2")); n != 1 {
		t.Errorf("ws-2 history length = %d, want 1 (no cross-workspace bleed)", n)
	}
}

func TestBootTrace_LifecycleEventsDropHistory(t *testing.T) {
	// A reprovision starts a NEW boot generation; online / provision-failed
	// end the boot; removal frees the workspace. All four must drop the
	// history — retaining finished boots would grow server memory with the
	// workspace count for a replay List never serves.
	for _, ev := range []EventType{
		EventWorkspaceProvisioning,
		EventWorkspaceOnline,
		EventWorkspaceProvisionFailed,
		EventWorkspaceRemoved,
	} {
		tr := NewBootTrace()
		tr.Observe(string(EventBootStep), "ws-1",
			bootPayload(1, 8, "PWR", "Provision compute", "ok", ""))
		tr.Observe(string(ev), "ws-1", map[string]interface{}{"name": "WS"})
		if got := tr.StepsFor("ws-1"); got != nil {
			t.Errorf("history after %s = %+v, want nil", ev, got)
		}
	}
}

func TestBootTrace_GenerationMarker(t *testing.T) {
	tr := NewBootTrace()

	// No entry at all → known=false (server restarted / never observed):
	// clients keep their local state.
	if _, gen, known := tr.Replay("ws-1"); known || gen != "" {
		t.Fatalf("empty registry: known=%v gen=%q, want unknown", known, gen)
	}

	// First observed step mints a generation; later steps keep it.
	tr.Observe(string(EventBootStep), "ws-1",
		bootPayload(1, 8, "PWR", "Provision compute", "running", "a"))
	_, gen1, known := tr.Replay("ws-1")
	if !known || gen1 == "" {
		t.Fatalf("after first step: known=%v gen=%q, want a generation", known, gen1)
	}
	tr.Observe(string(EventBootStep), "ws-1",
		bootPayload(1, 8, "PWR", "Provision compute", "running", "b"))
	if _, gen, _ := tr.Replay("ws-1"); gen != gen1 {
		t.Errorf("same boot changed generation: %q -> %q", gen1, gen)
	}

	// A reprovision keeps an ENTRY (authoritative empty) with a FRESH
	// generation — that is the "drop your stale state" signal clients need
	// even before the new boot's first step lands.
	tr.Observe(string(EventWorkspaceProvisioning), "ws-1", map[string]interface{}{"name": "WS"})
	steps, gen2, known2 := tr.Replay("ws-1")
	if !known2 || len(steps) != 0 {
		t.Fatalf("after reprovision: known=%v steps=%d, want known empty entry", known2, len(steps))
	}
	if gen2 == gen1 || gen2 == "" {
		t.Errorf("reprovision must mint a fresh generation: %q -> %q", gen1, gen2)
	}

	// Boot end frees the entry entirely.
	tr.Observe(string(EventWorkspaceOnline), "ws-1", nil)
	if _, _, known := tr.Replay("ws-1"); known {
		t.Errorf("entry retained after ONLINE")
	}
}

func TestBootTrace_RejectsMalformedPayloads(t *testing.T) {
	tr := NewBootTrace()

	// Not a map, missing fields, out-of-range step/total, empty status —
	// none of these may land as a replayable record.
	tr.Observe(string(EventBootStep), "ws-1", "not-a-map")
	tr.Observe(string(EventBootStep), "ws-1", map[string]interface{}{"step": 1})
	tr.Observe(string(EventBootStep), "ws-1",
		bootPayload(0, 8, "PWR", "x", "running", ""))
	tr.Observe(string(EventBootStep), "ws-1",
		bootPayload(5, 3, "PWR", "x", "running", ""))
	tr.Observe(string(EventBootStep), "ws-1",
		bootPayload(1, 8, "", "x", "running", ""))

	if got := tr.StepsFor("ws-1"); got != nil {
		t.Errorf("malformed payloads recorded: %+v", got)
	}
}

func TestBootTrace_CoercesJSONNumbers(t *testing.T) {
	tr := NewBootTrace()

	// A payload that round-tripped through JSON carries float64 numerics.
	tr.Observe(string(EventBootStep), "ws-1", map[string]interface{}{
		"step": float64(3), "total": float64(8),
		"key": "MCP", "label": "Management MCP", "status": "running",
	})
	got := tr.StepsFor("ws-1")
	if len(got) != 1 || got[0].Step != 3 || got[0].Total != 8 {
		t.Fatalf("float64 payload not coerced: %+v", got)
	}
}

func TestBootTrace_CapsHistoryDroppingOldest(t *testing.T) {
	tr := NewBootTrace()

	for i := 0; i < maxBootTraceLen+25; i++ {
		tr.Observe(string(EventBootStep), "ws-1",
			bootPayload(1, 8, "PWR", "Provision compute", "running", fmt.Sprintf("tick %d", i)))
	}
	got := tr.StepsFor("ws-1")
	if len(got) != maxBootTraceLen {
		t.Fatalf("capped length = %d, want %d", len(got), maxBootTraceLen)
	}
	// Oldest dropped, newest kept.
	if got[len(got)-1].Message != fmt.Sprintf("tick %d", maxBootTraceLen+24) {
		t.Errorf("newest entry lost: %+v", got[len(got)-1])
	}
	if got[0].Message != "tick 25" {
		t.Errorf("head should be the 26th tick, got %+v", got[0])
	}
}

func TestBootTrace_ReplayIsACopy(t *testing.T) {
	tr := NewBootTrace()

	tr.Observe(string(EventBootStep), "ws-1",
		bootPayload(1, 8, "PWR", "Provision compute", "running", "original"))
	got := tr.StepsFor("ws-1")
	got[0].Message = "mutated"
	if again := tr.StepsFor("ws-1"); again[0].Message != "original" {
		t.Errorf("StepsFor leaked internal slice: %+v", again[0])
	}
}
