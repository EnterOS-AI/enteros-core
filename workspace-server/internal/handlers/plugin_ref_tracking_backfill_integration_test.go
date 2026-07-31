//go:build integration
// +build integration

// plugin_ref_tracking_backfill_integration_test.go — REAL Postgres gate for the
// core#4977 fix: branch-pinned plugins must be visible to the drift sweeper.
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run TestIntegration_PluginRefTracking -v
//
// CI: piggybacks on the handlers-postgres-integration workflow — its path
// filter includes workspace-server/internal/handlers/** and migrations/**,
// and its runner selects tests with -run ^TestIntegration_.
//
// WHY THIS CANNOT BE A SQLMOCK TEST
// ---------------------------------
// The defect was a WHERE clause that matched zero rows in production. sqlmock
// never evaluates a WHERE clause — it matches the query text and returns
// whatever rows the test hands it, so a sqlmock sibling would have passed
// happily against the broken predicate for as long as the bug existed (it
// did, for the life of the sweeper). Only a real Postgres can prove that
// production-shaped rows actually come back.
//
// These tests execute plugins.DriftEligibleQuery — the exact constant
// sweepDriftOnce runs — not a restatement of it.
//
// NON-VACUITY / negative control
// ------------------------------
// Each test asserts BOTH legs:
//   - the branch-pinned row IS selected (fails if the fix regresses, or if the
//     backfill is dropped — this is the leg that reproduces the outage)
//   - a local:// row and a NULL-installed_sha row are NOT selected (fails if a
//     fix over-corrected into sweeping rows that have no upstream tip or no
//     content baseline, which would churn re-installs)
//
// A test that only asserted "> 0 rows" could pass by selecting everything, so
// the exclusion leg is load-bearing.
//
// NOT SAFE FOR t.Parallel() — seeds and clears shared tables.

package handlers

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// integrationDB_PluginRefTracking opens the integration Postgres and clears the
// rows this suite owns. Children cascade off workspaces.
func integrationDB_PluginRefTracking(t *testing.T) *sql.DB {
	t.Helper()
	url := requireIntegrationDBURL(t)
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	clear := func() {
		if _, err := conn.ExecContext(context.Background(),
			`DELETE FROM workspaces WHERE name LIKE 'itest-reftrack-%';`); err != nil {
			t.Fatalf("clear: %v", err)
		}
	}
	clear()
	t.Cleanup(func() { clear(); conn.Close() })
	return conn
}

func seedRefTrackWorkspace(t *testing.T, conn *sql.DB) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, status) VALUES ($1, $2, 'online')`,
		id, "itest-reftrack-"+id[:8]); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	return id
}

// seedPlugin inserts a workspace_plugins row. trackedRef is written verbatim so
// a test can seed the PRE-fix state ('none') and prove the backfill moves it.
func seedPlugin(t *testing.T, conn *sql.DB, wsID, name, sourceRaw, trackedRef string, installedSHA *string) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspace_plugins (workspace_id, plugin_name, source_raw, tracked_ref, installed_sha)
		 VALUES ($1, $2, $3, $4, $5)`,
		wsID, name, sourceRaw, trackedRef, installedSHA); err != nil {
		t.Fatalf("seed plugin %s: %v", name, err)
	}
}

// selectEligible runs the sweeper's OWN query and returns the plugin names it
// selected for the given workspace.
func selectEligible(t *testing.T, conn *sql.DB, wsID string) map[string]string {
	t.Helper()
	rows, err := conn.QueryContext(context.Background(), plugins.DriftEligibleQuery)
	if err != nil {
		t.Fatalf("DriftEligibleQuery: %v", err)
	}
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var id, workspaceID, pluginName, sourceRaw, trackedRef, installedSHA string
		if err := rows.Scan(&id, &workspaceID, &pluginName, &sourceRaw, &trackedRef, &installedSHA); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if workspaceID == wsID {
			got[pluginName] = trackedRef
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return got
}

// applyBackfillMigration runs the up-migration this change ships.
func applyBackfillMigration(t *testing.T, conn *sql.DB) {
	t.Helper()
	path := filepath.Join("..", "..", "migrations",
		"20260731000000_workspace_plugins_backfill_ref_tracking.up.sql")
	sqlBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), string(sqlBytes)); err != nil {
		t.Fatalf("apply migration: %v", err)
	}
}

// TestIntegration_PluginRefTracking_BackfillMakesBranchPinsSweepable is the
// regression gate for the outage in core#4977.
//
// It seeds the EXACT production row shape observed on the reno-stars tenant —
// every row at tracked_ref='none', a branch-pinned client plugin among them —
// proves the sweeper selects NOTHING (the outage), then applies the backfill
// and proves the branch pin becomes selectable.
func TestIntegration_PluginRefTracking_BackfillMakesBranchPinsSweepable(t *testing.T) {
	conn := integrationDB_PluginRefTracking(t)
	wsID := seedRefTrackWorkspace(t, conn)

	sha := "3d41a341aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	noSHA := (*string)(nil)

	// Production shape, pre-fix: everything at 'none'.
	seedPlugin(t, conn, wsID, "reno-stars-coordinator",
		"gitea://RenoStarsAI-production-client/reno-stars-coordinator#main", "none", &sha)
	seedPlugin(t, conn, wsID, "molecule-ai-plugin-digest-mail",
		"gitea://molecule-ai/molecule-ai-plugin-digest-mail#v0.2.1", "none", &sha)
	// Excluded for real reasons — must STAY excluded after the backfill.
	seedPlugin(t, conn, wsID, "seo-all", "local://seo-all", "none", &sha)
	seedPlugin(t, conn, wsID, "no-baseline",
		"gitea://molecule-ai/no-baseline#main", "none", noSHA)

	// LEG 1 — reproduce the outage: the sweeper sees nothing at all.
	if before := selectEligible(t, conn, wsID); len(before) != 0 {
		t.Fatalf("pre-backfill: expected the sweeper to select ZERO rows "+
			"(this is the core#4977 outage state), got %v", before)
	}

	applyBackfillMigration(t, conn)

	after := selectEligible(t, conn, wsID)

	// LEG 2 — the branch pin is now sweepable. This is the leg that fails if
	// the fix or the backfill regresses.
	if got, ok := after["reno-stars-coordinator"]; !ok || got != "ref:main" {
		t.Errorf("branch-pinned plugin: tracked_ref = %q (present=%v), want %q — "+
			"a commit on main cannot reach a running workspace without this",
			got, ok, "ref:main")
	}

	// A bare tag name is tracked the same way; it resolves to a constant SHA
	// and so simply never reports drift.
	if got, ok := after["molecule-ai-plugin-digest-mail"]; !ok || got != "ref:v0.2.1" {
		t.Errorf("bare-tag plugin: tracked_ref = %q (present=%v), want %q", got, ok, "ref:v0.2.1")
	}

	// LEG 3 — negative control. Rows with no upstream tip or no content
	// baseline must NOT become sweepable, or the sweeper would churn.
	if _, ok := after["seo-all"]; ok {
		t.Error("local:// plugin became sweepable: it has no upstream tip to chase")
	}
	if _, ok := after["no-baseline"]; ok {
		t.Error("plugin with NULL installed_sha became sweepable: no baseline to compare against")
	}
}

// TestIntegration_PluginRefTracking_BackfillIsIdempotent: the migration must be
// safe to re-run (migration runners retry, and operators re-apply).
func TestIntegration_PluginRefTracking_BackfillIsIdempotent(t *testing.T) {
	conn := integrationDB_PluginRefTracking(t)
	wsID := seedRefTrackWorkspace(t, conn)

	sha := "abc1230000000000000000000000000000000000"
	seedPlugin(t, conn, wsID, "branch-pinned",
		"gitea://o/r#main", "none", &sha)
	seedPlugin(t, conn, wsID, "tag-pinned",
		"gitea://o/r2#tag:v1.0", "tag:v1.0", &sha)

	applyBackfillMigration(t, conn)
	first := selectEligible(t, conn, wsID)
	applyBackfillMigration(t, conn)
	second := selectEligible(t, conn, wsID)

	if len(first) != len(second) {
		t.Fatalf("re-running the migration changed the result set: %v then %v", first, second)
	}
	for name, ref := range first {
		if second[name] != ref {
			t.Errorf("%s: tracked_ref drifted across re-runs, %q then %q", name, ref, second[name])
		}
	}
	// An immutable pin must survive untouched — the backfill only fills 'none'.
	if first["tag-pinned"] != "tag:v1.0" {
		t.Errorf("tag pin was rewritten by the backfill: got %q, want %q", first["tag-pinned"], "tag:v1.0")
	}
}
