//go:build integration
// +build integration

// workspace_reparent_integration_test.go — REAL Postgres gate for the
// parent_id guards on PATCH /workspaces/:id (client issue #5074).
//
// Run with:
//
//	INTEGRATION_DB_URL="postgres://postgres:test@localhost:55474/molecule?sslmode=disable" \
//	  go test -tags=integration ./internal/handlers/ -run TestIntegration_WorkspaceReparent -v
//
// CI: the handlers-postgres-integration workflow selects `-run ^TestIntegration_`
// with `-tags=integration` against a per-run Postgres with the full migration
// chain applied.
//
// WHY THIS IS NOT A SQLMOCK TEST
// ------------------------------
// Every assertion that matters here is about the ROW STATE AFTER the handler
// returns, and about what a SECOND, INDEPENDENT component (the memory
// namespace resolver) derives from that state. sqlmock can only prove "a
// statement of shape X fired against a canned row"; it cannot prove that a
// rejected move left parent_id untouched, because a guard that returns 422
// and a guard that returns 422 AFTER corrupting the row look identical to it.
// It also cannot execute a RECURSIVE CTE, which is the entire cycle and
// org-root mechanism. The negative controls below re-SELECT parent_id and
// re-run the real resolver.
//
// NON-VACUITY
// -----------
// Each rejection test is paired with an ACCEPTANCE test that differs in
// EXACTLY ONE input and travels the SAME code path (handler Update ->
// applyReparent):
//
//	cycle:     move M under S (S is M's child)      -> 422
//	           move M under C (C is not M's child)  -> 200      [one field: target]
//	cross-org: move S under a child of org root 2   -> 422
//	           move S under a child of org root 1   -> 200      [one field: target]
//
// So neither leg can pass by matching nothing: if the guard were deleted the
// 422 legs fail, and if the guard over-fired the 200 legs fail.
//
// NOT SAFE FOR t.Parallel() — these tests seed and clear shared tables and
// rebind the process-global db.DB.

package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/memory/namespace"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

const reparentTestPrefix = "itest-reparent-"

// integrationDB_Reparent opens the integration Postgres, rebinds the
// process-global db.DB (which the Update handler and applyReparent both read
// through), and clears this suite's rows on entry and exit.
//
// The slate-clear deletes LEAF-FIRST in a loop. workspaces.parent_id has no
// ON DELETE CASCADE onto itself, so a flat DELETE trips the self-FK depending
// on row order. The obvious alternative — NULL every parent_id first, then
// delete — trips workspaces_parent_name_uniq instead, because this suite
// deliberately seeds two workspaces with the SAME name under DIFFERENT
// parents (the name-conflict case), and collapsing both to parent_id NULL
// makes them collide. Deleting childless rows repeatedly avoids both.
func integrationDB_Reparent(t *testing.T) *sql.DB {
	t.Helper()
	url := requireIntegrationDBURL(t)
	conn, err := sql.Open("postgres", url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}

	clear := func() {
		if _, err := conn.ExecContext(context.Background(), `
			DO $$
			BEGIN
				LOOP
					DELETE FROM workspaces w
					WHERE w.name LIKE '`+reparentTestPrefix+`%'
					  AND NOT EXISTS (SELECT 1 FROM workspaces c WHERE c.parent_id = w.id);
					EXIT WHEN NOT FOUND;
				END LOOP;
			END $$;
		`); err != nil {
			t.Fatalf("clear slate: %v", err)
		}
		// Anything left means a cycle survived the suite (only reachable via
		// the deliberately-corrupt seed in the unwalkable-tree test). Break it
		// and finish, rather than leaking rows into the next suite.
		var left int
		if err := conn.QueryRowContext(context.Background(),
			`SELECT count(*) FROM workspaces WHERE name LIKE '`+reparentTestPrefix+`%'`).Scan(&left); err != nil {
			t.Fatalf("count leftovers: %v", err)
		}
		if left > 0 {
			if _, err := conn.ExecContext(context.Background(), `
				UPDATE workspaces SET parent_id = NULL
				WHERE name LIKE '`+reparentTestPrefix+`%'
				  AND id = (SELECT id FROM workspaces WHERE name LIKE '`+reparentTestPrefix+`%' LIMIT 1);
			`); err != nil {
				t.Fatalf("break residual cycle: %v", err)
			}
			if _, err := conn.ExecContext(context.Background(), `
				DO $$
				BEGIN
					LOOP
						DELETE FROM workspaces w
						WHERE w.name LIKE '`+reparentTestPrefix+`%'
						  AND NOT EXISTS (SELECT 1 FROM workspaces c WHERE c.parent_id = w.id);
						EXIT WHEN NOT FOUND;
					END LOOP;
				END $$;
			`); err != nil {
				t.Fatalf("clear slate (second pass): %v", err)
			}
		}
	}
	clear()

	prev := db.DB
	db.DB = conn
	t.Cleanup(func() {
		clear()
		db.DB = prev
		conn.Close()
	})
	return conn
}

// seedWS inserts one workspace row and returns its id. parent == "" means an
// org root (parent_id NULL).
func seedWS(t *testing.T, conn *sql.DB, label, parent string) string {
	t.Helper()
	id := uuid.NewString()
	var parentArg any
	if parent != "" {
		parentArg = parent
	}
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO workspaces (id, name, parent_id, status) VALUES ($1, $2, $3, 'online')`,
		id, reparentTestPrefix+label, parentArg,
	); err != nil {
		t.Fatalf("seed %s: %v", label, err)
	}
	return id
}

// parentOf re-reads parent_id straight from the table. This is the assertion
// that makes the negative controls real: a guard that returned 422 but still
// wrote would be caught here and nowhere else.
func parentOf(t *testing.T, conn *sql.DB, id string) string {
	t.Helper()
	var p sql.NullString
	if err := conn.QueryRowContext(context.Background(),
		`SELECT parent_id::text FROM workspaces WHERE id = $1`, id).Scan(&p); err != nil {
		t.Fatalf("read parent_id of %s: %v", id, err)
	}
	if !p.Valid {
		return ""
	}
	return p.String
}

// doPatch_Workspace fires PATCH /workspaces/:id through the REAL Update
// handler with an admin-token credential class — the same class
// callerCanEditWorkspaceInfrastructure requires for parent_id.
func doPatch_Workspace(t *testing.T, workspaceID, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: workspaceID}}
	c.Set("caller_credential_class", "admin-token")
	c.Set("caller_is_admin_token", true)
	c.Request = httptest.NewRequest("PATCH", "/workspaces/"+workspaceID, bytes.NewReader([]byte(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	h := &WorkspaceHandler{}
	h.Update(c)
	return w
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return out
}

// nsNames renders the resolver's readable set as "name(writable)" strings, so
// a comparison catches BOTH a namespace appearing/disappearing AND a
// read-only namespace silently becoming writable.
func nsNames(t *testing.T, conn *sql.DB, workspaceID string) []string {
	t.Helper()
	r := namespace.New(conn)
	list, err := r.ReadableNamespaces(context.Background(), workspaceID)
	if err != nil {
		t.Fatalf("ReadableNamespaces(%s): %v", workspaceID, err)
	}
	out := make([]string, 0, len(list))
	for _, ns := range list {
		out = append(out, fmt.Sprintf("%s(writable=%v)", ns.Name, ns.Writable))
	}
	return out
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 1. The client's actual request (#5074): insert an intermediate team node.
// ---------------------------------------------------------------------------

// TestIntegration_WorkspaceReparent_LegitimateMoveIsAcceptedAndOrgIsInvariant
// builds the exact tree from issue #5074 and performs the exact move asked
// for: SEO Agent moves from the org root to a newly created Marketing Team
// node under the same root.
//
// Asserts the move lands, the workspace id is UNCHANGED (the whole point —
// delete-and-recreate was changing it), and — the load-bearing part — that
// the memory namespace resolver reports the SAME org namespace before and
// after, with exactly the team namespace swapped.
func TestIntegration_WorkspaceReparent_LegitimateMoveIsAcceptedAndOrgIsInvariant(t *testing.T) {
	conn := integrationDB_Reparent(t)

	root := seedWS(t, conn, "reno-root", "")
	coordinator := seedWS(t, conn, "coordinator", root)
	seo := seedWS(t, conn, "seo-agent", root)
	marketing := seedWS(t, conn, "marketing-team", root)
	_ = coordinator

	before := nsNames(t, conn, seo)

	w := doPatch_Workspace(t, seo, `{"parent_id":"`+marketing+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("legitimate move: status=%d body=%s", w.Code, w.Body.String())
	}
	if got := parentOf(t, conn, seo); got != marketing {
		t.Fatalf("parent_id=%q after move, want %q", got, marketing)
	}

	body := decodeBody(t, w)
	rp, ok := body["reparented"].(map[string]any)
	if !ok {
		t.Fatalf("response has no `reparented` block: %s", w.Body.String())
	}
	if rp["old_parent_id"] != root || rp["new_parent_id"] != marketing || rp["org_root_id"] != root {
		t.Fatalf("reparented block wrong: %#v", rp)
	}
	// The delta must be stated, not implied — this is the retroactive-read
	// disclosure the handler exists to make explicit.
	lost, _ := rp["namespaces_lost"].([]any)
	gained, _ := rp["namespaces_gained"].([]any)
	if len(lost) != 1 || lost[0] != "team:"+root {
		t.Errorf("namespaces_lost=%#v, want [team:%s]", lost, root)
	}
	if len(gained) != 1 || gained[0] != "team:"+marketing {
		t.Errorf("namespaces_gained=%#v, want [team:%s]", gained, marketing)
	}
	if rp["memories_migrated"] != false {
		t.Errorf("memories_migrated=%#v, want false", rp["memories_migrated"])
	}

	after := nsNames(t, conn, seo)

	// The resolver is a separate package reading the same table. What it says
	// IS the privilege effect of the move.
	wantBefore := []string{
		"workspace:" + seo + "(writable=true)",
		"team:" + root + "(writable=true)",
		"org:" + root + "(writable=false)",
	}
	wantAfter := []string{
		"workspace:" + seo + "(writable=true)",
		"team:" + marketing + "(writable=true)",
		"org:" + root + "(writable=false)",
	}
	if !sameStrings(before, wantBefore) {
		t.Fatalf("namespaces BEFORE = %v, want %v", before, wantBefore)
	}
	if !sameStrings(after, wantAfter) {
		t.Fatalf("namespaces AFTER = %v, want %v", after, wantAfter)
	}
	// Explicit restatement of the invariant the whole design rests on.
	if before[2] != after[2] {
		t.Fatalf("org namespace CHANGED across an allowed move: %q -> %q", before[2], after[2])
	}

	// The subtree below an unmoved node must be untouched: coordinator keeps
	// team:root and org:root.
	coordAfter := nsNames(t, conn, coordinator)
	wantCoord := []string{
		"workspace:" + coordinator + "(writable=true)",
		"team:" + root + "(writable=true)",
		"org:" + root + "(writable=false)",
	}
	if !sameStrings(coordAfter, wantCoord) {
		t.Fatalf("sibling namespaces changed: %v, want %v", coordAfter, wantCoord)
	}
}

// ---------------------------------------------------------------------------
// 2. Cycle prevention — negative control varying exactly one input.
// ---------------------------------------------------------------------------

// TestIntegration_WorkspaceReparent_CycleRejected drives three cycle shapes
// and one acceptance leg through the same handler. The acceptance leg differs
// from the transitive-cycle leg in the TARGET PARENT ONLY.
func TestIntegration_WorkspaceReparent_CycleRejected(t *testing.T) {
	conn := integrationDB_Reparent(t)

	// root
	//  ├── coordinator
	//  └── marketing
	//        └── seo
	//              └── seo-junior
	root := seedWS(t, conn, "root", "")
	coordinator := seedWS(t, conn, "coordinator", root)
	marketing := seedWS(t, conn, "marketing", root)
	seo := seedWS(t, conn, "seo", marketing)
	junior := seedWS(t, conn, "seo-junior", seo)

	t.Run("self_parent", func(t *testing.T) {
		w := doPatch_Workspace(t, marketing, `{"parent_id":"`+marketing+`"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d body=%s, want 422", w.Code, w.Body.String())
		}
		if code := decodeBody(t, w)["code"]; code != reparentCodeSelf {
			t.Errorf("code=%v, want %s", code, reparentCodeSelf)
		}
		if got := parentOf(t, conn, marketing); got != root {
			t.Fatalf("parent_id MUTATED to %q on a rejected self-parent (want %q)", got, root)
		}
	})

	t.Run("direct_cycle_parent_under_own_child", func(t *testing.T) {
		// marketing -> child of seo, where seo is marketing's child.
		w := doPatch_Workspace(t, marketing, `{"parent_id":"`+seo+`"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d body=%s, want 422", w.Code, w.Body.String())
		}
		if code := decodeBody(t, w)["code"]; code != reparentCodeCycle {
			t.Errorf("code=%v, want %s", code, reparentCodeCycle)
		}
		if got := parentOf(t, conn, marketing); got != root {
			t.Fatalf("parent_id MUTATED to %q on a rejected cycle (want %q)", got, root)
		}
	})

	t.Run("transitive_cycle_two_levels_down", func(t *testing.T) {
		// marketing -> child of junior, which is marketing's GRANDchild.
		w := doPatch_Workspace(t, marketing, `{"parent_id":"`+junior+`"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d body=%s, want 422", w.Code, w.Body.String())
		}
		if code := decodeBody(t, w)["code"]; code != reparentCodeCycle {
			t.Errorf("code=%v, want %s", code, reparentCodeCycle)
		}
		if got := parentOf(t, conn, marketing); got != root {
			t.Fatalf("parent_id MUTATED to %q on a rejected transitive cycle (want %q)", got, root)
		}
	})

	// NEGATIVE CONTROL: identical request, identical code path, ONE field
	// different — the target is a node OUTSIDE marketing's subtree. Must be
	// ACCEPTED. If the cycle guard were "reject every move" this fails.
	t.Run("non_descendant_target_is_accepted", func(t *testing.T) {
		w := doPatch_Workspace(t, marketing, `{"parent_id":"`+coordinator+`"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200 — the cycle guard is over-firing", w.Code, w.Body.String())
		}
		if got := parentOf(t, conn, marketing); got != coordinator {
			t.Fatalf("parent_id=%q, want %q", got, coordinator)
		}
		// And the tree is still walkable: the resolver must terminate at the
		// unchanged root for the deepest node.
		got := nsNames(t, conn, junior)
		want := []string{
			"workspace:" + junior + "(writable=true)",
			"team:" + seo + "(writable=true)",
			"org:" + root + "(writable=false)",
		}
		if !sameStrings(got, want) {
			t.Fatalf("deep-node namespaces after legal move = %v, want %v", got, want)
		}
	})
}

// TestIntegration_WorkspaceReparent_CycleGuardPreventsAnUnwalkableTree is the
// reason the cycle guard is load-bearing rather than tidy.
//
// org_scope.go's orgRootSubtreeCTE — the primitive sameOrg() and therefore
// a2a routing, peer discovery, broadcast and org-token auth are built on —
// walks parent_id with NO depth bound. This test writes a 2-cycle DIRECTLY to
// the table (bypassing the handler, i.e. reproducing what the pre-fix
// unguarded UPDATE could commit) and shows that CTE does not terminate, then
// shows the handler refuses to create that state.
func TestIntegration_WorkspaceReparent_CycleGuardPreventsAnUnwalkableTree(t *testing.T) {
	conn := integrationDB_Reparent(t)

	root := seedWS(t, conn, "root", "")
	a := seedWS(t, conn, "node-a", root)
	b := seedWS(t, conn, "node-b", a)

	// Reproduce the pre-fix write: a -> child of b, straight to SQL.
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE workspaces SET parent_id = $2 WHERE id = $1`, a, b); err != nil {
		t.Fatalf("seed cycle: %v", err)
	}

	// The unbounded CTE cannot resolve this. Bounded by statement_timeout so
	// the test terminates; without the timeout it runs until the backend is
	// out of memory.
	txCtx := context.Background()
	tx, err := conn.BeginTx(txCtx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(txCtx, `SET LOCAL statement_timeout = '3s'`); err != nil {
		t.Fatalf("set timeout: %v", err)
	}
	var unusedRoot string
	err = tx.QueryRowContext(txCtx, orgRootSubtreeCTE, a).Scan(&unusedRoot)
	if err == nil {
		t.Fatalf("orgRootSubtreeCTE RESOLVED a cycle to %q — this test's premise is stale; "+
			"if a cycle guard was added to that CTE, re-point this assertion at it", unusedRoot)
	}
	t.Logf("confirmed: unbounded org-root CTE does not terminate on a cycle (%v)", err)

	// The bounded walk this change introduced terminates and fails CLOSED.
	tx2, err := conn.BeginTx(txCtx, nil)
	if err != nil {
		t.Fatalf("begin2: %v", err)
	}
	defer func() { _ = tx2.Rollback() }()
	if _, err := tx2.ExecContext(txCtx, `SET LOCAL statement_timeout = '10s'`); err != nil {
		t.Fatalf("set timeout2: %v", err)
	}
	gotRoot, ok, err := reparentOrgRoot(txCtx, tx2, a)
	if err != nil {
		t.Fatalf("reparentOrgRoot errored on a cycle instead of returning ok=false: %v", err)
	}
	if ok {
		t.Fatalf("reparentOrgRoot resolved a cycle to %q; it must fail closed", gotRoot)
	}

	// Repair, then prove the handler refuses to recreate the cycle.
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE workspaces SET parent_id = $2 WHERE id = $1`, a, root); err != nil {
		t.Fatalf("repair: %v", err)
	}
	w := doPatch_Workspace(t, a, `{"parent_id":"`+b+`"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("handler status=%d body=%s, want 422", w.Code, w.Body.String())
	}
	if got := parentOf(t, conn, a); got != root {
		t.Fatalf("parent_id MUTATED to %q; the handler committed the cycle the CTE cannot walk", got)
	}
}

// ---------------------------------------------------------------------------
// 3. Cross-org — negative control varying exactly one input.
// ---------------------------------------------------------------------------

// TestIntegration_WorkspaceReparent_CrossOrgRejected seeds TWO org roots in
// one datastore (the multi-org shape middleware/wsauth_middleware.go and
// org_scope.go both defend against) and proves a workspace cannot be moved
// between them, in either direction, nor by demoting a root.
//
// The acceptance leg differs from the rejection leg in the TARGET PARENT
// ONLY — same handler, same body shape, same credential.
func TestIntegration_WorkspaceReparent_CrossOrgRejected(t *testing.T) {
	conn := integrationDB_Reparent(t)

	// Org 1                       Org 2
	//   root1                       root2
	//    ├── team1                   └── team2
	//    │     └── seo
	//    └── team1b
	root1 := seedWS(t, conn, "root1", "")
	team1 := seedWS(t, conn, "team1", root1)
	team1b := seedWS(t, conn, "team1b", root1)
	seo := seedWS(t, conn, "seo", team1)
	root2 := seedWS(t, conn, "root2", "")
	team2 := seedWS(t, conn, "team2", root2)

	before := nsNames(t, conn, seo)

	t.Run("move_into_a_foreign_org_is_rejected", func(t *testing.T) {
		w := doPatch_Workspace(t, seo, `{"parent_id":"`+team2+`"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d body=%s, want 422", w.Code, w.Body.String())
		}
		body := decodeBody(t, w)
		if body["code"] != reparentCodeCrossOrg {
			t.Errorf("code=%v, want %s", body["code"], reparentCodeCrossOrg)
		}
		if body["org_root_id"] != root1 || body["target_org_root_id"] != root2 {
			t.Errorf("rejection did not name both roots: %#v", body)
		}
		if got := parentOf(t, conn, seo); got != team1 {
			t.Fatalf("parent_id MUTATED to %q on a rejected cross-org move (want %q)", got, team1)
		}
		// THE ESCALATION PROOF: the resolver's output must be byte-identical.
		// A successful cross-org move would have swapped org:root1 for
		// org:root2 — retroactive read of another org's entire memory corpus.
		after := nsNames(t, conn, seo)
		if !sameStrings(before, after) {
			t.Fatalf("namespaces CHANGED across a REJECTED cross-org move: %v -> %v", before, after)
		}
		if after[2] != "org:"+root1+"(writable=false)" {
			t.Fatalf("org namespace = %q, want org:%s(writable=false)", after[2], root1)
		}
	})

	t.Run("adopting_a_foreign_root_is_rejected", func(t *testing.T) {
		// The other direction: pull root2 (and its whole subtree) into org 1.
		w := doPatch_Workspace(t, root2, `{"parent_id":"`+team1+`"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d body=%s, want 422", w.Code, w.Body.String())
		}
		if code := decodeBody(t, w)["code"]; code != reparentCodeCrossOrg {
			t.Errorf("code=%v, want %s", code, reparentCodeCrossOrg)
		}
		if got := parentOf(t, conn, root2); got != "" {
			t.Fatalf("root2.parent_id MUTATED to %q; an org root was demoted into another org", got)
		}
		// team2 must still resolve to org 2.
		got := nsNames(t, conn, team2)
		if got[2] != "org:"+root2+"(writable=false)" {
			t.Fatalf("team2 org namespace = %q, want org:%s — a subtree changed org", got[2], root2)
		}
	})

	// NEGATIVE CONTROL: identical shape, ONE field different — the target is
	// in the SAME org. Must be ACCEPTED.
	t.Run("same_org_target_is_accepted", func(t *testing.T) {
		w := doPatch_Workspace(t, seo, `{"parent_id":"`+team1b+`"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s, want 200 — the cross-org guard is over-firing", w.Code, w.Body.String())
		}
		if got := parentOf(t, conn, seo); got != team1b {
			t.Fatalf("parent_id=%q, want %q", got, team1b)
		}
		after := nsNames(t, conn, seo)
		if after[1] != "team:"+team1b+"(writable=true)" {
			t.Fatalf("team namespace = %q, want team:%s", after[1], team1b)
		}
		if after[2] != before[2] {
			t.Fatalf("org namespace changed on an ALLOWED same-org move: %q -> %q", before[2], after[2])
		}
	})
}

// ---------------------------------------------------------------------------
// 4. parent_id: null — promote to org root.
// ---------------------------------------------------------------------------

// TestIntegration_WorkspaceReparent_RootPromotionRejected covers the
// `null = promote to org root` case the issue asked for by name.
//
// It is refused because it is not an org-chart edit: it MINTS A NEW ORG. The
// node and its whole subtree leave their org, lose org:<oldRoot>, and the
// node gains WRITE on the org namespace — which resolver.go grants to roots
// only. The sub-tests prove all three consequences would have followed.
func TestIntegration_WorkspaceReparent_RootPromotionRejected(t *testing.T) {
	conn := integrationDB_Reparent(t)

	root := seedWS(t, conn, "root", "")
	team := seedWS(t, conn, "team", root)
	child := seedWS(t, conn, "child", team)

	teamBefore := nsNames(t, conn, team)
	childBefore := nsNames(t, conn, child)

	w := doPatch_Workspace(t, team, `{"parent_id":null}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s, want 422", w.Code, w.Body.String())
	}
	body := decodeBody(t, w)
	if body["code"] != reparentCodeRootPromotion {
		t.Errorf("code=%v, want %s", body["code"], reparentCodeRootPromotion)
	}
	if got := parentOf(t, conn, team); got != root {
		t.Fatalf("parent_id MUTATED to %q on a rejected root promotion (want %q)", got, root)
	}
	if !sameStrings(teamBefore, nsNames(t, conn, team)) {
		t.Fatalf("team namespaces changed across a rejected promotion")
	}
	// The subtree consequence: child must still be in the original org.
	childAfter := nsNames(t, conn, child)
	if !sameStrings(childBefore, childAfter) {
		t.Fatalf("DESCENDANT namespaces changed across a rejected promotion: %v -> %v", childBefore, childAfter)
	}
	if childAfter[2] != "org:"+root+"(writable=false)" {
		t.Fatalf("child org namespace = %q, want org:%s", childAfter[2], root)
	}

	// Idempotent leg: sending null for a workspace that is ALREADY a root is
	// a no-op, not an error. Differs from the rejected leg in ONE input — the
	// target workspace's current parent.
	wRoot := doPatch_Workspace(t, root, `{"parent_id":null}`)
	if wRoot.Code != http.StatusOK {
		t.Fatalf("null on an existing root: status=%d body=%s, want 200", wRoot.Code, wRoot.Body.String())
	}
	if _, changed := decodeBody(t, wRoot)["reparented"]; changed {
		t.Errorf("no-op null reported a reparent: %s", wRoot.Body.String())
	}
	if got := parentOf(t, conn, root); got != "" {
		t.Fatalf("root.parent_id = %q after a no-op, want NULL", got)
	}
}

// ---------------------------------------------------------------------------
// 5. Remaining input guards.
// ---------------------------------------------------------------------------

func TestIntegration_WorkspaceReparent_InputGuards(t *testing.T) {
	conn := integrationDB_Reparent(t)

	root := seedWS(t, conn, "root", "")
	team := seedWS(t, conn, "team", root)
	other := seedWS(t, conn, "other", root)

	t.Run("nonexistent_parent", func(t *testing.T) {
		ghost := uuid.NewString()
		w := doPatch_Workspace(t, team, `{"parent_id":"`+ghost+`"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d body=%s, want 422", w.Code, w.Body.String())
		}
		if code := decodeBody(t, w)["code"]; code != reparentCodeNotFound {
			t.Errorf("code=%v, want %s", code, reparentCodeNotFound)
		}
		if got := parentOf(t, conn, team); got != root {
			t.Fatalf("parent_id MUTATED to %q", got)
		}
	})

	t.Run("removed_parent", func(t *testing.T) {
		dead := seedWS(t, conn, "dead-parent", root)
		if _, err := conn.ExecContext(context.Background(),
			`UPDATE workspaces SET status = 'removed' WHERE id = $1`, dead); err != nil {
			t.Fatalf("mark removed: %v", err)
		}
		w := doPatch_Workspace(t, team, `{"parent_id":"`+dead+`"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d body=%s, want 422", w.Code, w.Body.String())
		}
		if got := parentOf(t, conn, team); got != root {
			t.Fatalf("parent_id MUTATED to %q", got)
		}
	})

	t.Run("non_uuid_parent", func(t *testing.T) {
		w := doPatch_Workspace(t, team, `{"parent_id":"not-a-uuid"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s, want 400", w.Code, w.Body.String())
		}
		if got := parentOf(t, conn, team); got != root {
			t.Fatalf("parent_id MUTATED to %q", got)
		}
	})

	t.Run("non_string_parent", func(t *testing.T) {
		// Pre-fix this went straight into the driver, errored, was swallowed
		// by log.Printf, and answered 200 {"status":"updated"}.
		w := doPatch_Workspace(t, team, `{"parent_id":12345}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s, want 400 (pre-fix this was a swallowed 200)", w.Code, w.Body.String())
		}
	})

	t.Run("name_conflict_under_new_parent", func(t *testing.T) {
		// workspaces_parent_name_uniq is UNIQUE (COALESCE(parent_id,zero), name)
		// WHERE status != 'removed'. Two workspaces may share a name under
		// DIFFERENT parents; moving one under the other's parent collides.
		// Pre-fix that unique violation was swallowed by log.Printf and the
		// handler answered 200 {"status":"updated"} on a workspace that had
		// not moved — a false success, not a rejection.
		incumbent := seedWS(t, conn, "collide", other)
		mover := seedWS(t, conn, "collide", root) // same name, different parent: legal
		if incumbent == mover {
			t.Fatal("seed produced identical ids")
		}

		w := doPatch_Workspace(t, mover, `{"parent_id":"`+other+`"}`)
		if w.Code != http.StatusConflict {
			t.Fatalf("status=%d body=%s, want 409 (pre-fix this was a swallowed 200)", w.Code, w.Body.String())
		}
		if code := decodeBody(t, w)["code"]; code != reparentCodeNameConflict {
			t.Errorf("code=%v, want %s", code, reparentCodeNameConflict)
		}
		if got := parentOf(t, conn, mover); got != root {
			t.Fatalf("parent_id MUTATED to %q on a rejected name conflict (want %q)", got, root)
		}
		// NEGATIVE CONTROL: same mover, same handler, ONE field different —
		// a destination with no name clash. Must be ACCEPTED.
		free := seedWS(t, conn, "free-parent", root)
		w2 := doPatch_Workspace(t, mover, `{"parent_id":"`+free+`"}`)
		if w2.Code != http.StatusOK {
			t.Fatalf("clash-free move: status=%d body=%s, want 200", w2.Code, w2.Body.String())
		}
		if got := parentOf(t, conn, mover); got != free {
			t.Fatalf("parent_id=%q, want %q", got, free)
		}
	})

	t.Run("rename_plus_move_is_rejected_as_ambiguous", func(t *testing.T) {
		w := doPatch_Workspace(t, team, `{"parent_id":"`+other+`","name":"`+reparentTestPrefix+`renamed"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s, want 400", w.Code, w.Body.String())
		}
		if code := decodeBody(t, w)["code"]; code != reparentCodeAmbiguous {
			t.Errorf("code=%v, want %s", code, reparentCodeAmbiguous)
		}
		// NOTHING may have been written — not the move, and not the rename.
		if got := parentOf(t, conn, team); got != root {
			t.Fatalf("parent_id MUTATED to %q on a rejected combined patch", got)
		}
		var name string
		if err := conn.QueryRowContext(context.Background(),
			`SELECT name FROM workspaces WHERE id = $1`, team).Scan(&name); err != nil {
			t.Fatalf("read name: %v", err)
		}
		if name != reparentTestPrefix+"team" {
			t.Fatalf("name MUTATED to %q on a rejected combined patch", name)
		}
	})
}

// ---------------------------------------------------------------------------
// 6. The org plugin allowlist precondition.
// ---------------------------------------------------------------------------

// TestIntegration_WorkspaceReparent_AllowlistAppliesAtDepthTwo pins the
// org_plugin_allowlist fix that had to land WITH re-parenting.
//
// resolveOrgID used to return the DIRECT PARENT. At depth 1 that IS the org
// root, so the bug was invisible. Inserting an intermediate team node — the
// entire point of issue #5074 — puts workspaces at depth 2, where the old
// derivation resolved to the team node, found zero allowlist rows for it, and
// checkOrgPluginAllowlist's "no allowlist configured" arm let EVERY plugin
// through. Granting the re-parent request without this fix would have
// silently disabled plugin governance for the moved subtree.
//
// RED WITHOUT THE FIX: the depth-2 leg returns blocked=false.
func TestIntegration_WorkspaceReparent_AllowlistAppliesAtDepthTwo(t *testing.T) {
	conn := integrationDB_Reparent(t)

	root := seedWS(t, conn, "root", "")
	team := seedWS(t, conn, "team", root)
	deep := seedWS(t, conn, "deep-child", team)
	shallow := seedWS(t, conn, "shallow-child", root)

	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM org_plugin_allowlist WHERE org_id = $1`, root); err != nil {
		t.Fatalf("clear allowlist: %v", err)
	}
	// The org allows exactly one plugin.
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO org_plugin_allowlist (org_id, plugin_name, enabled_by) VALUES ($1, 'approved-plugin', $2)`,
		root, root); err != nil {
		t.Fatalf("seed allowlist: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(),
			`DELETE FROM org_plugin_allowlist WHERE org_id = $1`, root)
	})

	ctx := context.Background()

	// Depth 1 — worked before and must keep working (no regression).
	if blocked, _ := checkOrgPluginAllowlist(ctx, shallow, "rogue-plugin"); !blocked {
		t.Errorf("depth-1 workspace: rogue plugin NOT blocked — allowlist regressed")
	}
	if blocked, _ := checkOrgPluginAllowlist(ctx, shallow, "approved-plugin"); blocked {
		t.Errorf("depth-1 workspace: approved plugin was blocked")
	}

	// Depth 2 — the leg that was silently allow-all before the fix.
	if blocked, reason := checkOrgPluginAllowlist(ctx, deep, "rogue-plugin"); !blocked {
		t.Errorf("depth-2 workspace: rogue plugin NOT blocked (reason=%q) — "+
			"resolveOrgID resolved the org to the team node instead of the org root, "+
			"so the allowlist was bypassed for every workspace below an intermediate team", reason)
	}
	// Negative control: same call, same depth, ONE field different (the
	// plugin name). Must be ALLOWED — proving the guard is discriminating,
	// not blanket-denying.
	if blocked, reason := checkOrgPluginAllowlist(ctx, deep, "approved-plugin"); blocked {
		t.Errorf("depth-2 workspace: approved plugin was blocked (reason=%q)", reason)
	}

	// The org root itself resolves to itself.
	if blocked, _ := checkOrgPluginAllowlist(ctx, root, "rogue-plugin"); !blocked {
		t.Errorf("org root: rogue plugin NOT blocked")
	}
}
