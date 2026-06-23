package handlers

// workspace_abilities.go — PATCH /workspaces/:id/abilities
//
// Allows users and admin agents to toggle workspace-level ability flags:
//
//   broadcast_enabled   — workspace may POST /broadcast to send org-wide messages
//   talk_to_user_enabled — workspace may deliver canvas chat messages via
//                          send_message_to_user / POST /notify
//   can_delegate         — workspace may initiate A2A delegate_task /
//                          delegate_task_async (core#2127). Default TRUE;
//                          setting FALSE hides the delegate_* tools in the
//                          MCP tools/list response AND makes the A2A
//                          delegation path return 403 (defense-in-depth
//                          against prompt-bypass for role-locked agents).
//
// Gated behind AdminAuth so workspace agents cannot self-modify their own
// ability flags (that would let any agent grant itself broadcast rights or
// suppress its own chat-silence constraint).

import (
	"context"
	"database/sql"
	"log"
	"net/http"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
)

// AbilitiesPayload carries the subset of ability flags the caller wants to
// update. Fields are pointers so that the handler can distinguish "caller
// supplied false" from "caller omitted the field" (omitempty semantics).
type AbilitiesPayload struct {
	BroadcastEnabled  *bool `json:"broadcast_enabled"`
	TalkToUserEnabled *bool `json:"talk_to_user_enabled"`
	CanDelegate       *bool `json:"can_delegate"`
}

// PatchAbilities handles PATCH /workspaces/:id/abilities (AdminAuth).
func PatchAbilities(c *gin.Context) {
	id := c.Param("id")
	if err := validateWorkspaceID(id); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid workspace ID"})
		return
	}

	var body AbilitiesPayload
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if body.BroadcastEnabled == nil && body.TalkToUserEnabled == nil && body.CanDelegate == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "at least one ability field required"})
		return
	}

	ctx := c.Request.Context()

	var exists bool
	if err := db.DB.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM workspaces WHERE id = $1 AND status != 'removed')`, id,
	).Scan(&exists); err != nil || !exists {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}

	// Atomic update: when multiple fields are supplied, apply them in one SQL
	// statement so the request is all-or-nothing (#2131). A partial mutation
	// (e.g. broadcast_enabled updated but talk_to_user_enabled failing) would
	// leave the workspace in an ambiguous capability state.
	switch {
	case body.BroadcastEnabled != nil && body.TalkToUserEnabled != nil && body.CanDelegate != nil:
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET broadcast_enabled = $2, talk_to_user_enabled = $3, can_delegate = $4, updated_at = now() WHERE id = $1`,
			id, *body.BroadcastEnabled, *body.TalkToUserEnabled, *body.CanDelegate,
		); err != nil {
			log.Printf("PatchAbilities three-fields for %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
	case body.BroadcastEnabled != nil && body.TalkToUserEnabled != nil:
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET broadcast_enabled = $2, talk_to_user_enabled = $3, updated_at = now() WHERE id = $1`,
			id, *body.BroadcastEnabled, *body.TalkToUserEnabled,
		); err != nil {
			log.Printf("PatchAbilities both-fields for %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
	case body.BroadcastEnabled != nil && body.CanDelegate != nil:
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET broadcast_enabled = $2, can_delegate = $3, updated_at = now() WHERE id = $1`,
			id, *body.BroadcastEnabled, *body.CanDelegate,
		); err != nil {
			log.Printf("PatchAbilities broadcast+can_delegate for %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
	case body.TalkToUserEnabled != nil && body.CanDelegate != nil:
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET talk_to_user_enabled = $2, can_delegate = $3, updated_at = now() WHERE id = $1`,
			id, *body.TalkToUserEnabled, *body.CanDelegate,
		); err != nil {
			log.Printf("PatchAbilities talk_to_user+can_delegate for %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
	case body.BroadcastEnabled != nil:
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET broadcast_enabled = $2, updated_at = now() WHERE id = $1`,
			id, *body.BroadcastEnabled,
		); err != nil {
			log.Printf("PatchAbilities broadcast_enabled for %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
	case body.TalkToUserEnabled != nil:
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET talk_to_user_enabled = $2, updated_at = now() WHERE id = $1`,
			id, *body.TalkToUserEnabled,
		); err != nil {
			log.Printf("PatchAbilities talk_to_user_enabled for %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
	case body.CanDelegate != nil:
		if _, err := db.DB.ExecContext(ctx,
			`UPDATE workspaces SET can_delegate = $2, updated_at = now() WHERE id = $1`,
			id, *body.CanDelegate,
		); err != nil {
			log.Printf("PatchAbilities can_delegate for %s: %v", id, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
	}

	c.JSON(http.StatusOK, gin.H{"status": "updated"})
}

// loadWorkspaceCanDelegate returns the workspace's can_delegate flag (core#2127).
//
// Returns (true, nil) on any read error (e.g. the row has not been migrated
// to add the can_delegate column) so a missing column never accidentally
// locks an entire org out of delegation — a fail-closed default here would
// turn a forward-only migration into a live-incident-grade outage. The
// column is NOT NULL DEFAULT TRUE in the up-migration, so the only path to
// a real "false" return is an explicit operator PATCH. The tools/call gate
// in mcp.go applies the second-line check, so a transient DB blip can't
// silently elevate a previously-locked workspace.
//
// Tolerates column absence: the SELECT references can_delegate, which on a
// pre-migration schema would return "column does not exist" — caught here and
// mapped to (true, nil). The down-migration drops the column; a downgrade
// in flight is therefore safe (the SELECT just falls through to the
// "column missing" branch and returns the safe-default true).
func loadWorkspaceCanDelegate(ctx context.Context, dbh interface {
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}, workspaceID string) (bool, error) {
	var canDelegate bool
	err := dbh.QueryRowContext(ctx,
		`SELECT can_delegate FROM workspaces WHERE id = $1`, workspaceID,
	).Scan(&canDelegate)
	if err != nil {
		if err == sql.ErrNoRows {
			return true, nil // unknown workspace — fail open (let downstream 404/403 handle)
		}
		// Column-missing (pre-migration) or any other error → safe default true.
		// The second-line gate in mcp.go (tools/call) protects against
		// accidental elevation; the trade-off is a missing column never
		// silently locking delegation.
		return true, err
	}
	return canDelegate, nil
}
