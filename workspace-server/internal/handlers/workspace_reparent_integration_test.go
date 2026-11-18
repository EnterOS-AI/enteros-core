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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/memory/namespace"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
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

	// orgRootSubtreeCTE is now DEPTH-BOUNDED (orgRootMaxChainDepth). Before
	// that bound it did not terminate here at all — it ran until the statement
	// was cancelled. It must now return NO ROW promptly, i.e. fail closed:
	// "this chain has no resolvable org root" rather than a wrong answer or a
	// hang. statement_timeout stays as a tripwire — if the bound were ever
	// removed, this asserts a timeout error instead of ErrNoRows and fails.
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
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("orgRootSubtreeCTE on a cycle: root=%q err=%v — want sql.ErrNoRows. "+
			"A timeout here means the depth bound was removed; a nil error means it resolved a cycle to a bogus root.",
			unusedRoot, err)
	}
	t.Logf("confirmed: bounded org-root CTE fails CLOSED on a cycle (%v)", err)

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

// TestIntegration_WorkspaceReparent_ConcurrentDisjointMovesCannotCreateACycle
// is the concurrency test that actually exercises the mechanism.
//
// The sibling test below (A→B, B→A) is NOT evidence: both transactions lock the
// IDENTICAL set {A,B}, so an id-ordered FOR UPDATE serialises them trivially and
// the test would pass even with no protection at all beyond that ordering. It is
// kept as a regression case, not as proof.
//
// This one uses DISJOINT lock sets, which is the shape that breaks a
// lock-the-two-endpoints design:
//
//	R ─┬─ A ── A2
//	   └─ B ── B2
//
//	T1: A.parent = B2   locks {A, B2}
//	T2: B.parent = A2   locks {B, A2}
//
// The two sets share nothing, so nothing serialises them. Under READ COMMITTED
// each transaction's post-update re-walk reads the OTHER's pre-commit row, sees
// a chain that still terminates at R, and commits. The committed result is
// A → B2 → B → A2 → A: a four-node cycle, from two individually-legal moves that
// both returned 200 — and the unbounded org-root CTE then never terminates on
// it, which is the exact DoS these guards exist to prevent.
//
// The fix is to serialise on the ORG ROOT: every move takes a FOR UPDATE on the
// root of the org it is happening in, so any two moves that could possibly
// interact contend on one row. Then the loser re-runs its descendant check
// against the winner's COMMITTED tree and is correctly rejected as a cycle.
//
// Asserts the invariant, not which goroutine wins, so there is no timing
// dependency.
func TestIntegration_WorkspaceReparent_ConcurrentDisjointMovesCannotCreateACycle(t *testing.T) {
	conn := integrationDB_Reparent(t)

	// Repeat: a race that reproduces "sometimes" must not be able to hide
	// behind one lucky scheduling. Pre-fix this fires within the first rounds.
	for round := 0; round < 12; round++ {
		func() {
			root := seedWS(t, conn, fmt.Sprintf("r%d-root", round), "")
			a := seedWS(t, conn, fmt.Sprintf("r%d-a", round), root)
			b := seedWS(t, conn, fmt.Sprintf("r%d-b", round), root)
			a2 := seedWS(t, conn, fmt.Sprintf("r%d-a2", round), a)
			b2 := seedWS(t, conn, fmt.Sprintf("r%d-b2", round), b)

			var wg sync.WaitGroup
			errs := make([]error, 2)
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				_, errs[0] = applyReparent(context.Background(), conn, a, b2)
			}()
			go func() {
				defer wg.Done()
				<-start
				_, errs[1] = applyReparent(context.Background(), conn, b, a2)
			}()
			close(start)
			wg.Wait()

			for i, err := range errs {
				if err == nil {
					continue
				}
				var rej *reparentError
				if !errors.As(err, &rej) {
					t.Fatalf("round %d move %d: NON-rejection error (deadlock?): %v", round, i, err)
				}
			}

			// THE invariant: every node's chain must still terminate at root.
			// On a committed cycle the bounded walk returns ok=false — and the
			// UNBOUNDED CTE that org_scope.go's sameOrg() uses would not
			// terminate at all.
			for _, id := range []string{a, b, a2, b2} {
				tx, err := conn.BeginTx(context.Background(), nil)
				if err != nil {
					t.Fatalf("begin: %v", err)
				}
				gotRoot, ok, werr := reparentOrgRoot(context.Background(), tx, id)
				_ = tx.Rollback()
				if werr != nil {
					t.Fatalf("round %d: walk %s: %v", round, id, werr)
				}
				if !ok || gotRoot != root {
					pa, pb := parentOf(t, conn, a), parentOf(t, conn, b)
					pa2, pb2 := parentOf(t, conn, a2), parentOf(t, conn, b2)
					t.Fatalf("round %d: CYCLE COMMITTED — chain from %s does not terminate at root %s.\n"+
						"  A =%s parent->%s\n  B =%s parent->%s\n  A2=%s parent->%s\n  B2=%s parent->%s\n"+
						"  errs=%v",
						round, id, root, a, pa, b, pb, a2, pa2, b2, pb2, errs)
				}
			}
		}()
	}
}

// TestIntegration_WorkspaceReparent_ConcurrentSwapCannotCreateACycle fires the
// two moves that form a cycle only when combined — A becomes a child of B while
// B becomes a child of A — at the same time.
//
// ⚠️ THIS TEST IS NOT EVIDENCE OF CONCURRENCY SAFETY. READ BEFORE TRUSTING IT.
//
// Both transactions lock the IDENTICAL set {A,B}, so ANY id-ordered
// `FOR UPDATE` serialises them for free. It therefore passes whether or not
// the guard actually works — it passed against the revision whose only
// protection was locking the two endpoints, and that revision committed a
// four-node cycle on the first round of the disjoint case.
//
// The real proof is
// TestIntegration_WorkspaceReparent_ConcurrentDisjointMovesCannotCreateACycle
// above, where the two transactions share NO rows and only the org-root lock
// serialises them. Keep this one as a cheap regression case for the trivial
// shape; do NOT cite it, extend it, or treat a green run here as coverage of
// the mechanism.
//
// The assertion is on the INVARIANT, not on which goroutine wins, so there is
// no timing dependency and nothing to flake: whatever the scheduler does, at
// most one move may commit and the tree must remain walkable.
func TestIntegration_WorkspaceReparent_ConcurrentSwapCannotCreateACycle(t *testing.T) {
	conn := integrationDB_Reparent(t)

	root := seedWS(t, conn, "root", "")
	a := seedWS(t, conn, "node-a", root)
	b := seedWS(t, conn, "node-b", root)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = applyReparent(context.Background(), conn, a, b)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = applyReparent(context.Background(), conn, b, a)
	}()
	wg.Wait()

	succeeded := 0
	for i, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		var rej *reparentError
		if !errors.As(err, &rej) {
			// A deadlock or any other raw DB error would land here. The
			// id-ordered lock in step 1 exists so this does not happen.
			t.Fatalf("move %d failed with a NON-rejection error (deadlock?): %v", i, err)
		}
		if rej.Code != reparentCodeCycle {
			t.Errorf("move %d rejected with %s, want %s (%s)", i, rej.Code, reparentCodeCycle, rej.Message)
		}
	}
	if succeeded != 1 {
		t.Fatalf("%d of 2 swap moves committed; exactly 1 must win", succeeded)
	}

	// The invariant: the tree is still walkable to the original root from
	// both nodes. On a committed cycle this walk would not terminate.
	for _, id := range []string{a, b} {
		tx, err := conn.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		gotRoot, ok, err := reparentOrgRoot(context.Background(), tx, id)
		_ = tx.Rollback()
		if err != nil {
			t.Fatalf("resolve root of %s: %v", id, err)
		}
		if !ok {
			t.Fatalf("parent_id chain from %s does not terminate — a cycle was committed", id)
		}
		if gotRoot != root {
			t.Fatalf("org root of %s = %q, want %q", id, gotRoot, root)
		}
	}
}

// TestIntegration_WorkspaceReparent_OrgRootWalkIsBoundedOnACycle pins the depth
// bound on orgRootSubtreeCTE.
//
// resolveOrgID was repointed onto orgRootID (to fix the depth-2 allowlist
// bypass), and orgRootID's CTE had NO depth guard — so on a cyclic tree every
// plugin check burned the entire statement_timeout and then fell through
// checkOrgPluginAllowlist's fail-OPEN arm. The ACL outcome was no worse than
// before (a cyclic tree was already allow-all), but the multi-second stall per
// check was new, and self-inflicted.
//
// RED WITHOUT THE BOUND: this test times out / takes statement_timeout per call.
// GREEN: the walk terminates promptly and fails CLOSED (errNoOrgRoot).
func TestIntegration_WorkspaceReparent_OrgRootWalkIsBoundedOnACycle(t *testing.T) {
	conn := integrationDB_Reparent(t)

	root := seedWS(t, conn, "bound-root", "")
	x := seedWS(t, conn, "bound-x", root)
	y := seedWS(t, conn, "bound-y", x)
	// Seed the cycle DIRECTLY (what the pre-fix unguarded UPDATE could commit).
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE workspaces SET parent_id = $2 WHERE id = $1`, x, y); err != nil {
		t.Fatalf("seed cycle: %v", err)
	}

	prev := db.DB
	db.DB = conn
	t.Cleanup(func() { db.DB = prev })

	start := time.Now()
	gotRoot, err := orgRootID(context.Background(), conn, y)
	elapsed := time.Since(start)

	if !errors.Is(err, errNoOrgRoot) {
		t.Fatalf("orgRootID on a cycle: root=%q err=%v — want errNoOrgRoot (fail CLOSED)", gotRoot, err)
	}
	// The bound is what makes this fast. A statement_timeout-length stall is
	// the failure this test exists to catch, so hold it to something no
	// unbounded walk could ever achieve.
	if elapsed > 2*time.Second {
		t.Fatalf("orgRootID took %s on a cyclic chain — the walk is not bounded", elapsed)
	}
	t.Logf("bounded org-root walk on a cycle: %v in %s", err, elapsed)

	// And the caller that motivated the change no longer stalls either.
	start = time.Now()
	blocked, reason := checkOrgPluginAllowlist(context.Background(), y, "rogue-plugin")
	elapsed = time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("checkOrgPluginAllowlist took %s on a cyclic chain", elapsed)
	}
	t.Logf("allowlist check on a cyclic tree: blocked=%v reason=%q in %s (fails open by design)",
		blocked, reason, elapsed)

	// Repair so the suite's leaf-first cleanup can drain the rows.
	if _, err := conn.ExecContext(context.Background(),
		`UPDATE workspaces SET parent_id = $2 WHERE id = $1`, x, root); err != nil {
		t.Fatalf("repair: %v", err)
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

	t.Run("adopting_a_root_WITH_descendants_is_rejected", func(t *testing.T) {
		// root2 has team2 under it. Adopting it would silently re-home team2
		// into org 1 — a workspace the caller never named. That is an org
		// MERGE, not org assignment, and is refused.
		w := doPatch_Workspace(t, root2, `{"parent_id":"`+team1+`"}`)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status=%d body=%s, want 422", w.Code, w.Body.String())
		}
		body := decodeBody(t, w)
		if body["code"] != reparentCodeCrossOrg {
			t.Errorf("code=%v, want %s", body["code"], reparentCodeCrossOrg)
		}
		if body["descendant_count"] == nil {
			t.Errorf("rejection did not report the descendant count that caused it: %#v", body)
		}
		if got := parentOf(t, conn, root2); got != "" {
			t.Fatalf("root2.parent_id MUTATED to %q; an org root was demoted into another org", got)
		}
		// team2 must still resolve to org 2 — the whole point of the refusal.
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

// TestIntegration_WorkspaceReparent_ChildlessRootIsAdoptedIntoTheOrg pins the
// SHIPPED flow the first version of this guard broke.
//
// /registry/register's INSERT (registry.go:1094) omits parent_id, so every
// self-registering runtime lands as its own parentless org root. Linking one
// under an existing workspace afterwards is the only way it joins an org.
// tests/e2e/test_poll_mode_e2e.sh does exactly this and went red on
// WORKSPACE_REPARENT_CROSS_ORG when adoption was refused outright.
//
// This reproduces that shape in-process, and pins the org-anchor migration
// that makes it safe.
func TestIntegration_WorkspaceReparent_ChildlessRootIsAdoptedIntoTheOrg(t *testing.T) {
	conn := integrationDB_Reparent(t)

	// The e2e shape: two independently self-registered, parentless roots.
	pollTarget := seedWS(t, conn, "poll-target", "")
	caller := seedWS(t, conn, "caller", "")

	// Give the adoptee an org anchor so the migration is actually exercised.
	if _, err := conn.ExecContext(context.Background(),
		`INSERT INTO org_plugin_allowlist (org_id, plugin_name, enabled_by) VALUES ($1, 'legacy-plugin', $2)`,
		caller, "itest-admin"); err != nil {
		t.Fatalf("seed adoptee allowlist: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(),
			`DELETE FROM org_plugin_allowlist WHERE org_id = $1 OR org_id = $2`, caller, pollTarget)
	})

	before := nsNames(t, conn, caller)
	if before[2] != "org:"+caller+"(writable=true)" {
		t.Fatalf("precondition: adoptee should be its own root with WRITABLE org, got %v", before)
	}

	w := doPatch_Workspace(t, caller, `{"parent_id":"`+pollTarget+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("adoption: status=%d body=%s, want 200", w.Code, w.Body.String())
	}
	if got := parentOf(t, conn, caller); got != pollTarget {
		t.Fatalf("parent_id=%q, want %q", got, pollTarget)
	}

	// Comma-ok, not a bare assertion: in a reverted/broken state `reparented`
	// is absent and a bare assertion PANICS, which takes down the whole test
	// binary and hides every other result in the package. Fail this test, not
	// the run.
	rp, ok := decodeBody(t, w)["reparented"].(map[string]any)
	if !ok {
		t.Fatalf("response has no `reparented` block: %s", w.Body.String())
	}
	if rp["adopted_into_org"] != true {
		t.Errorf("adopted_into_org=%v, want true", rp["adopted_into_org"])
	}
	if rp["org_root_id"] != pollTarget {
		t.Errorf("org_root_id=%v, want %s (the NEW org)", rp["org_root_id"], pollTarget)
	}

	// The privilege delta the endpoint must not hide: it loses org WRITE.
	after := nsNames(t, conn, caller)
	want := []string{
		"workspace:" + caller + "(writable=true)",
		"team:" + pollTarget + "(writable=true)",
		"org:" + pollTarget + "(writable=false)",
	}
	if !sameStrings(after, want) {
		t.Fatalf("namespaces after adoption = %v, want %v", after, want)
	}

	// ORG ANCHORS MIGRATED — not left pointing at a row that is no longer a
	// root. This mirrors ensurePlatformAgent's behaviour.
	var atOld, atNew int
	if err := conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM org_plugin_allowlist WHERE org_id = $1`, caller).Scan(&atOld); err != nil {
		t.Fatalf("count old anchors: %v", err)
	}
	if err := conn.QueryRowContext(context.Background(),
		`SELECT count(*) FROM org_plugin_allowlist WHERE org_id = $1 AND plugin_name = 'legacy-plugin'`,
		pollTarget).Scan(&atNew); err != nil {
		t.Fatalf("count new anchors: %v", err)
	}
	if atOld != 0 {
		t.Errorf("%d org_plugin_allowlist row(s) still anchored to the adopted (no-longer-root) workspace", atOld)
	}
	if atNew != 1 {
		t.Errorf("allowlist row did not migrate to the new org root (found %d)", atNew)
	}

	// And a2a/delegation now sees them as the same org, which is the point of
	// the e2e flow this restores.
	same, err := sameOrg(context.Background(), conn, caller, pollTarget)
	if err != nil {
		t.Fatalf("sameOrg: %v", err)
	}
	if !same {
		t.Fatal("adopted workspace is not sameOrg with its new parent — CanCommunicate would still 403")
	}
}

// TestIntegration_WorkspaceReparent_ChildlessCheckCannotBeRaced pins the
// lock interaction that makes the adoption guard sound.
//
// Adoption is allowed only for a CHILDLESS root. If a child could be inserted
// between the count and the write, an org WITH a subtree would be adopted —
// exactly what the guard refuses — and the descendants would change org
// silently.
//
// It cannot happen, and the reason is not obvious enough to leave implicit:
// an `INSERT INTO workspaces (... parent_id = X)` must take `FOR KEY SHARE`
// on row X to satisfy workspaces_parent_id_fkey, and applyReparent already
// holds `FOR UPDATE` on X (the adoptee IS its own org root, so it is in the
// lock set twice over). FOR UPDATE conflicts with FOR KEY SHARE, so the
// inserting transaction blocks until the adoption commits.
//
// WHY THIS IS A DETERMINISTIC LOCK TEST AND NOT A CONCURRENT PROBE.
// A 250-round "fire both at once" probe was written first and MEASURED: it
// scored 0 adopted / 250 refused. The child INSERT is a single statement while
// applyReparent must parse, BeginTx and walk the chain twice before it reaches
// the lock, so the INSERT wins the head start essentially every time and the
// probe never reaches the interleaving it claims to test. It would have sat in
// the suite looking like coverage while asserting nothing about the lock. Do
// not re-add it; drive the lock directly, as below.
func TestIntegration_WorkspaceReparent_ChildlessCheckCannotBeRaced(t *testing.T) {
	conn := integrationDB_Reparent(t)
	ctx := context.Background()

	adoptee := seedWS(t, conn, "lock-adoptee", "")

	// Hold exactly the lock applyReparent's step 1 holds on the adoptee.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var lockedID string
	if err := tx.QueryRowContext(ctx,
		`SELECT id::text FROM workspaces WHERE id = $1 FOR UPDATE`, adoptee).Scan(&lockedID); err != nil {
		_ = tx.Rollback()
		t.Fatalf("take FOR UPDATE: %v", err)
	}

	// A second connection tries to add a child. workspaces_parent_id_fkey
	// forces it to take FOR KEY SHARE on the parent row, which conflicts with
	// the FOR UPDATE above — so it must BLOCK, and with lock_timeout set it
	// must fail with 55P03 lock_not_available rather than succeed.
	other, err := sql.Open("postgres", requireIntegrationDBURL(t))
	if err != nil {
		t.Fatalf("open second conn: %v", err)
	}
	defer other.Close()
	if _, err := other.ExecContext(ctx, `SET lock_timeout = '2s'`); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	childID := uuid.NewString()
	_, insErr := other.ExecContext(ctx,
		`INSERT INTO workspaces (id, name, parent_id, status) VALUES ($1, $2, $3, 'online')`,
		childID, reparentTestPrefix+"lock-child", adoptee)
	if insErr == nil {
		_ = tx.Rollback()
		t.Fatal("child INSERT SUCCEEDED while the parent row was held FOR UPDATE — " +
			"the childless check CAN be raced, and adoption's descendant guard is not sound")
	}
	var pqErr *pq.Error
	if !errors.As(insErr, &pqErr) || pqErr.Code.Name() != "lock_not_available" {
		_ = tx.Rollback()
		t.Fatalf("child INSERT failed with %v — expected a LOCK timeout (55P03). "+
			"A different error means this test is no longer exercising the FK's FOR KEY SHARE.", insErr)
	}
	t.Logf("confirmed: child INSERT blocks on the adoptee's FOR UPDATE (%v)", pqErr.Code.Name())

	// NEGATIVE CONTROL: exactly one thing changes — the lock is released.
	// The same INSERT must now succeed. Without this the test would pass if
	// the INSERT were failing for an unrelated reason (bad column, FK typo).
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := other.ExecContext(ctx,
		`INSERT INTO workspaces (id, name, parent_id, status) VALUES ($1, $2, $3, 'online')`,
		childID, reparentTestPrefix+"lock-child", adoptee); err != nil {
		t.Fatalf("child INSERT still failed after the lock was released: %v — "+
			"the blocked INSERT above was not caused by the lock", err)
	}

	// And with a child present, adoption is refused — the guard the lock protects.
	host := seedWS(t, conn, "lock-host", "")
	_, adoptErr := applyReparent(ctx, conn, adoptee, host)
	var rej *reparentError
	if !errors.As(adoptErr, &rej) || rej.Code != reparentCodeCrossOrg {
		t.Fatalf("adoption of a now-parented root: err=%v, want %s", adoptErr, reparentCodeCrossOrg)
	}
	if rej.Details["descendant_count"] == nil {
		t.Errorf("refusal did not report descendant_count: %#v", rej.Details)
	}
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

	t.Run("removed_workspace_cannot_be_moved", func(t *testing.T) {
		// Negative control for the removed_parent case above: same guard
		// family, opposite end of the edge. A soft-deleted workspace is exempt
		// from workspaces_parent_name_uniq, so re-attaching it could land on a
		// name already taken under the destination.
		gone := seedWS(t, conn, "gone", root)
		if _, err := conn.ExecContext(context.Background(),
			`UPDATE workspaces SET status = 'removed' WHERE id = $1`, gone); err != nil {
			t.Fatalf("mark removed: %v", err)
		}
		w := doPatch_Workspace(t, gone, `{"parent_id":"`+other+`"}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status=%d body=%s, want 404", w.Code, w.Body.String())
		}
		if got := parentOf(t, conn, gone); got != root {
			t.Fatalf("parent_id MUTATED to %q on a removed workspace", got)
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

// TestIntegration_WorkspaceReparent_CreateValidatesParentID covers the CREATE
// path, which took a caller-supplied parent_id straight into the INSERT with
// no validation at all.
//
// Create is the wider door: AdminAuth admits an org-token, and (when
// ADMIN_TOKEN is unset) any workspace token through its tier-3 fallback —
// whereas PATCH parent_id requires admin-token or cp-session. Creating under
// parent P grants the new workspace read+write on team:P, retroactively.
//
// Structural validation only — see validateCreateParentID's scope note on why
// org-scoping the create path is a separate change.
func TestIntegration_WorkspaceReparent_CreateValidatesParentID(t *testing.T) {
	conn := integrationDB_Reparent(t)
	ctx := context.Background()

	root := seedWS(t, conn, "create-root", "")
	dead := seedWS(t, conn, "create-dead", root)
	if _, err := conn.ExecContext(ctx,
		`UPDATE workspaces SET status = 'removed' WHERE id = $1`, dead); err != nil {
		t.Fatalf("mark removed: %v", err)
	}

	t.Run("nonexistent_parent_rejected", func(t *testing.T) {
		err := validateCreateParentID(ctx, conn, nil, uuid.NewString())
		var rej *reparentError
		if !errors.As(err, &rej) || rej.Code != reparentCodeNotFound {
			t.Fatalf("err=%v, want %s", err, reparentCodeNotFound)
		}
	})
	t.Run("removed_parent_rejected", func(t *testing.T) {
		err := validateCreateParentID(ctx, conn, nil, dead)
		var rej *reparentError
		if !errors.As(err, &rej) || rej.Code != reparentCodeNotFound {
			t.Fatalf("err=%v, want %s", err, reparentCodeNotFound)
		}
	})
	t.Run("non_uuid_parent_rejected", func(t *testing.T) {
		err := validateCreateParentID(ctx, conn, nil, "not-a-uuid")
		var rej *reparentError
		if !errors.As(err, &rej) || rej.Code != reparentCodeInvalid {
			t.Fatalf("err=%v, want %s", err, reparentCodeInvalid)
		}
	})
	// NEGATIVE CONTROLS: same function, one input different. A live parent
	// must be accepted, and an absent parent_id must stay absent (the
	// server-side default path below it must keep working).
	t.Run("live_parent_accepted", func(t *testing.T) {
		if err := validateCreateParentID(ctx, conn, nil, root); err != nil {
			t.Fatalf("live parent rejected: %v", err)
		}
	})
	t.Run("empty_is_not_validated", func(t *testing.T) {
		if err := validateCreateParentID(ctx, conn, nil, ""); err != nil {
			t.Fatalf("empty parent_id must be a no-op, got: %v", err)
		}
	})

	// ---- org scoping -----------------------------------------------------
	// An ANCHORED org token may only create under a parent in its own org.
	// This is the escalation the create path left open: AdminAuth admits an
	// org-token, and creating under parent P grants team:P read+write
	// retroactively.
	otherRoot := seedWS(t, conn, "create-other-root", "")
	otherChild := seedWS(t, conn, "create-other-child", otherRoot)

	withOrgAnchor := func(anchor string) *gin.Context {
		gin.SetMode(gin.TestMode)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		if anchor != "" {
			c.Set("org_id", anchor)
		}
		return c
	}

	t.Run("org_token_cannot_create_under_a_foreign_org", func(t *testing.T) {
		err := validateCreateParentID(ctx, conn, withOrgAnchor(root), otherChild)
		var rej *reparentError
		if !errors.As(err, &rej) || rej.Code != reparentCodeCrossOrg {
			t.Fatalf("err=%v, want %s", err, reparentCodeCrossOrg)
		}
		if rej.Status != http.StatusForbidden {
			t.Errorf("status=%d, want 403", rej.Status)
		}
	})
	// NEGATIVE CONTROL: same call, ONE field different — the anchor now names
	// the parent's OWN org. Must be accepted. Without this the guard could be
	// blanket-denying every anchored caller and still look correct above.
	t.Run("org_token_can_create_within_its_own_org", func(t *testing.T) {
		if err := validateCreateParentID(ctx, conn, withOrgAnchor(otherRoot), otherChild); err != nil {
			t.Fatalf("same-org create rejected: %v", err)
		}
	})
	// And an unanchored caller (ADMIN_TOKEN / cp-session) is unaffected —
	// this is the arm that would have 403'd creation fleet-wide had the
	// earlier "two namespaces" reasoning been acted on naively.
	t.Run("unanchored_caller_is_unrestricted", func(t *testing.T) {
		if err := validateCreateParentID(ctx, conn, withOrgAnchor(""), otherChild); err != nil {
			t.Fatalf("unanchored caller rejected: %v", err)
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
