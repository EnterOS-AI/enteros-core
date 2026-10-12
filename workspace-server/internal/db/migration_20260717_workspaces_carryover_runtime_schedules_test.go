package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression pin for core#4435 — the P4b volume-side org-re-import schedule
// inheritance buffer column.
//
// Like migration_20260630_drop_workspaces_llm_billing_mode_test.go this is a
// static-content lint, not a DB-execution test — the apply mechanism is already
// proven by postgres_schema_migrations_test.go. We pin the shape:
//
//   - up.sql ADDs COLUMN carryover_runtime_schedules on the workspaces table,
//     idempotently (IF NOT EXISTS), typed JSONB
//   - up.sql does NOT drop anything (this is ADDITIVE + DARK; the legacy
//     workspace_schedules table and its DB-world re-point path stay until P4b)
//   - down.sql DROPs the column (rollback path)
func TestMigration20260717_AddsWorkspacesCarryoverRuntimeSchedules(t *testing.T) {
	const migDir = "../../migrations"
	const upFile = "20260717000000_workspaces_carryover_runtime_schedules.up.sql"
	const downFile = "20260717000000_workspaces_carryover_runtime_schedules.down.sql"

	upBytes, err := os.ReadFile(filepath.Join(migDir, upFile))
	if err != nil {
		t.Fatalf("read %s: %v", upFile, err)
	}
	downBytes, err := os.ReadFile(filepath.Join(migDir, downFile))
	if err != nil {
		t.Fatalf("read %s: %v", downFile, err)
	}

	// Strip `-- ...` comments so the rationale prose (which names the legacy
	// table + the erase=false teardown constraint) doesn't trip the DDL guards.
	upDDL := stripSQLLineComments(strings.ToLower(string(upBytes)))
	downDDL := stripSQLLineComments(strings.ToLower(string(downBytes)))

	// up.sql MUST add the buffer column on the workspaces table, idempotently.
	if !strings.Contains(upDDL, "add column") || !strings.Contains(upDDL, "carryover_runtime_schedules") {
		t.Errorf("up.sql must ADD COLUMN carryover_runtime_schedules; got DDL:\n%s", upDDL)
	}
	if !strings.Contains(upDDL, "if not exists") {
		t.Errorf("up.sql column add must be idempotent (IF NOT EXISTS); got DDL:\n%s", upDDL)
	}
	if !strings.Contains(upDDL, "workspaces") {
		t.Errorf("up.sql must target the workspaces table; got DDL:\n%s", upDDL)
	}
	if !strings.Contains(upDDL, "jsonb") {
		t.Errorf("up.sql column must be JSONB (holds the runtime-schedule grid array); got DDL:\n%s", upDDL)
	}

	// CARE ZONE: this migration is ADDITIVE + DARK. The up DDL must NOT drop
	// anything — the legacy workspace_schedules table (and its DB-world re-point
	// path) stay until P4b. A DROP here would be exactly the "re-imports silently
	// lose user schedules" hazard the soak-green gate exists to prevent.
	if strings.Contains(upDDL, "drop") {
		t.Errorf("up.sql must be additive-only — no DROP until P4b. DDL:\n%s", upDDL)
	}

	// down.sql MUST drop the column (rollback path).
	if !strings.Contains(downDDL, "drop column") || !strings.Contains(downDDL, "carryover_runtime_schedules") {
		t.Errorf("down.sql must DROP COLUMN carryover_runtime_schedules (rollback path); got DDL:\n%s", downDDL)
	}
}

// TestMigration20260717_PairExists — both files exist and carry their header
// (RunMigrations consumes only the up, but a missing down breaks the dev-side
// manual rollback workflow silently).
func TestMigration20260717_PairExists(t *testing.T) {
	const migDir = "../../migrations"
	for _, f := range []string{
		"20260717000000_workspaces_carryover_runtime_schedules.up.sql",
		"20260717000000_workspaces_carryover_runtime_schedules.down.sql",
	} {
		info, err := os.Stat(filepath.Join(migDir, f))
		if err != nil {
			t.Errorf("expected migration file %s to exist: %v", f, err)
			continue
		}
		if info.Size() < 50 {
			t.Errorf("migration file %s is suspiciously small (%d bytes) — header comment missing?", f, info.Size())
		}
	}
}
