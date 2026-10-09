//go:build integration
// +build integration

// plugin_drift_queue_integration_test.go — REAL Postgres gate for
// queueDriftEntry's ON CONFLICT clause (core#4977 follow-up).
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run TestIntegration_DriftQueueInsert -v
//
// WHY THIS EXISTS
//
// plugin_update_queue's uniqueness is a PARTIAL index:
//
//	CREATE UNIQUE INDEX plugin_update_queue_pending_unique
//	  ON plugin_update_queue (workspace_id, plugin_name) WHERE status = 'pending'
//
// Postgres will only infer a partial index if the ON CONFLICT clause repeats
// the predicate. An inference clause without `WHERE status = 'pending'` matches
// NOTHING and the statement fails outright with
//
//	there is no unique or exclusion constraint matching the ON CONFLICT specification
//
// The sweeper logs that error and continues, so the queue silently stayed empty
// FOREVER: drift was detected, never enqueued, never applied. Observed in
// production — a workspace with real drift logged "drift detected ... queue
// drift ... failed" on every sweep.
//
// sqlmock cannot catch this class: it never executes the statement against a
// real planner, so a canned OK is returned for SQL Postgres would reject. Only
// a real INSERT proves the clause is inferable.
//
// NOT SAFE FOR t.Parallel() — seeds shared tables.

package handlers

import (
	"context"
	"database/sql"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func integrationDB_DriftQueue(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("postgres", requireIntegrationDBURL(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	clear := func() {
		if _, err := conn.ExecContext(context.Background(),
			`DELETE FROM workspaces WHERE name LIKE 'itest-dq-%';`); err != nil {
			t.Fatalf("clear: %v", err)
		}
	}
	clear()
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev; clear(); conn.Close() })
	return conn
}

func seedDQWorkspace(t *testing.T, conn *sql.DB) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status) VALUES ($1, $2, 'online')`,
		id, "itest-dq-"+id[:8]); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	return id
}

// TestIntegration_DriftQueueInsert_ActuallyEnqueues is the regression gate:
// the sweeper detecting drift must RESULT IN A ROW. Before the fix this failed
// with "no unique or exclusion constraint matching the ON CONFLICT
// specification" and the queue stayed empty forever.
func TestIntegration_DriftQueueInsert_ActuallyEnqueues(t *testing.T) {
	conn := integrationDB_DriftQueue(t)
	ws := seedDQWorkspace(t, conn)

	if err := plugins.QueueDriftEntryForTest(context.Background(),
		ws, "probe-plugin", "ref:main", "0000000000000000000000000000000000000000",
		"1111111111111111111111111111111111111111"); err != nil {
		t.Fatalf("enqueue failed — drift is detected but never queued, so nothing "+
			"ever converges: %v", err)
	}

	var n int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM plugin_update_queue WHERE workspace_id=$1 AND status='pending'`,
		ws).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("pending queue rows = %d, want 1 — the enqueue did not land", n)
	}
}

// TestIntegration_DriftQueueInsert_IsIdempotentWhilePending: re-detecting the
// same drift must NOT pile up duplicate pending rows — that is what the partial
// unique index is for, and what the ON CONFLICT predicate must match.
func TestIntegration_DriftQueueInsert_IsIdempotentWhilePending(t *testing.T) {
	conn := integrationDB_DriftQueue(t)
	ws := seedDQWorkspace(t, conn)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := plugins.QueueDriftEntryForTest(ctx, ws, "probe-plugin", "ref:main",
			"0000000000000000000000000000000000000000",
			"1111111111111111111111111111111111111111"); err != nil {
			t.Fatalf("enqueue %d failed: %v", i, err)
		}
	}

	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM plugin_update_queue WHERE workspace_id=$1 AND status='pending'`,
		ws).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("pending rows after 3 identical detections = %d, want 1 "+
			"(the partial unique index must dedupe)", n)
	}

	// Once retired, the SAME (workspace, plugin) must be able to queue again —
	// otherwise a workspace converges once and is never updated afterwards.
	if _, err := conn.ExecContext(ctx,
		`UPDATE plugin_update_queue SET status='applied' WHERE workspace_id=$1`, ws); err != nil {
		t.Fatalf("retire: %v", err)
	}
	if err := plugins.QueueDriftEntryForTest(ctx, ws, "probe-plugin", "ref:main",
		"1111111111111111111111111111111111111111",
		"2222222222222222222222222222222222222222"); err != nil {
		t.Fatalf("re-enqueue after applied failed — a workspace would converge once "+
			"and then never again: %v", err)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM plugin_update_queue WHERE workspace_id=$1 AND status='pending'`,
		ws).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("pending rows after retire+re-detect = %d, want 1", n)
	}
}
