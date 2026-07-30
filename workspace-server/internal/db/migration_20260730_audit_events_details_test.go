package db

// Shape test for 20260730120000_audit_events_details. The apply MECHANISM is
// already proven by postgres_schema_migrations_test.go, so this pins the
// properties that make the migration safe to run against live tenant DBs whose
// audit_events table is in production use:
//
//   - additive and idempotent (ADD COLUMN IF NOT EXISTS) — re-running or
//     landing on a partially-migrated tenant must not error;
//   - TEXT, never JSONB — the column is covered by the HMAC canonical form and
//     JSONB re-normalises stored bytes, which would make every signed row
//     verify as tampered;
//   - a matching down migration exists.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const auditDetailsMigrationBase = "20260730120000_audit_events_details"

func readAuditDetailsMigration(t *testing.T, suffix string) string {
	t.Helper()
	const migDir = "../../migrations"
	path := filepath.Join(migDir, auditDetailsMigrationBase+suffix)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestAuditEventsDetailsMigration_IsAdditiveAndIdempotent(t *testing.T) {
	up := readAuditDetailsMigration(t, ".up.sql")

	if !strings.Contains(up, "ADD COLUMN IF NOT EXISTS details") {
		t.Error("migration must add the details column idempotently (ADD COLUMN IF NOT EXISTS)")
	}
	for _, forbidden := range []string{"DROP TABLE", "DROP COLUMN", "DELETE FROM", "TRUNCATE"} {
		if strings.Contains(strings.ToUpper(up), forbidden) {
			t.Errorf("up migration must be additive; found %q — audit_events is an append-only ledger", forbidden)
		}
	}
}

// TestAuditEventsDetailsMigration_UsesTextNotJSONB is the load-bearing one.
// details participates in the HMAC canonical form; JSONB reorders keys and
// strips whitespace on store, so the bytes read back would not be the bytes
// signed and every row would verify as tampered.
func TestAuditEventsDetailsMigration_UsesTextNotJSONB(t *testing.T) {
	up := readAuditDetailsMigration(t, ".up.sql")

	idx := strings.Index(up, "ADD COLUMN IF NOT EXISTS details")
	if idx < 0 {
		t.Fatal("details column not added")
	}
	// Look at the column definition only, not the explanatory comment above it.
	decl := up[idx:]
	if nl := strings.IndexByte(decl, '\n'); nl >= 0 {
		decl = decl[:nl]
	}
	if !strings.Contains(strings.ToUpper(decl), "TEXT") {
		t.Errorf("details must be TEXT, got %q", decl)
	}
	if strings.Contains(strings.ToUpper(decl), "JSONB") {
		t.Error("details must NOT be JSONB — normalisation would invalidate every HMAC signature")
	}
}

func TestAuditEventsDetailsMigration_HasDownMigration(t *testing.T) {
	down := readAuditDetailsMigration(t, ".down.sql")
	if !strings.Contains(down, "DROP COLUMN IF EXISTS details") {
		t.Error("down migration must drop the details column idempotently")
	}
}
