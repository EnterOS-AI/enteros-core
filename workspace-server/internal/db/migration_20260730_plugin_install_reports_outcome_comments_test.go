package db

// Shape test for 20260730190000_workspace_plugin_install_reports_outcome_comments.
// The apply MECHANISM is already proven by postgres_schema_migrations_test.go, so
// this pins the properties that make THIS migration safe and make it the right
// shape of fix:
//
//   - it is a NEW file, not an edit to 20260730060000. That migration has been
//     APPLIED (core#4958, 2af1f3989d39, deployed to staging by
//     staging-tenant-cd) and the runner records applied filenames in
//     schema_migrations and skips them, so editing it would change what the repo
//     claims and nothing about any live database;
//   - it touches no schema and no data, in either direction — COMMENT ON COLUMN
//     only, which is also why it is idempotent;
//   - it does not disturb the partial index. `WHERE declared AND NOT swapped` is
//     the exact complement of the corrected liveness rule; it was already right,
//     and only the prose around it was wrong;
//   - it stops asserting `failed` non-empty ⇒ never promoted, which the runtime
//     does not hold.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pirCommentsMigrationBase = "20260730190000_workspace_plugin_install_reports_outcome_comments"

// The already-applied migration this one corrects. Named here so the assertion
// below fails loudly if someone renames or, worse, edits it.
const pirAppliedMigrationBase = "20260730060000_workspace_plugin_install_reports"

func readPIRCommentsMigration(t *testing.T, base, suffix string) string {
	t.Helper()
	const migDir = "../../migrations"
	path := filepath.Join(migDir, base+suffix)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestPIROutcomeCommentsMigration_IsCommentOnlyAndIdempotent(t *testing.T) {
	up := readPIRCommentsMigration(t, pirCommentsMigrationBase, ".up.sql")

	for _, col := range []string{
		"COMMENT ON COLUMN workspace_plugin_install_reports.failed",
		"COMMENT ON COLUMN workspace_plugin_install_reports.swapped",
	} {
		if !strings.Contains(up, col) {
			t.Errorf("migration must re-document %q — the stale text is what an operator reads out of \\d+", col)
		}
	}
	// COMMENT ON is a set, not an append, so the file is idempotent by
	// construction. What must NOT appear is anything that isn't.
	for _, forbidden := range []string{
		"CREATE TABLE", "ALTER TABLE", "DROP TABLE", "DROP COLUMN",
		"CREATE INDEX", "DROP INDEX", "INSERT INTO", "UPDATE ", "DELETE FROM", "TRUNCATE",
	} {
		if strings.Contains(strings.ToUpper(up), forbidden) {
			t.Errorf("this migration corrects DOCUMENTATION only; found %q — "+
				"the table is in production use on both prod tenants", forbidden)
		}
	}
}

// The partial index is the operator's first query and the exact complement of
// `declared AND swapped`. The finding was that the PROSE contradicted the
// runtime, not the predicate, and re-creating the index would be a schema change
// on a live table for no reason.
func TestPIROutcomeCommentsMigration_LeavesTheNotLiveIndexAlone(t *testing.T) {
	up := readPIRCommentsMigration(t, pirCommentsMigrationBase, ".up.sql")
	down := readPIRCommentsMigration(t, pirCommentsMigrationBase, ".down.sql")

	for name, body := range map[string]string{"up": up, "down": down} {
		upper := strings.ToUpper(body)
		if strings.Contains(upper, "DROP INDEX") || strings.Contains(upper, "CREATE INDEX") {
			t.Errorf("%s migration must not touch workspace_plugin_install_reports_not_live — "+
				"`WHERE declared AND NOT swapped` is already the complement of live", name)
		}
	}
}

// The corrected text must actually say the corrected thing. A migration that
// re-documents a column and repeats the wrong invariant is worse than none: it
// looks like the fix landed.
func TestPIROutcomeCommentsMigration_DropsTheNeverPromotedClaim(t *testing.T) {
	up := readPIRCommentsMigration(t, pirCommentsMigrationBase, ".up.sql")

	failedIdx := strings.Index(up, "COMMENT ON COLUMN workspace_plugin_install_reports.failed")
	if failedIdx < 0 {
		t.Fatal("no comment on failed")
	}
	// Just the COMMENT statements, not the explanatory header above them — the
	// header quotes the old wrong claim on purpose, to explain what changed.
	stmts := up[failedIdx:]

	if !strings.Contains(stmts, "NON-EMPTY DOES NOT\n     MEAN NOT PROMOTED") {
		t.Error("the failed comment must say outright that non-empty does not mean not promoted — " +
			"that inversion is the finding")
	}
	if !strings.Contains(stmts, "LIVENESS IS `declared AND swapped`") {
		t.Error("the swapped comment must state the corrected rule, or the column with no rule on it " +
			"is the one an operator reads")
	}
	if strings.Contains(stmts, "failed = []") || strings.Contains(stmts, "failed == []") {
		t.Error("the corrected comments must not re-assert the retired `failed == []` rule")
	}
}

func TestPIROutcomeCommentsMigration_HasADownThatRevertsToUncommented(t *testing.T) {
	down := readPIRCommentsMigration(t, pirCommentsMigrationBase, ".down.sql")

	for _, col := range []string{
		"COMMENT ON COLUMN workspace_plugin_install_reports.failed IS NULL",
		"COMMENT ON COLUMN workspace_plugin_install_reports.swapped IS NULL",
	} {
		if !strings.Contains(down, col) {
			t.Errorf("down must clear the comment (%q) — 20260730060000 never issued a COMMENT ON, "+
				"so uncommented is the state to revert TO; restoring the old text would re-assert "+
				"an invariant the runtime does not hold", col)
		}
	}
}

// THE constraint that made a new migration necessary rather than a one-line edit.
// 20260730060000 is applied; the runner skips applied filenames, so an edit would
// be invisible to every database that has already run it.
func TestPIROutcomeCommentsMigration_DoesNotEditTheAppliedMigration(t *testing.T) {
	applied := readPIRCommentsMigration(t, pirAppliedMigrationBase, ".up.sql")

	// The applied file must still contain its original (now superseded) text. If a
	// future change "fixes" it in place, that edit reaches no deployed tenant and
	// this says so.
	if !strings.Contains(applied, "Non-empty ⇒ never promoted") {
		t.Error("20260730060000 has been APPLIED (core#4958, deployed to staging) — it must not be " +
			"edited in place; the runner skips applied filenames, so the correction has to be a new " +
			"forward migration. If the intent was to correct it, correct it in " + pirCommentsMigrationBase)
	}
}
