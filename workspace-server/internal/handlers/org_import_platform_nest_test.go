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
// These tests exercise planTopLevelImport directly (the SSOT the Import handler
// consumes) so the parent_id contract is asserted deterministically without
// spinning up the full provisioning machinery. The WITH-platform assertion
// (parentID != nil AND == the platform id) FAILS against the old behavior,
// where the parent was always nil; the WITHOUT-platform assertion pins the
// preserved fallback.

import (
	"context"
	"database/sql"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// platformAnchorQueryRe is a regexp fragment (setupTestDB uses the default
// QueryMatcherRegexp) that uniquely identifies the platform-agent anchor
// lookup issued by lookupPlatformAgentAnchor.
const platformAnchorQueryRe = `WHERE w.kind = 'platform' AND w.parent_id IS NULL`

// TestPlanTopLevelImport_NestsUnderPlatformAgent asserts that when the org has
// a platform agent, every imported top-level workspace is parented to the
// platform-agent root (parent_id != NULL == the platform id) and positioned as
// a child of it (abs = platform canvas position + the child grid slot; rel =
// the slot itself). This is the core#3510 fix — it FAILS on the old code, which
// passed parent_id = nil for every top-level workspace.
func TestPlanTopLevelImport_NestsUnderPlatformAgent(t *testing.T) {
	mock := setupTestDB(t)

	const platformID = "11111111-1111-1111-1111-111111111111"
	const platX, platY = 500.0, 320.0
	mock.ExpectQuery(platformAnchorQueryRe).
		WillReturnRows(sqlmock.NewRows([]string{"id", "x", "y"}).AddRow(platformID, platX, platY))

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
		if *s.parentID != platformID {
			t.Errorf("slot[%d] (%s): parentID = %q, want the platform-agent id %q", i, roots[i].Name, *s.parentID, platformID)
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

// TestPlanTopLevelImport_FallbackToRootWhenNoPlatformAgent pins the preserved
// edge-case behavior: an org with NO platform agent imports its top-level
// workspaces at ROOT (parent_id NULL) at their own template canvas coords, so
// imports never break on a concierge-less org.
func TestPlanTopLevelImport_FallbackToRootWhenNoPlatformAgent(t *testing.T) {
	mock := setupTestDB(t)

	// No platform-agent row → the lookup gets sql.ErrNoRows.
	mock.ExpectQuery(platformAnchorQueryRe).WillReturnError(sql.ErrNoRows)

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
