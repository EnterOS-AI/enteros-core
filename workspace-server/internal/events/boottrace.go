package events

// boottrace.go — in-memory per-workspace boot-telemetry history.
//
// BOOT_STEP is a BroadcastOnly (never structure_events-recorded) presentation
// event, which historically meant the canvas boot screen lost its entire
// watchdog log whenever the client re-hydrated from GET /workspaces — a page
// reload, a WS reconnect, a tab-visibility wake or the 30s silence health
// check all reset the screen to "watchdog attached · waiting for boot
// telemetry" mid-boot. This trace keeps the history the broadcast path
// already carries so the List handler can replay it: every BOOT_STEP that
// flows through a *Broadcaster is appended to its BootTrace, and
// GET /workspaces attaches the history as `boot_steps` on rows whose status
// is `provisioning` (workspace.go List, via the Broadcaster.BootStepsFor
// capability).
//
// Lifecycle (Observe, called by the Broadcaster taps):
//   - append — every EventBootStep.
//   - reset  — a WORKSPACE_PROVISIONING event starts a NEW boot generation
//     (create, restart, provider switch, org import all funnel through
//     RecordAndBroadcast with that type), so stale history from the previous
//     boot never bleeds into the new boot screen.
//   - drop   — WORKSPACE_ONLINE / WORKSPACE_PROVISION_FAILED (the boot is
//     over; List only serves the replay while status is `provisioning`, so
//     retaining finished boots would just grow server memory with the
//     workspace count) and WORKSPACE_REMOVED. Late BOOT_STEPs posted after
//     the online flip re-accumulate a few entries; they are bounded by the
//     cap and cleared at the next provisioning/removal.
//
// Deliberately in-memory and per-instance: history is a boot-time UX replay,
// not an audit record (molecule-platform.log stays the durable trail).
// KNOWN LIMITATION — multi-instance deployments: only the instance that
// processes an event observes it, so an instance that never saw the
// lifecycle reset can serve a previous boot's trace, and an instance that
// saw no BOOT_STEPs serves an empty one (which the canvas treats as
// authoritative and clears its local log — it then re-accumulates from the
// live WS stream, the same behavior every hydrate had before this feature
// existed). Making the trace cross-instance-consistent needs the Redis
// event bus with origin filtering; not worth it for a boot-time UX replay.

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// BootStepRecord is one observed BOOT_STEP, in wire order. JSON tags mirror
// the broadcast payload (boot_event.go / cmd/server main.go) plus `at`, the
// server-side observation timestamp in unix milliseconds — the canvas uses it
// to derive stable per-line offsets that survive client remounts.
type BootStepRecord struct {
	Step    int    `json:"step"`
	Total   int    `json:"total"`
	Key     string `json:"key"`
	Label   string `json:"label"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	At      int64  `json:"at"`
}

// maxBootTraceLen caps the per-workspace history. A normal boot is ~20 lines
// (8 steps + build heartbeats); 300 gives a pathological runtime plenty of
// room without letting a hot loop grow memory unbounded. Oldest lines drop
// first — the tail is what the boot screen shows anyway. Deliberately equal
// to the canvas's MAX_BOOT_LOG_LINES (boot-telemetry.ts): records beyond the
// client cap would be serialized on every List poll only to be discarded on
// arrival.
const maxBootTraceLen = 300

// bootTraceEntry is one workspace's live boot: its records plus the
// GENERATION marker — a fresh opaque id minted whenever a new boot begins
// (the WORKSPACE_PROVISIONING reset, or the first BOOT_STEP observed with no
// entry). The canvas uses it to distinguish "same boot, merge the replay
// with what I streamed live" from "new boot, discard my stale state" —
// a distinction timestamps cannot make across server/client clocks.
type bootTraceEntry struct {
	gen   string
	steps []BootStepRecord
}

// BootTrace is the per-Broadcaster boot-telemetry registry. Instance state
// (not package globals) so tests construct their own and two Broadcasters
// never share history.
type BootTrace struct {
	mu sync.Mutex
	m  map[string]*bootTraceEntry
}

// NewBootTrace returns an empty registry.
func NewBootTrace() *BootTrace {
	return &BootTrace{m: map[string]*bootTraceEntry{}}
}

// bootGenSeq disambiguates generations minted within one clock tick —
// time.Now alone collides under Windows' coarse clock granularity.
var bootGenSeq atomic.Uint64

func newBootGeneration() string {
	// Timestamp (fresh across instance restarts) + per-instance counter
	// (unique within one instance regardless of clock granularity).
	return fmt.Sprintf("g%d-%d", time.Now().UnixNano(), bootGenSeq.Add(1))
}

// Observe applies one outgoing event to the trace. Keys purely on eventType
// so none of the many WORKSPACE_PROVISIONING call sites need to know the
// trace exists. Callers (the *Broadcaster taps) MUST invoke it only for
// events that actually go out — a failed RecordAndBroadcast persist must not
// wipe a live boot's history.
func (t *BootTrace) Observe(eventType, workspaceID string, payload interface{}) {
	switch EventType(eventType) {
	case EventBootStep:
		rec, ok := bootStepRecordFromPayload(payload)
		if !ok {
			return
		}
		rec.At = time.Now().UnixMilli()
		t.mu.Lock()
		defer t.mu.Unlock()
		e := t.m[workspaceID]
		if e == nil {
			e = &bootTraceEntry{gen: newBootGeneration()}
			t.m[workspaceID] = e
		}
		e.steps = append(e.steps, rec)
		if len(e.steps) > maxBootTraceLen {
			e.steps = e.steps[len(e.steps)-maxBootTraceLen:]
		}
	case EventWorkspaceProvisioning:
		// New boot generation: keep an ENTRY (empty steps, fresh gen) so the
		// replay tells clients "this is a different boot — drop stale state"
		// even before the first step lands.
		t.mu.Lock()
		defer t.mu.Unlock()
		t.m[workspaceID] = &bootTraceEntry{gen: newBootGeneration()}
	case EventWorkspaceOnline,
		EventWorkspaceProvisionFailed,
		EventWorkspaceRemoved:
		// Boot finished (either way) or workspace gone — drop the history
		// (see lifecycle in the file header).
		t.mu.Lock()
		defer t.mu.Unlock()
		delete(t.m, workspaceID)
	}
}

// Replay returns a copy of the workspace's observed boot history in arrival
// order plus its generation marker. known=false (and gen "") when this
// instance holds no entry at all — e.g. it restarted mid-boot, or another
// instance observed the boot — which clients treat as "server knows
// nothing; keep local state" rather than an authoritative empty.
func (t *BootTrace) Replay(workspaceID string) (steps []BootStepRecord, gen string, known bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.m[workspaceID]
	if e == nil {
		return nil, "", false
	}
	out := make([]BootStepRecord, len(e.steps))
	copy(out, e.steps)
	return out, e.gen, true
}

// StepsFor returns a copy of the workspace's observed boot history in
// arrival order (nil when none). Kept for callers that don't need the
// generation.
func (t *BootTrace) StepsFor(workspaceID string) []BootStepRecord {
	steps, _, _ := t.Replay(workspaceID)
	if len(steps) == 0 {
		return nil
	}
	return steps
}

// bootStepRecordFromPayload extracts a record from the broadcast payload map.
// Both emitters (boot_event.go, cmd/server main.go) build the same
// map[string]interface{} shape with int step/total; the numeric coercion also
// tolerates float64 in case a payload ever round-trips through JSON first.
func bootStepRecordFromPayload(payload interface{}) (BootStepRecord, bool) {
	m, ok := payload.(map[string]interface{})
	if !ok {
		return BootStepRecord{}, false
	}
	step, okStep := asInt(m["step"])
	total, okTotal := asInt(m["total"])
	key, _ := m["key"].(string)
	label, _ := m["label"].(string)
	status, _ := m["status"].(string)
	message, _ := m["message"].(string)
	if !okStep || !okTotal || step < 1 || total < step || key == "" || label == "" || status == "" {
		return BootStepRecord{}, false
	}
	return BootStepRecord{
		Step:    step,
		Total:   total,
		Key:     key,
		Label:   label,
		Status:  status,
		Message: message,
	}, true
}

func asInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	default:
		return 0, false
	}
}
