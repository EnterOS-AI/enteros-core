package handlers

// workspace_pause_context_test.go — pins the Pause handler's two honesty
// contracts:
//
//  1. The side effects (stop, mark-paused, key clear, event) must NOT run on
//     the request-scoped context. Pre-fix Pause took `ctx := c.Request.Context()`
//     and used it for all four; an aborted client connection cancelled every one
//     of them mid-flight while the handler still answered
//     200 {"status":"paused"}. Observed live on a tenant:
//
//     Pause: stop 35cf77b2-… failed: cp provisioner: stop: … context canceled
//     Pause: failed to set paused status for 35cf77b2-…: context canceled
//     RecordAndBroadcast: insert event error: context canceled
//     [GIN] 200 | 4.21s | POST "/workspaces/35cf77b2-…/pause"
//
//     …and eleven seconds later the same workspace read status="online"
//     routable=true. Restart already solved this (workspace_restart.go:463-474,
//     "detaches the dispatch from the request lifecycle so an aborted client
//     connection doesn't cancel the in-flight Stop/provision pair").
//
//  2. The response must report what actually happened. Pre-fix every failure was
//     `log.Printf` only and the handler ended in an UNCONDITIONAL
//     c.JSON(200, {"status":"paused"}). Same lie 6d64c183f closed for hibernate
//     (#4293), in a different handler.
//
// Why Pause detaches SYNCHRONOUSLY (WithoutCancel) rather than firing goAsync +
// context.Background() as Restart does: Restart's 200 says {"status":
// "provisioning"} — an honest "queued". Pause's 200 says {"status":"paused"} —
// a claim about a completed state. Moving the work behind goAsync would keep the
// body reading "done" while meaning "queued", i.e. re-introduce the same lie
// through the other door. The precedent for this exact shape is
// a2a_proxy_helpers.go:786-794 — "context.WithoutCancel: a client disconnect …
// SYNCHRONOUS (no goAsync): the row must be durable before the 200."

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
)

// ─────────────────────────────────────────────────────────────────────────────
// fake CP provisioner
// ─────────────────────────────────────────────────────────────────────────────

// pauseCPStop is a CPProvisionerAPI whose Stop is fully scriptable per
// workspace id, and which records the context it was handed. Recording the
// context is the only way to observe the detach: sqlmock cannot see a ctx, and
// StopWorkspaceAuto is the first consumer of whatever context Pause chose.
type pauseCPStop struct {
	mu sync.Mutex

	// errByID[id] is returned by Stop(id). Absent id ⇒ nil (success).
	errByID map[string]error
	// hook runs at the top of every Stop, before the error lookup. Tests use it
	// to cancel the inbound request mid-stop — exactly the prod sequence.
	hook func()

	stopped   []string
	seenCtx   []context.Context
	deadlines []time.Time
	hasNoDL   int
	errAtStop []error // ctx.Err() sampled INSIDE Stop, after hook ran
}

func (p *pauseCPStop) Stop(ctx context.Context, workspaceID string) error {
	p.mu.Lock()
	hook := p.hook
	p.mu.Unlock()
	if hook != nil {
		hook()
	}

	p.mu.Lock()
	p.stopped = append(p.stopped, workspaceID)
	p.seenCtx = append(p.seenCtx, ctx)
	// Sample AFTER the hook: if the hook cancelled the request context and this
	// ctx is still live, the detach held.
	p.errAtStop = append(p.errAtStop, ctx.Err())
	if dl, ok := ctx.Deadline(); ok {
		p.deadlines = append(p.deadlines, dl)
	} else {
		p.hasNoDL++
	}
	err := p.errByID[workspaceID]
	p.mu.Unlock()
	return err
}

func (p *pauseCPStop) StopAndPrune(ctx context.Context, id string) error { return p.Stop(ctx, id) }
func (p *pauseCPStop) Start(_ context.Context, _ provisioner.WorkspaceConfig) (string, error) {
	return "", nil
}
func (p *pauseCPStop) IsRunning(_ context.Context, _ string) (bool, error) { return false, nil }
func (p *pauseCPStop) EnsureImage(_ context.Context, _ provisioner.EnsureImageRequest) (provisioner.EnsureImageResult, error) {
	return provisioner.EnsureImageResult{Status: "ready"}, nil
}
func (p *pauseCPStop) GetConsoleOutput(_ context.Context, _ string) (string, error) { return "", nil }

func (p *pauseCPStop) stoppedSnapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.stopped))
	copy(out, p.stopped)
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// request helpers
// ─────────────────────────────────────────────────────────────────────────────

// pauseCall drives handler.Pause with a request whose context is `reqCtx`.
func pauseCall(handler *WorkspaceHandler, id, query string, reqCtx context.Context) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}}
	req := httptest.NewRequest("POST", "/workspaces/"+id+"/pause"+query, nil)
	if reqCtx != nil {
		req = req.WithContext(reqCtx)
	}
	c.Request = req
	handler.Pause(c)
	return w
}

func pauseBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%v): %s", err, w.Body.String())
	}
	return out
}

// expectPauseLookup queues the two read queries every Pause performs, returning
// `descendants` from the recursive CTE.
func expectPauseLookup(mock sqlmock.Sqlmock, id, name string, descendants [][2]string) {
	mock.ExpectQuery("SELECT status, name FROM workspaces WHERE id =").
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"status", "name"}).AddRow("online", name))
	rows := sqlmock.NewRows([]string{"id", "name"})
	for _, d := range descendants {
		rows = rows.AddRow(d[0], d[1])
	}
	mock.ExpectQuery("WITH RECURSIVE descendants").WithArgs(id).WillReturnRows(rows)
}

// expectPausedWrites queues the mark-paused UPDATE + the paused event INSERT for
// one workspace — i.e. what a workspace that genuinely paused must produce.
func expectPausedWrites(mock sqlmock.Sqlmock, id string) {
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusPaused, id).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))
}

func newPauseHandler(t *testing.T, cp *pauseCPStop) *WorkspaceHandler {
	t.Helper()
	broadcaster := newTestBroadcaster()
	h := NewWorkspaceHandler(broadcaster, nil, "http://localhost:8080", t.TempDir())
	if cp != nil {
		h.SetCPProvisioner(cp)
	}
	waitForHandlerAsyncBeforeDBCleanup(t, h)
	return h
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Detach — a cancelled client must still get its side effects
// ─────────────────────────────────────────────────────────────────────────────

// TestPauseHandler_ClientCancelMidStop_SideEffectsStillRun reproduces the prod
// trace exactly: the two lookups succeed, the client goes away while the stop is
// in flight, and the stop itself SUCCEEDS. Everything after it — the mark-paused
// UPDATE and the paused event INSERT — must still land.
//
// Pre-fix these ran on the request context, so database/sql short-circuits on
// ctx.Err() before ever reaching the driver: sqlmock never sees either statement
// and ExpectationsWereMet reports both unmet.
//
// The assertion is made IMMEDIATELY after Pause returns, with no drain — which
// simultaneously pins that the detached work is synchronous. A goAsync-shaped
// "fix" would fail here, and it should: a 200 body reading {"status":"paused"}
// must not mean "queued".
func TestPauseHandler_ClientCancelMidStop_SideEffectsStillRun(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cp := &pauseCPStop{errByID: map[string]error{}, hook: cancel}
	handler := newPauseHandler(t, cp)

	const wsID = "ws-pause-cancel"
	expectPauseLookup(mock, wsID, "Canceller", nil)
	expectPausedWrites(mock, wsID)

	w := pauseCall(handler, wsID, "", reqCtx)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("side effects did not run after the client disconnected: %v\n"+
			"Pause must not carry the request-scoped context into its writes.", err)
	}
	if got := cp.errAtStop[0]; got != nil {
		t.Errorf("Stop saw ctx.Err()=%v after the client cancelled; want nil — "+
			"the stop must run on a context detached from the request", got)
	}
	if w.Code != http.StatusOK {
		t.Errorf("want 200 (everything succeeded), got %d: %s", w.Code, w.Body.String())
	}
}

// TestPauseHandler_DetachedContextIsBounded pins the ceiling. A detached
// operation with no deadline is a different bug: the request context is the only
// thing that used to bound this work, so removing it without substituting a
// deadline would let a wedged CP Stop hold the handler goroutine (and its DB
// conns) forever.
//
// Two properties, both asserted from inside Stop:
//   - the context Pause hands out HAS a deadline, and it is within
//     pauseSideEffectBudget of now (not some inherited far-future one);
//   - it is NOT cancelled by the client going away.
//
// Pre-fix the request context has no deadline at all (httptest requests carry a
// plain background context), so ctx.Deadline() reports ok=false and this fails.
func TestPauseHandler_DetachedContextIsBounded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	cp := &pauseCPStop{errByID: map[string]error{}}
	handler := newPauseHandler(t, cp)

	const wsID = "ws-pause-bounded"
	expectPauseLookup(mock, wsID, "Bounded", nil)
	expectPausedWrites(mock, wsID)

	before := time.Now()
	_ = pauseCall(handler, wsID, "", nil)
	after := time.Now()

	if cp.hasNoDL != 0 {
		t.Fatalf("%d Stop call(s) received a context with NO deadline — a detached "+
			"pause must be bounded; pauseSideEffectBudget is the ceiling", cp.hasNoDL)
	}
	if len(cp.deadlines) != 1 {
		t.Fatalf("want 1 recorded deadline, got %d", len(cp.deadlines))
	}
	dl := cp.deadlines[0]
	if !dl.After(before) {
		t.Errorf("deadline %v is already in the past at dispatch", dl)
	}
	if max := after.Add(pauseSideEffectBudget); dl.After(max) {
		t.Errorf("deadline %v exceeds now+pauseSideEffectBudget (%v) — the ceiling is not enforced", dl, max)
	}
}

// TestPauseHandler_CascadeSharesOneBudget pins that the ceiling is TOTAL, not
// per-workspace. A per-workspace timeout would let an N-descendant cascade run
// for N × budget, which is not a bound. All three stops must see the SAME
// deadline instant — one context, created once, shared by the whole fan-out.
func TestPauseHandler_CascadeSharesOneBudget(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	cp := &pauseCPStop{errByID: map[string]error{}}
	handler := newPauseHandler(t, cp)

	const wsID = "ws-pause-budget-parent"
	expectPauseLookup(mock, wsID, "Parent", [][2]string{{"ws-b1", "B1"}, {"ws-b2", "B2"}})
	for _, id := range []string{wsID, "ws-b1", "ws-b2"} {
		expectPausedWrites(mock, id)
	}

	_ = pauseCall(handler, wsID, "?cascade=true", nil)

	if cp.hasNoDL != 0 {
		t.Fatalf("%d Stop call(s) had no deadline", cp.hasNoDL)
	}
	if len(cp.deadlines) != 3 {
		t.Fatalf("want 3 deadlines (parent + 2 descendants), got %d", len(cp.deadlines))
	}
	for i, dl := range cp.deadlines[1:] {
		if !dl.Equal(cp.deadlines[0]) {
			t.Errorf("stop %d deadline %v != stop 0 deadline %v — the cascade must share ONE "+
				"total budget, not restart the clock per workspace", i+1, dl, cp.deadlines[0])
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Honest status
// ─────────────────────────────────────────────────────────────────────────────

// TestPauseHandler_StopFails_MustNotReturn200 is the headline lie. A single
// workspace whose stop fails paused NOTHING — the compute is still running — so
// the response must not say "paused".
//
// It also pins that we do not write a false row: with the stop failed, the
// mark-paused UPDATE must NOT be issued. Pre-fix it was, with the comment
// "orphan sweeper will reconcile" — which is not true of any sweeper that
// exists: registry/orphan_sweeper.go and registry/cp_orphan_sweeper.go both
// select ONLY `status = 'removed'`. A 'paused' row over a live container has no
// backstop at all, so the truthful row is the one we leave alone, and the client
// retry is the recovery path.
func TestPauseHandler_StopFails_MustNotReturn200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "ws-pause-stopfail"
	cp := &pauseCPStop{errByID: map[string]error{wsID: errors.New("cp provisioner: stop: 502 bad gateway")}}
	handler := newPauseHandler(t, cp)

	expectPauseLookup(mock, wsID, "Stubborn", nil)
	// Deliberately NO write expectations: nothing was stopped, so nothing may be
	// written. sqlmock fails any statement it was not told to expect.

	w := pauseCall(handler, wsID, "", nil)

	if w.Code == http.StatusOK {
		t.Fatalf("Pause answered 200 while the stop failed and nothing was paused: %s", w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when every workspace failed, got %d: %s", w.Code, w.Body.String())
	}
	body := pauseBody(t, w)
	if body["status"] == "paused" {
		t.Errorf(`body claims status="paused" though nothing was stopped: %s`, w.Body.String())
	}
	if got, ok := body["paused_count"].(float64); !ok || got != 0 {
		t.Errorf("want paused_count 0, got %v", body["paused_count"])
	}
	fails, ok := body["failures"].([]any)
	if !ok || len(fails) != 1 {
		t.Fatalf("want 1 failure entry, got %v", body["failures"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestPauseHandler_GenuineSuccess_StillReturns200 is the negative control for
// the test above, varying exactly ONE input: the stop error. Everything else —
// handler, mocks, request — is identical. A pause that really works must still
// answer 200 {"status":"paused"} and must still write the row.
func TestPauseHandler_GenuineSuccess_StillReturns200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "ws-pause-stopfail"                // same id, same everything…
	cp := &pauseCPStop{errByID: map[string]error{}} // …only the stop error differs
	handler := newPauseHandler(t, cp)

	expectPauseLookup(mock, wsID, "Stubborn", nil)
	expectPausedWrites(mock, wsID)

	w := pauseCall(handler, wsID, "", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200 on a genuine pause, got %d: %s", w.Code, w.Body.String())
	}
	body := pauseBody(t, w)
	if body["status"] != "paused" {
		t.Errorf(`want status="paused", got %v`, body["status"])
	}
	if got, ok := body["paused_count"].(float64); !ok || got != 1 {
		t.Errorf("want paused_count 1, got %v", body["paused_count"])
	}
	if _, present := body["failures"]; present {
		t.Errorf("a clean pause must not carry a failures list: %s", w.Body.String())
	}
	if got := cp.stoppedSnapshot(); len(got) != 1 || got[0] != wsID {
		t.Errorf("stop calls = %v; want exactly [%s]", got, wsID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestPauseHandler_CascadePartialFailure_Returns207 pins that a partial result
// is REPRESENTABLE. The cascade fans out over descendants; collapsing "2 of 3
// paused" into either 200 or 500 loses the only information the caller can act
// on. 207 + the per-workspace failure list is what 6d64c183f's shape maps to
// when the operation is plural.
func TestPauseHandler_CascadePartialFailure_Returns207(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "ws-pause-partial"
	cp := &pauseCPStop{errByID: map[string]error{
		"ws-partial-child-1": errors.New("cp provisioner: stop: connection reset"),
	}}
	handler := newPauseHandler(t, cp)

	expectPauseLookup(mock, wsID, "Parent", [][2]string{
		{"ws-partial-child-1", "Child 1"},
		{"ws-partial-child-2", "Child 2"},
	})
	// Parent and child-2 pause; child-1's stop fails so it writes nothing.
	expectPausedWrites(mock, wsID)
	expectPausedWrites(mock, "ws-partial-child-2")

	w := pauseCall(handler, wsID, "?cascade=true", nil)

	if w.Code != http.StatusMultiStatus {
		t.Fatalf("want 207 on a partial cascade, got %d: %s", w.Code, w.Body.String())
	}
	body := pauseBody(t, w)
	if got, ok := body["paused_count"].(float64); !ok || got != 2 {
		t.Errorf("want paused_count 2 (only the ones that really paused), got %v", body["paused_count"])
	}
	fails, ok := body["failures"].([]any)
	if !ok || len(fails) != 1 {
		t.Fatalf("want exactly 1 failure entry, got %v", body["failures"])
	}
	entry, _ := fails[0].(map[string]any)
	if entry["workspace_id"] != "ws-partial-child-1" {
		t.Errorf("failure entry must name the workspace that failed, got %v", entry)
	}
	if entry["stage"] != "stop" {
		t.Errorf(`want stage "stop", got %v`, entry["stage"])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestPauseHandler_MarkPausedWriteFails_MustNotReturn200 covers the other
// failure stage. Here the container IS stopped but the row write fails — the
// workspace is down while the DB still says online. That is a real, reportable
// failure and it is NOT the same as the stop failing, so it carries its own
// stage.
func TestPauseHandler_MarkPausedWriteFails_MustNotReturn200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "ws-pause-writefail"
	cp := &pauseCPStop{errByID: map[string]error{}}
	handler := newPauseHandler(t, cp)

	expectPauseLookup(mock, wsID, "Wedged", nil)
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusPaused, wsID).
		WillReturnError(fmt.Errorf("connection reset by peer"))

	w := pauseCall(handler, wsID, "", nil)

	if got := cp.stoppedSnapshot(); len(got) != 1 {
		t.Fatalf("stop calls = %v; this test is only meaningful if the container was stopped", got)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when the mark-paused write failed, got %d: %s", w.Code, w.Body.String())
	}
	body := pauseBody(t, w)
	fails, ok := body["failures"].([]any)
	if !ok || len(fails) != 1 {
		t.Fatalf("want 1 failure entry, got %v", body["failures"])
	}
	entry, _ := fails[0].(map[string]any)
	if entry["stage"] != "mark_paused" {
		t.Errorf(`want stage "mark_paused" (the stop succeeded — conflating it with `+
			`"stop" would misdirect whoever debugs it), got %v`, entry["stage"])
	}
	// Same prose trap as the 207: the 500 fires for stop failures (workload still
	// running) AND for post-stop failures like this one (workload stopped). A
	// fixed string asserting "still running" is false on this path, and it is the
	// path where being wrong is most expensive — an operator told the box is up
	// will not go looking for a stopped one.
	if msg, _ := body["error"].(string); strings.Contains(msg, "still running") {
		t.Errorf("500 message asserts the workspace is still running, but the stop "+
			"SUCCEEDED and only the row write failed: %q", msg)
	}
}

// TestPauseHandler_ClaimMatchedNoRow_MustNotReturn200 is the atomic-claim half.
// Pre-fix the mark-paused UPDATE was `WHERE id = $2` with rowsAffected never
// read, so a row that left the pausable set between the eligibility SELECT and
// the write was silently reported as paused. Guarding the write and reading
// rowsAffected turns that silent no-op into a reported failure.
func TestPauseHandler_ClaimMatchedNoRow_MustNotReturn200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "ws-pause-claim-noop"
	cp := &pauseCPStop{errByID: map[string]error{}}
	handler := newPauseHandler(t, cp)

	expectPauseLookup(mock, wsID, "Racy", nil)
	// Someone removed / already paused the row between the SELECT and here.
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusPaused, wsID).
		WillReturnResult(sqlmock.NewResult(0, 0))

	w := pauseCall(handler, wsID, "", nil)

	if w.Code == http.StatusOK {
		t.Fatalf("Pause answered 200 though its claim matched no row: %s", w.Body.String())
	}
	body := pauseBody(t, w)
	fails, ok := body["failures"].([]any)
	if !ok || len(fails) != 1 {
		t.Fatalf("want 1 failure entry, got %v", body["failures"])
	}
	if entry, _ := fails[0].(map[string]any); entry["stage"] != "claim" {
		t.Errorf(`want stage "claim", got %v`, entry["stage"])
	}
}

// TestPauseHandler_ClaimIsStatusGuarded pins the claim's SQL. sqlmock does not
// evaluate a WHERE clause, so the only property it can observe is the text —
// which is exactly what 6d64c183f's hibernate tests assert on, for the same
// reason. The regex is anchored at the end of the whitespace-stripped query, so
// the pre-fix `WHERE id = $2` (no status predicate) fails to match.
func TestPauseHandler_ClaimIsStatusGuarded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "ws-pause-claim-sql"
	cp := &pauseCPStop{errByID: map[string]error{}}
	handler := newPauseHandler(t, cp)

	expectPauseLookup(mock, wsID, "Guarded", nil)
	mock.ExpectExec(`WHERE id = \$2 AND status NOT IN \('removed', 'paused'\)$`).
		WithArgs(models.StatusPaused, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnResult(sqlmock.NewResult(0, 1))

	w := pauseCall(handler, wsID, "", nil)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet DB expectations: %v", err)
	}
}

// TestPauseHandler_TelemetryOnlyFailure_MessageMustNotLie closes a lie the FIX
// introduced. A single workspace whose stop and claim both succeed but whose
// paused-event INSERT fails is a genuine 207 — the pause happened, the canvas
// may be stale — but the first cut of the partial-result message read:
//
//	"some workspaces in the cascade were not paused — they are still running"
//
// which is false on all three counts: it was not a cascade, they WERE paused,
// and they are NOT still running. The machine-readable fields were right and
// only the fixed string was wrong, which is the worst version of the bug this
// PR exists to fix — a correct payload with prose that contradicts it, because
// the prose is what a human reads first.
//
// The message must therefore describe what is actually known at that point: N
// of M paused, the rest is in failures[]. It must not assert anything about
// cascades or about workloads still running.
func TestPauseHandler_TelemetryOnlyFailure_MessageMustNotLie(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "ws-pause-telemetry"
	cp := &pauseCPStop{errByID: map[string]error{}}
	handler := newPauseHandler(t, cp)

	expectPauseLookup(mock, wsID, "Telemetry", nil)
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusPaused, wsID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Only the event INSERT dies.
	mock.ExpectExec("INSERT INTO structure_events").
		WillReturnError(fmt.Errorf("deadlock detected"))

	w := pauseCall(handler, wsID, "", nil)

	if w.Code != http.StatusMultiStatus {
		t.Fatalf("want 207 when only telemetry failed, got %d: %s", w.Code, w.Body.String())
	}
	body := pauseBody(t, w)
	if got, ok := body["paused_count"].(float64); !ok || got != 1 {
		t.Errorf("the workspace WAS paused; want paused_count 1, got %v", body["paused_count"])
	}
	fails, ok := body["failures"].([]any)
	if !ok || len(fails) != 1 {
		t.Fatalf("want 1 failure entry, got %v", body["failures"])
	}
	if entry, _ := fails[0].(map[string]any); entry["stage"] != "broadcast" {
		t.Errorf(`want stage "broadcast", got %v`, entry["stage"])
	}
	msg, _ := body["error"].(string)
	for _, lie := range []string{"cascade", "still running", "were not paused"} {
		if strings.Contains(msg, lie) {
			t.Errorf("partial-result message asserts %q, which is false here "+
				"(one workspace, it WAS paused, it is NOT running): %q", lie, msg)
		}
	}
}

// TestPauseHandler_HonestStatusSurvivesATotallyDeadDB is the reachability proof.
// The day's repeated lesson is a fix whose new error path cannot execute — a
// failure "reported" through a write that the very failure condition has already
// broken. So: kill EVERY write. Both the mark-paused UPDATE and the event INSERT
// error out for all three workspaces in the cascade.
//
// The honest status still comes back, because it is computed in memory and
// delivered on the HTTP response — it takes no DB, no Redis and no broadcast.
// That is the whole point of choosing the response as the reporting channel.
func TestPauseHandler_HonestStatusSurvivesATotallyDeadDB(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)

	const wsID = "ws-pause-deaddb"
	cp := &pauseCPStop{errByID: map[string]error{}}
	handler := newPauseHandler(t, cp)

	expectPauseLookup(mock, wsID, "DeadDB", [][2]string{{"ws-dd-1", "D1"}, {"ws-dd-2", "D2"}})
	for range []int{0, 1, 2} {
		mock.ExpectExec("UPDATE workspaces SET status =").
			WillReturnError(fmt.Errorf("driver: bad connection"))
	}

	w := pauseCall(handler, wsID, "?cascade=true", nil)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 with every write dead, got %d: %s", w.Code, w.Body.String())
	}
	body := pauseBody(t, w)
	if got, ok := body["paused_count"].(float64); !ok || got != 0 {
		t.Errorf("want paused_count 0, got %v", body["paused_count"])
	}
	fails, ok := body["failures"].([]any)
	if !ok || len(fails) != 3 {
		t.Fatalf("want 3 failure entries (one per workspace in the cascade), got %v", body["failures"])
	}
}
