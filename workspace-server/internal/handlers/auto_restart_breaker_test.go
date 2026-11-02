package handlers

// Tests for the crash-loop breaker on automatic restarts (core#5241). The
// incident shape: an OOMKilled workspace was auto-restarted 11 times in 51
// minutes by the CP instance reconciler and the A2A dead-container path, with
// no limit on how often an automatic path may restart the same workspace.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"
)

// crashLoopFailSQL is haltAutoRestarts' write, guard included: the guard is
// what keeps a paused/removed/provisioning row, or one already failed, as is.
const crashLoopFailSQL = `UPDATE workspaces\s+SET status = \$2, last_sample_error = \$3, updated_at = now\(\)\s+` +
	`WHERE id = \$1\s+AND status NOT IN \('removed', 'provisioning', 'paused', 'hibernated', 'failed'\)`

// breakerState reads wsID's crash-loop bookkeeping under its lock.
func breakerState(wsID string) (restarts int, halted bool) {
	sv, ok := restartStates.Load(wsID)
	if !ok {
		return 0, false
	}
	state := sv.(*restartState)
	state.mu.Lock()
	defer state.mu.Unlock()
	return len(state.autoRestarts), state.autoRestartsHalted
}

// fillAutoRestarts records n automatic restarts of wsID in the last n minutes.
func fillAutoRestarts(t *testing.T, wsID string, n int) {
	t.Helper()
	now := time.Now()
	for i := 0; i < n; i++ {
		if autoRestartBudget(wsID, now.Add(-time.Duration(n-i)*time.Minute), true) {
			t.Fatalf("setup: automatic restart %d of %d was refused", i+1, n)
		}
	}
}

func isolateRestartState(t *testing.T, wsID string) {
	t.Helper()
	resetRestartStatesFor(wsID)
	t.Cleanup(func() { resetRestartStatesFor(wsID) })
}

func TestAutoRestartBudget_TripsAtTheLimitAndStaysTrippedUntilReset(t *testing.T) {
	const wsID = "crash-loop-budget"
	isolateRestartState(t, wsID)

	t0 := time.Now()
	for i := 0; i < autoRestartLimit; i++ {
		if autoRestartBudget(wsID, t0.Add(time.Duration(i)*5*time.Minute), true) {
			t.Fatalf("automatic restart %d of %d was refused", i+1, autoRestartLimit)
		}
	}
	if !autoRestartBudget(wsID, t0.Add(15*time.Minute), false) {
		t.Fatalf("a restart after %d automatic restarts within %s was allowed", autoRestartLimit, autoRestartWindow)
	}
	// Time alone must not re-arm automatic restarts: the counted restarts have
	// aged out of the window here, and the workspace is still the one that kept
	// dying.
	if !autoRestartBudget(wsID, t0.Add(3*autoRestartWindow), true) {
		t.Fatal("the breaker released on its own; only a manual restart may reset it")
	}

	resetAutoRestarts(wsID)
	if n, halted := breakerState(wsID); n != 0 || halted {
		t.Fatalf("after reset: %d counted restarts, halted=%v; want 0, false", n, halted)
	}
	if autoRestartBudget(wsID, t0.Add(3*autoRestartWindow), true) {
		t.Fatal("the first automatic restart after a manual reset was refused")
	}
}

func TestAutoRestartBudget_RestartsOutsideTheWindowDoNotCount(t *testing.T) {
	const wsID = "crash-loop-window"
	isolateRestartState(t, wsID)

	t0 := time.Now()
	steps := []struct {
		at      time.Duration
		refused bool
	}{
		{0, false},
		{20 * time.Minute, false},
		{31 * time.Minute, false}, // the first has aged out: two inside the window
		{32 * time.Minute, false}, // three inside the window now
		{33 * time.Minute, true},
	}
	for _, s := range steps {
		if got := autoRestartBudget(wsID, t0.Add(s.at), true); got != s.refused {
			t.Fatalf("automatic restart at t+%s: refused=%v, want %v", s.at, got, s.refused)
		}
	}
}

func TestAutoRestartBudget_CheckingDoesNotCount(t *testing.T) {
	const wsID = "crash-loop-check-only"
	isolateRestartState(t, wsID)

	now := time.Now()
	for i := 0; i < 2*autoRestartLimit; i++ {
		if autoRestartBudget(wsID, now, false) {
			t.Fatalf("check %d refused a workspace that has never been restarted", i+1)
		}
	}
	if n, _ := breakerState(wsID); n != 0 {
		t.Fatalf("%d restarts counted by checks alone; want 0", n)
	}
}

// The cycle a coalesceRestart loop drains after the first one serves whoever
// coalesced into it — maybe a secrets or plugin restart, which the breaker
// must never refuse.
func TestAutoRestartCycle_OnlyTheFirstCycleIsAutomatic(t *testing.T) {
	var got []string
	cycle := autoRestartCycle("cp-instance-reconciler", func(autoTrigger string) {
		got = append(got, autoTrigger)
	})
	cycle()
	cycle()
	cycle()
	if want := []string{"cp-instance-reconciler", "", ""}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("cycle triggers = %q, want %q", got, want)
	}
}

func TestAutoRestartByID_TrippedBreakerMarksFailedInsteadOfRestarting(t *testing.T) {
	const wsID = "crash-loop-trip"
	isolateRestartState(t, wsID)
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	h := NewWorkspaceHandler(emitter, nil, "http://localhost:8080", t.TempDir())
	h.provisioner = &preflightLocalProv{} // HasProvisioner; panics if a restart reached Stop
	fillAutoRestarts(t, wsID, autoRestartLimit)

	mock.ExpectExec(crashLoopFailSQL).
		WithArgs(wsID, models.StatusFailed, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.AutoRestartByID(wsID, "cp-instance-reconciler")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the crash-looping workspace was not marked failed: %v", err)
	}
	if isRestarting(wsID) || shouldDebounceRestart(wsID) {
		t.Fatal("a restart cycle started for a workspace whose breaker had tripped")
	}
	if len(emitter.events) != 1 {
		t.Fatalf("want exactly one event, got %d: %+v", len(emitter.events), emitter.events)
	}
	ev := emitter.events[0]
	if ev.eventType != string(events.EventWorkspaceProvisionFailed) || ev.workspaceID != wsID {
		t.Fatalf("event = %s for %s; want %s for %s — the canvas flips a node to failed only on that event",
			ev.eventType, ev.workspaceID, events.EventWorkspaceProvisionFailed, wsID)
	}
	payload, _ := ev.payload.(map[string]interface{})
	if payload["source"] != "auto_restart_crash_loop" || payload["trigger"] != "cp-instance-reconciler" {
		t.Errorf("payload source/trigger = %v/%v; want auto_restart_crash_loop/cp-instance-reconciler",
			payload["source"], payload["trigger"])
	}
	if msg, _ := payload["error"].(string); !strings.Contains(msg, "restarted manually") {
		t.Errorf("the reason must tell the owner what to do next; got %q", msg)
	}
}

func TestAutoRestartByID_AlreadyFailedIsNotAnnouncedAgain(t *testing.T) {
	const wsID = "crash-loop-trip-repeat"
	isolateRestartState(t, wsID)
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	h := NewWorkspaceHandler(emitter, nil, "http://localhost:8080", t.TempDir())
	h.provisioner = &preflightLocalProv{}
	fillAutoRestarts(t, wsID, autoRestartLimit)

	// The guard matches nothing: the row is already failed (or paused, removed...).
	mock.ExpectExec(crashLoopFailSQL).
		WithArgs(wsID, models.StatusFailed, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	h.AutoRestartByID(wsID, "a2a-dead-container")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
	if len(emitter.events) != 0 {
		t.Fatalf("a row the guard left alone was announced as failed again: %+v", emitter.events)
	}
	if isRestarting(wsID) || shouldDebounceRestart(wsID) {
		t.Fatal("a restart cycle started for a workspace whose breaker had tripped")
	}
}

// One death, several probes: the first starts a cycle, the rest land in its
// self-fire debounce. Only the cycle counts — and it counts even though the
// restart then declined (pinned image unavailable), because it did reach the
// point of acting on the container.
func TestAutoRestartByID_CountsTheCycleNotTheProbes(t *testing.T) {
	const wsID = "crash-loop-count"
	isolateRestartState(t, wsID)
	mock := setupTestDB(t)
	setupTestRedis(t)
	stub := &prewarmCPProv{ensureErr: fmt.Errorf("%w: test", provisioner.ErrEnsureImagePermanent)}
	h := NewWorkspaceHandler(&capturingEmitter{}, nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(stub)
	waitForHandlerAsyncBeforeDBCleanup(t, h)

	mock.ExpectQuery(`SELECT name, status, tier, COALESCE\(runtime, 'claude-code'\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "tier", "runtime", "template", "kind"}).
			AddRow("Crashy", "offline", 1, "hermes", "", "workspace"))
	mock.ExpectQuery(`SELECT parent_id FROM workspaces WHERE id =`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))

	for i := 0; i < 3; i++ {
		h.AutoRestartByID(wsID, "a2a-dead-container")
	}
	h.waitAsyncForTest()

	if n, halted := breakerState(wsID); n != 1 || halted {
		t.Fatalf("counted %d automatic restarts (halted=%v) for one cycle; want 1, false", n, halted)
	}
	if len(stub.calls) != 1 || stub.calls[0] != "EnsureImage" {
		t.Fatalf("CP calls = %v; want exactly one restart cycle, declined before any stop", stub.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// A restart the cycle skips — here an external workspace, which has no
// container — must not reach the breaker at all, however full its window.
func TestRunRestartCycle_SkippedRestartNeverReachesTheBreaker(t *testing.T) {
	const wsID = "crash-loop-external"
	isolateRestartState(t, wsID)
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	h := NewWorkspaceHandler(emitter, nil, "http://localhost:8080", t.TempDir())
	h.provisioner = &preflightLocalProv{}
	fillAutoRestarts(t, wsID, autoRestartLimit)

	mock.ExpectQuery(`SELECT name, status, tier, COALESCE\(runtime, 'claude-code'\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "tier", "runtime", "template", "kind"}).
			AddRow("Laptop agent", "awaiting_agent", 1, "external", "", "workspace"))

	h.runRestartCycle(wsID, "liveness-monitor")

	if _, halted := breakerState(wsID); halted {
		t.Fatal("a restart the cycle skips was counted against the breaker")
	}
	if len(emitter.events) != 0 {
		t.Fatalf("a skipped restart produced events: %+v", emitter.events)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// The cycle's own check is the backstop for a request that passed the up-front
// check just before the window filled: it must refuse before anything is
// stopped.
func TestRunRestartCycle_FullWindowHaltsBeforeTouchingTheContainer(t *testing.T) {
	const wsID = "crash-loop-cycle-backstop"
	isolateRestartState(t, wsID)
	mock := setupTestDB(t)
	emitter := &capturingEmitter{}
	stub := &preflightLocalProv{} // Stop panics: reaching it fails the test
	h := NewWorkspaceHandler(emitter, nil, "http://localhost:8080", t.TempDir())
	h.provisioner = stub
	fillAutoRestarts(t, wsID, autoRestartLimit)

	mock.ExpectQuery(`SELECT name, status, tier, COALESCE\(runtime, 'claude-code'\)`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"name", "status", "tier", "runtime", "template", "kind"}).
			AddRow("Crashy", "online", 1, "hermes", "", "workspace"))
	mock.ExpectQuery(`SELECT parent_id FROM workspaces WHERE id =`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))
	mock.ExpectExec(crashLoopFailSQL).
		WithArgs(wsID, models.StatusFailed, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.runRestartCycle(wsID, "cp-instance-reconciler")

	if _, halted := breakerState(wsID); !halted {
		t.Fatal("the cycle ran a fourth automatic restart inside the window")
	}
	if len(emitter.events) != 1 || emitter.events[0].eventType != string(events.EventWorkspaceProvisionFailed) {
		t.Fatalf("events = %+v; want one %s", emitter.events, events.EventWorkspaceProvisionFailed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

// The user-facing restart is never refused by a tripped breaker, and it hands
// automatic restarts their full budget back.
func TestRestartHandler_ManualRestartResetsTheBreakerAndStillRuns(t *testing.T) {
	const wsID = "crash-loop-manual"
	isolateRestartState(t, wsID)
	mock := setupTestDB(t)
	setupTestRedis(t)
	stub := &prewarmCPProv{ensureErr: fmt.Errorf("%w: test", provisioner.ErrEnsureImagePermanent)}
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(stub)
	waitForHandlerAsyncBeforeDBCleanup(t, h)
	fillAutoRestarts(t, wsID, autoRestartLimit)
	if !autoRestartBudget(wsID, time.Now(), false) {
		t.Fatal("setup: the breaker should have tripped")
	}

	mock.ExpectQuery("SELECT status, name, tier, COALESCE").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"status", "name", "tier", "runtime", "template"}).
			AddRow("failed", "Crashy", 1, "hermes", ""))
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}).AddRow(nil))
	// Everything after the lookup is incidental to this test and left
	// unmocked; the dispatcher's pre-flight decline ends the async leg early.

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/workspaces/"+wsID+"/restart", strings.NewReader("{}"))
	c.Request.Header.Set("Content-Type", "application/json")
	h.Restart(c)
	h.waitAsyncForTest()

	if w.Code != http.StatusOK {
		t.Fatalf("manual restart answered %d: %s", w.Code, w.Body.String())
	}
	if n, halted := breakerState(wsID); n != 0 || halted {
		t.Fatalf("after a manual restart: %d counted restarts, halted=%v; want 0, false", n, halted)
	}
	if len(stub.calls) == 0 || stub.calls[0] != "EnsureImage" {
		t.Fatalf("the manual restart was not dispatched (CP calls %v)", stub.calls)
	}
}

func TestAdminRestart_ResetsTheBreaker(t *testing.T) {
	const wsID = "crash-loop-admin"
	isolateRestartState(t, wsID)
	h, mock := setupBootstrapHandler(t)
	fillAutoRestarts(t, wsID, autoRestartLimit)
	if !autoRestartBudget(wsID, time.Now(), false) {
		t.Fatal("setup: the breaker should have tripped")
	}

	mock.ExpectQuery(`SELECT 1 FROM workspaces WHERE id = \$1`).
		WithArgs(wsID).
		WillReturnRows(sqlmock.NewRows([]string{"x"}).AddRow(1))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: wsID}}
	c.Request = httptest.NewRequest(http.MethodPost, "/admin/workspaces/"+wsID+"/restart", nil)
	h.AdminRestart(c)
	h.waitAsyncForTest()

	if w.Code != http.StatusAccepted {
		t.Fatalf("admin restart answered %d: %s", w.Code, w.Body.String())
	}
	if n, halted := breakerState(wsID); n != 0 || halted {
		t.Fatalf("after an admin restart: %d counted restarts, halted=%v; want 0, false", n, halted)
	}
}
