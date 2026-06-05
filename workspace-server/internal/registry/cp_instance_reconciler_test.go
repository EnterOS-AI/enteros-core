package registry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// fakeRunningChecker implements InstanceRunningChecker for the
// instance-reconciler tests. Records every IsRunning call so tests can
// assert which workspace IDs were probed, and returns a per-id
// (running, err) pair so we can model CP's three answers:
//
//	(true,  nil) — instance is running.
//	(false, nil) — CLEAN "not running" (terminated/stopped/absent).
//	(true,  err) — transient DB/transport error (FAIL-SAFE path).
type fakeRunningChecker struct {
	mu      sync.Mutex
	running map[string]bool
	errs    map[string]error
	calls   []string
}

func (f *fakeRunningChecker) IsRunning(_ context.Context, wsID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, wsID)
	if err, ok := f.errs[wsID]; ok {
		// Mirror CPProvisioner.IsRunning: (true, err) on transient errors
		// so callers stay on the alive path.
		return true, err
	}
	return f.running[wsID], nil
}

// recordingOffline is an OfflineHandler that records the workspace IDs
// it was invoked with.
type recordingOffline struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingOffline) handler() OfflineHandler {
	return func(_ context.Context, wsID string) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, wsID)
	}
}

func (r *recordingOffline) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	copy(out, r.calls)
	return out
}

// expectReconcileQuery registers the reconciler's SELECT, pinning the
// scope-critical predicates: status='online', instance_id present, and
// runtime <> 'external'. A future widening that drops any of these (e.g.
// sweeping paused rows, or external rows the heartbeat pass owns) fails
// every test that uses this helper.
func expectReconcileQuery(mock sqlmock.Sqlmock, rows *sqlmock.Rows) {
	mock.ExpectQuery(`(?s)^\s*SELECT id::text\s+FROM workspaces\s+WHERE status = 'online'\s+AND instance_id IS NOT NULL\s+AND instance_id != ''\s+AND COALESCE\(runtime, ''\) <> 'external'\s+ORDER BY updated_at DESC\s+LIMIT \$1`).
		WithArgs(CPInstanceReconcileLimit).
		WillReturnRows(rows)
}

// TestReconcileOnce_NotRunning_FlipsOffline — the core bug (core#2247):
// an online SaaS workspace whose EC2 is terminated. CP reports a CLEAN
// (false, nil); onOffline MUST be called with that id so the existing
// auto-heal (status flip + RestartByID reprovision) kicks in.
func TestReconcileOnce_NotRunning_FlipsOffline(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{running: map[string]bool{"ws-dead": false}}
	off := &recordingOffline{}

	expectReconcileQuery(mock, sqlmock.NewRows([]string{"id"}).AddRow("ws-dead"))

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 1 || got[0] != "ws-dead" {
		t.Fatalf("expected onOffline(ws-dead), got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_Running_DoesNotFlip — healthy steady state. CP
// reports (true, nil); the workspace stays online, onOffline is NOT
// called.
func TestReconcileOnce_Running_DoesNotFlip(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{running: map[string]bool{"ws-alive": true}}
	off := &recordingOffline{}

	expectReconcileQuery(mock, sqlmock.NewRows([]string{"id"}).AddRow("ws-alive"))

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 0 {
		t.Fatalf("running workspace must NOT be flipped offline, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_TransientError_DoesNotFlip — FAIL-SAFE contract.
// IsRunning returns (true, err) on a transient DB/transport blip; the
// reconciler MUST NOT flip the workspace offline. This is the guardrail
// that stops a CP outage from cascading every healthy workspace through
// reprovision.
func TestReconcileOnce_TransientError_DoesNotFlip(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{
		errs: map[string]error{"ws-blip": errors.New("cp provisioner: status: connection reset")},
	}
	off := &recordingOffline{}

	expectReconcileQuery(mock, sqlmock.NewRows([]string{"id"}).AddRow("ws-blip"))

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 0 {
		t.Fatalf("fail-safe violated: transient IsRunning error must NOT flip offline, got %v", got)
	}
	if calls := checker.calls; len(calls) != 1 || calls[0] != "ws-blip" {
		t.Fatalf("expected IsRunning(ws-blip), got %v", checker.calls)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_QueryScopeExcludesExternalAndNonOnline — pins the
// SELECT predicate. The regex in expectReconcileQuery requires
// status='online' AND runtime <> 'external'; if a future edit widens the
// scope to include paused/hibernated/removed rows or external rows (owned
// by the heartbeat pass), this query no longer matches and sqlmock fails
// the test. With the predicate intact, a DB that has only out-of-scope
// rows returns empty → no IsRunning, no flip.
func TestReconcileOnce_QueryScopeExcludesExternalAndNonOnline(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{}
	off := &recordingOffline{}

	// The predicate filters out external + non-online rows server-side,
	// modelled as the empty result those filters produce.
	expectReconcileQuery(mock, sqlmock.NewRows([]string{"id"}))

	reconcileOnce(context.Background(), checker, off.handler())

	if len(checker.calls) != 0 {
		t.Fatalf("out-of-scope rows must never reach IsRunning, got %v", checker.calls)
	}
	if got := off.got(); len(got) != 0 {
		t.Fatalf("expected no offline flips for out-of-scope rows, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_MixedBatch — each row is judged independently: the
// dead one flips, the alive one and the transient-error one don't.
func TestReconcileOnce_MixedBatch(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{
		running: map[string]bool{"ws-dead": false, "ws-alive": true},
		errs:    map[string]error{"ws-blip": errors.New("503")},
	}
	off := &recordingOffline{}

	expectReconcileQuery(mock, sqlmock.NewRows([]string{"id"}).
		AddRow("ws-dead").
		AddRow("ws-alive").
		AddRow("ws-blip"))

	reconcileOnce(context.Background(), checker, off.handler())

	if got := off.got(); len(got) != 1 || got[0] != "ws-dead" {
		t.Fatalf("expected only ws-dead flipped, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_QueryError — DB transient failure. Reconcile returns
// without panicking and never probes IsRunning or flips anything.
func TestReconcileOnce_QueryError(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{}
	off := &recordingOffline{}

	mock.ExpectQuery(`(?s)^\s*SELECT id::text\s+FROM workspaces`).
		WithArgs(CPInstanceReconcileLimit).
		WillReturnError(errors.New("connection refused"))

	reconcileOnce(context.Background(), checker, off.handler())

	if len(checker.calls) != 0 || len(off.got()) != 0 {
		t.Fatalf("query error must short-circuit; calls=%v offline=%v", checker.calls, off.got())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// TestReconcileOnce_NilDB — defensive against db.DB being nil. Must not
// panic, must not probe, must not flip.
func TestReconcileOnce_NilDB(t *testing.T) {
	saved := db.DB
	db.DB = nil
	t.Cleanup(func() { db.DB = saved })

	checker := &fakeRunningChecker{}
	off := &recordingOffline{}
	reconcileOnce(context.Background(), checker, off.handler())

	if len(checker.calls) != 0 || len(off.got()) != 0 {
		t.Fatalf("nil db.DB must short-circuit; calls=%v offline=%v", checker.calls, off.got())
	}
}

// TestStartCPInstanceReconciler_NilCheckerDisabled — boot-safety: a SaaS
// CP without cpProv configured must not start the loop (immediate return,
// no goroutine leak).
func TestStartCPInstanceReconciler_NilCheckerDisabled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		StartCPInstanceReconciler(ctx, nil, nil, 60*time.Second)
		close(done)
	}()
	select {
	case <-done:
		// expected — nil checker short-circuits.
	case <-time.After(500 * time.Millisecond):
		t.Fatal("StartCPInstanceReconciler(nil) did not return immediately")
	}
}

// TestStartCPInstanceReconciler_RunsOnceImmediatelyAndExitsOnCancel —
// cadence contract: one sweep at boot (so a restart starts healing
// immediately), and the loop terminates on ctx cancel.
func TestStartCPInstanceReconciler_RunsOnceImmediatelyAndExitsOnCancel(t *testing.T) {
	mock := setupTestDB(t)
	checker := &fakeRunningChecker{}
	off := &recordingOffline{}

	// Boot sweep query. The 60s ticker won't fire inside the test window;
	// register a second optional expectation so a stray tick can't fail.
	expectReconcileQuery(mock, sqlmock.NewRows([]string{"id"}))
	expectReconcileQuery(mock, sqlmock.NewRows([]string{"id"}))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		StartCPInstanceReconciler(ctx, checker, off.handler(), 60*time.Second)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatal("StartCPInstanceReconciler did not exit on ctx cancel")
	}
}
