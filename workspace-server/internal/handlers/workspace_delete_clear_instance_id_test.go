package handlers

// workspace_delete_clear_instance_id_test.go — pins the delete path's
// obligation to clear `instance_id` once compute is confirmed stopped.
//
// Background (prod incident 2026-07-30 → 2026-08-03): CascadeDelete sets
// status='removed' but never cleared instance_id, even on a fully successful
// teardown. The CP orphan sweeper (internal/registry/cp_orphan_sweeper.go)
// selects exactly:
//
//	WHERE status = 'removed' AND instance_id IS NOT NULL AND instance_id != ''
//
// and its ONLY exit is that same UPDATE ... SET instance_id = NULL, applied
// after a successful CP Stop. So every successfully-deleted workspace still
// entered the sweeper queue and depended on a second, redundant CP round-trip
// to leave it. When that round-trip returned a non-2xx — as it did for four
// reno-stars workspaces whose CP ledger already said `deleted` — the rows never
// left the queue and the sweeper retried them every 60s forever.
//
// The CP-side fix (molecule-controlplane: Deprovision returns 200 for an
// already-deleted resource) drains the four stuck rows. THIS fix is the
// complementary one: stop them ever entering the queue. A workspace whose
// compute we just confirmed stopped has no live instance, so the field that
// means "a live instance is attached" must not stay populated.
//
// Deliberately NOT a bare attempt cap: the sweeper's retry is the leak
// detector, and capping attempts would silence the symptom while abandoning
// the detection. This removes the FALSE entries instead, so a real leak still
// stands out.

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

// TestClearWorkspaceInstanceIDAfterStop_NullsTheField pins the write itself:
// after a confirmed stop, instance_id must be NULLed for exactly that
// workspace. The UPDATE must mirror the sweeper's own clearing statement so the
// SELECT predicate and both writers stay in sync.
//
// Before the fix this FAILS to compile / find the helper — nothing clears the
// field on the delete path at all.
func TestClearWorkspaceInstanceIDAfterStop_NullsTheField(t *testing.T) {
	mock := setupTestDB(t)
	h := &WorkspaceHandler{}

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workspaces SET instance_id = NULL`)).
		WithArgs("ws-cleared").
		WillReturnResult(sqlmock.NewResult(0, 1))

	h.clearWorkspaceInstanceIDAfterStop(context.Background(), "ws-cleared")

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("delete path must clear instance_id after a confirmed stop: %v", err)
	}
}

// TestClearWorkspaceInstanceIDAfterStop_DBErrorIsNonFatal pins that a failed
// clear never escalates. Teardown has already succeeded by this point; the
// sweeper remains the backstop and will re-drive (and now succeed, given the
// CP-side idempotency fix). Blowing up here would fail a delete that actually
// worked.
func TestClearWorkspaceInstanceIDAfterStop_DBErrorIsNonFatal(t *testing.T) {
	mock := setupTestDB(t)
	buf := captureLog(t)
	h := &WorkspaceHandler{}

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE workspaces SET instance_id = NULL`)).
		WithArgs("ws-dberr").
		WillReturnError(errors.New("connection reset"))

	// Must not panic and must not propagate.
	h.clearWorkspaceInstanceIDAfterStop(context.Background(), "ws-dberr")

	if !strings.Contains(buf.String(), "clear instance_id") {
		t.Fatalf("a failed clear must be logged for operators; got %q", buf.String())
	}
}

// TestClearWorkspaceInstanceIDAfterStop_NilDBIsSafe pins defensiveness against
// an unset global handle, matching cpSweepOnce's own `if db.DB == nil` guard.
func TestClearWorkspaceInstanceIDAfterStop_NilDBIsSafe(t *testing.T) {
	prev := db.DB
	db.DB = nil
	t.Cleanup(func() { db.DB = prev })

	h := &WorkspaceHandler{}
	h.clearWorkspaceInstanceIDAfterStop(context.Background(), "ws-nildb") // must not panic
}
