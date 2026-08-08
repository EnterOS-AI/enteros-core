package handlers

// workspace_resume_context_test.go — pins the Resume handler's three honesty
// contracts. It is the Resume counterpart of workspace_pause_context_test.go
// (PR #5102); the defect is the same class, the blast radius is not.
//
//  1. DETACH. The status write and the event must NOT run on the request-scoped
//     context. Pre-fix Resume took `ctx := c.Request.Context()` and used it for
//     both; an aborted client connection cancelled them mid-flight while the
//     handler still answered 200 {"status":"provisioning"}.
//
//  2. ATOMIC CLAIM, BEFORE THE PROVISION. Pre-fix the write was
//     `UPDATE … WHERE id = $2` with rowsAffected never read, and
//     provisionWorkspaceAuto was dispatched unconditionally afterwards. Because
//     that dispatch builds its own context.Background() context, it ran to
//     completion whether or not the status write survived — starting and BILLING
//     a box whose row still read 'paused'. That state is unrecoverable by
//     construction: /registry/register's upsert
//     (`WHERE workspaces.status NOT IN ('removed','paused','hibernated')`), the
//     heartbeat's `recoverable` predicate, StartLivenessMonitor and
//     StartHealthSweep all refuse 'paused' rows, and StartCPOrphanSweeper reaps
//     `status='removed'` only. Claiming BEFORE dispatching makes "the row says
//     provisioning" a precondition of "a box gets started".
//
//  3. HONEST STATUS. Pre-fix every failure was `log.Printf` only and the handler
//     ended in an UNCONDITIONAL c.JSON(200, {"status":"provisioning"}).
//
// Note what does NOT change: the async dispatch itself, and the word
// "provisioning" in the success body. Unlike Pause's {"status":"paused"} — a
// claim about a COMPLETED state, which is why #5102 had to keep Pause
// synchronous — "provisioning" is an honest "queued". The provision's own
// outcome arrives later as a WORKSPACE_PROVISION_FAILED event plus
// status='failed' from markProvisionFailed, exactly as it does for Restart and
// Create. A failure landing after the 200 is therefore the documented contract,
// not a second defect.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gin-gonic/gin"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
)

// ─────────────────────────────────────────────────────────────────────────────
// probes
// ─────────────────────────────────────────────────────────────────────────────

// resumeArgProbe is a sqlmock.Argument that always matches and records that it
// was reached. sqlmock consults Argument.Match at the moment the statement is
// executed, so attaching one to an expectation turns that expectation into an
// observation point: `hit()` answers "did this exact statement run?".
//
// This is how the tests below observe whether a provision was dispatched
// WITHOUT adding a test-only seam to the production dispatcher. The first thing
// the provision leg of Resume does after the claim is withStoredCompute's
// `SELECT COALESCE(compute, …)`; a probe on that statement therefore reports
// dispatch-or-not, and it does so synchronously, before the handler returns —
// unlike cpProv.Start, which the provision goroutine may never reach
// (prepareProvisionContext aborts earlier on a credential-less fixture).
type resumeArgProbe struct {
	mu     sync.Mutex
	hits   int
	onHit  func()
	expect driver.Value // nil = match anything
}

func (p *resumeArgProbe) Match(v driver.Value) bool {
	p.mu.Lock()
	p.hits++
	hook := p.onHit
	want := p.expect
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	if want == nil {
		return true
	}
	return v == want
}

func (p *resumeArgProbe) hit() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits > 0
}

// ─────────────────────────────────────────────────────────────────────────────
// request + fixture helpers
// ─────────────────────────────────────────────────────────────────────────────

// resumeCall drives handler.Resume with a request whose context is `reqCtx`.
func resumeCall(handler *WorkspaceHandler, id, query string, reqCtx context.Context) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}}
	req := httptest.NewRequest("POST", "/workspaces/"+id+"/resume"+query, nil)
	if reqCtx != nil {
		req = req.WithContext(reqCtx)
	}
	c.Request = req
	handler.Resume(c)
	return w
}

func resumeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON (%v): %s", err, w.Body.String())
	}
	return out
}

// expectResumeReads queues the three read queries every Resume performs before
// it mutates anything: the eligibility SELECT, isParentPaused's parent lookup,
// and the recursive descendant CTE. `descendantArg` is the argument matcher for
// the CTE — pass a plain id, or a resumeArgProbe to hook the moment the last
// read completes.
func expectResumeReads(mock sqlmock.Sqlmock, id, name string, descendantArg driver.Value, descendants [][2]string) {
	mock.ExpectQuery("SELECT name, tier, COALESCE").
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"name", "tier", "runtime", "template"}).
			AddRow(name, 1, "claude-code", ""))
	mock.ExpectQuery("SELECT parent_id FROM workspaces WHERE id =").
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"parent_id"}))
	rows := sqlmock.NewRows([]string{"id", "name", "tier", "runtime", "template"})
	for _, d := range descendants {
		rows = rows.AddRow(d[0], d[1], 1, "claude-code", "")
	}
	mock.ExpectQuery("WITH RECURSIVE descendants").
		WithArgs(descendantArg).
		WillReturnRows(rows)
}

// expectResumeProvisionLeg queues everything Resume issues for ONE workspace it
// has successfully claimed: the paused event, then the two provision-input
// lookups. Returns a probe on the first of those lookups — i.e. on "a provision
// was dispatched for this workspace".
// The event INSERT is pinned to (WORKSPACE_PROVISIONING, id) rather than left
// argument-free: sqlmock treats a nil arg list as "matches anything", so the
// provisioner goroutine's WORKSPACE_PROVISION_FAILED insert — the same statement
// with three args — would otherwise be able to consume this slot and leave the
// handler's own event as the refused one, with ExpectationsWereMet still green.
func expectResumeProvisionLeg(mock sqlmock.Sqlmock, id string) *resumeArgProbe {
	mock.ExpectExec("INSERT INTO structure_events").
		WithArgs(string(events.EventWorkspaceProvisioning), id, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	probe := &resumeArgProbe{expect: id}
	mock.ExpectQuery(`SELECT COALESCE\(compute`).
		WithArgs(probe).
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow("{}"))
	mock.ExpectQuery(`SELECT COALESCE\(template`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"template"}).AddRow(""))
	return probe
}

func newResumeHandler(t *testing.T) (*WorkspaceHandler, *fakeCPProv) {
	t.Helper()
	cp := &fakeCPProv{}
	h := NewWorkspaceHandler(newTestBroadcaster(), nil, "http://localhost:8080", t.TempDir())
	h.SetCPProvisioner(cp)
	// The provision leg is a goroutine that reads db.DB. Drain it before the
	// sqlmock is torn down (mc#1264), or its statements race the restore.
	waitForHandlerAsyncBeforeDBCleanup(t, h)
	return h, cp
}

// shrinkResumeBudget swaps resumeSideEffectBudget for the duration of one test.
func shrinkResumeBudget(t *testing.T, d time.Duration) {
	t.Helper()
	prev := resumeSideEffectBudget
	resumeSideEffectBudget = d
	t.Cleanup(func() { resumeSideEffectBudget = prev })
}

// ─────────────────────────────────────────────────────────────────────────────
// 1. Detach — a cancelled client must still get its status write
// ─────────────────────────────────────────────────────────────────────────────

// TestResumeHandler_ClientCancelMidFlight_ClaimStillLands reproduces the prod
// sequence: the reads succeed, the client goes away, and the mutation phase
// begins. The claim UPDATE and the provisioning event must still land.
//
// The cancel is fired from a sqlmock Argument matcher on the LAST read, so it
// happens deterministically at the read→write boundary — no sleeps, no timing.
//
// Pre-fix these ran on the request context, so database/sql short-circuits on
// ctx.Err() in DB.conn before ever reaching the driver: sqlmock never sees the
// UPDATE and ExpectationsWereMet reports it unmet — while the handler still
// answers 200.
func TestResumeHandler_ClientCancelMidFlight_ClaimStillLands(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler, _ := newResumeHandler(t)

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	killer := &resumeArgProbe{expect: "ws-resume-cancel", onHit: cancel}
	expectResumeReads(mock, "ws-resume-cancel", "Agent A", killer, nil)

	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusProvisioning, "ws-resume-cancel").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectResumeProvisionLeg(mock, "ws-resume-cancel")

	w := resumeCall(handler, "ws-resume-cancel", "", reqCtx)
	handler.waitAsyncForTest()

	if !killer.hit() {
		t.Fatal("the cancel hook never fired — the fixture did not reproduce a mid-flight client disconnect")
	}
	if reqCtx.Err() == nil {
		t.Fatal("request context is still live; this test proves nothing")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("a cancelled client cancelled Resume's own writes — the side effects are still on the request context: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 once the writes landed, got %d: %s", w.Code, w.Body.String())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 2. Atomic claim — and no provision without one
// ─────────────────────────────────────────────────────────────────────────────

// TestResumeHandler_ClaimMissed_IsNot200_AndProvisionsNothing is the core
// regression. The workspace left the paused set between the eligibility SELECT
// and the claim (a concurrent Resume/Wake won, or it was removed), so the
// guarded UPDATE matches 0 rows.
//
// Two things must hold, and pre-fix NEITHER did: the caller must not be told
// 200, and — more importantly — no compute may be started. A provision
// dispatched for a row this request does not own is the exact shape that
// produced a running, billed box behind a 'paused' row.
func TestResumeHandler_ClaimMissed_IsNot200_AndProvisionsNothing(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler, _ := newResumeHandler(t)

	expectResumeReads(mock, "ws-resume-lost", "Agent A", "ws-resume-lost", nil)

	// rowsAffected = 0: the predicate `AND status = 'paused'` matched nothing.
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusProvisioning, "ws-resume-lost").
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Queued but NOT expected to run. If Resume dispatches a provision for a
	// workspace it did not claim, this probe records the hit.
	provisionProbe := &resumeArgProbe{}
	mock.ExpectQuery(`SELECT COALESCE\(compute`).
		WithArgs(provisionProbe).
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow("{}"))

	w := resumeCall(handler, "ws-resume-lost", "", nil)
	handler.waitAsyncForTest()

	if provisionProbe.hit() {
		t.Error("Resume dispatched a provision for a workspace whose claim matched 0 rows — " +
			"that starts and bills compute behind a row that still reads 'paused', which no sweeper, " +
			"register or heartbeat will ever reconcile")
	}
	if w.Code == http.StatusOK {
		t.Fatalf("want a non-200 when the claim matched no row, got 200: %s", w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", w.Code, w.Body.String())
	}
	body := resumeBody(t, w)
	if got, ok := body["resumed_count"].(float64); !ok || got != 0 {
		t.Errorf("want resumed_count 0, got %v", body["resumed_count"])
	}
	fails, ok := body["failures"].([]any)
	if !ok || len(fails) != 1 {
		t.Fatalf("want 1 failure entry, got %v", body["failures"])
	}
	if stage := fails[0].(map[string]any)["stage"]; stage != "claim" {
		t.Errorf(`want stage "claim", got %v`, stage)
	}
}

// TestResumeHandler_RefusedClaimWrite_IsNot200_AndProvisionsNothing is the same
// contract for the other way the claim can fail: the statement itself errors
// (dead connection, expired budget). Same two obligations.
func TestResumeHandler_RefusedClaimWrite_IsNot200_AndProvisionsNothing(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler, _ := newResumeHandler(t)

	expectResumeReads(mock, "ws-resume-dbdead", "Agent A", "ws-resume-dbdead", nil)

	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusProvisioning, "ws-resume-dbdead").
		WillReturnError(sql.ErrConnDone)

	provisionProbe := &resumeArgProbe{}
	mock.ExpectQuery(`SELECT COALESCE\(compute`).
		WithArgs(provisionProbe).
		WillReturnRows(sqlmock.NewRows([]string{"compute"}).AddRow("{}"))

	w := resumeCall(handler, "ws-resume-dbdead", "", nil)
	handler.waitAsyncForTest()

	if provisionProbe.hit() {
		t.Error("Resume dispatched a provision after its status write was REFUSED — the box starts, the row stays 'paused'")
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 when the status write is refused, got %d: %s", w.Code, w.Body.String())
	}
	body := resumeBody(t, w)
	fails, ok := body["failures"].([]any)
	if !ok || len(fails) != 1 {
		t.Fatalf("want 1 failure entry, got %v", body["failures"])
	}
	if stage := fails[0].(map[string]any)["stage"]; stage != "mark_provisioning" {
		t.Errorf(`want stage "mark_provisioning", got %v`, stage)
	}
	// The reporting path must not need the database it is reporting dead. The
	// mock has no expectation left beyond the deliberately-unused provision
	// probe, so a 500 that arrived with a complete body proves the status was
	// computed in memory and delivered on the HTTP response.
	if body["error"] == nil || body["failed_count"] == nil {
		t.Errorf("500 body is incomplete — the honest status must be reachable with the DB dead: %v", body)
	}
}

// TestResumeHandler_GenuineSuccess_Still200 is the negative control for the two
// tests above: byte-for-byte the same fixture, with exactly ONE input varied —
// the claim UPDATE reports 1 row affected instead of 0 / an error. The handler
// must answer 200 and must dispatch the provision.
//
// Without this, "not 200 on a refused write" is satisfiable by a handler that
// never returns 200 at all.
func TestResumeHandler_GenuineSuccess_Still200(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler, _ := newResumeHandler(t)

	expectResumeReads(mock, "ws-resume-ok", "Agent A", "ws-resume-ok", nil)

	// THE ONE VARIED INPUT: 1 row claimed.
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusProvisioning, "ws-resume-ok").
		WillReturnResult(sqlmock.NewResult(0, 1))
	provisionProbe := expectResumeProvisionLeg(mock, "ws-resume-ok")

	w := resumeCall(handler, "ws-resume-ok", "", nil)
	handler.waitAsyncForTest()

	if w.Code != http.StatusOK {
		t.Fatalf("want 200 on a genuine resume, got %d: %s", w.Code, w.Body.String())
	}
	body := resumeBody(t, w)
	if body["status"] != "provisioning" {
		t.Errorf(`want status "provisioning", got %v`, body["status"])
	}
	if got, ok := body["resumed_count"].(float64); !ok || got != 1 {
		t.Errorf("want resumed_count 1, got %v", body["resumed_count"])
	}
	if _, hasFailures := body["failures"]; hasFailures {
		t.Errorf("a clean resume must not carry failures[]: %v", body["failures"])
	}
	if !provisionProbe.hit() {
		t.Error("a claimed workspace was never provisioned — the claim gate is refusing valid work")
	}
}

// TestResumeHandler_PartialCascade_Is207 pins the third arm. A cascade fans out
// over descendants, so "2 of 3" is a real outcome: collapsing it into 200 or 500
// discards the only information the caller can act on.
//
// The middle workspace's claim matches no row; the other two succeed.
func TestResumeHandler_PartialCascade_Is207(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler, _ := newResumeHandler(t)

	expectResumeReads(mock, "ws-casc", "Parent", "ws-casc", [][2]string{
		{"ws-child-1", "Child 1"},
		{"ws-child-2", "Child 2"},
	})

	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusProvisioning, "ws-casc").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectResumeProvisionLeg(mock, "ws-casc")

	// child-1 lost the claim — removed, or resumed by a concurrent caller.
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusProvisioning, "ws-child-1").
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusProvisioning, "ws-child-2").
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectResumeProvisionLeg(mock, "ws-child-2")

	w := resumeCall(handler, "ws-casc", "?cascade=true", nil)
	handler.waitAsyncForTest()

	if w.Code != http.StatusMultiStatus {
		t.Fatalf("want 207 on a partial cascade, got %d: %s", w.Code, w.Body.String())
	}
	body := resumeBody(t, w)
	if got, ok := body["resumed_count"].(float64); !ok || got != 2 {
		t.Errorf("want resumed_count 2, got %v", body["resumed_count"])
	}
	if got, ok := body["failed_count"].(float64); !ok || got != 1 {
		t.Errorf("want failed_count 1, got %v", body["failed_count"])
	}
	fails, ok := body["failures"].([]any)
	if !ok || len(fails) != 1 {
		t.Fatalf("want 1 failure entry, got %v", body["failures"])
	}
	if id := fails[0].(map[string]any)["workspace_id"]; id != "ws-child-1" {
		t.Errorf("failures[] names the wrong workspace: %v", id)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// 3. The detached work is BOUNDED, and the ceiling is enforced
// ─────────────────────────────────────────────────────────────────────────────

// TestResumeSideEffectBudget_IsTheNamedCeiling pins the ceiling itself. The
// number is load-bearing: it is what replaces the request context as the bound
// on the detached phase, and it is deliberately NOT pauseSideEffectBudget's 60s
// (Pause's phase contains a synchronous CP HTTP stop; Resume's contains DB
// round-trips only — the provision runs on provisioner.ProvisionTimeout, off
// this budget entirely).
func TestResumeSideEffectBudget_IsTheNamedCeiling(t *testing.T) {
	if resumeSideEffectBudget != 30*time.Second {
		t.Errorf("resumeSideEffectBudget = %v, want 30s — the documented ceiling changed without its rationale", resumeSideEffectBudget)
	}
	if resumeSideEffectBudget <= 0 {
		t.Fatal("a non-positive budget means the detached phase is unbounded")
	}
}

// TestResumeHandler_DetachedWorkIsBounded proves the ceiling is ENFORCED by the
// transport rather than merely declared. Detaching with context.WithoutCancel
// removes the request as a bound; if the timeout were dropped (or the context
// were plain WithoutCancel), a wedged statement would pin the handler goroutine
// and its DB connection forever.
//
// The budget is shrunk to 1ms and the claim UPDATE is made to take 5s.
// sqlmock's ExecContext selects on ctx.Done(), so it returns as soon as the
// deadline fires. The handler must come back promptly with an honest failure —
// not hang, and not 200.
func TestResumeHandler_DetachedWorkIsBounded(t *testing.T) {
	mock := setupTestDB(t)
	setupTestRedis(t)
	handler, _ := newResumeHandler(t)
	shrinkResumeBudget(t, time.Millisecond)

	expectResumeReads(mock, "ws-resume-wedged", "Agent A", "ws-resume-wedged", nil)
	mock.ExpectExec("UPDATE workspaces SET status =").
		WithArgs(models.StatusProvisioning, "ws-resume-wedged").
		WillDelayFor(5 * time.Second).
		WillReturnResult(sqlmock.NewResult(0, 1))

	done := make(chan *httptest.ResponseRecorder, 1)
	start := time.Now()
	go func() { done <- resumeCall(handler, "ws-resume-wedged", "", nil) }()

	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Resume outran its own budget — the detached phase is not bounded by resumeSideEffectBudget")
	}
	handler.waitAsyncForTest()

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Resume took %v against a 1ms budget — the deadline is not being enforced on the statement", elapsed)
	}
	if w.Code == http.StatusOK {
		t.Fatalf("want a non-200 when the budget expired mid-write, got 200: %s", w.Body.String())
	}
	body := resumeBody(t, w)
	fails, ok := body["failures"].([]any)
	if !ok || len(fails) != 1 {
		t.Fatalf("want the expired write reported in failures[], got %v", body["failures"])
	}
	if stage := fails[0].(map[string]any)["stage"]; stage != "mark_provisioning" {
		t.Errorf(`want stage "mark_provisioning", got %v`, stage)
	}
}

// TestResumeClaimMissedError_NamesTheCondition keeps the sentinel's message
// actionable — it is what an operator reads in failures[].error.
func TestResumeClaimMissedError_NamesTheCondition(t *testing.T) {
	if !errors.Is(errResumeClaimMissed, errResumeClaimMissed) {
		t.Fatal("sentinel is not comparable")
	}
	for _, want := range []string{"claim", "paused", "concurrent"} {
		if !strings.Contains(errResumeClaimMissed.Error(), want) {
			t.Errorf("errResumeClaimMissed should mention %q: %q", want, errResumeClaimMissed.Error())
		}
	}
}
