//go:build integration
// +build integration

// platform_agent_ensure_integration_test.go — REAL Postgres regression gate
// for the EnsurePlatformAgent platform-root lookup.
//
// Run with:
//
//	docker run --rm -d --name pg-integration \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	for f in workspace-server/migrations/*.up.sql; do psql ... < "$f"; done
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_PlatformRootLookup -v
//
// CI: piggybacks on the handlers-postgres-integration workflow (path filter
// includes workspace-server/internal/handlers/**).
//
// Why this is NOT a sqlmock test
// ------------------------------
// The lookup shipped 100% broken in 8cd393187 with GREEN unit tests: the
// query wrapped the workspace_status ENUM column in COALESCE(status, ''),
// which Postgres rejects at PARSE time —
//
//	pq: invalid input value for enum workspace_status: ""
//
// — even when zero rows match. sqlmock regex-matches the SQL text and never
// plans it, so this bug class is structurally invisible to unit tests. This
// test executes the EXACT production query text (platformRootLookupQuery,
// shared const — not a copy) against a real Postgres, plus the old broken
// shape to prove the enum coercion is real on this schema.

package handlers

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func integrationDB_EnsureLookup(t *testing.T) *sql.DB {
	t.Helper()
	url := requireIntegrationDBURL(t)
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// TestIntegration_PlatformRootLookupEnumSafe proves four properties of the
// production platform-root lookup against a real Postgres:
//
//  1. the OLD COALESCE(status, ”) shape fails at parse time on this schema
//     (the 8cd393187 regression — this sub-check is what would have caught it);
//  2. the production query with ZERO matching rows scans as sql.ErrNoRows,
//     not a pq parse error;
//  3. a platform root with a NULL status scans as "" (the NULL-safety the
//     COALESCE exists for);
//  4. a real enum value round-trips through the ::text cast.
func TestIntegration_PlatformRootLookupEnumSafe(t *testing.T) {
	conn := integrationDB_EnsureLookup(t)
	ctx := context.Background()

	// 1. The old broken shape errors at parse time — regardless of data, so
	// it is safe to run outside the transaction. If this ever STOPS failing
	// the schema changed (status is no longer the enum) and the production
	// COALESCE cast can be revisited.
	oldShape := `SELECT id, COALESCE(status, '') FROM workspaces WHERE kind = 'platform' AND parent_id IS NULL LIMIT 1`
	var id, status string
	err := conn.QueryRowContext(ctx, oldShape).Scan(&id, &status)
	if err == nil || !strings.Contains(err.Error(), "invalid input value for enum workspace_status") {
		t.Fatalf("old COALESCE(status, '') shape: want enum-coercion parse error, got %v", err)
	}

	// Remaining checks mutate platform-root state, and the partial unique
	// index uniq_workspaces_one_platform_root forbids a second platform row —
	// so run them inside a transaction against a cleared slate and roll back,
	// leaving the shared integration DB untouched.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE kind = 'platform'`); err != nil {
		t.Fatalf("clear platform rows: %v", err)
	}

	// 2. Zero matching rows => ErrNoRows, NOT a pq error. (The regression
	// errored here even with an empty table.)
	err = tx.QueryRowContext(ctx, platformRootLookupQuery).Scan(&id, &status)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("zero-row lookup: want sql.ErrNoRows, got %v", err)
	}

	// 3. NULL status scans as "".
	rootID := uuid.New().String()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status, kind, parent_id)
		VALUES ($1, $2, 0, 'claude-code', NULL, 'platform', NULL)
	`, rootID, "itest-ensure-lookup-root"); err != nil {
		t.Fatalf("seed NULL-status platform root: %v", err)
	}
	if err := tx.QueryRowContext(ctx, platformRootLookupQuery).Scan(&id, &status); err != nil {
		t.Fatalf("NULL-status lookup: %v", err)
	}
	if id != rootID || status != "" {
		t.Fatalf("NULL-status lookup: got (id=%q, status=%q), want (%q, \"\")", id, status, rootID)
	}

	// 4. A real enum value round-trips through the ::text cast.
	if _, err := tx.ExecContext(ctx,
		`UPDATE workspaces SET status = 'online' WHERE id = $1`, rootID); err != nil {
		t.Fatalf("set status online: %v", err)
	}
	if err := tx.QueryRowContext(ctx, platformRootLookupQuery).Scan(&id, &status); err != nil {
		t.Fatalf("online lookup: %v", err)
	}
	if id != rootID || status != "online" {
		t.Fatalf("online lookup: got (id=%q, status=%q), want (%q, \"online\")", id, status, rootID)
	}
}
