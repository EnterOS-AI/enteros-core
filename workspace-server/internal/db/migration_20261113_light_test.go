package db

// Shape test for 20261113120000_light (Enter OS Light). The apply mechanism is
// proven by postgres_schema_migrations_test.go and the replay-from-scratch
// integration test; this pins the properties that make the file safe on live
// tenant databases:
//
//   - additive and idempotent: every object is IF NOT EXISTS, nothing is
//     dropped, deleted or truncated, and the file runs inside the runner's
//     transaction (no CONCURRENTLY, no transaction control of its own);
//   - the down file removes every object the up file adds.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const lightMigrationBase = "20261113120000_light"

func readLightMigration(t *testing.T, suffix string) string {
	t.Helper()
	path := filepath.Join("../../migrations", lightMigrationBase+suffix)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestLightMigration_IsAdditiveAndIdempotent(t *testing.T) {
	up := readLightMigration(t, ".up.sql")
	code := strings.ToUpper(maskNonCode(up))

	for _, want := range []string{
		"ADD COLUMN IF NOT EXISTS LIGHT_STATE ",
		"ADD COLUMN IF NOT EXISTS LIGHT_STATE_AT ",
		"ADD COLUMN IF NOT EXISTS LIGHT_OPENED_BY ",
		"ADD COLUMN IF NOT EXISTS LIGHT_RUN_TOKEN_ID ",
		"ADD COLUMN IF NOT EXISTS LIGHT_RETIRED_TOKEN_ID ",
		"ADD COLUMN IF NOT EXISTS LIGHT_LAST_ERROR ",
		"ADD COLUMN IF NOT EXISTS LIGHT_LAST_ERROR_AT ",
		"CREATE TABLE IF NOT EXISTS LIGHT_RUNS ",
		"CREATE TABLE IF NOT EXISTS LIGHT_SCHEDULES ",
		"CREATE TABLE IF NOT EXISTS LIGHT_PENDING_ACTIONS ",
		"ALTER TABLE ACTIVITY_LOGS ADD COLUMN IF NOT EXISTS RUN_ID ",
	} {
		if !strings.Contains(code, want) {
			t.Errorf("up migration lacks %q", want)
		}
	}
	// Every CREATE/ADD is guarded.
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`CREATE (UNIQUE )?INDEX (\w+)`),
		regexp.MustCompile(`CREATE TABLE (\w+)`),
		regexp.MustCompile(`ADD COLUMN (\w+)`),
	} {
		for _, m := range re.FindAllStringSubmatch(code, -1) {
			if !strings.Contains(m[0], "IF") {
				t.Errorf("unguarded statement %q: every object must be IF NOT EXISTS", m[0])
			}
		}
	}
	for _, forbidden := range []string{"DROP ", "DELETE FROM", "TRUNCATE", "CONCURRENTLY", "BEGIN", "COMMIT", "UPDATE "} {
		if strings.Contains(code, forbidden) {
			t.Errorf("up migration must be additive and runner-wrapped; found %q", forbidden)
		}
	}

	p, err := planMigration(lightMigrationBase+".up.sql", up)
	if err != nil {
		t.Fatalf("planMigration: %v", err)
	}
	if !p.Wrappable {
		t.Fatalf("the migration must commit together with its ledger row; not wrappable because %s", p.NotWrappableBecause)
	}
}

func TestLightMigration_DownRemovesEverythingUpAdds(t *testing.T) {
	up := strings.ToUpper(maskNonCode(readLightMigration(t, ".up.sql")))
	down := strings.ToUpper(maskNonCode(readLightMigration(t, ".down.sql")))

	for _, m := range regexp.MustCompile(`ADD COLUMN IF NOT EXISTS (\w+)`).FindAllStringSubmatch(up, -1) {
		if !strings.Contains(down, "DROP COLUMN IF EXISTS "+m[1]+";") {
			t.Errorf("down migration does not drop column %s", m[1])
		}
	}
	for _, m := range regexp.MustCompile(`CREATE TABLE IF NOT EXISTS (\w+)`).FindAllStringSubmatch(up, -1) {
		if !strings.Contains(down, "DROP TABLE IF EXISTS "+m[1]+";") {
			t.Errorf("down migration does not drop table %s", m[1])
		}
	}
	for _, m := range regexp.MustCompile(`CREATE INDEX IF NOT EXISTS (\w+)`).FindAllStringSubmatch(up, -1) {
		// An index on a dropped table goes with it; an index on a kept table
		// (workspaces, activity_logs) must be dropped by name.
		if strings.HasPrefix(m[1], "IDX_LIGHT_") {
			continue
		}
		if !strings.Contains(down, "DROP INDEX IF EXISTS "+m[1]+";") {
			t.Errorf("down migration does not drop index %s", m[1])
		}
	}
}
