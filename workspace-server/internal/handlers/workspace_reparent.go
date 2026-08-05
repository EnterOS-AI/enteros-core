package handlers

// workspace_reparent.go — enforcement for the `parent_id` field on
// PATCH /workspaces/:id (client issue #5074).
//
// WHAT THIS FILE IS FOR
// ---------------------
// #5074 asked for a NEW endpoint to re-parent a workspace, on the premise
// that "parent_id is create-only". That premise is wrong: PATCH
// /workspaces/:id has accepted `parent_id` since it was written
// (workspace_crud.go), and applied it as a bare
//
//	UPDATE workspaces SET parent_id = $2 WHERE id = $1
//
// with NO validation of any kind, and with the error swallowed by a
// log.Printf so a failed write still answered 200 {"status":"updated"}.
// So the feature shipped; the guards did not. This file supplies them.
//
// WHY parent_id IS NOT COSMETIC
// -----------------------------
// `workspaces` has no org_id column. An "org" IS the subtree reachable
// through the parent_id chain from a row with parent_id IS NULL — see
// org_scope.go, which says so explicitly and builds the cross-tenant
// isolation primitive (orgRootID / sameOrg) on exactly that walk. Every
// one of the following is therefore DERIVED from parent_id:
//
//   - memory ACLs. namespace/resolver.go derives a workspace's readable
//     and writable namespaces as workspace:<self>, team:<parent> (READ
//     AND WRITE) and org:<root> (read; write only when self IS the root).
//   - a2a routing / delegation, peer discovery, list_peers and broadcast,
//     all of which gate on sameOrg().
//   - the org-token auth check in middleware.WorkspaceAuth, which binds an
//     anchored org token to the workspace's org ROOT.
//   - org_plugin_allowlist.org_id and org_api_tokens.org_id, both of which
//     are FKs onto the workspace id that happens to be an org root.
//
// So a parent_id write is a privilege change on four separate planes. Two
// concrete consequences drove the rules below.
//
// 1. RETROACTIVE READ. memory_records.namespace is a denormalised TEXT
// string ("team:<uuid>") frozen at write time, in tables the memory plugin
// owns and workspace-server has "zero knowledge of"
// (cmd/memory-plugin-postgres/migrations/001_memory_v2.up.sql). Moving a
// workspace under parent N does not just grant it go-forward access to
// team:N — it grants read access to EVERY memory the N sibling group has
// EVER written. Symmetrically the mover's own team:<oldParent> rows stay
// behind, readable by the old siblings forever. Nothing can be rewritten to
// fix this: team: rows are shared, so "migrating" them would drag the old
// siblings' memories into the new team.
//
// 2. AN UNGUARDED WRITE CAN WEDGE THE ORG. orgRootSubtreeCTE in
// org_scope.go, the descendant CTE in workspace_crud.go's delete path, and
// workspace_broadcast.go all walk parent_id with NO depth guard. A two-node
// cycle (A→B→A) makes them recurse until the statement is cancelled or the
// server runs out of memory — verified against Postgres 16. That is a
// denial of service on the tenant-isolation primitive, reachable from a
// single PATCH. namespace/resolver.go survives a cycle only because it caps
// at maxChainDepth=50, and then derives an org root of "whatever node hop 50
// happens to land on" — arbitrary memory ACLs rather than a hang.
//
// THE INVARIANT THIS FILE ENFORCES
// --------------------------------
// A re-parent may NOT change any workspace's org. Formally: for every
// workspace in the tree, orgRoot(w) is invariant across the operation.
//
// That single rule does most of the work, and it is why the blast radius of
// an ALLOWED move is exactly one namespace swap on exactly one workspace:
//
//   - cross-org moves are rejected, so org:<root> never changes — no
//     retroactive read of another org's corpus, no sameOrg() flip, no
//     silently re-anchored org token or plugin allowlist.
//   - parent_id = null is rejected, because promoting a node to root MINTS
//     A NEW ORG: the node and its whole subtree leave their org, lose
//     org:<oldRoot>, stop being sameOrg() with every former peer (breaking
//     in-flight delegations at delivery), and the node gains WRITE on the
//     org namespace, which is root-only by design.
//   - demoting an existing root UNDER another tree is rejected by the same
//     rule for free: a root's org is itself, so any move of it is by
//     definition cross-org. Orgs can therefore be neither merged nor split
//     through this endpoint.
//   - descendants of the moved node keep their parent, and (by the rule)
//     keep their root — so their namespace sets are untouched.
//
// The residual, which is inherent and NOT hidden: moving between two teams
// inside one org crosses the only intra-org privacy boundary there is, and
// grants retroactive read of the destination team's history. The operation
// is therefore restricted to admin-token / cp-session callers (a workspace's
// own token can never move it — see workspaceInfrastructurePatchFields), it
// writes an audit row, and it returns the namespace delta in the response so
// the change is explicit at the call site instead of discovered later.
//
// Client issue #5074's actual request — inserting a NEWLY CREATED team node
// above an existing child, same org — has an EMPTY destination team
// namespace and an unchanged root, so it carries neither the retroactive
// read nor any org change. It is allowed.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// reparentMaxDepth bounds every parent_id walk in this file.
//
// It is NOT merely a sanity limit: this code must run correctly against a
// table that ALREADY contains a cycle written by the unguarded UPDATE this
// file replaces. An unbounded CTE (the shape org_scope.go and the delete
// path still use) would hang the request that is trying to diagnose the
// damage. Every walk here terminates.
//
// 64 > namespace/resolver.go's maxChainDepth of 50, so any chain this file
// accepts is one the memory resolver can also walk to its true root.
const reparentMaxDepth = 64

// Error codes returned to the caller. Stable strings — the canvas and the
// CLI branch on these, not on the human-readable message.
const (
	reparentCodeInvalid       = "WORKSPACE_REPARENT_INVALID_PARENT_ID"
	reparentCodeSelf          = "WORKSPACE_REPARENT_SELF"
	reparentCodeNotFound      = "WORKSPACE_REPARENT_PARENT_NOT_FOUND"
	reparentCodeCycle         = "WORKSPACE_REPARENT_CYCLE"
	reparentCodeCrossOrg      = "WORKSPACE_REPARENT_CROSS_ORG"
	reparentCodeRootPromotion = "WORKSPACE_REPARENT_ROOT_PROMOTION_FORBIDDEN"
	reparentCodeNameConflict  = "WORKSPACE_REPARENT_NAME_CONFLICT"
	reparentCodeAmbiguous     = "WORKSPACE_REPARENT_AMBIGUOUS_WITH_RENAME"
	reparentCodeUnresolvable  = "WORKSPACE_REPARENT_ORG_UNRESOLVABLE"
)

// reparentError carries the HTTP status and stable code for a rejected move.
type reparentError struct {
	Status  int
	Code    string
	Message string
	Details map[string]any
}

func (e *reparentError) Error() string { return e.Message }

func reparentReject(status int, code, msg string, details map[string]any) *reparentError {
	return &reparentError{Status: status, Code: code, Message: msg, Details: details}
}

// reparentOutcome describes an applied (or no-op) move.
type reparentOutcome struct {
	Changed   bool
	OldParent string // "" when the workspace had no parent
	NewParent string
	OrgRoot   string
	// Lost/Gained are the memory namespaces (namespace/resolver.go wire
	// strings) this move removes from and adds to the workspace's readable
	// AND writable set. Surfaced in the PATCH response — see the file
	// header on why this must not be silent.
	Lost   []string
	Gained []string
}

// parseReparentTarget normalises the raw JSON value of body["parent_id"].
//
// Accepts JSON null (-> wantRoot) or a UUID string. Anything else is a 400:
// the pre-fix code passed the raw interface{} straight into the driver, so a
// number or an object produced a driver error that was then swallowed.
func parseReparentTarget(raw any) (target string, wantRoot bool, err *reparentError) {
	if raw == nil {
		return "", true, nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", false, reparentReject(http.StatusBadRequest, reparentCodeInvalid,
			"parent_id must be a UUID string or null", nil)
	}
	if s == "" {
		// Distinct from null on the wire, and historically ambiguous. Reject
		// rather than guess which one the caller meant.
		return "", false, reparentReject(http.StatusBadRequest, reparentCodeInvalid,
			"parent_id must be a UUID string or null (empty string is not accepted; send null to mean 'no parent')", nil)
	}
	if _, perr := uuid.Parse(s); perr != nil {
		return "", false, reparentReject(http.StatusBadRequest, reparentCodeInvalid,
			"parent_id is not a valid UUID", nil)
	}
	return s, false, nil
}

// reparentOrgRoot resolves the org root of workspaceID with a BOUNDED walk,
// inside the caller's transaction.
//
// Returns ok=false when the chain does not terminate at a parent_id IS NULL
// row within reparentMaxDepth hops — i.e. the chain is already cyclic or
// pathologically deep. Callers MUST fail closed on ok=false: an unresolvable
// org is not "no org", it is "this row's tenancy cannot be established", and
// treating it as either would be the fail-open that org_scope.go's sameOrg
// deliberately avoids.
func reparentOrgRoot(ctx context.Context, tx *sql.Tx, workspaceID string) (root string, ok bool, err error) {
	const q = `
		WITH RECURSIVE chain AS (
			SELECT id, parent_id, 0 AS depth
			FROM workspaces
			WHERE id = $1
			UNION ALL
			SELECT w.id, w.parent_id, c.depth + 1
			FROM workspaces w
			JOIN chain c ON w.id = c.parent_id
			WHERE c.depth < $2
		)
		SELECT id::text, parent_id IS NULL AS is_root
		FROM chain
		ORDER BY depth DESC
		LIMIT 1
	`
	var id string
	var isRoot bool
	scanErr := tx.QueryRowContext(ctx, q, workspaceID, reparentMaxDepth).Scan(&id, &isRoot)
	if errors.Is(scanErr, sql.ErrNoRows) {
		return "", false, nil
	}
	if scanErr != nil {
		return "", false, scanErr
	}
	if !isRoot {
		// Hit the depth cap without finding a parentless row.
		return "", false, nil
	}
	return id, true, nil
}

// reparentIsDescendant reports whether candidate is workspaceID itself or
// anywhere in workspaceID's subtree. A move onto such a node is precisely
// the cycle case.
//
// Bounded for the same reason reparentOrgRoot is: the table may already
// contain a cycle. The `status != 'removed'` filter mirrors the delete
// path's descendant walk; a removed row is rejected earlier as a parent
// candidate, so it cannot sneak through here.
func reparentIsDescendant(ctx context.Context, tx *sql.Tx, workspaceID, candidate string) (bool, error) {
	if workspaceID == candidate {
		return true, nil
	}
	const q = `
		WITH RECURSIVE descendants AS (
			SELECT id, 0 AS depth
			FROM workspaces
			WHERE parent_id = $1 AND status != 'removed'
			UNION ALL
			SELECT w.id, d.depth + 1
			FROM workspaces w
			JOIN descendants d ON w.parent_id = d.id
			WHERE d.depth < $2 AND w.status != 'removed'
		)
		SELECT EXISTS(SELECT 1 FROM descendants WHERE id = $3)
	`
	var found bool
	if err := tx.QueryRowContext(ctx, q, workspaceID, reparentMaxDepth, candidate).Scan(&found); err != nil {
		return false, err
	}
	return found, nil
}

// applyReparent validates and applies a parent_id change as ONE transaction.
//
// Ordering matters: every check runs INSIDE the tx, after SELECT ... FOR
// UPDATE on both the moved row and the destination parent. Validating
// against a pre-transaction snapshot would let two concurrent PATCHes
// (A→child-of-B and B→child-of-A) each pass an individually-correct cycle
// check and commit a cycle between them. Locking both endpoints serialises
// any pair of moves that share a node.
//
// The post-update re-walk (step 8) is the backstop for anything the
// pre-checks miss: it re-derives the org root FROM THE UPDATED ROWS and
// rolls back unless the chain still terminates at the same root. A move that
// cannot prove that property does not commit.
func applyReparent(ctx context.Context, database *sql.DB, workspaceID string, raw any) (*reparentOutcome, error) {
	target, wantRoot, perr := parseReparentTarget(raw)
	if perr != nil {
		return nil, perr
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("reparent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1. Lock BOTH endpoints up front, in a single statement ordered by id.
	//
	// Locking them separately (self, then target) is a deadlock: two
	// concurrent moves that swap roles — A→child-of-B and B→child-of-A —
	// each take the other's second lock first, and Postgres has to abort one.
	// A single ORDER BY id ... FOR UPDATE gives every caller the same global
	// lock order, so those two transactions serialise instead of colliding.
	// (The pair is exactly the one the post-update re-walk in step 9 exists
	// to catch, so it is worth not provoking it in the first place.)
	//
	// wantRoot has no target, so it locks only the moved row.
	lockIDs := []string{workspaceID}
	if !wantRoot && target != workspaceID {
		lockIDs = append(lockIDs, target)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id::text, parent_id::text, status::text
		   FROM workspaces
		  WHERE id = ANY($1::uuid[])
		  ORDER BY id
		    FOR UPDATE`, pq.Array(lockIDs))
	if err != nil {
		return nil, fmt.Errorf("reparent: lock rows: %w", err)
	}
	type wsRow struct {
		parent sql.NullString
		status string
	}
	locked := make(map[string]wsRow, 2)
	for rows.Next() {
		var id, status string
		var parent sql.NullString
		if err := rows.Scan(&id, &parent, &status); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reparent: scan locked row: %w", err)
		}
		locked[id] = wsRow{parent: parent, status: status}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("reparent: iterate locked rows: %w", err)
	}
	rows.Close()

	self, ok := locked[workspaceID]
	if !ok {
		return nil, reparentReject(http.StatusNotFound, reparentCodeNotFound, "workspace not found", nil)
	}
	oldParent := ""
	if self.parent.Valid {
		oldParent = self.parent.String
	}

	// 2. parent_id: null. Idempotent when the workspace is ALREADY a root;
	//    otherwise this is org creation, not an org-chart edit.
	if wantRoot {
		if oldParent == "" {
			return &reparentOutcome{Changed: false, OrgRoot: workspaceID}, nil
		}
		return nil, reparentReject(http.StatusUnprocessableEntity, reparentCodeRootPromotion,
			"promoting a workspace to org root is not permitted: it would move this workspace and its entire subtree into a NEW org, "+
				"dropping their access to the current org's memories, breaking sameOrg() delegation with every former peer, and granting "+
				"write on the org memory namespace (root-only by design). Create the workspace under the intended parent instead.",
			map[string]any{"current_parent_id": oldParent})
	}

	// 3. Self-parent.
	if target == workspaceID {
		return nil, reparentReject(http.StatusUnprocessableEntity, reparentCodeSelf,
			"a workspace cannot be its own parent", nil)
	}

	// 4. No-op — same parent. Reported as unchanged so callers can tell a
	//    real move from a redundant one.
	if oldParent == target {
		root, ok, rerr := reparentOrgRoot(ctx, tx, workspaceID)
		if rerr != nil {
			return nil, fmt.Errorf("reparent: resolve org root: %w", rerr)
		}
		if !ok {
			return nil, reparentReject(http.StatusConflict, reparentCodeUnresolvable,
				"this workspace's org root cannot be resolved (the parent_id chain does not terminate); refusing to act on it", nil)
		}
		return &reparentOutcome{Changed: false, OldParent: oldParent, NewParent: target, OrgRoot: root}, nil
	}

	// 5. Destination must exist and be live. Already locked in step 1, so it
	//    cannot be moved or removed between here and the commit.
	newParent, ok := locked[target]
	if !ok {
		return nil, reparentReject(http.StatusUnprocessableEntity, reparentCodeNotFound,
			"target parent workspace does not exist", map[string]any{"parent_id": target})
	}
	if newParent.status == "removed" {
		return nil, reparentReject(http.StatusUnprocessableEntity, reparentCodeNotFound,
			"target parent workspace has been removed", map[string]any{"parent_id": target})
	}

	// 6. Cycle prevention — direct (A→A, handled at step 3) and transitive
	//    (A→B→…→A) at arbitrary depth.
	isDesc, err := reparentIsDescendant(ctx, tx, workspaceID, target)
	if err != nil {
		return nil, fmt.Errorf("reparent: descendant check: %w", err)
	}
	if isDesc {
		return nil, reparentReject(http.StatusUnprocessableEntity, reparentCodeCycle,
			"target parent is a descendant of this workspace; the move would create a parent_id cycle",
			map[string]any{"parent_id": target})
	}

	// 7. Same-org invariant. Fails CLOSED: an unresolvable chain on EITHER
	//    side denies the move.
	oldRoot, ok, err := reparentOrgRoot(ctx, tx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("reparent: resolve source org root: %w", err)
	}
	if !ok {
		return nil, reparentReject(http.StatusConflict, reparentCodeUnresolvable,
			"this workspace's org root cannot be resolved (the parent_id chain does not terminate); refusing to move it", nil)
	}
	targetRoot, ok, err := reparentOrgRoot(ctx, tx, target)
	if err != nil {
		return nil, fmt.Errorf("reparent: resolve target org root: %w", err)
	}
	if !ok {
		return nil, reparentReject(http.StatusConflict, reparentCodeUnresolvable,
			"the target parent's org root cannot be resolved (its parent_id chain does not terminate); refusing to move into it",
			map[string]any{"parent_id": target})
	}
	if oldRoot != targetRoot {
		// Covers three shapes at once: a genuine cross-tenant move, demoting
		// an org root under another tree (a root's org is ITSELF, so
		// oldRoot==workspaceID != targetRoot), and adopting a foreign root.
		return nil, reparentReject(http.StatusUnprocessableEntity, reparentCodeCrossOrg,
			"cross-org re-parent is not permitted: the target parent belongs to a different org root. "+
				"The org boundary IS the parent_id chain, so this move would change which org's memories, peers, "+
				"delegation targets and plugin allowlist this workspace resolves to.",
			map[string]any{"org_root_id": oldRoot, "target_org_root_id": targetRoot, "parent_id": target})
	}

	// 8. Apply.
	if _, err := tx.ExecContext(ctx,
		`UPDATE workspaces SET parent_id = $2, updated_at = now() WHERE id = $1`, workspaceID, target,
	); err != nil {
		// workspaces_parent_name_uniq: UNIQUE (COALESCE(parent_id, zero-uuid), name)
		// WHERE status != 'removed'. The destination may already hold a sibling
		// with this name. Pre-fix this surfaced as a swallowed log line and a
		// 200 {"status":"updated"} on a workspace that had not moved.
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code.Name() == "unique_violation" {
			return nil, reparentReject(http.StatusConflict, reparentCodeNameConflict,
				"the target parent already has a child with this workspace's name; rename before moving",
				map[string]any{"parent_id": target})
		}
		return nil, fmt.Errorf("reparent: update: %w", err)
	}

	// 9. Post-update verification, from the UPDATED rows, still inside the tx.
	//    Anything that got past steps 3-7 — a concurrent move we failed to
	//    serialise, a trigger, a future column default — has to survive this
	//    or the whole thing rolls back. This is the difference between "we
	//    checked before writing" and "the committed state is provably sane".
	postRoot, ok, err := reparentOrgRoot(ctx, tx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("reparent: post-verify: %w", err)
	}
	if !ok {
		return nil, reparentReject(http.StatusConflict, reparentCodeCycle,
			"post-update verification failed: the parent_id chain no longer terminates at an org root; the move was rolled back", nil)
	}
	if postRoot != oldRoot {
		return nil, reparentReject(http.StatusUnprocessableEntity, reparentCodeCrossOrg,
			"post-update verification failed: the org root changed; the move was rolled back",
			map[string]any{"org_root_id": oldRoot, "post_move_org_root_id": postRoot})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("reparent: commit: %w", err)
	}

	out := &reparentOutcome{
		Changed:   true,
		OldParent: oldParent,
		NewParent: target,
		OrgRoot:   oldRoot,
		Lost:      []string{"team:" + oldParent},
		Gained:    []string{"team:" + target},
	}
	// A workspace whose old parent was NULL cannot reach this point (step 7
	// rejects it as cross-org), so oldParent is always a real id here. Keep
	// the guard anyway rather than emit a "team:" with nothing after it.
	if oldParent == "" {
		out.Lost = nil
	}
	return out, nil
}
