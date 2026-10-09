//go:build integration
// +build integration

// plugin_drift_applier_integration_test.go — REAL Postgres gate for the
// detect → queue → apply → converge loop (core#4977).
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run TestIntegration_DriftApplier -v
//
// CI: piggybacks on the handlers-postgres-integration workflow — its path
// filter includes workspace-server/internal/handlers/** and migrations/**,
// and its runner selects tests with -run ^TestIntegration_.
//
// WHY THIS IS NOT (ONLY) A SQLMOCK TEST
// -------------------------------------
// The property that matters is a CROSS-STATEMENT one: after the applier runs,
// does the drift sweeper's own eligibility query still return the row? sqlmock
// returns canned rows and never evaluates a WHERE clause, so it structurally
// cannot answer that. The concierge case in particular is a silent-corruption
// bug — the pre-fix code left the DB claiming convergence — and only real SQL
// over real rows proves the claim.
//
// The git fetch and Docker restart are stubbed; everything touching the
// DATABASE is real.
//
// NOT SAFE FOR t.Parallel() — seeds shared tables and rebinds db.DB.

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

func integrationDB_DriftApplier(t *testing.T) *sql.DB {
	t.Helper()
	url := requireIntegrationDBURL(t)
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// The slate-clear mirrors admin_plugin_drift_integration_test.go: the
	// partial unique index uniq_workspaces_one_platform_root forbids a SECOND
	// parentless platform row, so a leaked concierge from a predecessor suite
	// makes our kind=platform INSERT collide. Caught exactly that way — this
	// file passed in isolation and failed inside the full ^TestIntegration_
	// run until the platform sweep was added.
	//
	// The platform sweep must take DESCENDANTS with it: workspaces.parent_id is
	// a plain FK (no ON DELETE CASCADE), so deleting a leaked concierge that
	// another suite left children under fails with workspaces_parent_id_fkey.
	// The recursive CTE removes the subtree bottom-up in one statement, at any
	// depth. Found the hard way — a flat `DELETE ... WHERE kind='platform'`
	// passed in isolation and broke inside the full run.
	//
	// Safe because the handlers integration suite runs sequentially (single DB,
	// no t.Parallel); every test seeds the state it needs in its own setup.
	clear := func() {
		if _, err := conn.ExecContext(context.Background(), `
			WITH RECURSIVE doomed AS (
			  SELECT id FROM workspaces WHERE kind = 'platform' OR name LIKE 'itest-applier-%'
			  UNION ALL
			  SELECT w.id FROM workspaces w JOIN doomed d ON w.parent_id = d.id
			)
			DELETE FROM workspaces WHERE id IN (SELECT id FROM doomed);
		`); err != nil {
			t.Fatalf("clear: %v", err)
		}
	}
	clear()

	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev; clear(); conn.Close() })
	return conn
}

// seedApplierWorkspace inserts a workspace of the given kind/status and returns its id.
func seedApplierWorkspace(t *testing.T, conn *sql.DB, kind, status string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status, kind) VALUES ($1, $2, $3, $4)`,
		id, "itest-applier-"+id[:8], status, kind); err != nil {
		t.Fatalf("seed workspace (kind=%s): %v", kind, err)
	}
	return id
}

// seedDriftRow inserts the installed plugin row plus a PENDING queue entry,
// i.e. exactly the state the drift sweeper leaves behind.
func seedDriftRow(t *testing.T, conn *sql.DB, wsID, pluginName, sourceRaw, oldSHA, newSHA string) string {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspace_plugins (workspace_id, plugin_name, source_raw, tracked_ref, installed_sha)
		 VALUES ($1, $2, $3, 'ref:main', $4)`,
		wsID, pluginName, sourceRaw, oldSHA); err != nil {
		t.Fatalf("seed workspace_plugins: %v", err)
	}
	var queueID string
	if err := conn.QueryRowContext(context.Background(),
		`INSERT INTO plugin_update_queue
		   (workspace_id, plugin_name, tracked_ref, current_sha, latest_sha, status)
		 VALUES ($1, $2, 'ref:main', $3, $4, 'pending') RETURNING id`,
		wsID, pluginName, oldSHA, newSHA).Scan(&queueID); err != nil {
		t.Fatalf("seed plugin_update_queue: %v", err)
	}
	return queueID
}

func queueStatus(t *testing.T, conn *sql.DB, queueID string) string {
	t.Helper()
	var s string
	if err := conn.QueryRowContext(context.Background(),
		`SELECT status FROM plugin_update_queue WHERE id = $1`, queueID).Scan(&s); err != nil {
		t.Fatalf("read queue status: %v", err)
	}
	return s
}

func installedSHA(t *testing.T, conn *sql.DB, wsID, pluginName string) string {
	t.Helper()
	var s sql.NullString
	if err := conn.QueryRowContext(context.Background(),
		`SELECT installed_sha FROM workspace_plugins WHERE workspace_id = $1 AND plugin_name = $2`,
		wsID, pluginName).Scan(&s); err != nil {
		t.Fatalf("read installed_sha: %v", err)
	}
	return s.String
}

// sweeperStillSeesDrift runs the drift sweeper's OWN eligibility query and
// reports whether this workspace's plugin is still selectable with a stale SHA.
func sweeperStillSeesDrift(t *testing.T, conn *sql.DB, wsID, pluginName, upstreamSHA string) bool {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), plugins.DriftEligibleQuery)
	if err != nil {
		t.Fatalf("DriftEligibleQuery: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, ws, name, src, ref, sha string
		if err := rows.Scan(&id, &ws, &name, &src, &ref, &sha); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if ws == wsID && name == pluginName {
			return sha != upstreamSHA // still behind upstream => drift visible
		}
	}
	return false
}

const (
	applierOldSHA = "0000000000000000000000000000000000000000"
	applierNewSHA = "1111111111111111111111111111111111111111"
	applierSource = "gitea://RenoStarsAI-production-client/reno-stars-coordinator#main"
)

// TestIntegration_DriftApplier_OrdinaryWorkspaceConverges is the end-to-end
// regression gate for the reported symptom: a branch-pinned plugin whose
// upstream moved must actually converge, with no human and no agent involved.
//
// NON-VACUITY: if the drainer stopped consuming the queue (the pre-fix state,
// where nothing ever called apply), the entry would remain 'pending' and the
// SHA would remain the old one — both assertions below fail.
func TestIntegration_DriftApplier_OrdinaryWorkspaceConverges(t *testing.T) {
	conn := integrationDB_DriftApplier(t)
	wsID := seedApplierWorkspace(t, conn, "workspace", "online")
	queueID := seedDriftRow(t, conn, wsID, "reno-stars-coordinator", applierSource, applierOldSHA, applierNewSHA)

	stubDriftStaging(t, "reno-stars-coordinator", applierNewSHA, false /* docker-less: deliver by pull */)
	spy := &restartSpy{}
	h := NewAdminPluginDriftHandler(NewPluginsHandler(t.TempDir(), nil, spy.fn))

	applied, failed := DrainPendingDrift(context.Background(), h.applyQueuedDrift)
	waitGlobalAsyncForTest()

	if applied != 1 || failed != 0 {
		t.Fatalf("drain applied=%d failed=%d, want 1/0 — the queue was not consumed", applied, failed)
	}
	if got := queueStatus(t, conn, queueID); got != "applied" {
		t.Errorf("queue status = %q, want %q", got, "applied")
	}
	if got := installedSHA(t, conn, wsID, "reno-stars-coordinator"); got != applierNewSHA {
		t.Errorf("installed_sha = %q, want %q — the workspace did not converge", got, applierNewSHA)
	}
	if calls := spy.snapshot(); len(calls) != 1 || calls[0] != wsID {
		t.Errorf("restart dispatches = %v, want exactly [%s] — on a docker-less tenant the restart IS the delivery",
			calls, wsID)
	}
	if sweeperStillSeesDrift(t, conn, wsID, "reno-stars-coordinator", applierNewSHA) {
		t.Error("sweeper still reports drift after a successful converge — it would re-queue and restart forever")
	}
}

// TestIntegration_DriftApplier_DeferredConciergeStaysVisible is the
// silent-corruption gate.
//
// The concierge's restart is deliberately deferred (self-brick guard), so on a
// docker-less tenant NOTHING reaches the box. The pre-fix code still advanced
// installed_sha, which made the sweeper compare new-vs-new and go quiet
// forever: permanently stale, reporting converged.
//
// NON-VACUITY: restore the unconditional re-pin and the last two assertions
// fail — installed_sha becomes the new SHA and the sweeper stops seeing drift.
func TestIntegration_DriftApplier_DeferredConciergeStaysVisible(t *testing.T) {
	conn := integrationDB_DriftApplier(t)
	wsID := seedApplierWorkspace(t, conn, "platform", "online") // concierge
	queueID := seedDriftRow(t, conn, wsID, "reno-stars-coordinator", applierSource, applierOldSHA, applierNewSHA)

	stubDriftStaging(t, "reno-stars-coordinator", applierNewSHA, false /* docker-less: deliver by pull */)
	spy := &restartSpy{}
	h := NewAdminPluginDriftHandler(NewPluginsHandler(t.TempDir(), nil, spy.fn))

	applied, failed := DrainPendingDrift(context.Background(), h.applyQueuedDrift)
	waitGlobalAsyncForTest()

	if applied != 1 || failed != 0 {
		t.Fatalf("drain applied=%d failed=%d, want 1/0", applied, failed)
	}
	// The entry is still retired so the drainer does not re-fetch it every tick.
	if got := queueStatus(t, conn, queueID); got != "applied" {
		t.Errorf("queue status = %q, want %q", got, "applied")
	}
	if calls := spy.snapshot(); len(calls) != 0 {
		t.Errorf("restart dispatched for a platform concierge (%v) — self-brick guard should defer it", calls)
	}
	if got := installedSHA(t, conn, wsID, "reno-stars-coordinator"); got != applierOldSHA {
		t.Errorf("installed_sha = %q, want the OLD %q — nothing reached the box, so claiming the new SHA "+
			"would hide the staleness from every future sweep", got, applierOldSHA)
	}
	if !sweeperStillSeesDrift(t, conn, wsID, "reno-stars-coordinator", applierNewSHA) {
		t.Error("sweeper no longer sees drift for a concierge that never materialized the new bytes — " +
			"this is the silent-corruption case: permanently stale while reporting converged")
	}
}
