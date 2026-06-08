package handlers

import (
	"context"
	"database/sql"
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
	var tier int
	err := db.DB.QueryRowContext(ctx, `
		SELECT status, name, tier, COALESCE(runtime, 'claude-code'),
		       COALESCE(compute->>'provider', ''), COALESCE(compute->>'data_persistence', '')
		FROM workspaces WHERE id = $1`, id,
	).Scan(&status, &wsName, &tier, &dbRuntime, &oldProvider, &dataPersistence)
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
	//    cpStopWithRetry resolves the old provider + old instance_id. Synchronous
	//    (bounded retry); proceeds even on exhaustion so a stuck old box never
	//    strands the switch — the audit log + reconciler are the backstop.
	h.cpStopWithRetry(ctx, id, "SwitchProvider")

	// 2. Clear instance_id + write the new provider (jsonb_set preserves
	//    instance_type/volume/display/data_persistence) + go provisioning.
	if _, err := db.DB.ExecContext(ctx, `
		UPDATE workspaces
		SET instance_id = NULL,
		    compute = jsonb_set(COALESCE(compute, '{}'::jsonb), '{provider}', to_jsonb($2::text)),
		    status = $3, url = '', updated_at = now()
		WHERE id = $1`, id, newProvider, models.StatusProvisioning); err != nil {
		log.Printf("SwitchProvider: failed to write new provider for %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to switch provider"})
		return
	}
	log.Printf("SwitchProvider: %s %s → %s (old box stopped, reprovisioning)", id, effectiveOld, newProvider)
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
