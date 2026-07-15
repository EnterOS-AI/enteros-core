//go:build integration
// +build integration

// phantom_busy_sweeper_integration_test.go — real-Postgres coverage of the
// phantom-busy drift predicate (finding #5). The unit test is sqlmock-only and
// by design cannot evaluate the WHERE clause; this test proves the actual SQL:
//
//	active_tasks > 0
//	AND status != 'removed'
//	AND id NOT IN (SELECT workspace_id FROM activity_logs
//	               WHERE created_at > now() - <stale>)
//
// resets exactly the stranded rows and leaves everything else untouched. It
// replaces TestIntegration_SweepPhantomBusy (#2149), which was deleted with the
// internal/scheduler package when the phantom-busy sweep moved here (P4).
//
// Run:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration -run TestIntegration_PhantomBusy ./internal/handlers/
//
// NOT SAFE FOR t.Parallel() — each test gets the tables to itself.

package handlers

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	_ "github.com/lib/pq"
)

func integrationDB_PhantomBusy(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Fatal("INTEGRATION_DB_URL not set; failing (local devs: see file header)")
	}
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := conn.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), `
		DELETE FROM activity_logs WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'test-phantom-%');
		DELETE FROM workspaces WHERE name LIKE 'test-phantom-%';
	`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev; conn.Close() })
	return conn
}

// seedPhantomWorkspace inserts a workspace with an explicit active_tasks/status
// and returns its id.
func seedPhantomWorkspace(t *testing.T, conn *sql.DB, name, status string, activeTasks int) string {
	t.Helper()
	var id string
	if err := conn.QueryRowContext(context.Background(), `
		INSERT INTO workspaces (id, name, status, active_tasks, current_task)
		VALUES (gen_random_uuid(), $1, $2, $3, 'doing-something')
		RETURNING id
	`, name, status, activeTasks).Scan(&id); err != nil {
		t.Fatalf("seedPhantomWorkspace %q: %v", name, err)
	}
	return id
}

// seedActivityAt inserts an activity_logs row with an explicit created_at.
func seedActivityAt(t *testing.T, conn *sql.DB, workspaceID string, ageMinutes int) {
	t.Helper()
	if _, err := conn.ExecContext(context.Background(), `
		INSERT INTO activity_logs (workspace_id, activity_type, method, status, created_at)
		VALUES ($1, 'a2a', 'message/send', 'ok', now() - ($2 * interval '1 minute'))
	`, workspaceID, ageMinutes); err != nil {
		t.Fatalf("seedActivityAt: %v", err)
	}
}

func activeTasksOf(t *testing.T, conn *sql.DB, id string) int {
	t.Helper()
	var n int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT active_tasks FROM workspaces WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("read active_tasks: %v", err)
	}
	return n
}

func TestIntegration_PhantomBusy_ResetsOnlyStrandedRows(t *testing.T) {
	conn := integrationDB_PhantomBusy(t)

	// stranded: busy, no recent activity → MUST reset to 0.
	stranded := seedPhantomWorkspace(t, conn, "test-phantom-stranded", "online", 2)
	seedActivityAt(t, conn, stranded, 30) // last activity 30 min ago (stale)

	// active: busy, recent activity → MUST be preserved (genuinely working).
	active := seedPhantomWorkspace(t, conn, "test-phantom-active", "online", 1)
	seedActivityAt(t, conn, active, 1) // last activity 1 min ago (fresh)

	// removed: busy but status='removed' → predicate excludes it, preserved.
	removed := seedPhantomWorkspace(t, conn, "test-phantom-removed", "removed", 1)

	// idle: active_tasks=0 → not matched, stays 0.
	idle := seedPhantomWorkspace(t, conn, "test-phantom-idle", "online", 0)

	n := NewPhantomBusySweeper(conn).Sweep(context.Background())
	if n != 1 {
		t.Fatalf("expected exactly 1 workspace reset (the stranded one); got %d", n)
	}

	if got := activeTasksOf(t, conn, stranded); got != 0 {
		t.Errorf("stranded workspace should be reset to 0; got %d", got)
	}
	if got := activeTasksOf(t, conn, active); got != 1 {
		t.Errorf("actively-working workspace must NOT be reset; got %d, want 1", got)
	}
	if got := activeTasksOf(t, conn, removed); got != 1 {
		t.Errorf("removed workspace must be excluded by the predicate; got %d, want 1", got)
	}
	if got := activeTasksOf(t, conn, idle); got != 0 {
		t.Errorf("idle workspace stays 0; got %d", got)
	}

	// current_task is also cleared on the row that was reset.
	var curr string
	if err := conn.QueryRowContext(context.Background(),
		`SELECT current_task FROM workspaces WHERE id = $1`, stranded).Scan(&curr); err != nil {
		t.Fatalf("read current_task: %v", err)
	}
	if curr != "" {
		t.Errorf("stranded workspace current_task should be cleared; got %q", curr)
	}
}
