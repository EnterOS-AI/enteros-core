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
// No workspace that ALREADY BELONGS to an org may change org, and no move
// may change the org of a workspace the caller did not name. Formally:
// orgRoot(w) is invariant for every w, with exactly one exception —
// a CHILDLESS ROOT joining an org (see ADOPTION below).
//
// That rule is why the blast radius of an ordinary move is exactly one
// namespace swap on exactly one workspace:
//
//   - cross-org moves are rejected, so org:<root> never changes — no
//     retroactive read of another org's corpus, no sameOrg() flip, no
//     silently re-anchored org token or plugin allowlist.
//   - parent_id = null is rejected, because promoting a node to root MINTS
//     A NEW ORG: the node and its whole subtree leave their org, lose
//     org:<oldRoot>, stop being sameOrg() with every former peer (breaking
//     in-flight delegations at delivery), and the node gains WRITE on the
//     org namespace, which is root-only by design.
//   - descendants of the moved node keep their parent, and (by the rule)
//     keep their root — so their namespace sets are untouched.
//
// ADOPTION — the one exception, and why refusing it was wrong
// -----------------------------------------------------------
// The first version of this guard refused EVERY root change and claimed
// "demoting a root falls out for free". That was a mistake: it removed a
// shipped capability. /registry/register's INSERT (registry.go:1094) omits
// parent_id, so every self-registering runtime lands as its OWN parentless
// org root, and linking it under an existing workspace afterwards is the
// only way it ever joins an org — root promotion is refused and
// create-under-parent is a different row. Refusing that stranded such
// runtimes permanently.
//
// So a CHILDLESS root may be adopted into an org, on the terms
// platform_agent.go's installer already uses: its org_api_tokens and
// org_plugin_allowlist rows migrate in the SAME transaction. "Childless" is
// the line between org ASSIGNMENT of an unaffiliated workspace and an org
// MERGE — adopting a root WITH a subtree would silently re-home every
// descendant, which a client-callable endpoint must not do.
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

	"github.com/gin-gonic/gin"
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
	reparentCodeConcurrent    = "WORKSPACE_REPARENT_CONCURRENT_MODIFICATION"
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
	Changed bool
	// Adopted is true when this move brought a parentless workspace INTO an
	// org (the one case where the org root legitimately changes).
	Adopted   bool
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

// validateCreateParentID structurally validates a CALLER-SUPPLIED parent_id on
// POST /workspaces.
//
// The create path took `payload.ParentID` straight into the INSERT with no
// validation at all — no UUID check, no existence check, no status check. A
// malformed value surfaced as a 500 from the FK or the type cast, and a
// `removed` parent silently produced a child hanging off a soft-deleted row.
//
// This matters for the same reason the PATCH guard does: creating a workspace
// under parent P grants the new workspace read+write on `team:P` — including
// RETROACTIVE read of everything P's existing children ever wrote — plus read
// of that subtree's `org:<root>`. Create is the wider door, because AdminAuth
// admits an org-token (and, when ADMIN_TOKEN is unset, any workspace token via
// its tier-3 fallback), whereas the PATCH path requires admin-token or
// cp-session.
//
// ORG SCOPING. An ANCHORED org token may only create under a parent in its
// OWN org. The anchor comparison goes through orgAnchorMatchesRoot (the same
// helper the a2a caller classifier uses), which accepts BOTH namespaces
// org_api_tokens.org_id is written in — the org-root workspace id, and the raw
// CP org UUID mapped forward through DeterministicPlatformAgentID — so it
// cannot 403 a concierge-minted token.
//
// (An earlier revision of this file declined to do this check at all, claiming
// a namespace mismatch would 403 creation fleet-wide. That was wrong, and the
// note is removed rather than softened: org_api_tokens.org_id carries
// `REFERENCES workspaces(id)`, so the raw-CP-UUID form cannot be persisted in
// the first place — verified on PG16, the insert fails
// org_api_tokens_org_id_fkey. Using orgAnchorMatchesRoot makes the point moot
// either way.)
//
// Callers WITHOUT an org anchor — ADMIN_TOKEN, cp-session, and unanchored
// tokens — are unaffected: they are tenant-wide principals here, and
// tightening them is a different decision from closing a cross-org hole.
// Fails CLOSED on a lookup error, matching sameOrg's posture.
func validateCreateParentID(ctx context.Context, database *sql.DB, c *gin.Context, raw string) error {
	if raw == "" {
		return nil
	}
	if _, err := uuid.Parse(raw); err != nil {
		return reparentReject(http.StatusBadRequest, reparentCodeInvalid,
			"parent_id is not a valid UUID", nil)
	}
	var status string
	err := database.QueryRowContext(ctx,
		`SELECT status::text FROM workspaces WHERE id = $1`, raw).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return reparentReject(http.StatusUnprocessableEntity, reparentCodeNotFound,
			"parent workspace does not exist", map[string]any{"parent_id": raw})
	}
	if err != nil {
		return fmt.Errorf("validate create parent: %w", err)
	}
	if status == "removed" {
		return reparentReject(http.StatusUnprocessableEntity, reparentCodeNotFound,
			"parent workspace has been removed", map[string]any{"parent_id": raw})
	}

	anchor := ""
	if c != nil {
		anchor = c.GetString("org_id")
	}
	if anchor == "" {
		return nil
	}
	parentRoot, rootErr := orgRootID(ctx, database, raw)
	if rootErr != nil {
		if errors.Is(rootErr, errNoOrgRoot) {
			return reparentReject(http.StatusUnprocessableEntity, reparentCodeUnresolvable,
				"the parent's org root cannot be resolved; refusing to create under it",
				map[string]any{"parent_id": raw})
		}
		return fmt.Errorf("validate create parent org: %w", rootErr)
	}
	if !orgAnchorMatchesRoot(anchor, parentRoot) {
		return reparentReject(http.StatusForbidden, reparentCodeCrossOrg,
			"this org token is not authorized to create a workspace under a parent in a different org",
			map[string]any{"parent_id": raw})
	}
	return nil
}

// applyReparent validates and applies a parent_id change as ONE transaction.
//
// CONCURRENCY. Every check runs INSIDE the tx, after the locks taken in step
// 1 — and the lock set is the endpoints PLUS THEIR ORG ROOTS. The org-root
// lock is the mechanism; locking only the two endpoints is NOT sufficient and
// was measured committing a four-node cycle (see step 1 for the disjoint
// A→B2 / B→A2 case and why READ COMMITTED makes a post-hoc check powerless
// against it).
//
// The post-update re-walk (step 9) is a BACKSTOP, not the mechanism: it
// re-derives the org root FROM THE UPDATED ROWS and rolls back unless it is
// the expected one, catching triggers, future column defaults, and anything
// else the pre-checks do not model. It cannot catch a concurrent sibling
// transaction, because under READ COMMITTED it cannot see uncommitted work —
// which is exactly why the serialisation happens up front.
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

	// 0. Find the org root of each endpoint with a BOUNDED, unlocked read, so
	//    we know which root rows to serialise on.
	rootSelfCandidate, selfRootOK, err := reparentOrgRoot(ctx, tx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("reparent: pre-locate source org root: %w", err)
	}
	rootTargetCandidate := ""
	if !wantRoot && target != workspaceID {
		rootTargetCandidate, _, err = reparentOrgRoot(ctx, tx, target)
		if err != nil {
			return nil, fmt.Errorf("reparent: pre-locate target org root: %w", err)
		}
	}

	// 1. Lock the endpoints AND THEIR ORG ROOTS, in one id-ordered statement.
	//
	// Locking only the two endpoints is NOT enough, and the obvious swap test
	// (A→B, B→A) hides it: those two transactions lock the IDENTICAL set
	// {A,B}, so id-ordering serialises them and everything looks fine. The
	// shape that breaks it has DISJOINT lock sets:
	//
	//	R ─┬─ A ── A2        T1: A.parent = B2   locks {A, B2}
	//	   └─ B ── B2        T2: B.parent = A2   locks {B, A2}
	//
	// Nothing is shared, so nothing serialises. Under READ COMMITTED each
	// transaction's post-update re-walk reads the OTHER's pre-commit row,
	// sees a chain that still ends at R, and commits — producing
	// A → B2 → B → A2 → A. Reproduced on the first round against PG16: two
	// individually-legal moves, both 200, one four-node cycle, and the
	// unbounded org-root CTE then never terminates on it.
	//
	// A post-hoc verification step cannot fix this, because under READ
	// COMMITTED there is no post-hoc moment at which either transaction can
	// see the other's uncommitted write. The serialisation has to happen
	// BEFORE the write, on a row both transactions must touch.
	//
	// The org root is that row. Every move takes a FOR UPDATE on the root of
	// the org(s) it involves, so any two moves that could possibly interact
	// contend on one row and run one after the other. The loser then re-runs
	// its descendant check against the winner's COMMITTED tree and is
	// correctly rejected as a cycle. Cross-org contention is nil: different
	// orgs have different roots. Moves are rare and admin-gated, so a
	// per-org write lock costs nothing real.
	lockIDs := []string{workspaceID}
	addLock := func(id string) {
		if id == "" {
			return
		}
		for _, existing := range lockIDs {
			if existing == id {
				return
			}
		}
		lockIDs = append(lockIDs, id)
	}
	if !wantRoot && target != workspaceID {
		addLock(target)
	}
	addLock(rootSelfCandidate)
	addLock(rootTargetCandidate)

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

	// 1a. The step-0 walk was UNLOCKED, so a concurrent move could have
	//     restructured the tree between it and the lock — meaning we may hold
	//     FOR UPDATE on a row that is no longer the org root, i.e. the wrong
	//     mutex. Re-derive under the lock and bail if it moved. Failing the
	//     request is right: the caller retries against a settled tree, and we
	//     never proceed holding a lock that does not serialise us.
	if recheck, ok2, rerr := reparentOrgRoot(ctx, tx, workspaceID); rerr != nil {
		return nil, fmt.Errorf("reparent: re-derive source org root under lock: %w", rerr)
	} else if ok2 != selfRootOK || recheck != rootSelfCandidate {
		return nil, reparentReject(http.StatusConflict, reparentCodeConcurrent,
			"the workspace tree changed while this move was being validated; retry", nil)
	}

	// 1b. A soft-deleted workspace is not an org-chart node. Delete() already
	//     NULLs its children's parent_id, so re-attaching it to a live tree
	//     would resurrect a removed row into the hierarchy — and it is exempt
	//     from workspaces_parent_name_uniq (which is partial on
	//     status != 'removed'), so it could land on a name already taken.
	if self.status == "removed" {
		return nil, reparentReject(http.StatusNotFound, reparentCodeNotFound,
			"workspace has been removed", nil)
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
	// ADOPTION — the one permitted exception to org-root invariance.
	//
	// The first version of this guard refused every root change and listed
	// "demoting a root falls out for free" as a benefit. It is not free: it
	// removed a SHIPPED capability. /registry/register's INSERT
	// (registry.go:1094) omits parent_id, so EVERY self-registering runtime
	// lands as its own parentless org root. Linking one under an existing
	// workspace afterwards is the only way it ever joins an org — root
	// promotion is refused, and create-under-parent is a different row. A
	// blanket refusal stranded those runtimes permanently and reddened
	// tests/e2e/test_poll_mode_e2e.sh, which does exactly this.
	//
	// So adoption is allowed, narrowly, on the terms platform_agent.go's
	// installer already uses (it re-parents old roots and migrates their org
	// anchors in the same transaction — see ensurePlatformAgent):
	//
	//   - the adopted workspace must BE a root (nothing else can change org)
	//   - it must be CHILDLESS. That is the line between org ASSIGNMENT of an
	//     unaffiliated workspace and an org MERGE. Adopting a root WITH a
	//     subtree silently re-homes every descendant — changing their
	//     org:<root>, their sameOrg() reachability and their plugin allowlist
	//     — workspaces the caller never named. platform_agent may do that
	//     because it is a one-time install operation on its own tenant; a
	//     client-callable endpoint may not.
	//   - its org anchors move WITH it, in this transaction (below), so
	//     nothing is left pointing at a row that is no longer a root.
	//
	// What the adopted workspace gains is real and is reported in the
	// response: it loses org WRITE (root-only) and its own team/org
	// namespaces, and gains read of the destination org's corpus. That is
	// inherent in joining an org, and it is admin-gated and audited.
	// WHY THE CHILDLESS CHECK CANNOT BE RACED. A concurrent
	// `INSERT INTO workspaces (... parent_id = <this workspace>)` must take
	// `FOR KEY SHARE` on the parent row to satisfy workspaces_parent_id_fkey.
	// Step 1 already holds `FOR UPDATE` on that same row (it is the adoptee,
	// and it is its own org root), and FOR UPDATE conflicts with FOR KEY
	// SHARE — so the inserting transaction blocks until this one commits, and
	// a child cannot appear between the count below and the write. Verified
	// with a 250-round probe (see the ChildlessCheckCannotBeRaced test).
	//
	// NOTE: a two-workspace org can never be adopted as a unit — its root
	// always has a child, so the childless arm refuses it, and the child is
	// not a root so it takes the ordinary same-org path. That is intended:
	// moving such a pair is an org MERGE, which is what this refuses.
	adoption := false
	if oldRoot != targetRoot {
		if oldParent == "" {
			var childCount int
			if err := tx.QueryRowContext(ctx,
				`SELECT count(*) FROM workspaces WHERE parent_id = $1 AND status != 'removed'`,
				workspaceID).Scan(&childCount); err != nil {
				return nil, fmt.Errorf("reparent: count children for adoption: %w", err)
			}
			if childCount > 0 {
				return nil, reparentReject(http.StatusUnprocessableEntity, reparentCodeCrossOrg,
					"this workspace is an org root WITH descendants; adopting it would move its entire subtree into "+
						"another org, changing the org memories, peers and plugin allowlist of workspaces you did not name. "+
						"Move or detach its children first.",
					map[string]any{"org_root_id": oldRoot, "target_org_root_id": targetRoot,
						"parent_id": target, "descendant_count": childCount})
			}
			adoption = true
		} else {
			return nil, reparentReject(http.StatusUnprocessableEntity, reparentCodeCrossOrg,
				"cross-org re-parent is not permitted: the target parent belongs to a different org root. "+
					"The org boundary IS the parent_id chain, so this move would change which org's memories, peers, "+
					"delegation targets and plugin allowlist this workspace resolves to.",
				map[string]any{"org_root_id": oldRoot, "target_org_root_id": targetRoot, "parent_id": target})
		}
	}
	// The root this workspace must resolve to AFTER the write. Same as before
	// for an ordinary move; the destination's root for an adoption.
	expectedRootAfter := oldRoot
	if adoption {
		expectedRootAfter = targetRoot
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

	// 8b. Adoption only: move the org anchors WITH the workspace, in this
	//     transaction. Mirrors ensurePlatformAgent (platform_agent.go), which
	//     is the existing precedent for re-homing a root — including its
	//     ON CONFLICT DO NOTHING dedup, because org_plugin_allowlist is
	//     UNIQUE(org_id, plugin_name) and the destination org may already
	//     allow the same plugin.
	//
	//     Without this, an adopted root's org_api_tokens.org_id and
	//     org_plugin_allowlist.org_id keep pointing at a workspace that is no
	//     longer a root — an org anchor for an org that no longer exists.
	if adoption {
		if _, err := tx.ExecContext(ctx,
			`UPDATE org_api_tokens SET org_id = $1 WHERE org_id = $2`, targetRoot, workspaceID); err != nil {
			return nil, fmt.Errorf("reparent: migrate org_api_tokens: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO org_plugin_allowlist (org_id, plugin_name, enabled_by, enabled_at)
			SELECT $1, plugin_name, enabled_by, enabled_at
			FROM org_plugin_allowlist
			WHERE org_id = $2
			ON CONFLICT (org_id, plugin_name) DO NOTHING
		`, targetRoot, workspaceID); err != nil {
			return nil, fmt.Errorf("reparent: migrate org_plugin_allowlist: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM org_plugin_allowlist WHERE org_id = $1`, workspaceID); err != nil {
			return nil, fmt.Errorf("reparent: clear old org_plugin_allowlist: %w", err)
		}
		// requests.org_id is deliberately NOT migrated.
		//
		// It is a LISTING FILTER, not an ACL — ListPendingForOrg uses it to
		// populate the org's "all agents' incoming" tab. Leaving it stamped
		// with the old root can only HIDE the adoptee's historical requests
		// from the new org's tab; it can never expose anything, because no
		// query reaches rows through an org_id nobody resolves to any more.
		//
		// Migrating would be the riskier choice: it would surface requests
		// raised while the workspace was its OWN org into the adopting org's
		// shared inbox. That is the same call made for memory — history stays
		// with the org it was created under — and it fails in the safe
		// direction.
	}

	// 9. Post-update verification, from the UPDATED rows, still inside the tx.
	//    Anything that got past the pre-checks — a trigger, a future column
	//    default, a concurrent write we failed to serialise — has to survive
	//    this or the whole thing rolls back. This is the difference between
	//    "we checked before writing" and "the committed state is provably
	//    sane". It is a BACKSTOP, not the concurrency mechanism: under READ
	//    COMMITTED it cannot see an uncommitted sibling transaction, which is
	//    why step 1 serialises on the org root instead of relying on this.
	postRoot, ok, err := reparentOrgRoot(ctx, tx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("reparent: post-verify: %w", err)
	}
	if !ok {
		return nil, reparentReject(http.StatusConflict, reparentCodeCycle,
			"post-update verification failed: the parent_id chain no longer terminates at an org root; the move was rolled back", nil)
	}
	if postRoot != expectedRootAfter {
		return nil, reparentReject(http.StatusUnprocessableEntity, reparentCodeCrossOrg,
			"post-update verification failed: the org root is not the expected one; the move was rolled back",
			map[string]any{"expected_org_root_id": expectedRootAfter, "post_move_org_root_id": postRoot})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("reparent: commit: %w", err)
	}

	out := &reparentOutcome{
		Changed:   true,
		Adopted:   adoption,
		OldParent: oldParent,
		NewParent: target,
		OrgRoot:   expectedRootAfter,
		Gained:    []string{"team:" + target},
	}
	if adoption {
		// It WAS a root, so resolver.go gave it team:<self> and a WRITABLE
		// org:<self>. Both go away; it picks up the destination team and a
		// read-only org. Spelling out all four is the point — this is the
		// largest privilege delta the endpoint can produce.
		out.Lost = []string{"team:" + workspaceID, "org:" + workspaceID + " (writable)"}
		out.Gained = append(out.Gained, "org:"+targetRoot+" (read-only)")
	} else {
		out.Lost = []string{"team:" + oldParent}
	}
	return out, nil
}
