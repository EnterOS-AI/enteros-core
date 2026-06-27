package handlers

// platform_agent_ensure.go — CORE-OWNED, self-contained platform-agent
// create/repair. (RFC docs/design/rfc-platform-agent.md)
//
// WHY THIS EXISTS / what moved out of the control plane (CP):
//
// In SaaS the CP used to be the ONLY thing that could bring an org's concierge
// (the kind='platform' org root) online: at org-provision time the CP derives a
// deterministic platform-agent id from the org id, POSTs it to the tenant's
// POST /admin/org/platform-agent (the install/re-parent), then POSTs a restart to
// trigger the workspace provision. That left the OSS canvas with a dead-end —
// when an org had no concierge (a failed/half install, a self-host stack, or an
// existing org that predated the platform-agent rollout) the canvas could only
// say "No platform agent yet" with no in-core way to fix it.
//
// This endpoint MOVES that orchestration into core so molecule-core owns the
// platform-agent lifecycle end-to-end with NO core->CP dependency:
//
//   1. id derivation — DeterministicPlatformAgentID reimplements the CP's
//      uuidv5(org id) derivation IN CORE (now the SSOT for the wire id; a golden
//      cross-impl test pins it to the exact value the CP produces so SaaS +
//      self-host agree on one id). PlatformAgentID picks the org-scoped id when
//      MOLECULE_ORG_ID is set (SaaS), else the fixed SelfHostedPlatformAgentID
//      (self-host) — so the resolved id matches whatever the CP would have
//      installed, keeping the operation idempotent across both worlds.
//   2. install — runs the EXACT same idempotent, transactional installPlatformAgent
//      the CP-driven POST /admin/org/platform-agent uses (upsert the platform
//      root, re-parent old roots, move org-anchor refs).
//   3. provision — triggers the workspace provision via the SAME RestartByID path
//      the boot-seed (MaybeProvisionPlatformAgentOnBoot) and the admin restart use,
//      which serves BOTH backends (local Docker self-host AND the CP/EC2
//      provisioner for SaaS) — RestartByID is itself debounced/coalesced so a
//      double-trigger is safe.
//
// It makes ZERO MOLECULE_CP_URL / /cp/* calls: everything runs against this
// tenant's own DB + provisioner. The canvas "Create / repair platform agent"
// button calls it same-origin.
//
// Idempotency (the create/repair contract):
//   - no platform root            -> install + provision      ("created")
//   - platform root, status=online -> no-op                   ("exists")
//   - platform root, degraded/failed/offline/etc OR force=true -> reinstall +
//     re-provision the SAME row in place                       ("repaired")
//
// CP->core is still allowed and unchanged: the CP's org-provision cloud-init may
// keep calling POST /admin/org/platform-agent + restart directly, OR a CP that
// wants the unified create/repair can call THIS endpoint (CP->core). Either way
// core never calls back into the CP.

import (
	"database/sql"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// DeterministicPlatformAgentID derives the platform agent's stable workspace id
// from the org id as an RFC-4122 v5 (SHA-1) UUID. This is the CORE SSOT for the
// platform-agent id wire contract: it reproduces, byte-for-byte, the id the
// control plane derives (molecule-controlplane/internal/provisioner/ec2.go
// deterministicPlatformAgentID) so a concierge installed by the CP and one
// created/repaired by core resolve to the SAME workspace id. The CP keeps its
// own copy of the pure function (CP->core would be allowed, but a deterministic
// hash mirrored under a shared golden test is the lighter SSOT-by-contract); the
// golden cross-impl test (platform_agent_ensure_test.go) is what guarantees the
// two never drift.
//
// uuid.NameSpaceURL is exactly the RFC-4122 URL namespace the CP hard-codes, and
// uuid.NewSHA1 performs the same SHA1(namespace||data) + version-5/variant bit
// twiddling + lowercase-hex formatting, so the output strings are identical.
func DeterministicPlatformAgentID(orgID string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("molecule-platform-agent:"+orgID)).String()
}

// PlatformAgentID resolves the platform-agent workspace id for THIS tenant,
// keeping SaaS and self-host consistent:
//   - MOLECULE_ORG_ID set (SaaS / CP-provisioned tenant) -> the org-scoped
//     deterministic id, identical to what the CP installs.
//   - MOLECULE_ORG_ID unset (self-host / local) -> the fixed SelfHostedPlatformAgentID
//     the boot-seed (EnsureSelfHostedPlatformAgent) already uses.
//
// Resolving the id the same way the existing install paths do is what makes the
// create/repair endpoint idempotent: it targets the id a prior install would
// have used, so a re-run reconciles the existing row rather than spawning a
// second concierge.
func PlatformAgentID() string {
	if orgID := strings.TrimSpace(os.Getenv("MOLECULE_ORG_ID")); orgID != "" {
		return DeterministicPlatformAgentID(orgID)
	}
	return SelfHostedPlatformAgentID
}

// platformAgentHealthy reports whether a platform-agent row in the given status
// is considered healthy enough to leave alone. Only 'online' counts: the row is
// seeded 'offline' (no container yet) and 'degraded'/'failed'/'starting'/
// 'provisioning' all warrant a (debounced) re-provision. Matching the canvas
// chat header, which lights the green dot on 'online' only for the concierge.
func platformAgentHealthy(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), "online")
}

// ensureAction is the decided outcome of an EnsurePlatformAgent call. Extracted
// as a pure function (decideEnsureAction) so the create/repair/no-op branching
// is unit-testable without a Postgres or a provisioner.
type ensureAction struct {
	// targetID is the workspace id to install/provision against. The EXISTING
	// platform root id when one is present (repair the real concierge in place,
	// never orphan it), else the freshly-derived id (create).
	targetID string
	// status is the past-tense outcome reported to the caller:
	// "created" | "repaired" | "exists".
	status string
	// provision is whether a workspace provision should be triggered. False for
	// a no-op ("exists") and on deployments with no provisioner wired.
	provision bool
}

// decideEnsureAction is the pure create/repair decision. derivedID is the
// core-derived canonical id (used only when no platform root exists yet);
// existingID/existingStatus describe the current platform root (existingFound
// false when there is none); force forces a repair even on a healthy agent (the
// repair-tool path); hasProvisioner gates whether a provision can be triggered.
func decideEnsureAction(derivedID, existingID, existingStatus string, existingFound, force, hasProvisioner bool) ensureAction {
	if !existingFound {
		return ensureAction{targetID: derivedID, status: "created", provision: hasProvisioner}
	}
	// Repair the EXISTING concierge in place (its id is canonical-by-construction
	// in every real flow — the CP/self-host install derived it the same way) so a
	// repair never demotes the user's configured concierge or spawns a duplicate.
	if platformAgentHealthy(existingStatus) && !force {
		return ensureAction{targetID: existingID, status: "exists", provision: false}
	}
	return ensureAction{targetID: existingID, status: "repaired", provision: hasProvisioner}
}

// ensureInstallFn is the platform-agent install EnsurePlatformAgent runs. It
// defaults to the real transactional installPlatformAgent; tests override it to
// assert the install was invoked (with the right id/runtime) without standing up
// a real Postgres — the full transactional install has its own integration test
// (platform_agent_integration_test.go), so the handler test only needs to prove
// the wiring, not re-prove the SQL.
var ensureInstallFn = installPlatformAgent

// triggerPlatformProvision fires the async workspace provision for the platform
// agent via the SAME RestartByID path the boot-seed + admin restart use (serves
// both the local Docker and CP/EC2 backends; debounced/coalesced so a
// double-trigger is safe). provisionTriggerOverride is set only in tests.
func (h *WorkspaceHandler) triggerPlatformProvision(id string) {
	if h.provisionTriggerOverride != nil {
		h.provisionTriggerOverride(id)
		return
	}
	h.goAsync(func() { h.RestartByID(id) })
}

// ensurePlatformAgentPayload is the (entirely optional) body of POST
// /admin/org/platform-agent/ensure. The canvas posts an empty body; all fields
// default.
type ensurePlatformAgentPayload struct {
	// Name is the concierge display name; defaults to defaultPlatformAgentName().
	Name string `json:"name"`
	// Runtime is the concierge runtime; empty falls back to the platform default
	// (conciergeDefaultRuntime), exactly like the CP-driven install.
	Runtime string `json:"runtime"`
	// Force repairs (reinstall + re-provision) even a healthy 'online' concierge —
	// the explicit "repair tool" path. Default false keeps the call idempotent.
	Force bool `json:"force"`
}

// EnsurePlatformAgent handles POST /admin/org/platform-agent/ensure (AdminAuth).
//
// Self-contained, core-only create/repair for the org's platform agent: derives
// the id in core, runs the idempotent install, and triggers the provision — with
// ZERO control-plane calls. See the file header for the full contract. Powers the
// canvas "Create / repair platform agent" button.
//
//	@Summary	Create or repair the org platform agent (concierge), core-only
//	@Tags		org
//	@Accept		json
//	@Produce	json
//	@Param		body	body	ensurePlatformAgentPayload	false	"optional name/runtime/force"
//	@Success	200	{object}	map[string]interface{}
//	@Router		/admin/org/platform-agent/ensure [post]
func (h *WorkspaceHandler) EnsurePlatformAgent(c *gin.Context) {
	var p ensurePlatformAgentPayload
	// The body is optional (the canvas posts none). Tolerate an empty body
	// (io.EOF) — everything defaults — but still reject a malformed non-empty one.
	if err := c.ShouldBindJSON(&p); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	ctx := c.Request.Context()
	derivedID := PlatformAgentID()

	// Look up the org's current platform root (if any). COALESCE keeps an
	// unexpected NULL status from failing the scan.
	var existingID, existingStatus string
	found := false
	err := db.DB.QueryRowContext(ctx,
		`SELECT id, COALESCE(status, '') FROM workspaces WHERE kind = 'platform' AND parent_id IS NULL LIMIT 1`).
		Scan(&existingID, &existingStatus)
	switch {
	case err == nil:
		found = true
	case errors.Is(err, sql.ErrNoRows):
		found = false
	default:
		log.Printf("EnsurePlatformAgent: platform-root lookup failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	decision := decideEnsureAction(derivedID, existingID, existingStatus, found, p.Force, h.HasProvisioner())

	// Healthy + not forced -> idempotent no-op.
	if decision.status == "exists" {
		c.JSON(http.StatusOK, gin.H{
			"status":            "exists",
			"platform_agent_id": decision.targetID,
			"kind":              models.KindPlatform,
			"agent_status":      existingStatus,
			"provisioning":      false,
		})
		return
	}

	// Create or repair: run the idempotent install (upsert + re-parent), then
	// trigger the provision. installPlatformAgent maps an empty runtime to the
	// platform default and is safe to re-run.
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = defaultPlatformAgentName()
	}
	if err := ensureInstallFn(ctx, db.DB, decision.targetID, name, p.Runtime); err != nil {
		log.Printf("EnsurePlatformAgent: install failed (id=%s): %v", decision.targetID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "install failed"})
		return
	}

	if decision.provision {
		h.triggerPlatformProvision(decision.targetID)
	}

	log.Printf("EnsurePlatformAgent: %s platform agent %s (provision=%v, prior_status=%q)",
		decision.status, decision.targetID, decision.provision, existingStatus)
	c.JSON(http.StatusOK, gin.H{
		"status":            decision.status,
		"platform_agent_id": decision.targetID,
		"kind":              models.KindPlatform,
		"agent_status":      existingStatus,
		"provisioning":      decision.provision,
	})
}
