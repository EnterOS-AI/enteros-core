//go:build integration
// +build integration

// plugins_reconcile_local_source_integration_test.go — REAL Postgres e2e for
// the local:// plugin-delivery restart-loop fix (plugins.Source.BoxFetchable).
//
// Run with (same incantation as delegation_ledger_integration_test.go):
//
//   docker run --rm -d --name pg-integration \
//     -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//     -p 55432:5432 postgres:15-alpine
//   # apply migrations for workspaces, workspace_declared_plugins, workspace_plugins
//   cd workspace-server
//   INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//     go test -tags=integration ./internal/handlers/ -run '^TestIntegration_Reconcile_'
//
// CI (.gitea/workflows/handlers-postgres-integration.yml) runs every
// `^TestIntegration_` here on every PR touching workspace-server/internal/handlers/**,
// with an anti-vacuous guard (≥1 "--- PASS:" line). No new workflow is added —
// this fix reuses the existing required real-Postgres gate.
//
// Why an integration test and not only the sqlmock unit tests
// -----------------------------------------------------------
// plugins_reconcile_warming_test.go pins the SuppressRestart FLAG with sqlmock,
// and source_test.go pins the BoxFetchable predicate. This test closes the gap
// sqlmock cannot: it drives the WHOLE reconcile path — real declared/installed
// reads, the real LocalResolver fetch, the real concierge-kind lookup, and the
// real INSERT — against Postgres, then SELECTs the recorded row back. It proves
// that on a real ordinary workspace a declared local:// plugin is delivered,
// its tracking row lands, AND the automatic restart (the loop trigger) is
// suppressed — all observable in true DB/row state.

package handlers

import (
	"context"
	"database/sql"
	"testing"
	"time"

	mdb "git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	_ "github.com/lib/pq"
)

func TestIntegration_Reconcile_LocalSourceSuppressesRestart(t *testing.T) {
	url := requireIntegrationDBURL(t)
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer pingCancel()
	if err := conn.PingContext(pingCtx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	// Background (never-cancelled) for the body + teardown: a WithTimeout ctx's
	// defer cancel() fires before t.Cleanup runs, which would cancel the reset.
	ctx := context.Background()

	// Wire the package-level db.DB so the production reconcile helpers read/
	// write this connection. NOT parallel-safe (global swap); restored on cleanup.
	prev := mdb.DB
	mdb.DB = conn
	t.Cleanup(func() { mdb.DB = prev; conn.Close() })

	const wsID = "a11a11a1-0000-4000-8000-00000000e2e0"

	// Idempotent start + guaranteed teardown. ON DELETE CASCADE removes the
	// declared/tracking rows when the workspace goes, but be explicit so a
	// partial prior run cannot poison this one.
	reset := func() {
		rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rcancel()
		for _, q := range []string{
			`DELETE FROM workspace_plugins WHERE workspace_id = $1`,
			`DELETE FROM workspace_declared_plugins WHERE workspace_id = $1`,
			`DELETE FROM workspaces WHERE id = $1`,
		} {
			if _, err := conn.ExecContext(rctx, q, wsID); err != nil {
				t.Fatalf("reset (%s): %v", q, err)
			}
		}
	}
	reset()
	t.Cleanup(reset)

	// An ORDINARY workspace (kind defaults to 'workspace') → the concierge
	// lifecycle guard does NOT fire, so any restart suppression must come from
	// the local:// (!BoxFetchable) guard alone — the exact case that looped.
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO workspaces (id, name, status) VALUES ($1, $2, 'online')`,
		wsID, "reconcile-local-e2e"); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO workspace_declared_plugins (workspace_id, plugin_name, source_raw) VALUES ($1, 'seo-all', 'local://seo-all')`,
		wsID); err != nil {
		t.Fatalf("insert declared plugin: %v", err)
	}

	// Real LocalResolver serving seo-all from a temp registry + captured deliver.
	h, _ := newReconcileHandler(t)
	var delivered, suppressed bool
	h.deliverOverride = func(_ context.Context, _ string, stage *stageResult) error {
		delivered = true
		suppressed = stage.SuppressRestart
		return nil
	}

	h.ReconcileWorkspacePlugins(ctx, wsID)

	if !delivered {
		t.Fatal("reconcile never reached delivery for the declared local:// plugin (real-DB read path broken)")
	}
	if !suppressed {
		t.Fatal("local:// delivery fired WITH automatic restart — the real-DB reconcile restart loop is NOT broken")
	}

	// The tracking row must have landed through the real INSERT path.
	var n int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM workspace_plugins WHERE workspace_id = $1 AND plugin_name = 'seo-all'`,
		wsID).Scan(&n); err != nil {
		t.Fatalf("select workspace_plugins: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 workspace_plugins tracking row, got %d", n)
	}
}
