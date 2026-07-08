package handlers

// Regression tests for core#3510: /org/import must nest an imported org's
// top-level workspaces UNDER the org's platform agent (the concierge) instead
// of landing them at ROOT (parent_id NULL) as siblings of the platform agent.
//
// The org's platform agent IS the org root — the single kind='platform' AND
// parent_id IS NULL row (platform_agent.go). org_scope.go scopes an org by
// walking the parent_id chain to that one NULL-parent root, so a top-level
// import workspace left at parent_id NULL is treated as its OWN org root
// (sibling of the concierge) rather than part of the concierge's subtree.
//
// The BUG lived in the Import handler's top-level loop, which unconditionally
// passed parent_id = nil. The fix routes that decision through
// planTopLevelImport, which:
//   - WITH a platform agent: parent_id = the platform-agent id, positioned in
//     the same subtree-aware child grid (childSlotInGrid) relative to the
//     platform agent's canvas position.
//   - WITHOUT a platform agent (edge case): falls back to parent_id NULL at the
//     template's own canvas coords (the historical behavior — never break an
//     import on an org that has no concierge).
//
// ORG-SCOPED ANCHOR (core#3510 review, REQUEST_CHANGES): lookupPlatformAgentAnchor
// resolves the concierge by the deterministic, org-scoped PlatformAgentID()
// (DeterministicPlatformAgentID(MOLECULE_ORG_ID) on SaaS, else the fixed
// SelfHostedPlatformAgentID) rather than a bare `kind='platform' AND parent_id
// IS NULL LIMIT 1`. The bare form is multi-root-unsafe: in a DB with more than
// one platform/tenant root it could attach the imported org under an ARBITRARY
// platform root. The primary lookup is id-scoped (WHERE w.id = $anchor ...); only
// when that deterministic id doesn't resolve does the code fall back to the
// structural single platform root (uniq_workspaces_one_platform_root guarantees
// at most one per per-org tenant DB), then to root placement.
//
// These tests exercise planTopLevelImport directly (the SSOT the Import handler
// consumes) so the parent_id contract is asserted deterministically without
// spinning up the full provisioning machinery.

import (
	"context"
	"database/sql"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// Query-fragment regexps (setupTestDB uses the default QueryMatcherRegexp) that
// uniquely identify the two anchor lookups lookupPlatformAgentAnchor issues:
//   - platformAnchorPrimaryRe: the PRIMARY, org-scoped lookup keyed on the
//     deterministic PlatformAgentID() (WHERE w.id = $1 AND w.kind = 'platform'
//     AND w.status != 'removed'). The `w.kind ... AND w.status` adjacency is
//     absent from the fallback (which interposes the parent_id clause), so the
//     fragment matches only the primary.
//   - platformAnchorFallbackRe: the structural FALLBACK on the single
//     kind='platform', parent_id NULL, non-removed root.
const (
	platformAnchorPrimaryRe  = `w.kind = 'platform' AND w.status != 'removed'`
	platformAnchorFallbackRe = `w.parent_id IS NULL AND w.status != 'removed'`
)

// TestPlanTopLevelImport_NestsUnderPlatformAgent asserts that when the org has
// a platform agent, every imported top-level workspace is parented to the
// platform-agent root (parent_id != NULL == the platform id) and positioned as
// a child of it (abs = platform canvas position + the child grid slot; rel =
// the slot itself). This is the core#3510 fix — it FAILS on the old code, which
// passed parent_id = nil for every top-level workspace. The anchor lookup is now
// resolved by the org-scoped deterministic id, which the WithArgs assertion pins.
func TestPlanTopLevelImport_NestsUnderPlatformAgent(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "org-nest-3510")
	mock := setupTestDB(t)

	// The concierge row id is, in every real flow, the deterministic
	// PlatformAgentID() (ensurePlatformAgentFlow / the CP install seed it there).
	anchorID := PlatformAgentID()
	const platX, platY = 500.0, 320.0
	mock.ExpectQuery(platformAnchorPrimaryRe).
		WithArgs(anchorID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "x", "y"}).AddRow(anchorID, platX, platY))

	roots := make([]OrgWorkspace, 2)
	roots[0].Name = "Root A"
	roots[0].Canvas.X, roots[0].Canvas.Y = 100, 100
	roots[1].Name = "Root B"
	roots[1].Canvas.X, roots[1].Canvas.Y = 400, 100

	h := &OrgHandler{}
	slots := h.planTopLevelImport(context.Background(), roots)

	if len(slots) != len(roots) {
		t.Fatalf("planTopLevelImport returned %d slots, want %d", len(slots), len(roots))
	}

	// Reconstruct the expected grid the SAME way recurseChildrenForImport does,
	// so the positioning assertion tracks the shared child-layout helper.
	siblingSizes := make([]nodeSize, len(roots))
	for i, ws := range roots {
		siblingSizes[i] = sizeOfSubtree(ws)
	}

	for i, s := range slots {
		if s.parentID == nil {
			t.Fatalf("slot[%d] (%s): parentID is nil — imported workspace landed at ROOT (the bug); want it nested under the platform agent", i, roots[i].Name)
		}
		if *s.parentID != anchorID {
			t.Errorf("slot[%d] (%s): parentID = %q, want the platform-agent id %q", i, roots[i].Name, *s.parentID, anchorID)
		}
		wantRelX, wantRelY := childSlotInGrid(i, siblingSizes)
		if s.relX != wantRelX || s.relY != wantRelY {
			t.Errorf("slot[%d] (%s): rel = (%v,%v), want the child grid slot (%v,%v)", i, roots[i].Name, s.relX, s.relY, wantRelX, wantRelY)
		}
		if s.absX != platX+wantRelX || s.absY != platY+wantRelY {
			t.Errorf("slot[%d] (%s): abs = (%v,%v), want platform-origin+slot (%v,%v)", i, roots[i].Name, s.absX, s.absY, platX+wantRelX, platY+wantRelY)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestPlanTopLevelImport_TwoPlatformRoots_ResolvesCallerOrgAnchor is the
// core#3510 REQUEST_CHANGES regression: with TWO platform roots present in the DB
// (a multi-root / co-tenant DB), the anchor MUST resolve to the CALLER org's
// concierge — its deterministic PlatformAgentID() — never an arbitrary
// kind='platform' root.
//
// The id-scoped primary lookup selects ONLY the caller's row (the WithArgs
// assertion pins the caller's deterministic id) even though a second, foreign
// platform root exists. If the lookup were the old bare `parent_id IS NULL LIMIT
// 1`, it would carry no id argument and could return the foreign root instead —
// the WithArgs pin is exactly what fails against that unsafe shape. No fallback
// expectation is registered, so the org-scoped primary must resolve on its own;
// reaching the (potentially arbitrary) structural fallback would surface as an
// unexpected query.
func TestPlanTopLevelImport_TwoPlatformRoots_ResolvesCallerOrgAnchor(t *testing.T) {
	t.Setenv("MOLECULE_ORG_ID", "caller-org-A")
	mock := setupTestDB(t)

	callerAnchorID := PlatformAgentID() // DeterministicPlatformAgentID("caller-org-A")
	const foreignRootID = "22222222-2222-2222-2222-222222222222"
	if callerAnchorID == foreignRootID {
		t.Fatalf("test setup: caller anchor id collided with the foreign root id")
	}
	const platX, platY = 140.0, 60.0

	mock.ExpectQuery(platformAnchorPrimaryRe).
		WithArgs(callerAnchorID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "x", "y"}).AddRow(callerAnchorID, platX, platY))

	roots := make([]OrgWorkspace, 2)
	roots[0].Name = "Root A"
	roots[0].Canvas.X, roots[0].Canvas.Y = 100, 100
	roots[1].Name = "Root B"
	roots[1].Canvas.X, roots[1].Canvas.Y = 400, 100

	h := &OrgHandler{}
	slots := h.planTopLevelImport(context.Background(), roots)

	if len(slots) != len(roots) {
		t.Fatalf("planTopLevelImport returned %d slots, want %d", len(slots), len(roots))
	}
	for i, s := range slots {
		if s.parentID == nil {
			t.Fatalf("slot[%d] (%s): parentID is nil — resolved to root instead of the caller's concierge", i, roots[i].Name)
		}
		if *s.parentID == foreignRootID {
			t.Fatalf("slot[%d] (%s): parentID = the FOREIGN platform root %q — anchor was not org-scoped (core#3510)", i, roots[i].Name, foreignRootID)
		}
		if *s.parentID != callerAnchorID {
			t.Errorf("slot[%d] (%s): parentID = %q, want the caller org's platform-agent id %q", i, roots[i].Name, *s.parentID, callerAnchorID)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestPlanTopLevelImport_FallbackToRootWhenNoPlatformAgent pins the preserved
// edge-case behavior: an org with NO platform agent imports its top-level
// workspaces at ROOT (parent_id NULL) at their own template canvas coords, so
// imports never break on a concierge-less org. Both the org-scoped primary
// lookup (by deterministic id) and the structural fallback miss, so the caller
// places the roots at parent_id NULL.
func TestPlanTopLevelImport_FallbackToRootWhenNoPlatformAgent(t *testing.T) {
	mock := setupTestDB(t)

	// Neither the deterministic-id primary nor the structural fallback finds a
	// platform row → the lookup returns found=false and roots stay at parent_id
	// NULL.
	mock.ExpectQuery(platformAnchorPrimaryRe).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(platformAnchorFallbackRe).WillReturnError(sql.ErrNoRows)

	roots := make([]OrgWorkspace, 2)
	roots[0].Name = "Root A"
	roots[0].Canvas.X, roots[0].Canvas.Y = 100, 180
	roots[1].Name = "Root B"
	roots[1].Canvas.X, roots[1].Canvas.Y = 400, 180

	h := &OrgHandler{}
	slots := h.planTopLevelImport(context.Background(), roots)

	if len(slots) != len(roots) {
		t.Fatalf("planTopLevelImport returned %d slots, want %d", len(slots), len(roots))
	}

	for i, s := range slots {
		if s.parentID != nil {
			t.Errorf("slot[%d] (%s): parentID = %q, want nil (root) — no platform agent present", i, roots[i].Name, *s.parentID)
		}
		if s.absX != roots[i].Canvas.X || s.absY != roots[i].Canvas.Y {
			t.Errorf("slot[%d] (%s): abs = (%v,%v), want the template canvas coords (%v,%v)", i, roots[i].Name, s.absX, s.absY, roots[i].Canvas.X, roots[i].Canvas.Y)
		}
		// A root's relative coords equal its absolute (no parent to be relative to).
		if s.relX != roots[i].Canvas.X || s.relY != roots[i].Canvas.Y {
			t.Errorf("slot[%d] (%s): rel = (%v,%v), want == abs/template coords (%v,%v)", i, roots[i].Name, s.relX, s.relY, roots[i].Canvas.X, roots[i].Canvas.Y)
		}
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}
