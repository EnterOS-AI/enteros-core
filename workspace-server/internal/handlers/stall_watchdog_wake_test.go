package handlers

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// stall_watchdog_wake_test.go — coverage for the wake-lifecycle GENERATION LOOP
// integration in the stall probe (PR-C), the ONE live desired-generation bump
// point this PR wires. Asserts the probe, when the wake hooks are wired:
//
//   - routes the fired probe through DecideWake with kind=WakeStall;
//   - on Fire=true uses the decision's IdempotencyKey as the EnqueueA2A
//     idempotency arg and marks the wake delivered after a successful enqueue;
//   - on Fire=false (duplicate within the dedup window) skips the enqueue AND the
//     'probed' state-row write, so a duplicate never advances the generation.
//
// The DecideWake/MarkWakeDelivered DB behaviour (the actual +1 bump + ON CONFLICT
// dedup) is proven against a real engine in stall_watchdog_wake_integration_test.go
// and wake_lifecycle_integration_test.go; here the hooks are injected fakes so the
// gating logic is asserted in isolation with sqlmock.

type recordedWakeDecider struct {
	calls   int
	lastWS  string
	lastKnd WakeKind
	lastSd  string
	ret     WakeDecision
}

func (r *recordedWakeDecider) decide(_ context.Context, ws string, kind WakeKind, seed string) (WakeDecision, error) {
	r.calls++
	r.lastWS = ws
	r.lastKnd = kind
	r.lastSd = seed
	return r.ret, nil
}

type recordedWakeDeliver struct {
	calls   int
	lastWS  string
	lastKey string
}

func (r *recordedWakeDeliver) deliver(_ context.Context, ws, key string) error {
	r.calls++
	r.lastWS = ws
	r.lastKey = key
	return nil
}

// --- Fire=true: probe uses the decision key + marks delivered ---

func TestStallProbe_WakeFire_UsesDecisionKeyAndMarksDelivered(t *testing.T) {
	mock := setupTestDB(t)
	sw, enq, _ := newTestStallWatchdog(t)

	const ws = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const wakeKey = "stall:aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1700000000"
	decider := &recordedWakeDecider{ret: WakeDecision{Fire: true, IdempotencyKey: wakeKey, Generation: 1}}
	deliver := &recordedWakeDeliver{}
	sw.SetWakeHooks(decider.decide, deliver.deliver)

	staleAct := time.Now().Add(-20 * time.Minute)
	mock.ExpectQuery(stallRows).
		WillReturnRows(sqlmock.NewRows(stallCols()).AddRow(ws, staleAct, nil, nil, nil, nil))
	mock.ExpectExec(`INSERT INTO workspace_stall_state`).
		WithArgs(ws, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO activity_logs`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res := sw.Sweep(context.Background())

	if res.Probed != 1 || res.Errors != 0 {
		t.Fatalf("expected exactly one probe; got %+v", res)
	}
	if decider.calls != 1 || decider.lastKnd != WakeStall || decider.lastWS != ws {
		t.Errorf("DecideWake: calls=%d kind=%q ws=%q, want 1 / %q / %q", decider.calls, decider.lastKnd, decider.lastWS, WakeStall, ws)
	}
	if decider.lastSd == "" {
		t.Errorf("expected a non-empty dedup seed (the hourly bucket)")
	}
	if enq.idemKey != wakeKey {
		t.Errorf("enqueue idempotency key = %q, want the decision key %q", enq.idemKey, wakeKey)
	}
	if deliver.calls != 1 || deliver.lastKey != wakeKey {
		t.Errorf("MarkWakeDelivered: calls=%d key=%q, want 1 / %q", deliver.calls, deliver.lastKey, wakeKey)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// --- Fire=false: duplicate wake skips enqueue AND the state-row write ---

func TestStallProbe_WakeNoFire_SkipsEnqueueAndStateWrite(t *testing.T) {
	mock := setupTestDB(t)
	sw, enq, _ := newTestStallWatchdog(t)

	const ws = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	decider := &recordedWakeDecider{ret: WakeDecision{Fire: false}}
	deliver := &recordedWakeDeliver{}
	sw.SetWakeHooks(decider.decide, deliver.deliver)

	staleAct := time.Now().Add(-20 * time.Minute)
	// Only the sweep SELECT is expected — a no-fire decision must NOT write the
	// 'probed' state row (no INSERT expectation), so an accidental write fails.
	mock.ExpectQuery(stallRows).
		WillReturnRows(sqlmock.NewRows(stallCols()).AddRow(ws, staleAct, nil, nil, nil, nil))

	res := sw.Sweep(context.Background())

	if res.Probed != 0 || res.Errors != 0 {
		t.Fatalf("duplicate wake must be a no-op probe; got %+v", res)
	}
	if decider.calls != 1 {
		t.Errorf("DecideWake calls = %d, want 1", decider.calls)
	}
	if enq.calls != 0 {
		t.Errorf("no-fire must skip the enqueue; got %d", enq.calls)
	}
	if deliver.calls != 0 {
		t.Errorf("no-fire must not mark delivered; got %d", deliver.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet (an INSERT fired despite no-fire?): %v", err)
	}
}
