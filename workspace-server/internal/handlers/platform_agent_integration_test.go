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

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
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
//	Case 3 — the default-runtime install path still seeds 'claude-code', so the
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
	if template != "codex-platform-agent" {
		t.Fatalf("fresh install: template=%q, want %q — the template must map per-runtime (conciergeTemplateForRuntime)", template, "codex-platform-agent")
	}

	// Case 2 (PROVE-FAIL): a re-install (ON CONFLICT path) must NOT revert the
	// runtime back to claude-code, AND the (runtime, template) pair must stay
	// MATCHED. We call the DEFAULT-runtime install path on purpose — even when
	// the re-install requests no specific runtime (empty → default claude-code,
	// which maps to template 'platform-agent'), the existing 'codex' row must be
	// PRESERVED for BOTH columns. Against the pre-P3b `ON CONFLICT ... SET runtime
	// = 'claude-code'` clause the runtime check fails; against head 9e4e5d08
	// (`... SET ... template = $4`, i.e. the REQUESTED runtime's template) the
	// runtime is preserved but the template is silently reverted to
	// 'platform-agent' → a runtime/template MISMATCH (RC 13985). The fix derives
	// the template on conflict from the PRESERVED (existing) runtime, so this
	// case only passes once template stays 'codex-platform-agent'.
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
		t.Fatalf("RC 13985 regression: re-install via the default path desynced the template to %q, "+
			"want %q (the template MATCHING the PRESERVED runtime %q). The ON CONFLICT clause must "+
			"derive `template` from the existing `workspaces.runtime`, never stamp the REQUESTED "+
			"runtime's template — a default reinstall must keep (runtime, template) a matched pair.",
			templateAfterReinstall, wantTemplate, codexRuntime)
	}

	// Case 3: the default-runtime install path seeds 'claude-code' on a FRESH row
	// (backward compatibility for self-host seed + CP callers that send no runtime).
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
	if freshRuntime != "claude-code" {
		t.Fatalf("default install path: runtime=%q, want 'claude-code'", freshRuntime)
	}
	if freshTemplate != "platform-agent" {
		t.Fatalf("default install path: template=%q, want 'platform-agent' (the claude-code concierge keeps the historical template name)", freshTemplate)
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
