//go:build integration

package handlers

// auto_restart_crash_loop_integration_test.go — core#5241's SQL against a real
// Postgres: the source_type JSON path the restart-context dedupe searches by,
// and the guarded write a tripped crash-loop breaker makes. sqlmock pins the
// statement text; only a real database proves it matches the rows it is meant
// to.

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"github.com/google/uuid"
)

// pendingRestartContexts counts wsID's restart-context snapshots per active
// a2a_queue status.
func pendingRestartContexts(t *testing.T, conn *sql.DB, wsID string) (queued, dispatched int) {
	t.Helper()
	rows, err := conn.Query(`
		SELECT status, count(*) FROM a2a_queue
		 WHERE workspace_id = $1
		   AND body #>> '{params,message,metadata,source_type}' = $2
		 GROUP BY status`, wsID, restartContextSourceType)
	if err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		switch status {
		case "queued":
			queued = n
		case "dispatched":
			dispatched = n
		}
	}
	return queued, dispatched
}

func TestIntegration_RestartContext_AtMostOneSnapshotPending(t *testing.T) {
	conn := restartDeclineDB(t)
	id, _ := seedRoutableWorkspace(t, conn, "rc-one-pending")
	h := &WorkspaceHandler{}
	ctx := context.Background()

	// The incident shape: an earlier snapshot the drain handed to a busy target
	// was re-queued under its messageId, so the per-workspace key no longer
	// collides with it.
	body, err := buildRestartA2APayload(id, buildRestartContextMessage(restartContextData{RestartAt: time.Now()}))
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO a2a_queue (workspace_id, priority, body, method, idempotency_key)
		VALUES ($1, 50, $2::jsonb, 'message/send', $3)`, id, string(body), uuid.New().String()); err != nil {
		t.Fatalf("seed re-queued snapshot: %v", err)
	}

	h.enqueueRestartContext(ctx, id, restartContextData{RestartAt: time.Now()}, "integration")
	if queued, dispatched := pendingRestartContexts(t, conn, id); queued != 1 || dispatched != 0 {
		t.Fatalf("after a new restart: %d queued + %d dispatched snapshots; want exactly 1 queued (the new one)", queued, dispatched)
	}

	// The drain is now handing that one over: a further restart must not queue
	// another next to it.
	if _, err := conn.Exec(`UPDATE a2a_queue SET status = 'dispatched' WHERE workspace_id = $1 AND status = 'queued'`, id); err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	h.enqueueRestartContext(ctx, id, restartContextData{RestartAt: time.Now()}, "integration")
	if queued, dispatched := pendingRestartContexts(t, conn, id); queued != 0 || dispatched != 1 {
		t.Fatalf("while one is being delivered: %d queued + %d dispatched snapshots; want 0 + 1", queued, dispatched)
	}
}

func TestIntegration_CrashLoopBreaker_FailsTheWorkspaceWithTheReason(t *testing.T) {
	conn := restartDeclineDB(t)
	setupTestRedis(t) // RecordAndBroadcast publishes to Redis before it reaches the hub
	id, _ := seedRoutableWorkspace(t, conn, "crash-loop-online")
	isolateRestartState(t, id)
	h := &WorkspaceHandler{provisioner: &preflightLocalProv{}, broadcaster: newTestBroadcaster()}
	fillAutoRestarts(t, id, autoRestartLimit)

	h.AutoRestartByID(id, "cp-instance-reconciler")

	var status string
	var reason sql.NullString
	if err := conn.QueryRow(`SELECT status, last_sample_error FROM workspaces WHERE id = $1`, id).Scan(&status, &reason); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if status != "failed" || !strings.Contains(reason.String, "restarted manually") {
		t.Fatalf("status=%q last_sample_error=%q; want failed with the crash-loop reason", status, reason.String)
	}
	var events_ int
	if err := conn.QueryRow(`
		SELECT count(*) FROM structure_events
		 WHERE workspace_id = $1 AND event_type = $2 AND payload->>'source' = 'auto_restart_crash_loop'`,
		id, string(events.EventWorkspaceProvisionFailed)).Scan(&events_); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events_ != 1 {
		t.Fatalf("%d crash-loop events recorded; want 1", events_)
	}

	// Refused again while already failed: no second announcement.
	h.AutoRestartByID(id, "a2a-dead-container")
	if err := conn.QueryRow(`
		SELECT count(*) FROM structure_events
		 WHERE workspace_id = $1 AND event_type = $2 AND payload->>'source' = 'auto_restart_crash_loop'`,
		id, string(events.EventWorkspaceProvisionFailed)).Scan(&events_); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if events_ != 1 {
		t.Fatalf("a workspace already failed was announced again (%d events)", events_)
	}
}

func TestIntegration_CrashLoopBreaker_LeavesAPausedWorkspaceAlone(t *testing.T) {
	conn := restartDeclineDB(t)
	id, _ := seedRoutableWorkspace(t, conn, "crash-loop-paused")
	if _, err := conn.Exec(`UPDATE workspaces SET status = 'paused' WHERE id = $1`, id); err != nil {
		t.Fatalf("pause: %v", err)
	}
	isolateRestartState(t, id)
	h := &WorkspaceHandler{provisioner: &preflightLocalProv{}, broadcaster: newTestBroadcaster()}
	fillAutoRestarts(t, id, autoRestartLimit)

	h.AutoRestartByID(id, "liveness-monitor")

	var status string
	if err := conn.QueryRow(`SELECT status FROM workspaces WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	if status != "paused" {
		t.Fatalf("a paused workspace became %q; the breaker must not overwrite a dormant state", status)
	}
}
