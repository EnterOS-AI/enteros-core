//go:build integration
// +build integration

// migration_atomicity_test.go — REAL Postgres gate on the property the boot
// migration runner did not have: a migration's DDL and its ledger row commit
// TOGETHER, so a boot killed mid-migration is always resumable.
//
// # THE MEASURED DEFECT (rehearsal migration mig-rehearsal-a41c7, 2026-08-07)
//
// The tenant was killed by the kubelet's liveness probe partway through its
// first boot on longhorn. The kill landed BETWEEN
//
//	DB.Exec(<20260624000000_push_tokens.up.sql>)   -- CREATE TABLE push_tokens
//	DB.Exec("INSERT INTO schema_migrations ...")   -- the ledger row
//
// two separate transactions. The table existed; the ledger did not record it.
// Every subsequent boot therefore re-ran the file and died on
//
//	Migrations failed: exec 20260624000000_push_tokens.up.sql:
//	  pq: relation "push_tokens" already exists
//
// — a permanent CrashLoopBackOff no restart can clear. The last successfully
// ledgered migration was 20260623090000_workspaces_can_delegate.
//
// And it DEADLOCKS the migration that produced it: the control plane waits for
// tenant readiness before the Transfer step, Transfer (`pg_restore --clean
// --if-exists`) is what would overwrite the half-applied schema, and the tenant
// cannot become ready until that has happened.
//
// # RUN
//
//	docker run --rm -d --name pg-atomic \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55433:5432 pgvector/pgvector:pg16
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@127.0.0.1:55433/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/db/ -run TestIntegration_MigrationAtomicity
//
// 127.0.0.1, not localhost: `localhost` resolves to ::1 first on this platform
// and the container publishes v4 only.

package db

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errKilledMidMigration is what the crash seam returns. It lives HERE and not
// beside the seam: production never produces it, and a sentinel declared in
// production code that only an integration-tagged test can reach reads as dead
// code to every linter run without that tag (golangci-lint caught exactly
// that).
var errKilledMidMigration = errors.New("migration boot killed after DDL, before the ledger row (test seam)")

// crashAfterDDL wires the production crash seam so the run fails at exactly the
// point the kubelet killed the rehearsal tenant: after the migration's own SQL
// has been executed, before the ledger row is written.
func crashAfterDDL(t *testing.T, forFile string) {
	t.Helper()
	prev := migrationCrashAfterDDL
	migrationCrashAfterDDL = func(filename string) error {
		if filename == forFile {
			return errKilledMidMigration
		}
		return nil
	}
	t.Cleanup(func() { migrationCrashAfterDDL = prev })
}

// writeOneShotMigration lays down a single NON-IDEMPOTENT migration — a bare
// CREATE TABLE, exactly like 20260624000000_push_tokens.up.sql. Non-idempotent
// is the point: an idempotent file survives a lost ledger row by accident, and
// a gate built on one would pass over the defect.
func writeOneShotMigration(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	body := "CREATE TABLE push_tokens_probe (\n" +
		"  id   BIGSERIAL PRIMARY KEY,\n" +
		"  tok  TEXT NOT NULL\n" +
		");\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write migration: %v", err)
	}
	return dir
}

func tableExists(t *testing.T, name string) bool {
	t.Helper()
	var ok bool
	if err := DB.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, "public."+name).Scan(&ok); err != nil {
		t.Fatalf("to_regclass(%s): %v", name, err)
	}
	return ok
}

func ledgerHas(t *testing.T, filename string) bool {
	t.Helper()
	var ok bool
	if err := DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)`, filename).Scan(&ok); err != nil {
		t.Fatalf("ledger read: %v", err)
	}
	return ok
}

// TestIntegration_MigrationAtomicity_InterruptedBootIsResumable is the GUARD.
//
// It asserts the recovery property in the only terms that matter to a tenant:
// after a boot dies partway through a migration, the NEXT boot completes.
func TestIntegration_MigrationAtomicity_InterruptedBootIsResumable(t *testing.T) {
	DB = freshIntegrationDB(t) // RunMigrations operates on the package-global DB.
	const name = "20260624000000_push_tokens_probe.up.sql"
	dir := writeOneShotMigration(t, name)

	// ---- boot 1: killed between the DDL and the ledger row -----------------
	crashAfterDDL(t, name)
	if err := RunMigrations(dir); err == nil {
		t.Fatalf("boot 1 was supposed to be killed mid-migration but RunMigrations returned nil")
	}

	// The observable consequence of atomicity: the two facts AGREE. Either both
	// landed or neither did — never the table without its ledger row, which is
	// the state that cannot be booted out of.
	tbl, led := tableExists(t, "push_tokens_probe"), ledgerHas(t, name)
	if tbl != led {
		t.Fatalf("SPLIT STATE after an interrupted boot: table exists=%v, ledger row exists=%v. "+
			"That disagreement IS the crash-loop: the next boot re-runs the file and dies on "+
			"\"relation already exists\", and no restart can clear it", tbl, led)
	}

	// ---- boot 2: no crash. It must succeed, whichever way boot 1 resolved ---
	migrationCrashAfterDDL = nil
	if err := RunMigrations(dir); err != nil {
		t.Fatalf("boot 2 FAILED after an interrupted boot 1 — the tenant is in the "+
			"unrecoverable CrashLoopBackOff this gate exists to prevent: %v", err)
	}
	if !tableExists(t, "push_tokens_probe") {
		t.Fatalf("boot 2 reported success but the table does not exist")
	}
	if !ledgerHas(t, name) {
		t.Fatalf("boot 2 reported success but did not record the migration — the NEXT boot would re-run it")
	}

	// ---- boot 3: the ledger must actually hold. Re-running a recorded,
	//      non-idempotent migration is the same crash by another route.
	if err := RunMigrations(dir); err != nil {
		t.Fatalf("boot 3 FAILED: the ledger row from boot 2 did not suppress the re-run: %v", err)
	}
}

// TestIntegration_MigrationAtomicity_LegacyShapeIsTheMeasuredFailure is the
// NEGATIVE CONTROL.
//
// It runs the SAME scenario through the exact two-Exec shape that shipped, and
// requires it to produce the crash-loop. If this ever passes, the guard above
// is no longer measuring anything — either the scenario stopped reproducing the
// kill or the harness stopped being able to observe it.
func TestIntegration_MigrationAtomicity_LegacyShapeIsTheMeasuredFailure(t *testing.T) {
	DB = freshIntegrationDB(t) // RunMigrations operates on the package-global DB.
	const name = "20260624000000_push_tokens_probe.up.sql"
	dir := writeOneShotMigration(t, name)

	if err := legacyRunMigrationsForNegativeControl(dir, name); err == nil {
		t.Fatalf("legacy boot 1 was supposed to be killed mid-migration but returned nil")
	}
	// The split state the fix eliminates.
	if !tableExists(t, "push_tokens_probe") {
		t.Fatalf("negative control DEAD: the legacy shape did not leave the table behind, "+
			"so it never reproduced the split state (%s)", name)
	}
	if ledgerHas(t, name) {
		t.Fatalf("negative control DEAD: the legacy shape recorded the ledger row despite the kill")
	}

	// And the next boot is unrecoverable.
	err := legacyRunMigrationsForNegativeControl(dir, "")
	if err == nil {
		t.Fatalf("negative control DEAD: the legacy shape RECOVERED on the second boot; " +
			"the guard above would then pass for a runner that had not been fixed")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("negative control produced the wrong failure: %v (want \"already exists\")", err)
	}
	t.Logf("negative control alive: legacy shape crash-loops with %v", err)
}

// legacyRunMigrationsForNegativeControl is the runner EXACTLY as it was before
// this change — DDL and ledger row in two separate transactions — reproduced
// here and nowhere else. It exists so the negative control tests the real
// historical shape rather than a caricature of it.
//
// crashAfter names the file to die on after its DDL; "" never dies.
func legacyRunMigrationsForNegativeControl(migrationsDir, crashAfter string) error {
	if _, err := DB.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		filename TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
	)`); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil {
		return err
	}
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasSuffix(base, ".down.sql") {
			continue
		}
		var exists bool
		if err := DB.QueryRow("SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE filename = $1)", base).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		content, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := DB.Exec(string(content)); err != nil {
			return err
		}
		if base == crashAfter {
			return errKilledMidMigration
		}
		if _, err := DB.Exec("INSERT INTO schema_migrations (filename) VALUES ($1)", base); err != nil {
			return err
		}
	}
	return nil
}

// TestIntegration_MigrationAtomicity_ConcurrentIndexStillApplies proves the
// EXCEPTION is real rather than decorative.
//
// 20260506000000_workspaces_unique_parent_name.up.sql builds a UNIQUE INDEX
// CONCURRENTLY, and Postgres refuses that inside ANY transaction block. A
// blanket transaction wrap would therefore have broken every fresh tenant with
// `CREATE INDEX CONCURRENTLY cannot run inside a transaction block` — which is
// why the runner classifies rather than wraps everything.
func TestIntegration_MigrationAtomicity_ConcurrentIndexStillApplies(t *testing.T) {
	DB = freshIntegrationDB(t) // RunMigrations operates on the package-global DB.
	dir := t.TempDir()
	const name = "20260506000000_concurrent_probe.up.sql"
	body := "CREATE TABLE IF NOT EXISTS conc_probe (id BIGSERIAL PRIMARY KEY, name TEXT);\n"
	if err := os.WriteFile(filepath.Join(dir, "001_base.up.sql"), []byte(body), 0o644); err != nil {
		t.Fatalf("write base: %v", err)
	}
	idx := "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS conc_probe_name_uniq ON conc_probe (name);\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(idx), 0o644); err != nil {
		t.Fatalf("write concurrent migration: %v", err)
	}
	if err := RunMigrations(dir); err != nil {
		t.Fatalf("CREATE INDEX CONCURRENTLY migration failed — the transaction exception is not "+
			"being honoured: %v", err)
	}
	if !ledgerHas(t, name) {
		t.Fatalf("the concurrent migration applied but was not recorded")
	}
}

// TestIntegration_MigrationAtomicity_SelfManagedTransactionStillApplies covers
// the OTHER exception: eleven forward migrations open and commit their own
// transaction (043 sets a LOCAL lock_timeout inside one). Nesting those in an
// outer transaction makes their COMMIT close the OUTER one, after which the
// ledger INSERT is no longer in the transaction it was supposed to be atomic
// with — the exact property being bought would be silently lost.
func TestIntegration_MigrationAtomicity_SelfManagedTransactionStillApplies(t *testing.T) {
	DB = freshIntegrationDB(t) // RunMigrations operates on the package-global DB.
	dir := t.TempDir()
	const name = "20260101000000_selftx_probe.up.sql"
	body := "BEGIN;\nSET LOCAL lock_timeout = '5s';\nCREATE TABLE IF NOT EXISTS selftx_probe (id INT);\nCOMMIT;\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := RunMigrations(dir); err != nil {
		t.Fatalf("self-managed-transaction migration failed: %v", err)
	}
	if !tableExists(t, "selftx_probe") || !ledgerHas(t, name) {
		t.Fatalf("self-managed migration did not land (table=%v ledger=%v)",
			tableExists(t, "selftx_probe"), ledgerHas(t, name))
	}
}
