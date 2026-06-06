//go:build integration
// +build integration

// kind_platform_root_integration_test.go — REAL Postgres gate for the
// platform-agent participant kind (RFC docs/design/rfc-platform-agent.md).
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_PlatformKind -v
//
// CI: piggybacks on the handlers-postgres-integration workflow (path filter
// includes workspace-server/internal/handlers/** and migrations/**).
//
// Why this is NOT a sqlmock test
// ------------------------------
// The invariant "a platform agent must be the org root (parent_id IS NULL),
// which structurally also means at most one platform agent per org" is enforced
// by the workspaces_platform_root_check CHECK constraint in migration
// 20260606000000_workspaces_kind. sqlmock cannot execute DDL or evaluate a CHECK
// constraint, so only a real Postgres can prove the constraint actually rejects
// a non-root platform agent and accepts a root one. The Register handler's
// isPlatformRootViolation()/409 path depends on this constraint firing.

package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func integrationDB_PlatformKind(t *testing.T) *sql.DB {
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

// TestIntegration_PlatformKind_RootAllowed_NonRootRejected proves the three
// guarantees of the kind column against a real Postgres:
//
//  1. a fresh workspace defaults to kind='workspace';
//  2. a root row (parent_id IS NULL) may be kind='platform';
//  3. a non-root row (parent_id set) may NOT be kind='platform' — the
//     workspaces_platform_root_check constraint rejects it (23514).
func TestIntegration_PlatformKind_RootAllowed_NonRootRejected(t *testing.T) {
	conn := integrationDB_PlatformKind(t)
	ctx := context.Background()

	prefix := fmt.Sprintf("itest-kind-%s", uuid.New().String()[:8])
	cleanup := func() {
		if _, err := conn.ExecContext(ctx,
			`DELETE FROM workspaces WHERE name LIKE $1`, prefix+"%"); err != nil {
			t.Logf("cleanup (non-fatal): %v", err)
		}
	}
	t.Cleanup(cleanup)
	cleanup() // pre-test hygiene in the shared integration DB

	rootID := uuid.New().String()
	childID := uuid.New().String()

	// 1. Default kind is 'workspace' when the column is omitted on INSERT.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
		VALUES ($1, $2, 2, 'claude-code', 'online', NULL)
	`, rootID, prefix+"-root"); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	var gotKind string
	if err := conn.QueryRowContext(ctx,
		`SELECT kind FROM workspaces WHERE id = $1`, rootID).Scan(&gotKind); err != nil {
		t.Fatalf("read kind: %v", err)
	}
	if gotKind != "workspace" {
		t.Fatalf("default kind = %q, want \"workspace\"", gotKind)
	}

	// 2. The root row may become a platform agent.
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET kind = 'platform' WHERE id = $1`, rootID); err != nil {
		t.Fatalf("promote root to platform: unexpected error: %v", err)
	}

	// A child of the platform root (an ordinary workspace) inserts fine.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
		VALUES ($1, $2, 2, 'claude-code', 'online', $3)
	`, childID, prefix+"-child", rootID); err != nil {
		t.Fatalf("seed child: %v", err)
	}

	// 3. The non-root child may NOT be a platform agent — the CHECK rejects it.
	_, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET kind = 'platform' WHERE id = $1`, childID)
	if err == nil {
		t.Fatalf("non-root child accepted kind='platform' — constraint did not fire")
	}
	if !strings.Contains(err.Error(), "workspaces_platform_root_check") {
		t.Fatalf("non-root platform rejection wanted workspaces_platform_root_check, got: %v", err)
	}

	// And the unknown-kind value is rejected by workspaces_kind_check.
	_, err = conn.ExecContext(ctx,
		`UPDATE workspaces SET kind = 'bogus' WHERE id = $1`, rootID)
	if err == nil || !strings.Contains(err.Error(), "workspaces_kind_check") {
		t.Fatalf("unknown kind wanted workspaces_kind_check rejection, got: %v", err)
	}
}
