package db

// migration_transaction_test.go — the classifier, and the gate on the migration
// set it classifies. No database: every assertion here is about a decision made
// from the file's bytes.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const forwardMigrationsDir = "../../migrations"

func mustPlan(t *testing.T, name, body string) migrationPlan {
	t.Helper()
	p, err := planMigration(name, body)
	if err != nil {
		t.Fatalf("planMigration(%s): %v", name, err)
	}
	return p
}

// TestPlanMigration_WrapsTheOrdinaryCase — the default must be atomic.
func TestPlanMigration_WrapsTheOrdinaryCase(t *testing.T) {
	p := mustPlan(t, "20260624000000_push_tokens.up.sql", "CREATE TABLE push_tokens (id BIGSERIAL PRIMARY KEY);\n")
	if !p.Wrappable {
		t.Fatalf("a plain CREATE TABLE must be wrappable, got %q", p.NotWrappableBecause)
	}
	if p.Body != "CREATE TABLE push_tokens (id BIGSERIAL PRIMARY KEY);\n" {
		t.Fatalf("body was rewritten for no reason: %q", p.Body)
	}
}

// TestPlanMigration_ConcurrentlyIsNotWrappable — Postgres refuses CONCURRENTLY
// inside any transaction block, so wrapping it would break the boot.
func TestPlanMigration_ConcurrentlyIsNotWrappable(t *testing.T) {
	p := mustPlan(t, "x.up.sql", "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS foo ON t (a);\n")
	if p.Wrappable {
		t.Fatalf("CREATE INDEX CONCURRENTLY was classified wrappable — a transaction wrap makes " +
			"Postgres refuse it with \"cannot run inside a transaction block\"")
	}
	if p.NotWrappableBecause == "" {
		t.Fatalf("a non-wrappable plan must say why")
	}
}

// TestPlanMigration_CommentedKeywordsDoNotDecide is the NEGATIVE CONTROL on the
// masking. Two real migrations (048, 20260714120000) discuss CONCURRENTLY at
// length while deliberately not using it. A grep-the-raw-text classifier marks
// both non-wrappable and silently drops atomicity for them — and nothing else
// in the suite would notice.
func TestPlanMigration_CommentedKeywordsDoNotDecide(t *testing.T) {
	cases := map[string]string{
		"line comment": "-- CONCURRENTLY would be ideal but goose wraps in a transaction.\n" +
			"CREATE INDEX IF NOT EXISTS idx_a ON t (a);\n",
		"block comment": "/* why not CONCURRENTLY: the runner uses an implicit transaction */\n" +
			"CREATE INDEX IF NOT EXISTS idx_a ON t (a);\n",
		"string literal": "INSERT INTO notes (body) VALUES ('we considered CONCURRENTLY here');\n",
		"dollar quoted": "CREATE FUNCTION f() RETURNS void AS $$\nBEGIN\n  -- CONCURRENTLY;\n  " +
			"RAISE NOTICE 'x';\nEND;\n$$ LANGUAGE plpgsql;\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := mustPlan(t, "x.up.sql", body)
			if !p.Wrappable {
				t.Fatalf("classified non-wrappable because of a keyword that is not SQL here (%s): %s",
					name, p.NotWrappableBecause)
			}
		})
	}

	// The control for the control: real CONCURRENTLY, same helper, must flip.
	if p := mustPlan(t, "x.up.sql", "CREATE INDEX CONCURRENTLY idx ON t (a);\n"); p.Wrappable {
		t.Fatalf("NEGATIVE CONTROL DEAD: real CONCURRENTLY was also classified wrappable, so the " +
			"cases above prove nothing about the masking")
	}
}

// TestPlanMigration_DollarQuotedBodyIsNotReadAsStatements — a plpgsql body
// contains BEGIN, END and semicolons. Reading them as top-level statements
// would refuse four real migrations at boot.
func TestPlanMigration_DollarQuotedBodyIsNotReadAsStatements(t *testing.T) {
	body := "CREATE OR REPLACE FUNCTION touch() RETURNS trigger AS $$\n" +
		"BEGIN\n  NEW.updated_at = now();\n  RETURN NEW;\nEND;\n$$ LANGUAGE plpgsql;\n"
	p := mustPlan(t, "x.up.sql", body)
	if !p.Wrappable {
		t.Fatalf("a plpgsql function body was mistaken for transaction control: %s", p.NotWrappableBecause)
	}
	if p.Body != body {
		t.Fatalf("the function body was mutilated:\n%s", p.Body)
	}
}

// TestPlanMigration_NormalisesASelfManagedTransaction — one leading BEGIN and
// one trailing COMMIT are REMOVED so the body joins the runner's transaction
// and commits with the ledger row. Leaving them in place would make the file's
// COMMIT close the runner's transaction early, and the ledger row would land in
// a different one — the guarantee would read as bought and not be.
func TestPlanMigration_NormalisesASelfManagedTransaction(t *testing.T) {
	body := "BEGIN;\n\nSET LOCAL lock_timeout = '5s';\n\nCREATE TYPE s AS ENUM ('a');\n\nCOMMIT;\n"
	p := mustPlan(t, "043_workspace_status_enum.up.sql", body)
	if !p.Wrappable {
		t.Fatalf("a leading BEGIN / trailing COMMIT must be normalised, not refused: %s", p.NotWrappableBecause)
	}
	masked := maskNonCode(p.Body)
	for _, s := range statementHeads(masked) {
		if isTransactionControl(s.Word) {
			t.Fatalf("transaction control %q survived normalisation; body:\n%s", s.Word, p.Body)
		}
	}
	if !strings.Contains(p.Body, "SET LOCAL lock_timeout") || !strings.Contains(p.Body, "CREATE TYPE s AS ENUM") {
		t.Fatalf("normalisation removed more than the BEGIN/COMMIT:\n%s", p.Body)
	}
	if len(p.Body) != len(body) {
		t.Fatalf("normalisation changed the length (%d != %d), so error line numbers no longer match the file",
			len(p.Body), len(body))
	}
}

// TestPlanMigration_RefusesTransactionControlItCannotNormalise — fail closed.
func TestPlanMigration_RefusesTransactionControlItCannotNormalise(t *testing.T) {
	cases := map[string]string{
		"mid-file commit": "BEGIN;\nCREATE TABLE a (i INT);\nCOMMIT;\nBEGIN;\nCREATE TABLE b (i INT);\nCOMMIT;\n",
		"savepoint":       "BEGIN;\nSAVEPOINT s1;\nCREATE TABLE a (i INT);\nCOMMIT;\n",
		"dangling begin":  "BEGIN;\nCREATE TABLE a (i INT);\n",
		"commit not last": "BEGIN;\nCREATE TABLE a (i INT);\nCOMMIT;\nCREATE TABLE b (i INT);\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := planMigration("x.up.sql", body); err == nil {
				t.Fatalf("planMigration accepted transaction control it cannot make atomic (%s). "+
					"Falling back to the old two-transaction shape for an unfamiliar migration is "+
					"exactly how the guarantee is lost silently", name)
			}
		})
	}
}

// ---- the gate on the REAL migration set -------------------------------------

func forwardMigrationFiles(t *testing.T) []string {
	t.Helper()
	all, err := filepath.Glob(filepath.Join(forwardMigrationsDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	out := make([]string, 0, len(all))
	for _, f := range all {
		if strings.HasSuffix(filepath.Base(f), ".down.sql") {
			continue
		}
		out = append(out, f)
	}
	if len(out) < 100 {
		t.Fatalf("only %d forward migrations found under %s — the gate would be vacuous",
			len(out), forwardMigrationsDir)
	}
	return out
}

// TestEveryForwardMigrationIsPlannable — no file in the tree may be one the
// runner refuses. A refusal at boot is a tenant that cannot start.
func TestEveryForwardMigrationIsPlannable(t *testing.T) {
	wrappable, exempt := 0, 0
	for _, f := range forwardMigrationFiles(t) {
		content, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		p, err := planMigration(filepath.Base(f), string(content))
		if err != nil {
			t.Fatalf("%s cannot be planned and would fail the tenant's boot: %v", filepath.Base(f), err)
		}
		if p.Wrappable {
			wrappable++
		} else {
			exempt++
			t.Logf("NOT atomic: %s — %s", filepath.Base(f), p.NotWrappableBecause)
		}
	}
	t.Logf("%d migrations commit atomically with their ledger row, %d are exempt", wrappable, exempt)
	if wrappable == 0 {
		t.Fatalf("VACUOUS: not one migration was classified wrappable, so the atomicity fix covers nothing")
	}
}

// idempotentStarts are the statement forms that survive being re-run. A
// non-wrappable migration's ONLY resumability is its idempotence, so this is the
// property that replaces the transaction for it.
var idempotentRe = regexp.MustCompile(`(?i)\b(IF NOT EXISTS|IF EXISTS|OR REPLACE|ON CONFLICT)\b`)

// TestNonWrappableMigrationsAreIdempotent — the gate that makes the exemption
// safe. A migration that cannot be atomic with its ledger row MUST be re-runnable,
// or an interrupted boot strands the tenant exactly as 20260624000000_push_tokens
// did.
func TestNonWrappableMigrationsAreIdempotent(t *testing.T) {
	checked := 0
	for _, f := range forwardMigrationFiles(t) {
		content, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		p, err := planMigration(filepath.Base(f), string(content))
		if err != nil || p.Wrappable {
			continue
		}
		checked++
		for _, s := range statementHeads(maskNonCode(string(content))) {
			stmt := string(content)[s.Start:s.End]
			if !idempotentRe.MatchString(maskNonCode(stmt)) {
				t.Errorf("%s runs OUTSIDE a transaction (%s) but statement %d is not idempotent:\n  %s\n"+
					"An interrupted boot re-runs it and the tenant crash-loops. Either make it "+
					"idempotent or make it wrappable",
					filepath.Base(f), p.NotWrappableBecause, s.Index, strings.TrimSpace(stmt))
			}
		}
	}
	t.Logf("checked %d non-wrappable migration(s)", checked)
}

// TestNonWrappableIdempotenceGate_NegativeControl proves the gate above can
// fail. It runs the identical check over a synthetic migration that is
// non-wrappable AND non-idempotent — the shape that must never merge.
func TestNonWrappableIdempotenceGate_NegativeControl(t *testing.T) {
	body := "CREATE TABLE push_tokens (id INT);\nCREATE INDEX CONCURRENTLY idx ON push_tokens (id);\n"
	p, err := planMigration("bad.up.sql", body)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if p.Wrappable {
		t.Fatalf("NEGATIVE CONTROL DEAD: the synthetic file was classified wrappable")
	}
	offenders := 0
	for _, s := range statementHeads(maskNonCode(body)) {
		if !idempotentRe.MatchString(maskNonCode(body[s.Start:s.End])) {
			offenders++
		}
	}
	if offenders == 0 {
		t.Fatalf("NEGATIVE CONTROL DEAD: a bare CREATE TABLE beside a CONCURRENTLY index was scored " +
			"idempotent, so TestNonWrappableMigrationsAreIdempotent cannot fail")
	}
	t.Logf("negative control alive: %d non-idempotent statement(s) detected", offenders)
}
