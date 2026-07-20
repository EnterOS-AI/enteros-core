package handlers

// approval_gate.go — server-side gate for destructive org operations.
// (RFC docs/design/rfc-platform-agent.md — Phase 4)
//
// requireApproval is the choke point a destructive handler calls before
// executing. It is the trust boundary: the platform-management MCP is a CLIENT
// of these handlers, so enforcing here (not in the MCP) means anything holding
// an org-admin token still goes through the gate. The flow:
//
//   - if a matching APPROVED + unconsumed approval exists, consume it (single-
//     use) and let the operation proceed;
//   - otherwise create (or reuse) a PENDING approval, broadcast it to the canvas
//     (and escalate to the parent if any), and the handler returns HTTP 202 so a
//     human can decide. The agent retries after approval and the gate passes.
//
// Matching is by (workspace_id, action, request_hash) where request_hash is a
// stable digest of the operation + its context, so a retried op reuses its own
// request instead of flooding the table, and an approval for "delete ws A"
// cannot be replayed to "delete ws B".

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/approvals"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"github.com/gin-gonic/gin"
)

// approvalRequestHash is a stable digest of the gated operation. Go's
// json.Marshal sorts map keys, so the same context always hashes the same.
func approvalRequestHash(workspaceID, action string, contextMap map[string]interface{}) string {
	cj, err := json.Marshal(contextMap)
	if err != nil || cj == nil {
		cj = []byte("{}")
	}
	sum := sha256.Sum256([]byte(workspaceID + "\x00" + action + "\x00" + string(cj)))
	return hex.EncodeToString(sum[:])
}

// requireApproval returns (approved=true, consumedID) when a matching approval
// exists and was just consumed; otherwise it creates/reuses a pending approval
// and returns (false, pendingID). A non-nil error is a server error.
func requireApproval(ctx context.Context, b events.EventEmitter, workspaceID string, action approvals.Action, reason string, contextMap map[string]interface{}) (bool, string, error) {
	hash := approvalRequestHash(workspaceID, string(action), contextMap)

	// 1. Atomically consume an approved + unconsumed request, if one exists.
	//    The conditional UPDATE ... RETURNING makes consumption race-safe: two
	//    concurrent destructive calls cannot both consume the same approval.
	var consumedID string
	err := db.DB.QueryRowContext(ctx, `
		UPDATE approval_requests SET consumed_at = now()
		WHERE id = (
			SELECT id FROM approval_requests
			WHERE workspace_id = $1 AND action = $2 AND request_hash = $3
			  AND status = 'approved' AND consumed_at IS NULL
			ORDER BY decided_at DESC NULLS LAST
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id
	`, workspaceID, string(action), hash).Scan(&consumedID)
	if err == nil {
		return true, consumedID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, "", fmt.Errorf("consume approval: %w", err)
	}

	// 2. No usable approval — create a pending one, or reuse an existing pending
	//    request for the same operation so retries don't flood the table.
	cj, mErr := json.Marshal(contextMap)
	if mErr != nil || cj == nil {
		cj = []byte("{}")
	}
	var approvalID string
	err = db.DB.QueryRowContext(ctx, `
		WITH existing AS (
			SELECT id FROM approval_requests
			WHERE workspace_id = $1 AND action = $2 AND request_hash = $3 AND status = 'pending'
			LIMIT 1
		), ins AS (
			INSERT INTO approval_requests (workspace_id, action, reason, context, request_hash)
			SELECT $1, $2, $4, $5::jsonb, $3
			WHERE NOT EXISTS (SELECT 1 FROM existing)
			RETURNING id
		)
		SELECT id FROM ins UNION ALL SELECT id FROM existing LIMIT 1
	`, workspaceID, string(action), hash, reason, string(cj)).Scan(&approvalID)
	if err != nil {
		return false, "", fmt.Errorf("create approval: %w", err)
	}

	// Broadcast to the canvas (the user-facing signal). For a platform agent the
	// parent_id is NULL, so the requested-event on its own workspace IS the user
	// prompt; ordinary workspaces also escalate to their parent.
	//
	// b may be nil: stateless handlers (e.g. org-token mint — OrgTokenHandler is
	// an empty struct with no broadcaster) still gate; they just can't push a
	// live canvas event. The pending approval row is persisted regardless, so
	// the request is never lost — only the notification is skipped.
	if b != nil {
		if bErr := b.RecordAndBroadcast(ctx, string(events.EventApprovalRequested), workspaceID, map[string]interface{}{
			"approval_id": approvalID,
			"action":      string(action),
			"reason":      reason,
		}); bErr != nil {
			log.Printf("approval_gate: broadcast requested failed (ws=%s): %v", workspaceID, bErr)
		}
	}
	var parentID *string
	if pErr := db.DB.QueryRowContext(ctx, `SELECT parent_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&parentID); pErr != nil {
		log.Printf("approval_gate: parent lookup failed (ws=%s): %v", workspaceID, pErr)
	}
	if parentID != nil && b != nil {
		if bErr := b.RecordAndBroadcast(ctx, string(events.EventApprovalEscalated), *parentID, map[string]interface{}{
			"approval_id":       approvalID,
			"from_workspace_id": workspaceID,
			"action":            string(action),
			"reason":            reason,
		}); bErr != nil {
			log.Printf("approval_gate: broadcast escalated failed (ws=%s): %v", workspaceID, bErr)
		}
	}
	return false, approvalID, nil
}

// gateDestructive runs requireApproval for a gated action and, when approval is
// still pending, writes the 202 response and returns false (caller must stop).
// Returns true when the caller may proceed (action consumed an approval).
func gateDestructive(c *gin.Context, b events.EventEmitter, workspaceID string, action approvals.Action, reason string, contextMap map[string]interface{}) bool {
	if !approvals.IsGated(action) {
		return true
	}
	// Scope (RFC platform-agent Phase 4b, hardened by core#2574). The
	// activation policy lives here, centrally, so it is uniform and testable:
	//   - admin-token callers (Tier 2b ADMIN_TOKEN bootstrap + Tier 3
	//     workspace-token fallback) are ALWAYS gated when the action is
	//     gated — the rollout flag does NOT bypass them. This closes the
	//     core#2574 privilege-escalation hole: the concierge agent holds
	//     the tenant ADMIN_TOKEN and was minting org tokens + writing
	//     secrets with ZERO pending approvals because the old code only
	//     checked for org_token_id, which admin-token callers never set.
	//     Admin-token-bearing agents are EXACTLY the human-in-the-loop
	//     bypass risk the RFC Phase 4 was supposed to prevent.
	//   - org-token callers (Tier 2a, user-minted via canvas UI) follow
	//     the rollout flag (default-OFF; set MOLECULE_PLATFORM_APPROVAL_GATE=1
	//     to enable). The default-off posture protects existing org-token
	//     automation from a surprise async-approval behaviour change.
	//   - non-agent callers (workspace tokens, session cookies) bypass
	//     entirely — ordinary operation is byte-identical.
	isAdminToken := callerIsAdminToken(c)
	if !isAdminToken {
		if !destructiveGateEnabled() {
			return true
		}
		if !callerHoldsOrgToken(c) {
			return true
		}
	}
	approved, approvalID, err := requireApproval(c.Request.Context(), b, workspaceID, action, reason, contextMap)
	if err != nil {
		log.Printf("gateDestructive: %v (ws=%s action=%s)", err, workspaceID, action)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "approval gate failed"})
		return false
	}
	if !approved {
		c.JSON(http.StatusAccepted, gin.H{
			"status":      "pending_approval",
			"approval_id": approvalID,
			"action":      string(action),
			"reason":      reason,
		})
		return false
	}
	return true
}

// destructiveGateEnabled is the default-off rollout flag for the org-level
// destructive-op approval gate. Inert until an operator sets
// MOLECULE_PLATFORM_APPROVAL_GATE=1 (or "true") — typically when the platform
// agent is deployed to the org. Keeps 4b's wiring shipped-but-dormant, matching
// the platform-agent feature's default-off posture (3a/3c).
//
// (core#2574) The flag is now an ORG-TOKEN-ONLY switch: the admin-token
// path is gated regardless of the flag (see gateDestructive).
func destructiveGateEnabled() bool {
	v := os.Getenv("MOLECULE_PLATFORM_APPROVAL_GATE")
	return v == "1" || v == "true"
}

// callerHoldsOrgToken reports whether the request authenticated with an org
// token (the auth middleware sets org_token_id, see middleware/wsauth_middleware.go).
// The platform agent uses an org-admin token; ordinary workspace-token agents
// and human CP sessions do not, so they bypass the gate entirely.
func callerHoldsOrgToken(c *gin.Context) bool {
	_, ok := c.Get("org_token_id")
	return ok
}

// callerIsAdminToken reports whether the request authenticated with the
// tenant ADMIN_TOKEN (Tier 2b) or the Tier 3 workspace-token fallback
// (which is admin-equivalent — closes #684 if the operator forgot to
// set ADMIN_TOKEN). The auth middleware sets caller_is_admin_token in
// both cases.
//
// (core#2574) This is the missing context that closes the privilege-
// escalation bypass: the concierge agent holds ADMIN_TOKEN (NOT an
// org token) and was minting org tokens + writing secrets with ZERO
// pending approvals because callerHoldsOrgToken returned false for it
// and the gate's rollout flag was off. The fix makes gateDestructive
// always gate admin-token callers regardless of the rollout flag.
func callerIsAdminToken(c *gin.Context) bool {
	v, ok := c.Get("caller_is_admin_token")
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// ── Scoped single-use grant for PRIVILEGED / boundary-crossing delegations ──
//
// FIX-1 (single-use grant) + FIX-2 (proof-gated / verify-before-act) for the
// privileged-delegation subset. This is deliberately NOT applied to routine
// intra-org sibling A2A: a workspace-token peer delegating to a sibling via
// list_peers holds neither an admin token nor an org token, so it never enters
// this gate and keeps flowing with no grant. ONLY a privileged caller — the
// concierge/platform agent holding the tenant ADMIN_TOKEN, or an org-token
// caller — crosses the trust boundary this gate protects.
//
// The gate is a strict superset of "do nothing" until an operator arms it:
// privilegedDelegationGateEnabled() is default-OFF, so with no env set the gate
// is byte-identical to prior behaviour (every delegation proceeds). It is
// FULLY flag-gated — including the admin-token path — because, unlike the
// destructive-op gate (which has a live human-approval subsystem), there is not
// yet a grant-minting UX for delegations; arming it unconditionally would strand
// the concierge with no way to obtain a grant. This mirrors the shipped-dormant
// posture of MOLECULE_PLATFORM_APPROVAL_GATE (core RFC platform-agent 4b).

// privilegedDelegationGateEnabled is the default-off rollout flag for the
// privileged-delegation single-use grant gate. Inert until an operator sets
// MOLECULE_PRIVILEGED_DELEGATION_GATE=1 (or "true"). While off, the gate is a
// no-op and all delegations proceed exactly as before.
func privilegedDelegationGateEnabled() bool {
	v := os.Getenv("MOLECULE_PRIVILEGED_DELEGATION_GATE")
	return v == "1" || v == "true"
}

// delegationRequiresGrant reports whether THIS caller's delegation is a
// privileged / boundary-crossing handoff that must present a single-use grant.
// It mirrors gateDestructive's activation policy: admin-token and org-token
// callers are the privileged surface; workspace-token, canvas-session, and
// system callers (routine intra-org sibling A2A, the busy/offline queue+wake
// path, self-host) are NEVER privileged and pass through untouched.
func delegationRequiresGrant(c *gin.Context) bool {
	if !privilegedDelegationGateEnabled() {
		return false
	}
	return callerIsAdminToken(c) || callerHoldsOrgToken(c)
}

// privilegedDelegationContext binds a grant to a SPECIFIC (target, task) so an
// approved grant for "delegate task T to A" cannot be replayed to a different
// target or a different task. The task is digested (not stored raw) to keep the
// approval context bounded; requireApproval folds this map into request_hash.
func privilegedDelegationContext(targetID, task string) map[string]interface{} {
	sum := sha256.Sum256([]byte(task))
	return map[string]interface{}{
		"target_id": targetID,
		"task_hash": hex.EncodeToString(sum[:]),
	}
}

// gatePrivilegedDelegation is the choke point the delegation authorize path
// calls BEFORE any side effect (row insert, A2A dispatch, broadcast). For a
// privileged caller it requires — and atomically CONSUMES (single-use) — a
// matching approved grant, reusing requireApproval's verify-before-act
// consumption. Because the verification happens before the act, a consequential
// handoff cannot proceed to (or report) completion without the verified
// precondition (FIX-2). Returns true when the caller may proceed:
//   - not a privileged caller (or gate dormant) → true, no DB touch, no grant
//     required — routine A2A is unaffected;
//   - privileged + a grant was consumed → true;
//   - privileged + no consumable grant → writes 403 (a pending grant request is
//     recorded so a human can approve it) and returns false;
//   - server error → writes 500 and returns false.
func gatePrivilegedDelegation(c *gin.Context, b events.EventEmitter, sourceID, targetID, task string) bool {
	if !delegationRequiresGrant(c) {
		return true
	}
	approved, grantID, err := requireApproval(
		c.Request.Context(), b, sourceID,
		approvals.ActionPrivilegedDelegate,
		"privileged delegation to "+targetID,
		privilegedDelegationContext(targetID, task),
	)
	if err != nil {
		log.Printf("gatePrivilegedDelegation: %v (ws=%s target=%s)", err, sourceID, targetID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "delegation grant gate failed"})
		return false
	}
	if !approved {
		log.Printf("gatePrivilegedDelegation: rejected privileged delegation without grant (ws=%s target=%s grant_request=%s)", sourceID, targetID, grantID)
		c.JSON(http.StatusForbidden, gin.H{
			"error":            "privileged delegation requires an approved single-use grant",
			"action":           string(approvals.ActionPrivilegedDelegate),
			"grant_request_id": grantID,
		})
		return false
	}
	return true
}
