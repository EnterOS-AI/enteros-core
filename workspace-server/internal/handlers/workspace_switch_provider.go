package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/events"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/gin-gonic/gin"
)

// SwitchProvider handles POST /workspaces/:id/switch-provider — move an EXISTING
// workspace to a different cloud (compute.provider). The VM is cloud-specific, so
// this reprovisions the box on the new cloud; the agent's durable identity
// (agent_card, config, secrets, tokens, memory) lives in the tenant DB and is
// preserved because the row is never deleted.
//
// CRITICAL ORDERING (the wrong-backend leak, RFC #622 Hazard 1): the stop must
// run with the OLD provider BEFORE the DB provider is changed. cpStopWithRetry
// resolves provider + instance_id from the workspaces row at call time; if we
// wrote the new provider first, the stop would issue
// DELETE …?instance_id=<old>&provider=<new> → CP routes teardown to the NEW
// backend → the old box is never terminated and leaks (billed forever, and not
// covered by the status='removed' orphan sweeper). So the sequence is strictly:
//
//  1. stop OLD box (DB still has old provider + old instance_id)
//  2. clear instance_id + write new provider (jsonb_set, preserving the rest)
//  3. provision NEW box (withStoredCompute now reads the new provider)
//
// Clearing instance_id in step 2 also makes a retried switch safe: a second call
// finds no stale instance to (mis-)deprovision against the new backend.
func (h *WorkspaceHandler) SwitchProvider(c *gin.Context) {
	id := c.Param("id")
	ctx := c.Request.Context()

	var body struct {
		Provider        string `json:"provider"`
		ConfirmDataLoss bool   `json:"confirm_data_loss"`
		// MigrateData is accepted for forward-compat (RFC #622 PR4 wires the
		// filesystem migration). Until then it is a no-op and a persistent
		// volume still requires confirm_data_loss.
		MigrateData bool `json:"migrate_data"`
	}
	if err := c.ShouldBindJSON(&body); err != nil && err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON body"})
		return
	}
	newProvider := strings.ToLower(strings.TrimSpace(body.Provider))
	if newProvider == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider is required"})
		return
	}
	if _, ok := workspaceComputeProviderAllowlist[newProvider]; !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported provider (want aws|gcp|hetzner)"})
		return
	}

	var status, wsName, dbRuntime, oldProvider, dataPersistence string
	var oldInstanceID sql.NullString
	var tier int
	err := db.DB.QueryRowContext(ctx, `
		SELECT status, name, tier, COALESCE(runtime, 'claude-code'),
		       COALESCE(compute->>'provider', ''), COALESCE(compute->>'data_persistence', ''),
		       instance_id
		FROM workspaces WHERE id = $1`, id,
	).Scan(&status, &wsName, &tier, &dbRuntime, &oldProvider, &dataPersistence, &oldInstanceID)
	if err == sql.ErrNoRows || status == string(models.StatusRemoved) {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	// Switching the cloud backend is a SaaS-only concept — a self-hosted Docker
	// workspace has no cloud provider to switch. external/mock runtimes have no
	// box at all.
	if h.cpProv == nil {
		c.JSON(http.StatusConflict, gin.H{"error": "provider switching is only available for cloud (SaaS) workspaces"})
		return
	}
	if isExternalLikeRuntime(dbRuntime) || dbRuntime == "mock" {
		c.JSON(http.StatusConflict, gin.H{"error": dbRuntime + " workspaces have no cloud box to switch"})
		return
	}
	if paused, parentName := isParentPaused(ctx, id); paused {
		c.JSON(http.StatusConflict, gin.H{"error": "parent workspace \"" + parentName + "\" is paused — resume it first"})
		return
	}

	// "" provider means the default AWS path.
	effectiveOld := oldProvider
	if effectiveOld == "" {
		effectiveOld = "aws"
	}
	if newProvider == effectiveOld {
		c.JSON(http.StatusOK, gin.H{"status": "noop", "provider": newProvider, "message": "workspace is already on this provider"})
		return
	}

	// Data gate: a persistent data volume is cloud-specific and cannot move as a
	// block device. Until the filesystem migration lands (RFC #622 PR3/PR4), the
	// switch is allowed only with an explicit confirm_data_loss.
	if dataPersistence == "persist" && !body.ConfirmDataLoss {
		c.JSON(http.StatusConflict, gin.H{
			"error":  "DATA_LOSS_UNCONFIRMED",
			"detail": "this workspace has a persistent data volume that cannot move across clouds; set confirm_data_loss=true to switch with a fresh volume (cross-cloud data migration is RFC #622, not yet wired). Identity/config/secrets/memory are preserved regardless.",
		})
		return
	}

	// --- ordered switch (see doc-comment) ---

	// 1. Stop the OLD box with the OLD provider. DB is unchanged here, so
	//    cpStopWithRetryErr resolves the old provider + old instance_id. Bounded
	//    retry; on exhaustion it returns an error but we STILL proceed (a stuck
	//    old box must not strand the switch) — except we capture the failure so
	//    step 2.5 can emit a durable audit row, because step 2 nulls instance_id
	//    and flips provider, which otherwise orphans the old box with no DB
	//    pointer for the sweeper to find (review finding #3).
	stopErr := h.cpStopWithRetryErr(ctx, id, "SwitchProvider", false)

	// 2. Atomically claim the switch AND clear instance_id + write the new
	//    provider. The CAS (status not already provisioning, provider still the
	//    one we read) makes concurrent/duplicate switch calls safe: only the
	//    first winner launches a provision; a racing call sees 0 rows → 409,
	//    never a second provision against a second backend (review finding #4).
	//    jsonb_set preserves instance_type/volume/display/data_persistence.
	res, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces
		SET instance_id = NULL,
		    compute = jsonb_set(COALESCE(compute, '{}'::jsonb), '{provider}', to_jsonb($2::text)),
		    status = $3, url = '', updated_at = now()
		WHERE id = $1
		  AND status <> $3
		  AND COALESCE(compute->>'provider', '') IS NOT DISTINCT FROM $4`,
		id, newProvider, models.StatusProvisioning, oldProvider)
	if err != nil {
		log.Printf("SwitchProvider: failed to write new provider for %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to switch provider"})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Lost the CAS: another switch already flipped the provider / set
		// provisioning, or the row changed under us. Do NOT launch a second
		// provision (would orphan a box).
		c.JSON(http.StatusConflict, gin.H{"error": "ALREADY_SWITCHING", "detail": "a provider switch or provision is already in progress for this workspace"})
		return
	}

	// 2.5. If the old box never confirmed stopped, the old instance is now
	//      orphaned with no DB pointer (we just nulled instance_id). Emit a
	//      durable audit row carrying the old instance_id + provider so a CP-side
	//      reconciler can find and terminate it (review finding #3).
	if stopErr != nil && oldInstanceID.Valid && oldInstanceID.String != "" {
		h.emitSwitchProviderStopExhausted(ctx, id, oldInstanceID.String, effectiveOld, newProvider, stopErr)
	}
	log.Printf("SwitchProvider: %s %s → %s (old box stop err=%v, reprovisioning)", id, effectiveOld, newProvider, stopErr)
	h.broadcaster.RecordAndBroadcast(ctx, string(events.EventWorkspaceProvisioning), id, map[string]interface{}{
		"name":          wsName,
		"tier":          tier,
		"runtime":       dbRuntime,
		"provider_from": effectiveOld,
		"provider_to":   newProvider,
	})

	// 3. Provision the NEW box. withStoredCompute re-reads compute (now carrying
	//    the new provider) → provisionWorkspaceCP routes to the new backend.
	//    Reuse the existing config volume (templatePath="") so identity/config
	//    are preserved. Detached context: the reprovision outlives the request.
	payload := withStoredCompute(context.Background(), id, models.CreateWorkspacePayload{Name: wsName, Tier: tier, Runtime: dbRuntime})
	h.goAsync(func() { h.provisionWorkspaceCP(id, "", nil, payload) })

	c.JSON(http.StatusAccepted, gin.H{
		"status":       "switching",
		"workspace_id": id,
		"from":         effectiveOld,
		"to":           newProvider,
	})
}

// emitSwitchProviderStopExhausted records a durable audit row when a provider
// switch could not confirm the OLD box stopped before its instance_id was
// cleared from the row. Without it the old box is an un-pointed orphan that the
// status='removed' sweeper won't catch; a CP-side reconciler reads these rows by
// old_instance_id + old_provider to terminate the leaked box. Mirrors
// emitDeleteTerminateRetryExhausted (workspace_dispatchers.go). Best-effort.
func (h *WorkspaceHandler) emitSwitchProviderStopExhausted(ctx context.Context, workspaceID, oldInstanceID, oldProvider, newProvider string, cause error) {
	if db.DB == nil {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"workspace_id":    workspaceID,
		"old_instance_id": oldInstanceID,
		"old_provider":    oldProvider,
		"new_provider":    newProvider,
		"last_error":      cause.Error(),
		"recovery_path":   "cp_orphan_reconciler",
	})
	if err != nil {
		log.Printf("emitSwitchProviderStopExhausted: marshal failed for %s: %v", workspaceID, err)
		return
	}
	if _, err := db.DB.ExecContext(ctx, `
		INSERT INTO structure_events (event_type, workspace_id, payload, created_at)
		VALUES ($1, $2, $3, now())
	`, "workspace.provider.switch_stop_exhausted", workspaceID, payload); err != nil {
		log.Printf("emitSwitchProviderStopExhausted: insert failed for %s: %v", workspaceID, err)
	}
}
