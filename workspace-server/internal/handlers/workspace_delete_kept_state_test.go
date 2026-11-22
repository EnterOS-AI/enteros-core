package handlers

// workspace_delete_kept_state_test.go — a delete that keeps the workspace's
// saved data must leave behind what is needed to erase that data later
// (molecule-core#5193).
//
// A plain DELETE (no erase_data, no purge) stops the workspace keep-state: on
// Kubernetes the Deployment goes and the identity claim, the seed Secret and
// the Service stay. The row stays too, status='removed'. Before this fix the
// delete then NULLed instance_id, and the control plane's teardown — the only
// call that can erase a workspace's state — refuses a request without the
// instance id (DELETE /cp/workspaces/:id?instance_id=...). So nothing could ever
// erase that state again: every plain delete kept one identity claim against
// the namespace's persistentvolumeclaims quota for good, and a tenant that
// recreated a workspace a dozen times could provision nothing at all.
//
// The fix keeps the stopped instance's id in kept_state_instance_id (and the
// time in kept_state_since) so the kept-state reaper
// (registry/cp_kept_state_reaper.go) can erase the state once the retention
// window has passed. A delete that erased clears both: nothing is kept.

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
)

var (
	keepStateClearRe   = regexp.QuoteMeta(db.ClearInstanceIDKeepingStateSQL)
	erasedStateClearRe = regexp.QuoteMeta(db.ClearInstanceIDStateErasedSQL)
)

// The keep-state form must actually move the stopped id into
// kept_state_instance_id, and the erased form must not keep it when there was an
// instance to erase. Pinned on the text because sqlmock cannot run the UPDATE.
func TestKeptStateClearStatements(t *testing.T) {
	if !strings.Contains(db.ClearInstanceIDKeepingStateSQL, "kept_state_instance_id = COALESCE(NULLIF(instance_id, ''), kept_state_instance_id)") ||
		!strings.Contains(db.ClearInstanceIDKeepingStateSQL, "instance_id = NULL") {
		t.Errorf("keep-state clear does not remember the stopped instance: %s", db.ClearInstanceIDKeepingStateSQL)
	}
	if !strings.Contains(db.ClearInstanceIDStateErasedSQL, "kept_state_instance_id = CASE WHEN COALESCE(instance_id, '') = '' THEN kept_state_instance_id END") {
		t.Errorf("erased clear must forget the kept state once an instance was erased: %s", db.ClearInstanceIDStateErasedSQL)
	}
}

// A plain delete keeps the data, and with it the instance id the erase needs.
func TestDelete_PlainDeleteRemembersTheKeptStateForTheReaper(t *testing.T) {
	cp, status := deleteLeafOnCPExpecting(t, "", false, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(keepStateClearRe).
			WithArgs(deleteEraseWS).
			WillReturnResult(sqlmock.NewResult(0, 1))
	})
	if status != "removed" {
		t.Errorf("status = %q, want removed", status)
	}
	if len(cp.pruned) != 0 {
		t.Errorf("a plain delete asked the control plane to erase %v", cp.pruned)
	}
}

// erase_data=true erased the state, so nothing is kept for the reaper.
func TestDelete_EraseDataForgetsTheKeptState(t *testing.T) {
	deleteLeafOnCPExpecting(t, "&erase_data=true", false, func(mock sqlmock.Sqlmock) {
		mock.ExpectExec(erasedStateClearRe).
			WithArgs(deleteEraseWS).
			WillReturnResult(sqlmock.NewResult(0, 1))
	})
}

// The two forms of the write, directly.
func TestClearWorkspaceInstanceIDAfterStop_KeptVersusErased(t *testing.T) {
	mock := setupTestDB(t)
	h := &WorkspaceHandler{}

	mock.ExpectExec(keepStateClearRe).WithArgs("ws-kept").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(erasedStateClearRe).WithArgs("ws-erased").WillReturnResult(sqlmock.NewResult(0, 1))

	h.clearWorkspaceInstanceIDAfterStop(context.Background(), "ws-kept", false)
	h.clearWorkspaceInstanceIDAfterStop(context.Background(), "ws-erased", true)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("kept vs erased clear: %v", err)
	}
}
