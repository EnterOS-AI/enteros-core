//go:build integration
// +build integration

// light_schema_integration_test.go — real-Postgres coverage of the Enter OS
// Light schema (migration 20261113120000_light): the CHECK constraints that
// keep light_state / run status / source to the values core writes, and the
// cascades that take a workspace's runs, rendered schedules and queued actions
// with it.
//
// Run:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration -run TestIntegration_Light ./internal/handlers/
//
// NOT SAFE FOR t.Parallel() — the helper deletes every 'test-light-%' row.

package handlers

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/lib/pq"
)

// integrationDB_Light connects to INTEGRATION_DB_URL, removes every row a
// previous Light integration test left, and points db.DB at the connection for
// the test's lifetime.
func integrationDB_Light(t *testing.T) *sql.DB {
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
		DELETE FROM activity_logs WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'test-light-%');
		DELETE FROM workspace_auth_tokens WHERE workspace_id IN (SELECT id FROM workspaces WHERE name LIKE 'test-light-%');
		DELETE FROM workspaces WHERE name LIKE 'test-light-%';
	`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev; conn.Close() })
	return conn
}

// seedLightWorkspace inserts a hermes workspace with the given status and Light
// state ("" = NULL) and returns its id.
func seedLightWorkspace(t *testing.T, conn *sql.DB, name, status, lightState string) string {
	t.Helper()
	var id string
	var ls interface{}
	if lightState != "" {
		ls = lightState
	}
	if err := conn.QueryRowContext(context.Background(), `
		INSERT INTO workspaces (id, name, status, runtime, light_state, light_state_at)
		VALUES (gen_random_uuid(), $1, $2, 'hermes', $3, now())
		RETURNING id
	`, name, status, ls).Scan(&id); err != nil {
		t.Fatalf("seedLightWorkspace %q: %v", name, err)
	}
	return id
}

func isCheckViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23514"
}

func TestIntegration_LightSchema_RejectsUnknownStateStatusAndSource(t *testing.T) {
	conn := integrationDB_Light(t)
	ctx := context.Background()
	id := seedLightWorkspace(t, conn, "test-light-schema", "online", "open")

	if _, err := conn.ExecContext(ctx, `UPDATE workspaces SET light_state = 'bogus' WHERE id = $1`, id); !isCheckViolation(err) {
		t.Errorf("light_state 'bogus': want a check violation, got %v", err)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE workspaces SET light_opened_by = 'cron' WHERE id = $1`, id); !isCheckViolation(err) {
		t.Errorf("light_opened_by 'cron': want a check violation, got %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO light_runs (workspace_id, run_id, schedule, status, source) VALUES ($1, 'r1', 's', 'done', 'report')`, id); !isCheckViolation(err) {
		t.Errorf("status 'done': want a check violation, got %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO light_runs (workspace_id, run_id, schedule, status, source) VALUES ($1, 'r1', 's', 'running', 'cron')`, id); !isCheckViolation(err) {
		t.Errorf("source 'cron': want a check violation, got %v", err)
	}
	big := make([]byte, 65537)
	for i := range big {
		big[i] = 'x'
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO light_runs (workspace_id, run_id, schedule, status, source, output) VALUES ($1, 'r1', 's', 'succeeded', 'report', $2)`, id, string(big)); !isCheckViolation(err) {
		t.Errorf("output of 65537 bytes: want a check violation, got %v", err)
	}
	for _, st := range []string{"open", "parking", "parked", "opening"} {
		if _, err := conn.ExecContext(ctx, `UPDATE workspaces SET light_state = $2 WHERE id = $1`, id, st); err != nil {
			t.Errorf("light_state %q refused: %v", st, err)
		}
	}
}

func TestIntegration_LightSchema_RunsSchedulesAndActionsGoWithTheWorkspace(t *testing.T) {
	conn := integrationDB_Light(t)
	ctx := context.Background()
	id := seedLightWorkspace(t, conn, "test-light-cascade", "hibernated", "parked")

	for _, stmt := range []string{
		`INSERT INTO light_runs (workspace_id, run_id, schedule, status, source) VALUES ($1, 'r1', 'daily', 'succeeded', 'report')`,
		`INSERT INTO light_schedules (workspace_id, name, cron, prompt) VALUES ($1, 'daily', '0 9 * * *', 'p')`,
		`INSERT INTO light_pending_actions (workspace_id, kind) VALUES ($1, 'refresh')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt, id); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, id); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}
	for _, table := range []string{"light_runs", "light_schedules", "light_pending_actions"} {
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE workspace_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s kept %d row(s) of a deleted workspace", table, n)
		}
	}
}
