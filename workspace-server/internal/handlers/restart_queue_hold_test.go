package handlers

// Tests for the post-restart A2A queue hold (core#5241): queued turns wait
// until the container an automatic restart brought up has stayed ready for
// restartQueueHoldGrace, instead of being drained into it the moment it
// reports ready.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const holdReadSQL = "SELECT status, last_heartbeat_at FROM workspaces WHERE id = $1"

// holdArmedAt reads when wsID's queue hold was armed (zero: no hold).
func holdArmedAt(wsID string) time.Time {
	sv, ok := restartStates.Load(wsID)
	if !ok {
		return time.Time{}
	}
	state := sv.(*restartState)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.queueHeldSince
}

func expectHoldRead(mock sqlmock.Sqlmock, wsID, status string, heartbeat interface{}) {
	mock.ExpectQuery(holdReadSQL).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "last_heartbeat_at"}).AddRow(status, heartbeat))
}

type holdStep struct {
	status    string
	heartbeat interface{}
	held      bool
	why       string
}

func runHoldSteps(t *testing.T, mock sqlmock.Sqlmock, wsID string, steps []holdStep) {
	t.Helper()
	for i, s := range steps {
		expectHoldRead(mock, wsID, s.status, s.heartbeat)
		if got := holdQueueAfterRestart(context.Background(), wsID); got != s.held {
			t.Fatalf("step %d (%s, heartbeat %v): held=%v, want %v — %s", i, s.status, s.heartbeat, got, s.held, s.why)
		}
	}
}

func TestHoldQueueAfterRestart_NoRestartNoHold(t *testing.T) {
	const wsID = "hold-never-restarted"
	isolateRestartState(t, wsID)
	_ = setupTestDBForQueueTests(t)

	if holdQueueAfterRestart(context.Background(), wsID) {
		t.Fatal("a workspace no restart has touched must drain as before")
	}
	if _, ok := restartStates.Load(wsID); ok {
		t.Fatal("checking the hold created restart state for a workspace that has none")
	}
}

func TestHoldQueueAfterRestart_ReleasesOnlyOnceTheReplacementStayedReady(t *testing.T) {
	const wsID = "hold-steady"
	isolateRestartState(t, wsID)
	mock := setupTestDBForQueueTests(t)
	armRestartQueueHold(wsID)
	armedAt := holdArmedAt(wsID)
	ready := armedAt.Add(5 * time.Second)

	runHoldSteps(t, mock, wsID, []holdStep{
		{"provisioning", nil, true, "the replacement has not reported in yet"},
		{"online", armedAt.Add(-time.Second), true, "a heartbeat from before the restart belongs to the old container"},
		{"online", ready, true, "the first ready heartbeat starts the grace"},
		{"online", ready.Add(restartQueueHoldGrace - time.Second), true, "still inside the grace"},
		{"degraded", ready.Add(restartQueueHoldGrace), false, "ready (degraded counts) for the whole grace"},
	})

	if heldSince := holdArmedAt(wsID); !heldSince.IsZero() {
		t.Fatal("a released hold must not be consulted again until the next restart")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestHoldQueueAfterRestart_LeavingReadyStartsTheGraceOver(t *testing.T) {
	const wsID = "hold-flapping"
	isolateRestartState(t, wsID)
	mock := setupTestDBForQueueTests(t)
	armRestartQueueHold(wsID)
	armedAt := holdArmedAt(wsID)
	first := armedAt.Add(5 * time.Second)
	back := first.Add(60 * time.Second)

	runHoldSteps(t, mock, wsID, []holdStep{
		{"online", first, true, "ready: grace starts"},
		{"offline", first.Add(30 * time.Second), true, "went away: the clock resets"},
		{"online", back, true, "ready again: the grace starts over from this heartbeat"},
		{"online", back.Add(restartQueueHoldGrace - time.Second), true, "a grace measured from the first ready beat would have ended already"},
		{"online", back.Add(restartQueueHoldGrace), false, "ready for a whole grace since it came back"},
	})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestHoldQueueAfterRestart_ANewRestartStartsTheHoldOver(t *testing.T) {
	const wsID = "hold-rearmed"
	isolateRestartState(t, wsID)
	mock := setupTestDBForQueueTests(t)
	armRestartQueueHold(wsID)
	armedAt := holdArmedAt(wsID)
	ready := armedAt.Add(5 * time.Second)

	runHoldSteps(t, mock, wsID, []holdStep{
		{"online", ready, true, "ready: grace starts"},
	})
	armRestartQueueHold(wsID) // the container died and was restarted again
	runHoldSteps(t, mock, wsID, []holdStep{
		{"online", ready.Add(restartQueueHoldGrace), true, "first ready beat of the NEW container: its grace starts now"},
		{"online", ready.Add(2 * restartQueueHoldGrace), false, "the new container stayed ready for a whole grace"},
	})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestHoldQueueAfterRestart_UnreadableStatusDoesNotHold(t *testing.T) {
	const wsID = "hold-read-error"
	isolateRestartState(t, wsID)
	mock := setupTestDBForQueueTests(t)
	armRestartQueueHold(wsID)

	mock.ExpectQuery(holdReadSQL).WithArgs(wsID).WillReturnError(errors.New("connection reset"))

	if holdQueueAfterRestart(context.Background(), wsID) {
		t.Fatal("a status the hold cannot read must not hold the queue")
	}
	if heldSince := holdArmedAt(wsID); heldSince.IsZero() {
		t.Fatal("a failed read released the hold; the next drain should still check")
	}
}

// stopOnlyLocalProv lets a restart cycle stop the container and nothing more:
// any later provisioner call panics, which the provision goroutine's own
// recover turns into a failed provision.
type stopOnlyLocalProv struct {
	preflightLocalProv
	stops atomic.Int32
}

func (p *stopOnlyLocalProv) Stop(context.Context, string) error {
	p.stops.Add(1)
	return nil
}

// runRestartThroughStop drives one restart cycle of a crashed, restartable
// workspace up to and past its stop. What follows the stop is left unmocked:
// the provision fails on the missing rows and the cycle ends.
func runRestartThroughStop(t *testing.T, wsID string, restart func(h *WorkspaceHandler)) {
	t.Helper()
	mock := setupTestDB(t)
	setupTestRedis(t)
	stub := &stopOnlyLocalProv{}
	h := NewWorkspaceHandler(&capturingEmitter{}, nil, "http://localhost:8080", t.TempDir())
	h.provisioner = stub
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	mock.ExpectQuery(`SELECT name, status, tier, COALESCE\(runtime, 'claude-code'\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "tier", "runtime", "template", "kind"}).
			AddRow("Crashy", "offline", 1, "hermes", "", "workspace"))
	mock.ExpectQuery(`SELECT parent_id FROM workspaces WHERE id =`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))

	restart(h)
	h.waitAsyncForTest()

	if got := stub.stops.Load(); got != 1 {
		t.Fatalf("the restart cycle stopped the container %d time(s); want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestRestartCycle_OnlyAnAutomaticRestartHoldsTheQueue(t *testing.T) {
	t.Run("automatic restart", func(t *testing.T) {
		const wsID = "hold-armed-by-auto-restart"
		isolateRestartState(t, wsID)
		before := time.Now()
		runRestartThroughStop(t, wsID, func(h *WorkspaceHandler) { h.AutoRestartByID(wsID, "cp-instance-reconciler") })
		if heldSince := holdArmedAt(wsID); heldSince.Before(before) {
			t.Fatalf("an automatic restart replaced the container without holding the queue (held since %v)", heldSince)
		}
	})
	t.Run("user or mutation restart", func(t *testing.T) {
		const wsID = "hold-not-armed-by-restart-by-id"
		isolateRestartState(t, wsID)
		runRestartThroughStop(t, wsID, func(h *WorkspaceHandler) { h.RestartByID(wsID) })
		if heldSince := holdArmedAt(wsID); !heldSince.IsZero() {
			t.Fatal("a restart that is not automatic held the queue; it must drain exactly as before")
		}
	})
}

// A manual restart behaves as it did before any automatic restart, so it also
// drops a hold the last automatic restart left armed.
func TestResetAutoRestarts_DropsTheQueueHold(t *testing.T) {
	const wsID = "hold-dropped-by-manual-restart"
	isolateRestartState(t, wsID)
	armRestartQueueHold(wsID)

	resetAutoRestarts(wsID)

	if heldSince := holdArmedAt(wsID); !heldSince.IsZero() {
		t.Fatal("a manual restart left the automatic restart's queue hold in place")
	}
}

// End to end through the drain: nothing reaches the agent while the hold is
// on, and the same item drains normally once the replacement stayed ready.
//
// Expectations are unordered and declared up front so that removing the hold
// makes the first drain dispatch into the agent instead of tripping over a
// statement out of order.
func TestDrainQueueForWorkspace_HoldsTheQueueUntilTheReplacementStayedReady(t *testing.T) {
	item := drainItem("ws-held-after-restart")
	isolateRestartState(t, item.WorkspaceID)
	mock, handler, mr := drainSetup(t, item.WorkspaceID)
	mock.MatchExpectationsInOrder(false)

	var agentHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&agentHits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"result":{"status":"ok"}}`)
	}))
	defer srv.Close()
	seedRedisURL(t, mr, item.WorkspaceID, srv.URL)

	armRestartQueueHold(item.WorkspaceID)
	armedAt := holdArmedAt(item.WorkspaceID)
	ready := armedAt.Add(5 * time.Second)

	expectHoldRead(mock, item.WorkspaceID, "online", ready)
	expectHoldRead(mock, item.WorkspaceID, "online", ready.Add(restartQueueHoldGrace))
	expectDequeueNextOk(mock, item)
	expectAgentURLResolve(mock, item.WorkspaceID, srv.URL)
	expectQueueBudgetCheck(mock, item.WorkspaceID)
	expectCompleted(mock, item.ID, `{"result":{"status":"ok"}}`)

	// ── just restarted, first ready heartbeat: hold.
	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)
	if got := atomic.LoadInt32(&agentHits); got != 0 {
		t.Fatalf("the drain dispatched %d queued turn(s) into a container that had only just reported ready; want 0", got)
	}

	// ── ready for the whole grace: the same item drains.
	handler.DrainQueueForWorkspace(context.Background(), item.WorkspaceID, 1)
	if got := atomic.LoadInt32(&agentHits); got != 1 {
		t.Errorf("after the grace the queued turn must drain: agent got %d request(s), want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
