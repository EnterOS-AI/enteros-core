package handlers

// a2a_queue_enqueue_expired_test.go — regression for CR3 RC 9853.
//
// Bug: a pending buffered tick that expires before the drain reaches it is
// skipped by the drain (it filters out expired pending rows) yet still occupies
// the active set the idempotency check guards. A later tick for the SAME key
// would then collapse onto that dead row and be silently swallowed — the exact
// drop the busy-buffer path was built to prevent.
//
// Fix: EnqueueA2A retires any already-expired pending row for the key BEFORE the
// insert, so the fresh tick buffers (and the stale row is cleaned up) instead of
// being dropped.
//
// These tests use the QueryMatcherEqual mock (setupTestDBForQueueTests) so the
// SQL strings below must match the handler's queries verbatim.

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	enqWorkspaceID = "ws-enq-expired"
	enqKey         = "sched-aaaa-bbbb" // schedule_id used as idempotency key
	enqBody        = `{"method":"message/send"}`
	enqMethod      = "message/send"
)

// expectSupersedeExpired registers the cleanup UPDATE EnqueueA2A issues before
// the insert when an idempotency key is present. rowsRetired is how many expired
// pending rows the UPDATE claims to have dropped.
func expectSupersedeExpired(mock sqlmock.Sqlmock, workspaceID, key string, rowsRetired int64) {
	mock.ExpectExec(`
			UPDATE a2a_queue
			SET status = 'dropped',
			    last_error = 'superseded: expired before drain; replaced by a fresh enqueue'
			WHERE workspace_id = $1
			  AND idempotency_key = $2
			  AND status = 'queued'
			  AND expires_at IS NOT NULL
			  AND expires_at <= now()
		`).
		WithArgs(workspaceID, key).
		WillReturnResult(sqlmock.NewResult(0, rowsRetired))
}

// expectInsert registers the INSERT ... ON CONFLICT DO NOTHING RETURNING id.
// newID is the id the insert returns (non-conflict / fresh enqueue path).
func expectInsert(mock sqlmock.Sqlmock, newID string) {
	mock.ExpectQuery(`
		INSERT INTO a2a_queue (workspace_id, caller_id, priority, body, method, idempotency_key, expires_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, $6, $7)
		ON CONFLICT (workspace_id, idempotency_key)
			WHERE idempotency_key IS NOT NULL AND status IN ('queued','dispatched')
			DO NOTHING
		RETURNING id
	`).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(newID))
}

// expectDepth registers the trailing queue-depth count query.
func expectDepth(mock sqlmock.Sqlmock, workspaceID string, depth int) {
	mock.ExpectQuery(`
		SELECT COUNT(*) FROM a2a_queue
		WHERE workspace_id = $1 AND status = 'queued'
	`).WithArgs(workspaceID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(depth))
}

// TestEnqueueA2A_ExpiredRowDoesNotBlockFreshTick is the core CR3 regression:
// an existing expired pending row for a schedule's key must NOT cause the next
// tick's enqueue to be dropped. The expired row is retired first, then the
// fresh tick inserts and returns a NEW id.
func TestEnqueueA2A_ExpiredRowDoesNotBlockFreshTick(t *testing.T) {
	mock := setupTestDBForQueueTests(t)

	// One expired pending row exists for this key and gets retired.
	expectSupersedeExpired(mock, enqWorkspaceID, enqKey, 1)
	// With the active set cleared, the insert proceeds (no conflict) → new id.
	const freshID = "fresh-tick-id"
	expectInsert(mock, freshID)
	expectDepth(mock, enqWorkspaceID, 1)

	nextRun := time.Now().Add(30 * time.Second)
	id, depth, err := EnqueueA2A(
		context.Background(), enqWorkspaceID, "", PriorityTask,
		[]byte(enqBody), enqMethod, enqKey, &nextRun,
	)
	if err != nil {
		t.Fatalf("EnqueueA2A returned error: %v", err)
	}
	if id != freshID {
		t.Errorf("expected the fresh tick to enqueue with a new id %q, got %q "+
			"(an expired row must not swallow the new tick)", freshID, id)
	}
	if depth != 1 {
		t.Errorf("expected depth 1, got %d", depth)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestEnqueueA2A_NoExpiredRow_NormalEnqueue: when no expired row exists the
// supersede UPDATE simply affects zero rows and the enqueue proceeds normally.
func TestEnqueueA2A_NoExpiredRow_NormalEnqueue(t *testing.T) {
	mock := setupTestDBForQueueTests(t)

	expectSupersedeExpired(mock, enqWorkspaceID, enqKey, 0) // nothing to retire
	const newID = "new-id"
	expectInsert(mock, newID)
	expectDepth(mock, enqWorkspaceID, 2)

	nextRun := time.Now().Add(30 * time.Second)
	id, depth, err := EnqueueA2A(
		context.Background(), enqWorkspaceID, "", PriorityTask,
		[]byte(enqBody), enqMethod, enqKey, &nextRun,
	)
	if err != nil {
		t.Fatalf("EnqueueA2A returned error: %v", err)
	}
	if id != newID {
		t.Errorf("expected id %q, got %q", newID, id)
	}
	if depth != 2 {
		t.Errorf("expected depth 2, got %d", depth)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// TestEnqueueA2A_NoKey_SkipsSupersede: with no idempotency key there is no
// active-set conflict to guard, so the supersede cleanup is skipped entirely
// and only the insert + depth queries run.
func TestEnqueueA2A_NoKey_SkipsSupersede(t *testing.T) {
	mock := setupTestDBForQueueTests(t)

	// No expectSupersedeExpired — it must NOT be issued when key is empty.
	const newID = "no-key-id"
	expectInsert(mock, newID)
	expectDepth(mock, enqWorkspaceID, 1)

	id, _, err := EnqueueA2A(
		context.Background(), enqWorkspaceID, "", PriorityTask,
		[]byte(enqBody), enqMethod, "", nil,
	)
	if err != nil {
		t.Fatalf("EnqueueA2A returned error: %v", err)
	}
	if id != newID {
		t.Errorf("expected id %q, got %q", newID, id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}
