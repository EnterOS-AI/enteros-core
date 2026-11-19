package handlers

// light_sql_record_test.go — proving that a refused call wrote nothing.
//
// sqlmock answers a statement nobody expected with an error, and the
// best-effort writes on these paths (the A2A ingest row, the register failure
// stamp) swallow it, so mock.ExpectationsWereMet() cannot see an extra write.
// recordSQL records every statement the code under test sends through the
// test's sqlmock, expected or not, and assertNoWrites fails on any write.

import (
	"regexp"
	"strings"
	"sync"
	"testing"
)

// recordSQL starts recording the statements of this test's sqlmock (see
// statementObserver) and returns a reader of what was recorded so far.
func recordSQL(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var got []string
	f := func(q string) {
		mu.Lock()
		got = append(got, q)
		mu.Unlock()
	}
	statementObserver.Store(&f)
	t.Cleanup(func() { statementObserver.Store(nil) })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// sqlWriteRe finds each write in a statement and the table it writes.
var sqlWriteRe = regexp.MustCompile(`(?is)\b(?:INSERT\s+INTO\s+([a-z_]+)|UPDATE\s+([a-z_]+)\s+SET|DELETE\s+FROM\s+([a-z_]+))`)

// sqlWrites keeps the statements that write a table not in allowed.
func sqlWrites(stmts []string, allowed ...string) []string {
	ok := map[string]bool{}
	for _, a := range allowed {
		ok[a] = true
	}
	var out []string
	for _, q := range stmts {
		for _, m := range sqlWriteRe.FindAllStringSubmatch(q, -1) {
			table := strings.ToLower(m[1] + m[2] + m[3])
			if !ok[table] {
				out = append(out, strings.Join(strings.Fields(q), " "))
				break
			}
		}
	}
	return out
}

// assertNoWrites fails for every recorded statement that writes a table other
// than the allowed ones (e.g. workspace_auth_tokens: auth's own last_used_at).
func assertNoWrites(t *testing.T, stmts []string, allowed ...string) {
	t.Helper()
	for _, w := range sqlWrites(stmts, allowed...) {
		t.Errorf("a refused call issued a write: %s", w)
	}
}

// assertNoTransaction fails when the code opened a transaction: sqlmock
// refuses an unexpected BEGIN, so the writes it was opened for never show up
// as statements.
func assertNoTransaction(t *testing.T, stmts []string) {
	t.Helper()
	for _, q := range stmts {
		if q == "BEGIN" {
			t.Errorf("a refused call opened a transaction (statements: %v)", stmts)
			return
		}
	}
}

func TestSQLWritesSeesEveryWriteShape(t *testing.T) {
	stmts := []string{
		`SELECT COALESCE(light_state, '') FROM workspaces WHERE id = $1`,
		`SELECT id FROM workspaces WHERE id = $1 FOR UPDATE`,
		`WITH ins AS (INSERT INTO activity_logs (workspace_id) VALUES ($1)) UPDATE workspaces SET last_activity_at = now() WHERE id = $1`,
		`UPDATE workspaces SET last_register_failure_at = now() WHERE id = $1`,
		`DELETE FROM light_schedules WHERE workspace_id = $1`,
		`UPDATE workspace_auth_tokens SET last_used_at = now() WHERE id = $1`,
	}
	if got := sqlWrites(stmts); len(got) != 4 {
		t.Fatalf("writes = %v, want the four write statements", got)
	}
	if got := sqlWrites(stmts, "workspace_auth_tokens"); len(got) != 3 {
		t.Fatalf("writes = %v, want three once auth's token bump is allowed", got)
	}
}
