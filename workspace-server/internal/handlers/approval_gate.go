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
func requireApproval(ctx context.Context, b *events.Broadcaster, workspaceID string, action approvals.Action, reason string, contextMap map[string]interface{}) (bool, string, error) {
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
	if bErr := b.RecordAndBroadcast(ctx, string(events.EventApprovalRequested), workspaceID, map[string]interface{}{
		"approval_id": approvalID,
		"action":      string(action),
		"reason":      reason,
	}); bErr != nil {
		log.Printf("approval_gate: broadcast requested failed (ws=%s): %v", workspaceID, bErr)
	}
	var parentID *string
	if pErr := db.DB.QueryRowContext(ctx, `SELECT parent_id FROM workspaces WHERE id = $1`, workspaceID).Scan(&parentID); pErr != nil {
		log.Printf("approval_gate: parent lookup failed (ws=%s): %v", workspaceID, pErr)
	}
	if parentID != nil {
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
func gateDestructive(c *gin.Context, b *events.Broadcaster, workspaceID string, action approvals.Action, reason string, contextMap map[string]interface{}) bool {
	if !approvals.IsGated(action) {
		return true
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
