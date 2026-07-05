package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression pin for internal#691 — billing-mode removal, Release 2 (contract).
//
// Release 1 (2026-06-30) made the code stop reading the per-workspace
// `workspaces.llm_billing_mode` column: platform-vs-BYOK is now decided
// solely by the resolved provider (providers.Manifest.DeriveProvider →
// MOLECULE_RESOLVED_PROVIDER) and MOLECULE_LLM_BILLING_MODE is no longer
// emitted (guarded by handlers/no_billing_mode_env_test.go). This Release-2
// migration drops the now-dead column.
//
// Like migration_20260520_drop_runtime_image_pins_test.go this is a
// static-content lint, not a DB-execution test — the apply mechanism is
// already proven by postgres_schema_migrations_test.go. We pin the shape:
//
//   - up.sql DROPs COLUMN llm_billing_mode on the workspaces table
//   - up.sql does NOT touch the resolved-provider signal (no scope creep)
//   - down.sql re-adds the column (rollback path)
func TestMigration20260630_DropsWorkspacesLLMBillingMode(t *testing.T) {
	const migDir = "../../migrations"
	const upFile = "20260630120000_drop_workspaces_llm_billing_mode.up.sql"
	const downFile = "20260630120000_drop_workspaces_llm_billing_mode.down.sql"

	upBytes, err := os.ReadFile(filepath.Join(migDir, upFile))
	if err != nil {
		t.Fatalf("read %s: %v", upFile, err)
	}
	downBytes, err := os.ReadFile(filepath.Join(migDir, downFile))
	if err != nil {
		t.Fatalf("read %s: %v", downFile, err)
	}

	// Strip `-- ...` comments so the rationale prose (which names the
	// resolved-provider signal) doesn't trip the DDL-touch guards.
	upDDL := stripSQLLineComments(strings.ToLower(string(upBytes)))
	downDDL := stripSQLLineComments(strings.ToLower(string(downBytes)))

	// up.sql MUST drop the dead column on the workspaces table.
	if !strings.Contains(upDDL, "drop column") || !strings.Contains(upDDL, "llm_billing_mode") {
		t.Errorf("up.sql must DROP COLUMN llm_billing_mode; got DDL:\n%s", upDDL)
	}
	if !strings.Contains(upDDL, "workspaces") {
		t.Errorf("up.sql must target the workspaces table; got DDL:\n%s", upDDL)
	}

	// CARE ZONE: the up DDL must not re-introduce the retired env signal or
	// touch the replacement provider signal — those are app-code concerns,
	// not schema. A reference here would be scope-creep.
	if strings.Contains(upDDL, "molecule_resolved_provider") || strings.Contains(upDDL, "molecule_llm_billing_mode") {
		t.Errorf("up.sql DDL must not reference the provider/billing env signals — that is app-code, not schema. DDL:\n%s", upDDL)
	}

	// down.sql MUST re-add the column (rollback path).
	if !strings.Contains(downDDL, "add column") || !strings.Contains(downDDL, "llm_billing_mode") {
		t.Errorf("down.sql must ADD COLUMN llm_billing_mode (rollback path); got DDL:\n%s", downDDL)
	}
}

// TestMigration20260630_PairExists — both files exist and carry their header
// (RunMigrations consumes only the up, but a missing down breaks the dev-side
// manual rollback workflow silently).
func TestMigration20260630_PairExists(t *testing.T) {
	const migDir = "../../migrations"
	for _, f := range []string{
		"20260630120000_drop_workspaces_llm_billing_mode.up.sql",
		"20260630120000_drop_workspaces_llm_billing_mode.down.sql",
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
