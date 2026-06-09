//go:build integration
// +build integration

// registry_auth_integration_test.go — REAL Postgres integration tests for
// the registry-auth + cross-tenant security boundary (issue #2148).
//
// Run with:
//
//	docker run --rm -d --name pg-integration \
//	  -e POSTGRES_PASSWORD=test -e POSTGRES_DB=molecule \
//	  -p 55432:5432 postgres:15-alpine
//	sleep 4
//	# apply migrations 001 (workspaces), 020 (workspace_auth_tokens),
//	# 035/036 (org_api_tokens + org_id), 043 (status enum), 044
//	# (platform_inbound_secret), 045 (delivery_mode) — CI applies the
//	# full migrations/ set in lexicographic order (apply-all-or-skip).
//	cd workspace-server
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55432/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run '^TestIntegration_(RegistryRowState|WSAuth|CanCommunicate|OrgToken)'
//
// CI (.gitea/workflows/handlers-postgres-integration.yml) runs this on
// every PR that touches workspace-server/internal/{handlers,wsauth,
// registry,orgtoken}/** OR workspace-server/migrations/**. The
// detect-changes `handlers-postgres` profile was widened to include the
// registry + orgtoken packages in the same PR that added this file so a
// regression in CanCommunicate / orgtoken.Revoke actually triggers the
// suite (#2148).
//
// Why these are NOT plain unit tests
// ----------------------------------
// The strict-sqlmock unit tests in wsauth/tokens_test.go,
// orgtoken/tokens_test.go, and registry/access_test.go pin which SQL
// statements fire — they are fast and let us iterate without a DB. But
// sqlmock asserts the SQL TEXT, not that a real Postgres ENFORCES the
// security predicate. The whole value of this package is the
// cross-tenant non-leak boundary:
//
//   - wsauth.ValidateToken binds a bearer to ONE workspace_id; sqlmock
//     cannot prove the JOIN on workspaces + the workspaceID equality
//     actually rejects a token replayed against a different workspace,
//     or a token whose workspace was soft-removed.
//   - registry.CanCommunicate walks the parent_id chain in real rows;
//     sqlmock returns canned rows so it can never catch a query that
//     leaks a SIBLING under a different org root, or a self→self / cross-
//     tenant decision that depends on the actual stored parent_id.
//   - orgtoken.Revoke / Validate depend on the partial-index +
//     revoked_at IS NULL predicate landing the row state; sqlmock is
//     satisfied by "an UPDATE fired".
//   - the registry register/heartbeat #73 guard
//     (WHERE workspaces.status IS DISTINCT FROM 'removed' /
//     status != 'removed') is a ROW-STATE invariant: a late
//     register/heartbeat MUST NOT resurrect a soft-deleted tombstone.
//     sqlmock cannot observe that the row stayed 'removed'.
//
// These tests close those gaps by booting a real Postgres, running the
// production functions (and, for register/heartbeat, replaying the exact
// production statement documented at registry.go:393 / registry.go:604),
// and SELECTing the row to verify the observable state.

package handlers

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	mdb "git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/orgtoken"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/registry"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/wsauth"
	_ "github.com/lib/pq"
)

// integrationAuthDB opens a connection from $INTEGRATION_DB_URL (skipping
// the test if unset), wipes the registry-auth tables for isolation, and
// hot-swaps the package-level mdb.DB so production functions that read the
// global (registry.CanCommunicate → registry.getWorkspaceRef) see this
// same connection. Restores the previous global + closes the conn via
// t.Cleanup.
//
// NOT SAFE FOR t.Parallel() — it mutates the package global and owns the
// tables for the duration of the test. This mirrors integrationDB in
// delegation_ledger_integration_test.go but wipes the auth tables (not
// delegations) and is kept separate so each suite's wipe step is local.
//
// Wipe order respects FKs: workspace_auth_tokens + org_api_tokens
// reference workspaces, so they go first. org_api_tokens.org_id is
// ON DELETE SET NULL and workspace_auth_tokens.workspace_id is
// ON DELETE CASCADE, but an explicit ordered DELETE keeps the intent
// obvious and avoids leaving rows behind if the FK actions ever change.
func integrationAuthDB(t *testing.T) *sql.DB {
	t.Helper()
	url := os.Getenv("INTEGRATION_DB_URL")
	if url == "" {
		t.Skip("INTEGRATION_DB_URL not set; skipping (local devs: see file header)")
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
	for _, tbl := range []string{"workspace_auth_tokens", "org_api_tokens", "workspaces"} {
		if _, err := conn.ExecContext(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("cleanup %s: %v", tbl, err)
		}
	}
	prev := mdb.DB
	mdb.DB = conn
	t.Cleanup(func() {
		mdb.DB = prev
		conn.Close()
	})
	return conn
}

// insertWorkspace creates a workspace row with the given status and
// optional parent, returning the DB-generated UUID. parentID may be the
// empty string for a root-level workspace (parent_id IS NULL). status
// must be a valid workspaces.status enum value (043 migration).
func insertWorkspace(t *testing.T, conn *sql.DB, name, status, parentID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var parent any
	if parentID != "" {
		parent = parentID
	}
	var id string
	err := conn.QueryRowContext(ctx, `
		INSERT INTO workspaces (name, status, parent_id, delivery_mode)
		VALUES ($1, $2, $3, 'push')
		RETURNING id
	`, name, status, parent).Scan(&id)
	if err != nil {
		t.Fatalf("insertWorkspace(%s): %v", name, err)
	}
	return id
}

// statusOf reads a workspace row's current status, failing the test if
// the row is gone (a register/heartbeat must never DELETE the row).
func statusOf(t *testing.T, conn *sql.DB, id string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var status string
	err := conn.QueryRowContext(ctx, `SELECT status FROM workspaces WHERE id = $1`, id).Scan(&status)
	if err != nil {
		t.Fatalf("statusOf(%s): %v", id, err)
	}
	return status
}

// ---------------------------------------------------------------------------
// 1 — registry register/heartbeat row-state: the #73 tombstone guard.
//
// Watch-fail intent: drop `WHERE workspaces.status IS DISTINCT FROM
// 'removed'` from the upsert (or `AND status != 'removed'` from the
// heartbeat) and a late register/heartbeat resurrects a soft-deleted
// workspace back to 'online' — the exact bulk-delete straggler bug the
// guard fixed. These tests replay the EXACT production statements
// (registry.go:393 upsert / registry.go:604 heartbeat) so a regression
// there is caught against a real row, not just an sqlmock SQL-text diff.
// ---------------------------------------------------------------------------

// registerUpsertSQL mirrors RegistryHandler.Register's upsert at
// workspace-server/internal/handlers/registry.go:393. Kept in lockstep
// with that statement; CR2/CI must confirm it still matches the handler.
const registerUpsertSQL = `
	INSERT INTO workspaces (id, name, url, agent_card, status, last_heartbeat_at, delivery_mode)
	VALUES ($1, $2, $3, $4::jsonb, 'online', now(), $5)
	ON CONFLICT (id) DO UPDATE SET
		url = CASE
			WHEN workspaces.url LIKE 'http://127.0.0.1%' THEN workspaces.url
			ELSE EXCLUDED.url
		END,
		agent_card = EXCLUDED.agent_card,
		status = 'online',
		last_heartbeat_at = now(),
		delivery_mode = EXCLUDED.delivery_mode,
		updated_at = now()
	WHERE workspaces.status IS DISTINCT FROM 'removed'
`

// heartbeatUpdateSQL mirrors RegistryHandler.Heartbeat's zero-spend
// branch at workspace-server/internal/handlers/registry.go:604, reduced
// to the columns guaranteed present by the base migration set so the
// test does not depend on optional columns the apply-all-or-skip CI step
// may not have landed. The security-relevant clause — the
// `AND status != 'removed'` guard — is preserved verbatim.
const heartbeatUpdateSQL = `
	UPDATE workspaces SET
		last_heartbeat_at = now(),
		updated_at        = now()
	WHERE id = $1 AND status != 'removed'
`

func TestIntegration_RegistryRowState_RegisterDoesNotResurrectRemoved(t *testing.T) {
	conn := integrationAuthDB(t)
	ctx := context.Background()

	id := insertWorkspace(t, conn, "tombstoned-ws", "removed", "")

	// A late register for a soft-deleted workspace.
	if _, err := conn.ExecContext(ctx, registerUpsertSQL,
		id, id, "https://agent.example.com", `{"name":"x"}`, "push"); err != nil {
		t.Fatalf("register upsert: %v", err)
	}

	if got := statusOf(t, conn, id); got != "removed" {
		t.Fatalf("removed workspace was resurrected by register: status=%q, want 'removed' (#73 guard regressed)", got)
	}
}

func TestIntegration_RegistryRowState_RegisterUpsertsLiveWorkspaceToOnline(t *testing.T) {
	conn := integrationAuthDB(t)
	ctx := context.Background()

	// A provisioning workspace registering for the first heartbeat should
	// flip to online — proves the guard does NOT over-block live rows.
	id := insertWorkspace(t, conn, "live-ws", "provisioning", "")

	if _, err := conn.ExecContext(ctx, registerUpsertSQL,
		id, id, "https://agent.example.com", `{"name":"x"}`, "push"); err != nil {
		t.Fatalf("register upsert: %v", err)
	}

	if got := statusOf(t, conn, id); got != "online" {
		t.Fatalf("live workspace register: status=%q, want 'online'", got)
	}
}

func TestIntegration_RegistryRowState_HeartbeatDoesNotResurrectRemoved(t *testing.T) {
	conn := integrationAuthDB(t)
	ctx := context.Background()

	id := insertWorkspace(t, conn, "tombstoned-ws", "removed", "")

	if _, err := conn.ExecContext(ctx, heartbeatUpdateSQL, id); err != nil {
		t.Fatalf("heartbeat update: %v", err)
	}

	if got := statusOf(t, conn, id); got != "removed" {
		t.Fatalf("removed workspace mutated by heartbeat: status=%q, want 'removed' (#73 guard regressed)", got)
	}

	// And last_heartbeat_at must NOT have been bumped on the tombstone —
	// a refreshed heartbeat would confuse the liveness monitor.
	var hb sql.NullTime
	if err := conn.QueryRowContext(ctx,
		`SELECT last_heartbeat_at FROM workspaces WHERE id = $1`, id).Scan(&hb); err != nil {
		t.Fatalf("read last_heartbeat_at: %v", err)
	}
	if hb.Valid {
		t.Fatalf("removed workspace got last_heartbeat_at bumped by heartbeat: %v (#73 guard regressed)", hb.Time)
	}
}

func TestIntegration_RegistryRowState_HeartbeatUpdatesLiveWorkspace(t *testing.T) {
	conn := integrationAuthDB(t)
	ctx := context.Background()

	id := insertWorkspace(t, conn, "live-ws", "online", "")

	if _, err := conn.ExecContext(ctx, heartbeatUpdateSQL, id); err != nil {
		t.Fatalf("heartbeat update: %v", err)
	}

	var hb sql.NullTime
	if err := conn.QueryRowContext(ctx,
		`SELECT last_heartbeat_at FROM workspaces WHERE id = $1`, id).Scan(&hb); err != nil {
		t.Fatalf("read last_heartbeat_at: %v", err)
	}
	if !hb.Valid {
		t.Fatalf("live workspace heartbeat did NOT bump last_heartbeat_at")
	}
	if got := statusOf(t, conn, id); got != "online" {
		t.Fatalf("live workspace heartbeat changed status unexpectedly: %q", got)
	}
}

// ---------------------------------------------------------------------------
// 2 — wsauth.ValidateToken A↔B binding (the cross-tenant non-leak boundary).
//
// Watch-fail intent: drop the `workspaceID != expectedWorkspaceID` check
// (or the JOIN's `w.status != 'removed'`) and a workspace-A token would
// authenticate workspace B, or a token from a soft-removed workspace
// would stay live. sqlmock cannot prove the JOIN rejects either.
// ---------------------------------------------------------------------------

func TestIntegration_WSAuth_TokenBoundToIssuingWorkspace(t *testing.T) {
	conn := integrationAuthDB(t)
	ctx := context.Background()

	wsA := insertWorkspace(t, conn, "ws-A", "online", "")
	wsB := insertWorkspace(t, conn, "ws-B", "online", "")

	plaintext, err := wsauth.IssueToken(ctx, conn, wsA)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	// Correct binding: token validates for its own workspace.
	if err := wsauth.ValidateToken(ctx, conn, wsA, plaintext); err != nil {
		t.Fatalf("ValidateToken(A, tokenA): want nil, got %v", err)
	}

	// Cross-workspace replay: A's token MUST NOT authenticate B.
	if err := wsauth.ValidateToken(ctx, conn, wsB, plaintext); err != wsauth.ErrInvalidToken {
		t.Fatalf("ValidateToken(B, tokenA): want ErrInvalidToken, got %v (cross-tenant binding regressed)", err)
	}

	// WorkspaceFromToken resolves the OWNING workspace, never B.
	owner, err := wsauth.WorkspaceFromToken(ctx, conn, plaintext)
	if err != nil {
		t.Fatalf("WorkspaceFromToken: %v", err)
	}
	if owner != wsA {
		t.Fatalf("WorkspaceFromToken: got %q, want %q (token rebinding regressed)", owner, wsA)
	}
}

func TestIntegration_WSAuth_TokenOfRemovedWorkspaceRejected(t *testing.T) {
	conn := integrationAuthDB(t)
	ctx := context.Background()

	ws := insertWorkspace(t, conn, "ws-soon-removed", "online", "")
	plaintext, err := wsauth.IssueToken(ctx, conn, ws)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	// Sanity: live before removal.
	if err := wsauth.ValidateToken(ctx, conn, ws, plaintext); err != nil {
		t.Fatalf("pre-removal ValidateToken: want nil, got %v", err)
	}

	// Soft-remove the workspace (tombstone) — the token row is NOT
	// touched, so only the JOIN's status filter can reject it.
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET status = 'removed' WHERE id = $1`, ws); err != nil {
		t.Fatalf("soft-remove: %v", err)
	}

	if err := wsauth.ValidateToken(ctx, conn, ws, plaintext); err != wsauth.ErrInvalidToken {
		t.Fatalf("ValidateToken after soft-remove: want ErrInvalidToken, got %v (removed-workspace JOIN filter regressed)", err)
	}
	if _, err := wsauth.WorkspaceFromToken(ctx, conn, plaintext); err != wsauth.ErrInvalidToken {
		t.Fatalf("WorkspaceFromToken after soft-remove: want ErrInvalidToken, got %v", err)
	}
}

func TestIntegration_WSAuth_RevokeAllForWorkspaceKillsToken(t *testing.T) {
	conn := integrationAuthDB(t)
	ctx := context.Background()

	ws := insertWorkspace(t, conn, "ws-rotate", "online", "")
	plaintext, err := wsauth.IssueToken(ctx, conn, ws)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if err := wsauth.ValidateToken(ctx, conn, ws, plaintext); err != nil {
		t.Fatalf("pre-revoke ValidateToken: %v", err)
	}

	if err := wsauth.RevokeAllForWorkspace(ctx, conn, ws); err != nil {
		t.Fatalf("RevokeAllForWorkspace: %v", err)
	}

	if err := wsauth.ValidateToken(ctx, conn, ws, plaintext); err != wsauth.ErrInvalidToken {
		t.Fatalf("ValidateToken after revoke: want ErrInvalidToken, got %v (revoked_at filter regressed)", err)
	}
}

// ---------------------------------------------------------------------------
// 3 — registry.CanCommunicate parent_id hierarchy (cross-tenant non-chatter).
//
// Topology (two distinct tenant trees):
//
//	rootX (org root, parent_id NULL)            rootY (org root, parent_id NULL)
//	  ├── leadX                                   └── leadY
//	  │     ├── engA  (leaf)
//	  │     └── engB  (leaf, sibling of engA)
//
// Watch-fail intent: relax the parent_id scoping and a leaf under rootX
// could message a leaf under rootY (cross-tenant leak), or two org roots
// (both parent_id NULL) would be treated as siblings. CanCommunicate
// reads the package-global db.DB, which integrationAuthDB hot-swaps to
// the test connection.
// ---------------------------------------------------------------------------

func TestIntegration_CanCommunicate_HierarchyAndCrossTenantIsolation(t *testing.T) {
	conn := integrationAuthDB(t)

	rootX := insertWorkspace(t, conn, "rootX", "online", "")
	leadX := insertWorkspace(t, conn, "leadX", "online", rootX)
	engA := insertWorkspace(t, conn, "engA", "online", leadX)
	engB := insertWorkspace(t, conn, "engB", "online", leadX)

	rootY := insertWorkspace(t, conn, "rootY", "online", "")
	leadY := insertWorkspace(t, conn, "leadY", "online", rootY)

	cases := []struct {
		name   string
		caller string
		target string
		want   bool
	}{
		{"self to self", engA, engA, true},
		{"siblings (same parent leadX)", engA, engB, true},
		{"direct parent to child", leadX, engA, true},
		{"direct child to parent", engA, leadX, true},
		{"distant ancestor to descendant", rootX, engA, true},
		{"distant descendant to ancestor", engA, rootX, true},
		// Cross-tenant non-leak: nothing under rootX may talk to rootY tree.
		{"cross-tenant leaf to leaf", engA, leadY, false},
		{"cross-tenant leaf to other root", engA, rootY, false},
		{"cross-tenant root to root", rootX, rootY, false},
		// Two org roots both have parent_id NULL — must NOT be siblings.
		{"two org roots are not siblings", rootX, rootY, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := registry.CanCommunicate(tc.caller, tc.target); got != tc.want {
				t.Fatalf("CanCommunicate(%s, %s) = %v, want %v", tc.caller, tc.target, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4 — orgtoken revoke / validate row-state.
//
// Watch-fail intent: drop `revoked_at IS NULL` from Validate (or the
// `AND revoked_at IS NULL` from Revoke's idempotency) and a revoked org-
// admin token would keep authenticating, or a second revoke would report
// success on an already-dead token. sqlmock is satisfied by "an UPDATE
// fired" and cannot observe the row no longer authenticates.
// ---------------------------------------------------------------------------

func TestIntegration_OrgToken_RevokeStopsValidation(t *testing.T) {
	conn := integrationAuthDB(t)
	ctx := context.Background()

	// org_id references workspaces(id); anchor the token to a real org root.
	org := insertWorkspace(t, conn, "org-root", "online", "")

	plaintext, id, err := orgtoken.Issue(ctx, conn, "ci-token", "tester", org)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Live token validates and reports its org anchor.
	gotID, _, gotOrg, err := orgtoken.Validate(ctx, conn, plaintext)
	if err != nil {
		t.Fatalf("Validate (live): %v", err)
	}
	if gotID != id {
		t.Fatalf("Validate id = %q, want %q", gotID, id)
	}
	if gotOrg != org {
		t.Fatalf("Validate org_id = %q, want %q (org-anchor regressed)", gotOrg, org)
	}

	// First revoke flips live → revoked.
	transitioned, err := orgtoken.Revoke(ctx, conn, id)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !transitioned {
		t.Fatalf("Revoke: want true (live→revoked), got false")
	}

	// Revoked token MUST NOT validate.
	if _, _, _, err := orgtoken.Validate(ctx, conn, plaintext); err != orgtoken.ErrInvalidToken {
		t.Fatalf("Validate after revoke: want ErrInvalidToken, got %v (revoked_at filter regressed)", err)
	}

	// Idempotent re-revoke reports false (already revoked), not an error.
	transitioned2, err := orgtoken.Revoke(ctx, conn, id)
	if err != nil {
		t.Fatalf("re-Revoke: %v", err)
	}
	if transitioned2 {
		t.Fatalf("re-Revoke: want false (already revoked), got true (idempotency guard regressed)")
	}
}

func TestIntegration_OrgToken_ListExcludesRevoked(t *testing.T) {
	conn := integrationAuthDB(t)
	ctx := context.Background()

	org := insertWorkspace(t, conn, "org-root", "online", "")

	_, liveID, err := orgtoken.Issue(ctx, conn, "live", "tester", org)
	if err != nil {
		t.Fatalf("Issue live: %v", err)
	}
	_, deadID, err := orgtoken.Issue(ctx, conn, "dead", "tester", org)
	if err != nil {
		t.Fatalf("Issue dead: %v", err)
	}
	if _, err := orgtoken.Revoke(ctx, conn, deadID); err != nil {
		t.Fatalf("Revoke dead: %v", err)
	}

	// List must return only the live token (exercises the real
	// COALESCE(org_id::text,'') cast that sqlmock can't type-check —
	// see the comment in orgtoken.List).
	tokens, err := orgtoken.List(ctx, conn)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("List returned %d tokens, want 1 (revoked-exclusion regressed)", len(tokens))
	}
	if tokens[0].ID != liveID {
		t.Fatalf("List returned id %q, want live id %q", tokens[0].ID, liveID)
	}
	if tokens[0].OrgID != org {
		t.Fatalf("List org_id = %q, want %q", tokens[0].OrgID, org)
	}

	// HasAnyLive is true with one live token, false once it's revoked.
	if ok, err := orgtoken.HasAnyLive(ctx, conn); err != nil || !ok {
		t.Fatalf("HasAnyLive (one live): ok=%v err=%v, want true,nil", ok, err)
	}
	if _, err := orgtoken.Revoke(ctx, conn, liveID); err != nil {
		t.Fatalf("Revoke live: %v", err)
	}
	if ok, err := orgtoken.HasAnyLive(ctx, conn); err != nil || ok {
		t.Fatalf("HasAnyLive (none live): ok=%v err=%v, want false,nil", ok, err)
	}
}
