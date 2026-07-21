package handlers

// admin_plugin_drift.go — admin endpoints for plugin version-subscription drift queue
// (core#123).
//
// Routes:
//   GET  /admin/plugin-updates-pending  — list all pending drift entries
//   POST /admin/plugin-updates/:id/apply — apply a queued drift update

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/plugins"
	"github.com/gin-gonic/gin"
)

// AdminPluginDriftHandler handles admin endpoints for the plugin drift queue.
type AdminPluginDriftHandler struct {
	pluginsHandler *PluginsHandler // used to re-trigger plugin install on apply
}

// NewAdminPluginDriftHandler constructs a handler wired to the plugins handler.
func NewAdminPluginDriftHandler(ph *PluginsHandler) *AdminPluginDriftHandler {
	return &AdminPluginDriftHandler{pluginsHandler: ph}
}

// FragmentChanged handles POST /admin/plugin-fragment-changed.
//
// Fix (c): the trigger a fragment repo fires (via the CP fleet fan-out) on
// merge-to-main so a changed plugin fragment propagates PROMPTLY to running
// concierges instead of waiting for the next natural online-beat reconcile. For
// each reconcilable workspace DECLARING the plugin it fires the (content-aware,
// fix (b)) reconcile to deliver the new bytes, then — ONLY for a box whose
// fragment was actually stale AND whose reconcile deferred the restart (the
// concierge lifecycle guard) — restarts it so the running mgmt-MCP relaunches
// with the new command/env. Non-concierge plugins already get their restart
// from the reconcile's own classifier, so they are never double-restarted.
//
// Fire-and-forget per workspace (202 Accepted): a slow/offline box never blocks
// the response, and the natural online-beat reconcile still backstops any box
// this trigger misses (webhook = latency optimization, not a correctness
// dependency).
func (h *AdminPluginDriftHandler) FragmentChanged(c *gin.Context) {
	var req struct {
		PluginName string `json:"plugin_name"`
		Source     string `json:"source"` // optional, informational
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.PluginName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "plugin_name is required"})
		return
	}
	if h.pluginsHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "plugins handler not wired"})
		return
	}

	ctx := c.Request.Context()
	pluginName := strings.TrimSpace(req.PluginName)
	workspaceIDs, err := listWorkspacesDeclaringPlugin(ctx, pluginName)
	if err != nil {
		log.Printf("AdminPluginDrift: fragment-changed: list workspaces declaring %q failed: %v", pluginName, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve affected workspaces"})
		return
	}

	for _, id := range workspaceIDs {
		wsID := id
		// Detach: the request ctx ends when this handler returns, well before a
		// clone+deliver(+restart) completes. Mirror registry.fireReconcileOnline.
		globalGoAsync(func() {
			bg := context.WithoutCancel(ctx)
			// Capture BEFORE the reconcile re-records the SHA: was this box's
			// fragment actually stale, and did the reconcile defer its restart
			// (concierge lifecycle guard)? Both must hold for us to restart —
			// a non-concierge stale plugin is restarted by the reconcile itself,
			// and an unchanged box needs no restart at all.
			stale := h.pluginsHandler.PluginFragmentStaleForWorkspace(bg, wsID, pluginName)
			restartDeferred := platformConciergeReconcileShouldSkipRestart(bg, wsID)

			h.pluginsHandler.ReconcileWorkspacePlugins(bg, wsID)

			if stale && restartDeferred {
				if restart := h.pluginsHandler.GetRestartFunc(); restart != nil {
					log.Printf("AdminPluginDrift: fragment-changed: restarting workspace=%s to pick up updated fragment %s", wsID, pluginName)
					restart(wsID)
				}
			}
		})
	}

	log.Printf("AdminPluginDrift: fragment-changed plugin=%s — reconciling %d workspace(s)", pluginName, len(workspaceIDs))
	c.JSON(http.StatusAccepted, gin.H{
		"plugin_name": pluginName,
		"reconciling": len(workspaceIDs),
	})
}

// ListPending handles GET /admin/plugin-updates-pending.
//
// Returns a JSON array of pending drift entries, newest-first. Empty array
// means no plugins have drifted since the last sweep cycle.
func (h *AdminPluginDriftHandler) ListPending(c *gin.Context) {
	rows, err := plugins.ListPendingUpdates(c.Request.Context())
	if err != nil {
		log.Printf("AdminPluginDrift: ListPendingUpdates failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list pending updates"})
		return
	}
	c.JSON(http.StatusOK, rows)
}

// Apply handles POST /admin/plugin-updates/:id/apply.
//
//  1. Reads the queue entry and verifies it's still pending.
//  2. Reads the workspace_plugins row to get the plugin's source.
//  3. Re-installs the plugin from source_raw (re-fetch from upstream at the
//     same tracked ref — the drift was caused by upstream moving).
//  4. Marks the queue entry as applied.
//  5. Triggers workspace restart.
//
// Idempotent: if the entry is already 'applied', returns 200 with the
// workspace_id and plugin_name so callers can still poll for confirmation.
func (h *AdminPluginDriftHandler) Apply(c *gin.Context) {
	queueID := c.Param("id")
	if queueID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "queue id is required"})
		return
	}

	ctx := c.Request.Context()

	// Step 1: read and lock the queue entry.
	var entry struct {
		WorkspaceID string `json:"workspace_id"`
		PluginName  string `json:"plugin_name"`
		TrackedRef  string `json:"tracked_ref"`
		Status      string `json:"status"`
	}
	err := db.DB.QueryRowContext(ctx, `
		SELECT workspace_id, plugin_name, tracked_ref, status
		  FROM plugin_update_queue
		 WHERE id = $1
	`, queueID).Scan(&entry.WorkspaceID, &entry.PluginName, &entry.TrackedRef, &entry.Status)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("queue entry %s not found", queueID)})
		return
	}
	if err != nil {
		log.Printf("AdminPluginDrift: apply: query queue entry: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read queue entry"})
		return
	}

	if entry.Status == "applied" {
		// Idempotent — already applied.
		c.JSON(http.StatusOK, gin.H{
			"status":       "already_applied",
			"workspace_id": entry.WorkspaceID,
			"plugin_name":  entry.PluginName,
			"message":      "drift update was already applied",
		})
		return
	}

	if entry.Status == "dismissed" {
		c.JSON(http.StatusConflict, gin.H{
			"error":        "queue entry was dismissed",
			"workspace_id": entry.WorkspaceID,
			"plugin_name":  entry.PluginName,
		})
		return
	}

	// Step 2: read the workspace_plugins row to get source_raw.
	var sourceRaw string
	err = db.DB.QueryRowContext(ctx, `
		SELECT source_raw FROM workspace_plugins
		 WHERE workspace_id = $1 AND plugin_name = $2
	`, entry.WorkspaceID, entry.PluginName).Scan(&sourceRaw)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusNotFound, gin.H{
			"error":        "workspace_plugins row not found — plugin may have been uninstalled",
			"workspace_id": entry.WorkspaceID,
			"plugin_name":  entry.PluginName,
		})
		return
	}
	if err != nil {
		log.Printf("AdminPluginDrift: apply: query workspace_plugins: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read plugin record"})
		return
	}

	// Step 3: re-install the plugin.
	//
	// We call h.pluginsHandler.Install indirectly via a subrequest to reuse
	// the full install pipeline (resolve → stage → deliver → record).
	// Construct the apply installRequest: same source as the existing row,
	// same tracked_ref. The source_raw already encodes the pinned ref
	// (e.g. "github://owner/repo#tag:v1.0.0"), so the resolver fetches
	// the latest commit at that ref — the drift.
	installReq := installRequest{
		Source: sourceRaw,
		Track:  entry.TrackedRef,
	}
	result, instErr := h.pluginsHandler.ResolveAndStageForApply(ctx, installReq)
	if instErr != nil {
		var he *httpErr
		if errors.As(instErr, &he) {
			c.JSON(he.Status, gin.H{
				"error":    fmt.Sprintf("plugin install failed: %v", he.Body["error"]),
				"queue_id": queueID,
			})
			return
		}
		log.Printf("AdminPluginDrift: apply: install failed for %s/%s: %v",
			entry.WorkspaceID, entry.PluginName, instErr)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin install failed", "queue_id": queueID})
		return
	}
	defer func() { _ = os.RemoveAll(result.StagedDir) }()

	// Deliver to the workspace container. Docker-less tenant (#206): the
	// docker-push is RETIRED — errNoPushTarget means "deliver by pull". The
	// declared row already carries the tracked ref, so the restart in step 5 lets
	// the boot materializer pull the new commit. Skip the push and keep going
	// (record the new SHA + restart below); no bytes copied in.
	if err := h.pluginsHandler.DeliverForApply(ctx, entry.WorkspaceID, result); err != nil {
		if errors.Is(err, errNoPushTarget) {
			log.Printf("AdminPluginDrift: apply: docker-less workspace %s/%s — retiring docker-push, re-materialize on restart (pull)",
				entry.WorkspaceID, entry.PluginName)
		} else {
			var he *httpErr
			if errors.As(err, &he) {
				c.JSON(he.Status, gin.H{"error": fmt.Sprintf("plugin deliver failed: %v", he.Body["error"]), "queue_id": queueID})
				return
			}
			log.Printf("AdminPluginDrift: apply: deliver failed for %s/%s: %v",
				entry.WorkspaceID, entry.PluginName, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "plugin deliver failed", "queue_id": queueID})
			return
		}
	}

	// Record the install with the new SHA. This updates installed_sha on the
	// workspace_plugins row so the next drift sweep finds no drift.
	if err := recordWorkspacePluginInstall(ctx, entry.WorkspaceID, result.PluginName,
		result.Source.Raw(), entry.TrackedRef, result.InstalledSHA); err != nil {
		log.Printf("AdminPluginDrift: apply: recordWorkspacePluginInstall failed: %v (install succeeded)", err)
		// Non-fatal: the plugin IS installed; just log and continue.
	}

	// Step 4: mark queue entry as applied.
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE plugin_update_queue SET status = 'applied' WHERE id = $1
	`, queueID); err != nil {
		log.Printf("AdminPluginDrift: apply: failed to mark queue entry %s as applied: %v", queueID, err)
		// Non-fatal: install succeeded; operator can retry or mark manually.
	}

	// Step 5: trigger workspace restart — UNLESS the target is the self-host
	// platform concierge in its fragile lifecycle window (self-brick guard).
	restarting := h.applyRestartAfterDrift(ctx, entry.WorkspaceID)

	log.Printf("AdminPluginDrift: applied drift update for %s/%s (queue_id=%s, restarting=%t)",
		entry.WorkspaceID, entry.PluginName, queueID, restarting)
	c.JSON(http.StatusOK, gin.H{
		"status":        "applied",
		"workspace_id":  entry.WorkspaceID,
		"plugin_name":   entry.PluginName,
		"installed_sha": result.InstalledSHA,
		"restarting":    restarting,
	})
}

// applyRestartAfterDrift triggers the post-apply workspace restart for a drift
// update, EXCEPT when the target is a kind=platform concierge in its fragile
// lifecycle window (provisioning/online) — the SELF-BRICK guard. Returns true
// iff a restart was actually dispatched.
//
// Why the guard: the concierge's plugin-auto-update cron (0 3 * * *) can enqueue
// drift for its OWN workspace and then drive this Apply against itself. The
// restart here is UNCONDITIONAL — no health probe, no rollback — so a bad
// upstream ref that installs but fails to boot would restart the org-root
// concierge into a brick, mid-auto-apply-batch, with nothing left running to
// recover it. We DEFER the restart the same way the reconcile path does
// (platformConciergeReconcileShouldSkipRestart): the new ref is already re-pinned
// on the workspace_plugins row (step 3) and the queue entry is marked applied
// (step 4); the concierge picks the new bytes up on its next DELIBERATE restart
// rather than an auto-apply side effect. Non-concierge workspaces keep the
// immediate re-pin + restart — auto-apply stays intact (the product decision).
//
// Fail-open: platformConciergeReconcileShouldSkipRestart returns false on any DB
// error, so a transient blip never masks a needed non-concierge restart.
func (h *AdminPluginDriftHandler) applyRestartAfterDrift(ctx context.Context, wsID string) bool {
	if h.pluginsHandler == nil {
		return false
	}
	if platformConciergeReconcileShouldSkipRestart(ctx, wsID) {
		log.Printf("AdminPluginDrift: apply: DEFERRING restart of platform concierge workspace=%s "+
			"(self-brick guard) — plugin re-pinned; restart must be a deliberate action", wsID)
		return false
	}
	// RFC internal#524 Layer 1: globalGoAsync so the detached restart is drained
	// before a db.DB swap (see workspace.go:globalGoAsync). Async so the HTTP
	// response returns immediately after the install; the restart is best-effort.
	globalGoAsync(func() {
		// restartFunc takes a workspaceID (not a plugin name); pass wsID.
		if restart := h.pluginsHandler.GetRestartFunc(); restart != nil {
			restart(wsID)
		}
	})
	return true
}
