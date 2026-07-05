package handlers

import (
	"log"
	"net/http"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"github.com/gin-gonic/gin"
	sdkcp "go.moleculesai.app/sdk/cloudprovider"
)

// SetComputeInstanceRequest is the body the control plane POSTs after a
// re-provision or cross-cloud migration cutover to repoint the tenant's
// workspace record at the box on the NEW backend.
type SetComputeInstanceRequest struct {
	// InstanceID is the workspace's box id on the new provider (an EC2
	// instance-id for AWS, a Hetzner/GCP server id for those clouds, or the
	// local-docker CONTAINER NAME for the Molecules-Server backend).
	InstanceID string `json:"instance_id"`
	// Provider is the new backend: any selectable cloudprovider id
	// (aws|hetzner|gcp|molecules-server) or its aliases (local|docker →
	// molecules-server). Validated against the shared SDK SSOT, never a
	// hand-maintained cloud-only allowlist.
	Provider string `json:"provider"`
}

// SetComputeInstance repoints a workspace's tenant record (instance_id +
// compute.provider) at the box on its NEW cloud, WITHOUT deprovisioning
// anything. The control-plane migrator calls this right after a cross-cloud
// cutover is verified healthy.
//
// Why this exists — cross-cloud migration ↔ self-heal coordination (#806):
// migrate-provider moves a workspace's infra (e.g. AWS→Hetzner) on the CP side,
// but the tenant's workspaces row keeps the OLD instance_id + compute.provider.
// The CP instance reconciler (cp_instance_reconciler.go) polls IsRunning on that
// stale AWS instance_id, sees the terminated source as "offline", and self-heals
// by restarting the workspace on AWS — fighting the migration into a split-brain
// (live box on Hetzner, tenant record + cp#326 EBS data-volume reattach storm on
// AWS). /registry/register flips the advertised url but NOT instance_id/provider,
// so the reconciler never learns the box moved. This endpoint closes that gap:
// after the cutover the CP tells the tenant the new (instance_id, provider) so
// the reconciler checks the Hetzner box (healthy) and stops re-provisioning on
// AWS.
//
// Distinct from the user-driven cloud-provider switch (workspace_crud.go Update),
// which DEPROVISIONS the old box before overwriting compute. Here the migration
// already provisioned the new box and retired the source, so this is a pure
// record repoint — never deprovisions (a deprovision here would tear down the
// just-migrated box).
//
// AdminAuth-gated (only the CP, holding the tenant admin token, repoints infra).
// Idempotent: repointing to the same values is a 200 no-op.
func (h *WorkspaceHandler) SetComputeInstance(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "workspace id required"})
		return
	}
	var req SetComputeInstanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	instanceID := strings.TrimSpace(req.InstanceID)
	reqProvider := strings.ToLower(strings.TrimSpace(req.Provider))
	if instanceID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "instance_id required"})
		return
	}
	// The provider is still REQUIRED (the CP always supplies the org's backend),
	// but it is validated against the shared cloudprovider SSOT — NOT a hardcoded
	// cloud-only {aws,hetzner,gcp} allowlist. That old allowlist rejected the
	// Molecules-Server backend (org.provider="local", the CP re-provision path
	// POSTs provider="local") with a 400, so RepointTenantComputeInstance failed
	// and the CP admin re-provision returned a cosmetic 500 even though the box
	// came up fine (#190). Route validation through the SDK so molecules-server /
	// its aliases (local, docker) are handled correctly and the cloud abstraction
	// (aws/gcp/hetzner) is preserved unchanged.
	if reqProvider == "" || !sdkcp.IsValidID(reqProvider) {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "provider must be one of " + strings.Join(sdkcp.IDs(), "|") + " (aliases: local, docker → molecules-server)",
		})
		return
	}
	// Persist the SDK BACKEND KEY (clouds map to themselves; molecules-server and
	// its aliases map to "local"), which is exactly what the persisted
	// compute->>'provider' value drives downstream: the tenant CPProvisioner
	// resolveProvider→Stop/status path forwards it as ?provider= to the CP
	// Deprovision handler, whose effectiveTeardownProvider routes on backend keys.
	// So a Molecules-Server workspace lands provider="local" and tears down via
	// the local backend instead of the absent AWS path.
	provider, _ := sdkcp.BackendKey(reqProvider)

	// Repoint instance_id + compute.provider only. jsonb_set preserves every
	// other compute key (sizing, display, data_persistence, …) and creates the
	// provider key if absent. No deprovision, no status change — the box already
	// exists and registered.
	res, err := db.DB.ExecContext(c.Request.Context(), `
		UPDATE workspaces
		   SET instance_id = $2,
		       compute = jsonb_set(COALESCE(compute, '{}'::jsonb), '{provider}', to_jsonb($3::text), true),
		       updated_at = now()
		 WHERE id = $1
	`, id, instanceID, provider)
	if err != nil {
		log.Printf("SetComputeInstance: db update %s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db update failed"})
		return
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "workspace not found"})
		return
	}
	log.Printf("SetComputeInstance: workspace %s repointed → instance=%s provider=%s (re-provision / cross-cloud cutover)", id, instanceID, provider)
	c.JSON(http.StatusOK, gin.H{"status": "repointed", "workspace_id": id, "instance_id": instanceID, "provider": provider})
}
