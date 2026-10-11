//go:build integration

package handlers

// degraded_plugin_sweeper_relation_integration_test.go — core#5025 finding 8:
// the gauge and the endpoint must be counting the same thing.
//
// molecule_plugin_install_degraded_workspaces advertises "Live workspaces whose
// declared plugin set is degraded". The count behind it read
// workspace_plugin_install_reports ALONE, while the contract endpoint the HELP
// text points an operator at (GET /admin/plugin-install-reports?status=degraded)
// evaluates the same predicate over reports JOINed to workspaces.
//
// Deletes in this system are SOFT — workspace_crud marks status='removed' and
// leaves the row — so the reports table's ON DELETE CASCADE never fires. A
// deleted workspace's report row therefore keeps satisfying the predicate
// forever: the gauge can never return to zero, the alert can never clear, and
// the endpoint an operator opens to find the culprit lists fewer workspaces than
// the number that paged them.
//
// This is an integration test because the defect IS the relation. A mock told
// which query to expect will happily return whatever count the test wants,
// whichever tables the statement actually names.

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func degradedRelationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("INTEGRATION_DB_URL")
	if dsn == "" {
		t.Skip("INTEGRATION_DB_URL unset")
	}
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Skipf("no database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// seedDegradedReport creates a workspace in `status` and gives it a report row
// that satisfies degradedFleetPredicate (declared AND swapped AND failed != []).
func seedDegradedReport(t *testing.T, conn *sql.DB, name, status string) string {
	t.Helper()
	id := uuid.New().String()
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, kind, tier, runtime, status)
		VALUES ($1, $2, 'workspace', 0, 'claude-code', $3)`, id, name, status); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(`DELETE FROM workspaces WHERE id = $1`, id) })
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspace_plugin_install_reports
		       (workspace_id, declared, plugins_dir, installed, skipped, failed, swapped)
		VALUES ($1, true, '/configs/plugins', '["gitea://a"]'::jsonb, '[]'::jsonb,
		        '["gitea://enteros-wechat-channel"]'::jsonb, true)`, id); err != nil {
		t.Fatalf("seed report: %v", err)
	}
	return id
}

func countVia(t *testing.T, conn *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query failed:\n%s\nerr: %v", query, err)
	}
	return n
}

// TestIntegration_DegradedCount_ExcludesSoftDeletedWorkspaces is the finding.
func TestIntegration_DegradedCount_ExcludesSoftDeletedWorkspaces(t *testing.T) {
	conn := degradedRelationDB(t)
	live := seedDegradedReport(t, conn, "degraded-live", "online")
	gone := seedDegradedReport(t, conn, "degraded-removed", "removed")

	got := countVia(t, conn, degradedCountQuery())
	if got != 1 {
		t.Fatalf("degraded count = %d, want 1 (live=%s counted, soft-deleted=%s not). "+
			"Deletes are soft, so the report row of a removed workspace still satisfies the "+
			"predicate — the gauge can never return to zero and its alert can never clear.",
			got, live, gone)
	}
}

// TestIntegration_DegradedCount_AgreesWithTheContractEndpoint is the stronger
// statement, and the one that keeps them from drifting apart again: the number
// on the gauge must equal the number of rows an operator sees when they open the
// endpoint the metric's own HELP text sends them to.
func TestIntegration_DegradedCount_AgreesWithTheContractEndpoint(t *testing.T) {
	conn := degradedRelationDB(t)
	seedDegradedReport(t, conn, "agree-live-1", "online")
	seedDegradedReport(t, conn, "agree-live-2", "degraded")
	seedDegradedReport(t, conn, "agree-removed", "removed")
	// A live workspace whose plugins are fine — proves the predicate is still
	// doing work and the two queries are not merely both counting everything.
	healthy := uuid.New().String()
	if _, err := conn.Exec(`
		INSERT INTO workspaces (id, name, kind, tier, runtime, status)
		VALUES ($1, 'agree-healthy', 'workspace', 0, 'claude-code', 'online')`, healthy); err != nil {
		t.Fatalf("seed healthy workspace: %v", err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(`DELETE FROM workspaces WHERE id = $1`, healthy) })
	if _, err := conn.Exec(`
		INSERT INTO workspace_plugin_install_reports
		       (workspace_id, declared, plugins_dir, installed, skipped, failed, swapped)
		VALUES ($1, true, '/configs/plugins', '["gitea://a"]'::jsonb, '[]'::jsonb, '[]'::jsonb, true)`,
		healthy); err != nil {
		t.Fatalf("seed healthy report: %v", err)
	}

	gauge := countVia(t, conn, degradedCountQuery())

	rows, err := conn.Query(fleetReportQuery(degradedFleetPredicate), maxListLimit)
	if err != nil {
		t.Fatalf("endpoint query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	endpoint := 0
	for rows.Next() {
		endpoint++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("endpoint rows: %v", err)
	}

	if gauge != endpoint {
		t.Fatalf("the gauge says %d degraded workspaces and the endpoint its HELP text points at lists "+
			"%d. One of the two is lying to whoever is holding the pager.", gauge, endpoint)
	}
	if gauge != 2 {
		t.Fatalf("expected exactly the 2 live degraded workspaces, got %d", gauge)
	}
}

// TestIntegration_DegradedCount_IsDerivedFromOneRelation is the anti-drift leg.
//
// The point is not that today's two statements happen to agree; it is that there
// is only ONE definition of "which rows are in scope" to disagree about. A future
// change to the endpoint's FROM/JOIN must move the gauge with it automatically.
func TestIntegration_DegradedCount_IsDerivedFromOneRelation(t *testing.T) {
	conn := degradedRelationDB(t)
	if _, err := conn.Query(degradedCountQuery()); err != nil {
		t.Fatalf("the count query must be valid SQL against the real schema: %v", err)
	}
	if !containsAll(degradedCountQuery(), degradedFleetRelation, degradedFleetPredicate) {
		t.Fatalf("degradedCountQuery must be BUILT from the shared relation and predicate, not retyped:\n%s",
			degradedCountQuery())
	}
	if !containsAll(fleetReportQuery(degradedFleetPredicate), degradedFleetRelation) {
		t.Fatalf("fleetReportQuery must be built from the same shared relation:\n%s",
			fleetReportQuery(degradedFleetPredicate))
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
