//go:build integration
// +build integration

// platform_agent_integration_test.go — REAL Postgres gate for installPlatformAgent.
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run Integration_PlatformAgentInstall -v
//
// CI: handlers-postgres-integration workflow (handlers + migrations path filter).
//
// Why this is NOT a sqlmock test
// ------------------------------
// The install re-parents the org's existing root under the platform agent AND
// moves the org-anchor references (org_api_tokens.org_id, org_plugin_allowlist.
// org_id) from old root to platform agent, atomically. The whole point is the
// post-transaction row state: orgRootID() must resolve every node to the platform
// agent, sameOrg() must still hold, and the auth/allowlist anchors must point at
// the new root. Only a real Postgres can prove that; sqlmock cannot.

package handlers

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

func integrationDB_PlatformAgentInstall(t *testing.T) *sql.DB {
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

// TestIntegration_PlatformAgentInstall_ReparentsRootAndMovesAnchors builds a
// real org in Postgres:
//
//	root (parent_id NULL, kind=workspace)
//	└── child
//	+ an org_api_token  anchored to root
//	+ an org_plugin_allowlist entry anchored to root
//
// then installs the platform agent and asserts:
//   - the platform agent is the new sole root (kind=platform, parent_id NULL);
//   - the old root is re-parented under it; the child is untouched;
//   - both org-anchor references now point at the platform agent;
//   - a second install is a no-op (idempotent).
func TestIntegration_PlatformAgentInstall_ReparentsRootAndMovesAnchors(t *testing.T) {
	conn := integrationDB_PlatformAgentInstall(t)
	ctx := context.Background()

	tag := uuid.New().String()[:8]
	prefix := fmt.Sprintf("itest-pinstall-%s", tag)
	rootID := uuid.New().String()
	childID := uuid.New().String()
	platformID := uuid.New().String()

	cleanup := func() {
		_, _ = conn.ExecContext(ctx, `DELETE FROM org_plugin_allowlist WHERE plugin_name = $1`, prefix+"-plugin")
		_, _ = conn.ExecContext(ctx, `DELETE FROM org_api_tokens WHERE prefix = $1`, tag)
		// child + old root (prefixed names) first, then the platform agent by id
		// (root.parent_id references it, so it must go last).
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name LIKE $1`, prefix+"%")
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, platformID)
	}
	t.Cleanup(cleanup)
	cleanup()

	// Seed org tree.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
		VALUES ($1, $2, 2, 'claude-code', 'online', NULL)`, rootID, prefix+"-root"); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
		VALUES ($1, $2, 2, 'claude-code', 'online', $3)`, childID, prefix+"-child", rootID); err != nil {
		t.Fatalf("seed child: %v", err)
	}
	// Org-anchor rows keyed to the OLD root.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO org_api_tokens (token_hash, prefix, name, org_id)
		VALUES ($1, $2, $3, $4)`,
		[]byte("hash-"+tag), tag, prefix+"-tok", rootID); err != nil {
		t.Fatalf("seed org_api_token: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO org_plugin_allowlist (org_id, plugin_name, enabled_by)
		VALUES ($1, $2, $3)`, rootID, prefix+"-plugin", childID); err != nil {
		t.Fatalf("seed allowlist: %v", err)
	}

	// Install.
	if err := installPlatformAgent(ctx, conn, platformID, "Org Concierge"); err != nil {
		t.Fatalf("install: %v", err)
	}

	assertState := func(stage string) {
		// platform agent is a kind=platform root.
		var kind string
		var parent sql.NullString
		if err := conn.QueryRowContext(ctx,
			`SELECT kind, parent_id FROM workspaces WHERE id = $1`, platformID).Scan(&kind, &parent); err != nil {
			t.Fatalf("[%s] read platform agent: %v", stage, err)
		}
		if kind != "platform" || parent.Valid {
			t.Fatalf("[%s] platform agent kind=%q parent=%v, want platform/NULL", stage, kind, parent)
		}
		// old root re-parented under the platform agent.
		var rootParent sql.NullString
		if err := conn.QueryRowContext(ctx,
			`SELECT parent_id FROM workspaces WHERE id = $1`, rootID).Scan(&rootParent); err != nil {
			t.Fatalf("[%s] read old root: %v", stage, err)
		}
		if !rootParent.Valid || rootParent.String != platformID {
			t.Fatalf("[%s] old root parent=%v, want %s", stage, rootParent, platformID)
		}
		// child untouched.
		var childParent sql.NullString
		if err := conn.QueryRowContext(ctx,
			`SELECT parent_id FROM workspaces WHERE id = $1`, childID).Scan(&childParent); err != nil {
			t.Fatalf("[%s] read child: %v", stage, err)
		}
		if !childParent.Valid || childParent.String != rootID {
			t.Fatalf("[%s] child parent=%v, want %s (unchanged)", stage, childParent, rootID)
		}
		// org-anchor references moved to the platform agent.
		var tokOrg, alOrg string
		if err := conn.QueryRowContext(ctx,
			`SELECT org_id FROM org_api_tokens WHERE prefix = $1`, tag).Scan(&tokOrg); err != nil {
			t.Fatalf("[%s] read token org_id: %v", stage, err)
		}
		if tokOrg != platformID {
			t.Fatalf("[%s] org_api_tokens.org_id=%s, want %s", stage, tokOrg, platformID)
		}
		if err := conn.QueryRowContext(ctx,
			`SELECT org_id FROM org_plugin_allowlist WHERE plugin_name = $1`, prefix+"-plugin").Scan(&alOrg); err != nil {
			t.Fatalf("[%s] read allowlist org_id: %v", stage, err)
		}
		if alOrg != platformID {
			t.Fatalf("[%s] org_plugin_allowlist.org_id=%s, want %s", stage, alOrg, platformID)
		}
		// orgRootID + sameOrg now resolve everything to the platform agent.
		got, err := orgRootID(ctx, conn, childID)
		if err != nil {
			t.Fatalf("[%s] orgRootID(child): %v", stage, err)
		}
		if got != platformID {
			t.Fatalf("[%s] orgRootID(child)=%s, want %s", stage, got, platformID)
		}
		same, err := sameOrg(ctx, conn, childID, platformID)
		if err != nil || !same {
			t.Fatalf("[%s] sameOrg(child, platform)=%v err=%v, want true", stage, same, err)
		}
	}

	assertState("first install")

	// Idempotent: second install must not error or change state.
	if err := installPlatformAgent(ctx, conn, platformID, "Org Concierge"); err != nil {
		t.Fatalf("second install (idempotent): %v", err)
	}
	assertState("second install")

	// Neither seeded team node is a root any more — the platform agent is.
	var nRoots int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM workspaces WHERE parent_id IS NULL AND id IN ($1, $2)`,
		rootID, childID).Scan(&nRoots); err != nil {
		t.Fatalf("count roots: %v", err)
	}
	if nRoots != 0 {
		t.Fatalf("team roots after install = %d, want 0 (old root re-parented under platform agent)", nRoots)
	}
}
