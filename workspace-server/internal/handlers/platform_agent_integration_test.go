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
	"errors"
	"fmt"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/google/uuid"
	"github.com/lib/pq"
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

	paName := "Org Concierge " + tag
	cleanup := func() {
		_, _ = conn.ExecContext(ctx, `DELETE FROM org_plugin_allowlist WHERE plugin_name = $1`, prefix+"-plugin")
		_, _ = conn.ExecContext(ctx, `DELETE FROM org_api_tokens WHERE prefix = $1`, tag)
		// child + old root (prefixed names) first, then the platform agent by id
		// (root.parent_id references it, so it must go last).
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name LIKE $1`, prefix+"%")
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name = $1`, paName)
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
	if err := installPlatformAgent(ctx, conn, platformID, paName, defaultConciergeRuntime); err != nil {
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
	if err := installPlatformAgent(ctx, conn, platformID, paName, defaultConciergeRuntime); err != nil {
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

// TestIntegration_PlatformAgentInstall_MultiRootAllowlistDedup guards the
// UNIQUE(org_id, plugin_name) collision on org_plugin_allowlist. When N>1
// old roots have allowlisted the SAME plugin, a plain UPDATE…SET org_id
// collides 23505. The INSERT…SELECT…ON CONFLICT DO NOTHING + DELETE leftovers
// path must survive this deterministically (core#2508).
func TestIntegration_PlatformAgentInstall_MultiRootAllowlistDedup(t *testing.T) {
	conn := integrationDB_PlatformAgentInstall(t)
	ctx := context.Background()

	tag := uuid.New().String()[:8]
	prefix := fmt.Sprintf("itest-pallow-%s", tag)
	rootA := uuid.New().String()
	rootB := uuid.New().String()
	platformID := uuid.New().String()

	paName := "Org Concierge " + tag
	cleanup := func() {
		_, _ = conn.ExecContext(ctx, `DELETE FROM org_plugin_allowlist WHERE plugin_name = $1`, prefix+"-plugin")
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name LIKE $1`, prefix+"%")
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name = $1`, paName)
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, platformID)
	}
	t.Cleanup(cleanup)
	cleanup()

	// Seed TWO old roots with the SAME plugin allowlisted.
	for _, rid := range []string{rootA, rootB} {
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
			VALUES ($1, $2, 2, 'claude-code', 'online', NULL)`, rid, prefix+"-"+rid[:8]); err != nil {
			t.Fatalf("seed root %s: %v", rid, err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO org_plugin_allowlist (org_id, plugin_name, enabled_by)
			VALUES ($1, $2, $3)`, rid, prefix+"-plugin", rid); err != nil {
			t.Fatalf("seed allowlist for %s: %v", rid, err)
		}
	}

	// Install must NOT fail with 23505 unique violation.
	if err := installPlatformAgent(ctx, conn, platformID, paName, defaultConciergeRuntime); err != nil {
		t.Fatalf("install with duplicate allowlist: %v", err)
	}

	// The platform agent must end up with exactly ONE row for the plugin.
	var count int
	if err := conn.QueryRowContext(ctx,
		`SELECT count(*) FROM org_plugin_allowlist WHERE org_id = $1`, platformID).Scan(&count); err != nil {
		t.Fatalf("count platform-agent allowlist: %v", err)
	}
	if count != 1 {
		t.Fatalf("platform-agent allowlist count = %d, want 1 (deduped from 2 old roots)", count)
	}

	// Old roots must have ZERO allowlist rows left.
	for _, rid := range []string{rootA, rootB} {
		var left int
		if err := conn.QueryRowContext(ctx,
			`SELECT count(*) FROM org_plugin_allowlist WHERE org_id = $1`, rid).Scan(&left); err != nil {
			t.Fatalf("count old-root allowlist %s: %v", rid, err)
		}
		if left != 0 {
			t.Fatalf("old root %s still has %d allowlist rows, want 0 (leftovers deleted)", rid, left)
		}
	}
}

// TestIntegration_PlatformAgentInstall_StatusNotOnline prevents the green-dot
// lie: a freshly inserted platform-agent row must NOT claim status='online'
// when there is no container yet (core#2508).
func TestIntegration_PlatformAgentInstall_StatusNotOnline(t *testing.T) {
	conn := integrationDB_PlatformAgentInstall(t)
	ctx := context.Background()

	platformID := uuid.New().String()
	paName := "Org Concierge " + platformID[:8]

	cleanup := func() {
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name = $1`, paName)
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, platformID)
	}
	t.Cleanup(cleanup)
	cleanup()

	if err := installPlatformAgent(ctx, conn, platformID, paName, defaultConciergeRuntime); err != nil {
		t.Fatalf("install: %v", err)
	}

	var status string
	if err := conn.QueryRowContext(ctx,
		`SELECT status FROM workspaces WHERE id = $1`, platformID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status == "online" {
		t.Fatalf("fresh platform-agent status=%q, must not be 'online' (no container yet)", status)
	}
	if status != "offline" {
		// Allow future changes, but catch the specific lie.
		t.Logf("fresh platform-agent status=%q (expected 'offline', but accepting non-online)", status)
	}
}

// TestIntegration_PlatformAgentInstall_RuntimeIsParameterAndNotClobbered is the
// PROVE-FAIL gate for the P3b runtime generalization. The concierge runtime is
// now a PARAMETER of installPlatformAgent, and — critically — the ON CONFLICT
// clause must NOT revert an existing concierge's runtime back to claude-code on a
// re-install (CP backfill, idempotent re-call). This INVERTS the pre-P3b core#2496
// behavior (which deliberately hard-set `runtime = 'claude-code'` on conflict).
//
// Cases:
//
//	Case 1 — fresh INSERT seeds the requested runtime ('codex'), proving the
//	         INSERT VALUES uses the parameter, not a hardcoded literal.
//	Case 2 — a re-install (the ON CONFLICT path) does NOT revert 'codex' back to
//	         'claude-code'. Against the pre-P3b clause (`SET ... runtime =
//	         'claude-code' ...`) this assertion FAILS; against the P3b clause
//	         (runtime omitted from the conflict SET) it passes. This is the
//	         prove-fail core of item 3.
//	Case 3 — the default-runtime install path still seeds the compiled-in default, so the
//	         backward-compatible callers (self-host seed, CP without runtime) are
//	         unaffected.
func TestIntegration_PlatformAgentInstall_RuntimeIsParameterAndNotClobbered(t *testing.T) {
	conn := integrationDB_PlatformAgentInstall(t)
	ctx := context.Background()

	tag := uuid.New().String()[:8]
	platformID := uuid.New().String()
	paName := "Org Concierge " + tag
	const codexRuntime = "codex"

	// Robust-to-ordering pre-seed: downgrade any existing platform root to
	// 'workspace' to free the uniq-platform-root slot (FK-safe; mirrors
	// installPlatformAgent's own step 0).
	cleanup := func() {
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name = $1`, paName)
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, platformID)
		_, _ = conn.ExecContext(ctx, `UPDATE workspaces SET kind = 'workspace' WHERE kind = 'platform' AND parent_id IS NULL AND id <> $1`, platformID)
	}
	t.Cleanup(cleanup)
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET kind = 'workspace' WHERE kind = 'platform' AND parent_id IS NULL`); err != nil {
		t.Fatalf("pre-seed: downgrade existing platform-agent rows: %v", err)
	}
	cleanup()

	// Case 1: fresh INSERT with runtime='codex' seeds the requested runtime.
	if err := installPlatformAgent(ctx, conn, platformID, paName, codexRuntime); err != nil {
		t.Fatalf("install (codex): %v", err)
	}
	var runtime, template string
	if err := conn.QueryRowContext(ctx,
		`SELECT runtime, COALESCE(template, '') FROM workspaces WHERE id = $1`, platformID).Scan(&runtime, &template); err != nil {
		t.Fatalf("read post-install runtime/template: %v", err)
	}
	if runtime != codexRuntime {
		t.Fatalf("fresh install: runtime=%q, want %q — the INSERT VALUES must use the runtime PARAMETER, not a hardcoded 'claude-code'", runtime, codexRuntime)
	}
	if template != "platform-agent" {
		t.Fatalf("fresh install: template=%q, want %q — the concierge persona template is runtime-agnostic (conciergeTemplateForRuntime returns 'platform-agent' for every runtime, tenant-agent BUG 1)", template, "platform-agent")
	}

	// Case 2 (PROVE-FAIL): a re-install (ON CONFLICT path) must NOT revert the
	// runtime back to claude-code. The concierge persona template is now RUNTIME-
	// AGNOSTIC (a single 'platform-agent' entry serves every runtime, tenant-agent
	// BUG 1), so the (runtime, template) pair can never desync: runtime is PRESERVED
	// on conflict, template is unconditionally 'platform-agent'. Against the pre-P3b
	// `ON CONFLICT ... SET runtime = 'claude-code'` clause the runtime check fails.
	// We call the DEFAULT-runtime install path on purpose to prove the existing
	// 'codex' runtime survives a default reinstall while the template stays
	// 'platform-agent'.
	if err := installPlatformAgent(ctx, conn, platformID, paName, defaultConciergeRuntime); err != nil {
		t.Fatalf("re-install (default runtime): %v", err)
	}
	var runtimeAfterReinstall, templateAfterReinstall string
	if err := conn.QueryRowContext(ctx,
		`SELECT runtime, COALESCE(template, '') FROM workspaces WHERE id = $1`, platformID).Scan(&runtimeAfterReinstall, &templateAfterReinstall); err != nil {
		t.Fatalf("read runtime/template after re-install: %v", err)
	}
	if runtimeAfterReinstall != codexRuntime {
		t.Fatalf("P3b regression: re-install reverted runtime to %q, want %q PRESERVED. "+
			"The ON CONFLICT DO UPDATE SET clause must NOT include `runtime = 'claude-code'` — "+
			"re-installing a codex/openclaw concierge must not clobber it back to claude-code.",
			runtimeAfterReinstall, codexRuntime)
	}
	if wantTemplate := conciergeTemplateForRuntime(codexRuntime); templateAfterReinstall != wantTemplate {
		t.Fatalf("re-install via the default path changed the concierge template to %q, want %q. "+
			"The concierge persona template is runtime-agnostic (tenant-agent BUG 1): the ON CONFLICT "+
			"clause must set `template = 'platform-agent'` unconditionally, and runtime %q must be PRESERVED.",
			templateAfterReinstall, wantTemplate, codexRuntime)
	}

	// Case 3: the default-runtime install path seeds the compiled-in default
	// runtime on a FRESH row (backward compatibility for self-host seed + CP
	// callers that send no runtime). Asserting against the const rather than a
	// literal keeps this correct across future default flips. The persona
	// template stays 'platform-agent' for EVERY runtime
	// (runtime-agnostic, tenant-agent BUG 1).
	freshID := uuid.New().String()
	freshName := "Org Concierge fresh " + tag
	if err := installPlatformAgent(ctx, conn, freshID, freshName, defaultConciergeRuntime); err != nil {
		t.Fatalf("fresh default install: %v", err)
	}
	var freshRuntime, freshTemplate string
	if err := conn.QueryRowContext(ctx,
		`SELECT runtime, COALESCE(template, '') FROM workspaces WHERE id = $1`, freshID).Scan(&freshRuntime, &freshTemplate); err != nil {
		t.Fatalf("read fresh runtime/template: %v", err)
	}
	if freshRuntime != defaultConciergeRuntime {
		t.Fatalf("default install path: runtime=%q, want %q (the compiled-in default)", freshRuntime, defaultConciergeRuntime)
	}
	if freshTemplate != "platform-agent" {
		t.Fatalf("default install path: template=%q, want 'platform-agent' (runtime-agnostic concierge template)", freshTemplate)
	}
	_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, freshID)
}

// TestIntegration_PlatformAgentInstall_PreservesRemovedFlag is the real-Postgres
// proof of CR2 RC 14676 fix 2: a re-install of a REMOVED (tombstoned) concierge
// must PRESERVE status='removed' — the ON CONFLICT upsert must not silently
// un-delete it. (The deliberate un-tombstone is the ensure handler's explicit
// reviveRemovedPlatformAgent step, NOT a side-effect of install.) sqlmock can
// prove install issues no status statement; only real Postgres can prove the
// post-upsert ROW status, which is what this asserts.
func TestIntegration_PlatformAgentInstall_PreservesRemovedFlag(t *testing.T) {
	conn := integrationDB_PlatformAgentInstall(t)
	ctx := context.Background()

	tag := uuid.New().String()[:8]
	platformID := uuid.New().String()
	paName := "Org Concierge removed " + tag

	cleanup := func() {
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name = $1`, paName)
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, platformID)
	}
	t.Cleanup(cleanup)
	cleanup()

	// Seed a platform-agent row, then tombstone it (exactly what CascadeDelete
	// does: stamp status='removed', leaving kind='platform' + parent_id NULL).
	if err := installPlatformAgent(ctx, conn, platformID, paName, defaultConciergeRuntime); err != nil {
		t.Fatalf("seed install: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET status = 'removed' WHERE id = $1`, platformID); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	// Re-install (the ON CONFLICT path) MUST NOT clear the removed flag.
	if err := installPlatformAgent(ctx, conn, platformID, paName, defaultConciergeRuntime); err != nil {
		t.Fatalf("re-install of removed row: %v", err)
	}
	var status string
	if err := conn.QueryRowContext(ctx,
		`SELECT status FROM workspaces WHERE id = $1`, platformID).Scan(&status); err != nil {
		t.Fatalf("read status after re-install: %v", err)
	}
	if status != "removed" {
		t.Fatalf("re-install un-tombstoned the concierge: status=%q, want 'removed' PRESERVED. "+
			"The ON CONFLICT upsert must NOT reset status; only the ensure handler's explicit "+
			"reviveRemovedPlatformAgent step may clear it.", status)
	}

	// And the deliberate revive DOES clear it (offline) — the explicit un-tombstone.
	if err := reviveRemovedPlatformAgent(ctx, conn, platformID); err != nil {
		t.Fatalf("revive: %v", err)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT status FROM workspaces WHERE id = $1`, platformID).Scan(&status); err != nil {
		t.Fatalf("read status after revive: %v", err)
	}
	if status != string(models.StatusOffline) {
		t.Fatalf("revive must clear the tombstone to 'offline', got status=%q", status)
	}

	// Revive is idempotent + scoped: a second revive on a non-removed row is a
	// strict no-op (0 rows), leaving the status untouched.
	if err := reviveRemovedPlatformAgent(ctx, conn, platformID); err != nil {
		t.Fatalf("idempotent revive: %v", err)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT status FROM workspaces WHERE id = $1`, platformID).Scan(&status); err != nil {
		t.Fatalf("read status after second revive: %v", err)
	}
	if status != string(models.StatusOffline) {
		t.Fatalf("second revive changed a non-removed row: status=%q, want 'offline' unchanged", status)
	}
}

// TestIntegration_PlatformAgentInstall_ConciergeBornWithBroadcast pins the
// org-wide birth default: a freshly-installed concierge (the kind='platform'
// org root) MUST be provisioned with broadcast_enabled=TRUE so every new org's
// top orchestrator can POST /broadcast to its team out of the box — no manual
// PATCH /workspaces/:id/abilities. It also proves the ability stays scoped to
// the orchestrator: an ordinary sub-agent (kind='workspace', created the way
// org import / team expand do) is BORN with broadcast_enabled=FALSE (schema
// default). Finally it proves the birth default does NOT weaken the operator's
// control — a re-install (the ON CONFLICT path) PRESERVES a deliberately-
// disabled concierge rather than silently re-enabling it.
//
// This is the real-artifact gate: sqlmock cannot prove the column's post-INSERT
// value, only that an INSERT fired. The broadcast_enabled column lands via
// migration 20260514120000_workspace_abilities (applied by the workflow's
// migration replay), so a real Postgres is required.
func TestIntegration_PlatformAgentInstall_ConciergeBornWithBroadcast(t *testing.T) {
	conn := integrationDB_PlatformAgentInstall(t)
	ctx := context.Background()

	tag := uuid.New().String()[:8]
	prefix := fmt.Sprintf("itest-bcastdefault-%s", tag)
	platformID := uuid.New().String()
	subAgentID := uuid.New().String()
	paName := "Org Concierge bcast " + tag

	cleanup := func() {
		// Sub-agent (child) first — it references the platform row via parent_id.
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name LIKE $1`, prefix+"%")
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name = $1`, paName)
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = $1`, platformID)
	}
	t.Cleanup(cleanup)
	cleanup()

	// 1. Install the concierge fresh (the create path — a brand-new org).
	if err := installPlatformAgent(ctx, conn, platformID, paName, defaultConciergeRuntime); err != nil {
		t.Fatalf("install concierge: %v", err)
	}

	// 2. The concierge is BORN with broadcast_enabled=TRUE (and is the sole
	//    kind='platform' org root, so the assertion targets the real concierge).
	var kind string
	var conciergeBroadcast bool
	if err := conn.QueryRowContext(ctx,
		`SELECT kind, broadcast_enabled FROM workspaces WHERE id = $1`, platformID).
		Scan(&kind, &conciergeBroadcast); err != nil {
		t.Fatalf("read concierge row: %v", err)
	}
	if kind != "platform" {
		t.Fatalf("installed concierge kind=%q, want 'platform'", kind)
	}
	if !conciergeBroadcast {
		t.Fatalf("fresh concierge broadcast_enabled=false, want TRUE — the org's top " +
			"orchestrator must be born able to broadcast to its team without a manual " +
			"PATCH /workspaces/:id/abilities (birth-default regression)")
	}

	// 3. An ordinary sub-agent (kind='workspace', the org-import/team-expand
	//    shape) is BORN with broadcast_enabled=FALSE — broadcast stays an
	//    orchestrator-only ability. We insert exactly the columns those paths
	//    set (no broadcast_enabled), so the schema default (FALSE) applies.
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, tier, runtime, status, parent_id)
		VALUES ($1, $2, 2, 'claude-code', 'online', $3)`,
		subAgentID, prefix+"-subagent", platformID); err != nil {
		t.Fatalf("seed sub-agent: %v", err)
	}
	var subAgentBroadcast bool
	if err := conn.QueryRowContext(ctx,
		`SELECT broadcast_enabled FROM workspaces WHERE id = $1`, subAgentID).
		Scan(&subAgentBroadcast); err != nil {
		t.Fatalf("read sub-agent row: %v", err)
	}
	if subAgentBroadcast {
		t.Fatalf("sub-agent broadcast_enabled=true, want FALSE — only the concierge/" +
			"orchestrator should hold broadcast at birth; a sub-agent must not")
	}

	// 4. A re-install (the idempotent ON CONFLICT path) PRESERVES the TRUE value.
	if err := installPlatformAgent(ctx, conn, platformID, paName, defaultConciergeRuntime); err != nil {
		t.Fatalf("re-install concierge: %v", err)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT broadcast_enabled FROM workspaces WHERE id = $1`, platformID).
		Scan(&conciergeBroadcast); err != nil {
		t.Fatalf("read concierge after re-install: %v", err)
	}
	if !conciergeBroadcast {
		t.Fatalf("re-install reset broadcast_enabled to false, want TRUE preserved")
	}

	// 5. The birth default does NOT override operator intent: if an admin
	//    deliberately DISABLES broadcast (PATCH /abilities → broadcast_enabled=
	//    FALSE), a subsequent re-install (ON CONFLICT) must NOT silently
	//    re-enable it. The ON CONFLICT clause deliberately leaves the column
	//    alone, exactly like it does for status/runtime.
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET broadcast_enabled = FALSE WHERE id = $1`, platformID); err != nil {
		t.Fatalf("operator-disable broadcast: %v", err)
	}
	if err := installPlatformAgent(ctx, conn, platformID, paName, defaultConciergeRuntime); err != nil {
		t.Fatalf("re-install after operator-disable: %v", err)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT broadcast_enabled FROM workspaces WHERE id = $1`, platformID).
		Scan(&conciergeBroadcast); err != nil {
		t.Fatalf("read concierge after re-install-post-disable: %v", err)
	}
	if conciergeBroadcast {
		t.Fatalf("re-install re-enabled a deliberately-disabled concierge broadcast_enabled, " +
			"want FALSE preserved — the ON CONFLICT upsert must not clobber the operator's " +
			"PATCH /abilities choice (only the fresh-INSERT VALUES sets the birth default)")
	}
}

// TestIntegration_PlatformAgentInstall_RefusesToDisplaceOnlinePlatformRoot is the
// PROVE-FAIL gate for the concierge-destruction bug.
//
// The raw install is row-only: it upserts the caller's id and NEVER provisions
// (that contract is frozen for the CP — see InstallPlatformAgent). Step 0 also
// downgrades any platform root whose id differs from the caller's, so that a
// non-canonical root cannot block uniq_workspaces_one_platform_root. Those two
// facts compose into a destructive one: post a FOREIGN id at an org whose
// concierge is ONLINE, and the live concierge is demoted to an ordinary
// workspace while a container-less row takes its place — with nothing left to
// bring it up. The canvas then resolves the new root, its status never reaches
// 'online', and the composer is disabled forever.
//
// That is not a hypothetical. canvas/e2e/staging-concierge.spec.ts posted a
// hardcoded uuid on every staging E2E run and destroyed the org's concierge for
// every spec that ran after it (staging-slow-cold-greeting: "concierge composer
// never enabled (agent unreachable)"), from bae6a1cdb (2026-06-11, which added
// the step-0 downgrade and thereby replaced the unique-index COLLISION that had
// been rejecting the foreign id loudly) until this guard.
//
// Case 1 (fail-before): a foreign id aimed at an ONLINE root is REFUSED, and the
//
//	live concierge survives untouched. Without the guard, step 0 demotes
//	it and this fails on the kind assertion.
//
// Case 2 (no false-positive): the same foreign id against a NOT-online root is
//
//	still allowed through — the legitimate non-canonical-id repair the
//	downgrade was written for keeps working.
//
// Case 3 (CP unaffected): re-installing the SAME id on an online root is a
//
//	normal idempotent upsert, never a conflict. This is the only path the
//	CP ever takes, and it must stay byte-identical.
func TestIntegration_PlatformAgentInstall_RefusesToDisplaceOnlinePlatformRoot(t *testing.T) {
	conn := integrationDB_PlatformAgentInstall(t)
	ctx := context.Background()

	liveID := uuid.New().String()
	foreignID := uuid.New().String()
	liveName := "Live Concierge " + liveID[:8]
	foreignName := "Foreign Concierge " + foreignID[:8]

	cleanup := func() {
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE id = ANY($1)`,
			pq.Array([]string{liveID, foreignID}))
		_, _ = conn.ExecContext(ctx, `DELETE FROM workspaces WHERE name = ANY($1)`,
			pq.Array([]string{liveName, foreignName}))
	}
	t.Cleanup(cleanup)
	cleanup()

	// Seed the org's real, ONLINE concierge as the sole platform root.
	//
	// uniq_workspaces_one_platform_root is a PARTIAL UNIQUE INDEX and these
	// integration tests share one database, so a platform root left behind by an
	// earlier test in the file (e.g. ConciergeBornWithBroadcast) collides with
	// this seed. Downgrade whatever root is standing before planting ours —
	// exactly what installPlatformAgent's own step 0 does to a non-live root.
	seedOnlineRoot := func(t *testing.T) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, `
			UPDATE workspaces SET kind = 'workspace'
			WHERE kind = 'platform' AND parent_id IS NULL AND id <> $1
		`, liveID); err != nil {
			t.Fatalf("clear pre-existing platform root: %v", err)
		}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO workspaces (id, name, kind, parent_id, status, runtime, template)
			VALUES ($1, $2, 'platform', NULL, 'online', $3, $4)
			ON CONFLICT (id) DO UPDATE SET kind = 'platform', parent_id = NULL, status = 'online'
		`, liveID, liveName, defaultConciergeRuntime, conciergeTemplateForRuntime(defaultConciergeRuntime)); err != nil {
			t.Fatalf("seed live concierge: %v", err)
		}
	}

	readKindStatus := func(t *testing.T, id string) (kind, status string, found bool) {
		t.Helper()
		err := conn.QueryRowContext(ctx,
			`SELECT COALESCE(kind::text, ''), COALESCE(status::text, '') FROM workspaces WHERE id = $1`,
			id).Scan(&kind, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false
		}
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return kind, status, true
	}

	// ── Case 1: foreign id vs ONLINE root → refused, live concierge intact ──
	seedOnlineRoot(t)
	err := installPlatformAgent(ctx, conn, foreignID, foreignName, defaultConciergeRuntime)
	var conflict *platformRootConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("installing foreign id %s over an ONLINE platform root must return "+
			"*platformRootConflictError (it would otherwise demote a live concierge and "+
			"install a container-less replacement it never provisions); got err=%v", foreignID, err)
	}
	if conflict.ExistingID != liveID {
		t.Fatalf("conflict names the wrong root: got %s, want the online root %s",
			conflict.ExistingID, liveID)
	}
	if kind, status, found := readKindStatus(t, liveID); !found || kind != "platform" || status != "online" {
		t.Fatalf("the LIVE concierge was damaged by a refused install: "+
			"found=%v kind=%q status=%q (want kind=platform status=online)", found, kind, status)
	}
	if _, _, found := readKindStatus(t, foreignID); found {
		t.Fatalf("the refused install still wrote the foreign row %s — the transaction "+
			"must roll back entirely", foreignID)
	}

	// ── Case 2: foreign id vs a DEGRADED root → ALSO refused ───────────────
	//
	// This is the case the first version of the guard got wrong. It probed
	// `status = 'online'` alone, so a DEGRADED root fell straight through to the
	// destructive step-0 downgrade. A degraded concierge is NOT a dead one: it has
	// a real, running container and is merely failing its health probe — which is
	// why registry/cp_instance_reconciler.go:168 and :294, healthsweep, hibernation
	// and wedged_agent all define the live set as IN ('online','degraded'). Row-only
	// installing over it orphans the container and leaves the org with a concierge
	// that has none: the exact destruction this guard exists to prevent, reachable
	// through a narrower window.
	//
	// Negative control: this case FAILS against the `status = 'online'` predicate
	// (install returns nil, liveID is demoted to kind='workspace') and passes only
	// with IN ('online','degraded').
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET status = 'degraded' WHERE id = $1`, liveID); err != nil {
		t.Fatalf("mark root degraded: %v", err)
	}
	err = installPlatformAgent(ctx, conn, foreignID, foreignName, defaultConciergeRuntime)
	if !errors.As(err, &conflict) {
		t.Fatalf("installing foreign id %s over a DEGRADED platform root must ALSO return "+
			"*platformRootConflictError — a degraded concierge still has a running container, "+
			"so a row-only install orphans it; got err=%v", foreignID, err)
	}
	if kind, status, found := readKindStatus(t, liveID); !found || kind != "platform" || status != "degraded" {
		t.Fatalf("the DEGRADED concierge was damaged by a refused install: "+
			"found=%v kind=%q status=%q (want kind=platform status=degraded)", found, kind, status)
	}
	if _, _, found := readKindStatus(t, foreignID); found {
		t.Fatalf("the refused install still wrote the foreign row %s — the transaction "+
			"must roll back entirely", foreignID)
	}

	// ── Case 3: foreign id vs a NOT-live root → the repair path still works ──
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET status = 'failed' WHERE id = $1`, liveID); err != nil {
		t.Fatalf("mark root failed: %v", err)
	}
	if err := installPlatformAgent(ctx, conn, foreignID, foreignName, defaultConciergeRuntime); err != nil {
		t.Fatalf("installing a foreign id over a NOT-online root must still be allowed "+
			"(this is the non-canonical-id repair the step-0 downgrade exists for); got %v", err)
	}
	if kind, _, _ := readKindStatus(t, foreignID); kind != "platform" {
		t.Fatalf("repair install did not make %s the platform root (kind=%q)", foreignID, kind)
	}
	if kind, _, _ := readKindStatus(t, liveID); kind != "workspace" {
		t.Fatalf("repair install did not downgrade the broken root %s (kind=%q)", liveID, kind)
	}

	// ── Case 4: same id on an ONLINE root → plain idempotent upsert (the CP path) ──
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET status = 'online' WHERE id = $1`, foreignID); err != nil {
		t.Fatalf("mark root online: %v", err)
	}
	if err := installPlatformAgent(ctx, conn, foreignID, foreignName, defaultConciergeRuntime); err != nil {
		t.Fatalf("re-installing the SAME id on an online root is the CP's own idempotent "+
			"path and must never conflict; got %v", err)
	}
	if kind, status, _ := readKindStatus(t, foreignID); kind != "platform" || status != "online" {
		t.Fatalf("idempotent re-install disturbed the root: kind=%q status=%q", kind, status)
	}
}
