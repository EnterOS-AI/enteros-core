package handlers

// platform_agent.go — installs the org-level platform agent as the org root.
// (RFC docs/design/rfc-platform-agent.md)
//
// The platform agent IS the org root: an org is the subtree under the single
// parent_id IS NULL row (org_scope.go), so making the concierge the user's
// universal A2A peer means making it that root. Installing it therefore:
//
//	1. upserts the platform-agent row (kind='platform', parent_id NULL);
//	2. re-parents the org's existing root(s) under it;
//	3. moves the org-anchor references — org_api_tokens.org_id and
//	   org_plugin_allowlist.org_id, both of which key off the root workspace id
//	   (see migrations 035/036 + 026) — from each old root to the platform agent.
//
// All of that happens in ONE transaction so a tenant's auth tokens and plugin
// allowlist never point at a stale anchor. The operation is idempotent: a second
// call finds the platform agent already the sole root and does nothing.
//
// Routing (CanCommunicate/sameOrg in registry/access.go + org_scope.go) needs NO
// change — once the platform agent is the root, the existing ancestor/descendant
// rules already give it universal in-org reach and keep tenant isolation intact.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// SelfHostedPlatformAgentID is the deterministic platform-agent id used when no
// control plane is present to derive a per-org id (self-hosted / local). There
// is one platform agent per self-hosted tenant, so a fixed namespaced uuidv5 is
// sufficient and stable across restarts.
var SelfHostedPlatformAgentID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("molecule:self-hosted:platform-agent")).String()

// defaultPlatformAgentName returns the display name for the org's platform
// agent (the concierge). When the tenant server is told its org's name via the
// MOLECULE_ORG_NAME env (the self-hosted docker-compose sets it; SaaS passes an
// explicit name in the CP install payload instead), the concierge is named
// "<org name> Agent" — e.g. org "Molecule AI" → "Molecule AI Agent". With no
// org name configured it falls back to the legacy "Org Concierge".
func defaultPlatformAgentName() string {
	if orgName := os.Getenv("MOLECULE_ORG_NAME"); orgName != "" {
		return fmt.Sprintf("%s Agent", orgName)
	}
	return "Org Concierge"
}

// EnsureSelfHostedPlatformAgent installs the org's platform agent (the concierge,
// the org root) on a tenant that has no control plane to do it — i.e. self-hosted
// or local. In SaaS the CP calls InstallPlatformAgent at org-provision time; this
// is the no-CP equivalent. Idempotent: returns early if a kind='platform' root
// already exists (a prior boot, or a CP install in a hybrid setup). The CALLER
// gates this on the MOLECULE_SEED_PLATFORM_AGENT flag (set by the self-hosted
// docker-compose) so CI harnesses and SaaS tenants are unaffected.
func EnsureSelfHostedPlatformAgent(ctx context.Context, database *sql.DB) error {
	var existing string
	err := database.QueryRowContext(ctx,
		`SELECT id FROM workspaces WHERE kind = 'platform' AND parent_id IS NULL LIMIT 1`).Scan(&existing)
	if err == nil {
		return nil // platform agent already present — nothing to do
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("check existing platform agent: %w", err)
	}
	log.Printf("boot: no platform agent present — self-seeding %s (self-hosted)", SelfHostedPlatformAgentID)
	return installPlatformAgent(ctx, database, SelfHostedPlatformAgentID, defaultPlatformAgentName())
}

// OrgIdentityResponse is the body of GET /org/identity.
type OrgIdentityResponse struct {
	// Name is the org's display name (MOLECULE_ORG_NAME, "" when unset).
	Name string `json:"name"`
	// Slug is the org's URL slug (MOLECULE_ORG_SLUG, "" when unset). Empty on
	// a self-hosted stack where no control plane assigns a slug.
	Slug string `json:"slug"`
	// OrgID is the org's UUID (MOLECULE_ORG_ID, "" when unset). Empty on a
	// self-hosted stack where no control plane assigns an org id.
	OrgID string `json:"org_id"`
	// PlatformManagedAvailable reports whether a Molecule LLM proxy is wired
	// into this workspace-server process — i.e. whether platform_managed billing
	// can actually work. True on SaaS (the CP provisioner exports the proxy base
	// URL + usage token), false on a self-hosted stack (no hosted proxy / no
	// credit ledger). The canvas reads this pre-login to decide whether to offer
	// the "Platform (proxy)" billing option or hide it and default to BYOK.
	PlatformManagedAvailable bool `json:"platform_managed_available"`
}

// OrgIdentity handles GET /org/identity (open / CORS-friendly, no auth).
//
// Returns the org's display name from the MOLECULE_ORG_NAME env (empty string
// when unset), its slug (MOLECULE_ORG_SLUG) and id (MOLECULE_ORG_ID) — both ""
// on self-host where no control plane assigns them — plus a
// platform_managed_available capability flag. The canvas topbar reads `name` to
// render "<org name>" without an admin token; the Settings → Organization tab
// reads name+slug+org_id to render the org-identity card on self-host (where the
// control-plane /cp/orgs endpoint does not exist); and the Settings billing card
// reads `platform_managed_available` to decide whether to offer platform-managed
// (proxy) billing — exactly like /health and /buildinfo, it exposes only
// non-sensitive identity/capability signals.
//
// platform_managed_available is true iff a Molecule LLM proxy is configured in
// this process env (PlatformManagedProxyConfigured — the same base-URL + usage-
// token precondition the strip gate enforces). On self-host both are unset, so
// it is false and the canvas hides the "Platform (proxy)" option + defaults BYOK.
//
//	@Summary	Get the org's display name + billing capability
//	@Tags		org
//	@Produce	json
//	@Success	200	{object}	OrgIdentityResponse
//	@Router		/org/identity [get]
func OrgIdentity(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"name":                       os.Getenv("MOLECULE_ORG_NAME"),
		"slug":                       os.Getenv("MOLECULE_ORG_SLUG"),
		"org_id":                     os.Getenv("MOLECULE_ORG_ID"),
		"platform_managed_available": PlatformManagedProxyConfigured(),
	})
}

// MaybeProvisionPlatformAgentOnBoot best-effort provisions a container for the
// self-hosted org's platform agent (the concierge) at boot. The boot-seed
// (EnsureSelfHostedPlatformAgent) only creates the DB row; on a fresh self-host
// that leaves the concierge with no container. This brings it online
// automatically once creds exist.
//
// STRICTLY self-host + best-effort:
//   - The CALLER gates this on MOLECULE_SEED_PLATFORM_AGENT set AND the local
//     Docker provisioner being active (prov != nil, i.e. MOLECULE_ORG_ID unset).
//     SaaS (cpProv) never reaches here.
//   - It looks up the kind='platform' root; if absent (seed disabled / failed)
//     it no-ops. If the container is already running (prov.IsRunning) it no-ops.
//   - Otherwise it kicks off ONE provision via the same path the restart
//     endpoint uses (WorkspaceHandler.RestartByID), which reads the row's
//     runtime ('claude-code' as seeded) + config and provisions accordingly.
//
// On a fresh self-host with no LLM credentials the provision will fail (missing
// key) and the agent stays 'failed' until the user configures BYOK via
// Settings — that's expected. This never fatals and never loops: RestartByID is
// itself debounced/coalesced, and this runs exactly once at boot. Run it in a
// goroutine so a slow Docker pull doesn't delay the HTTP server coming up.
func MaybeProvisionPlatformAgentOnBoot(ctx context.Context, database *sql.DB, prov localProvisionerIsRunning, restartByID func(string)) {
	if prov == nil || restartByID == nil {
		return
	}
	var id, status string
	err := database.QueryRowContext(ctx,
		`SELECT id, status FROM workspaces WHERE kind = 'platform' AND parent_id IS NULL LIMIT 1`).Scan(&id, &status)
	if err == sql.ErrNoRows {
		log.Printf("boot: platform-agent provision skipped — no platform agent row present")
		return
	}
	if err != nil {
		log.Printf("boot: platform-agent provision lookup failed (non-fatal): %v", err)
		return
	}
	// Already online AND a live container? Nothing to do. We still provision
	// when status=='online' but the container is gone (stale row from a prior
	// boot) — IsRunning is the authoritative check, status is the cheap one.
	running, _ := prov.IsRunning(ctx, id)
	if running {
		log.Printf("boot: platform-agent %s already running — skipping provision", id)
		return
	}
	log.Printf("boot: platform-agent %s not running (status=%s) — kicking off best-effort provision", id, status)
	go restartByID(id)
}

// localProvisionerIsRunning is the minimal slice of the local Docker
// provisioner that MaybeProvisionPlatformAgentOnBoot needs — just the
// "is this workspace's container live?" probe. Narrowed to an interface so the
// boot helper is unit-testable without a real Docker daemon.
type localProvisionerIsRunning interface {
	IsRunning(ctx context.Context, workspaceID string) (bool, error)
}

type installPlatformAgentPayload struct {
	// ID is the platform agent's workspace id (a deterministic uuidv5 the
	// control plane derives per org). Required.
	ID string `json:"id" binding:"required"`
	// Name is the display name; defaults to "Org Concierge" when omitted.
	Name string `json:"name"`
}

// InstallPlatformAgent handles POST /admin/org/platform-agent (AdminAuth).
//
// Idempotently installs the platform agent as the org root for THIS tenant. The
// control plane calls it at org-provision time (new orgs) and during the
// existing-org backfill rollout. Safe to call repeatedly.
func InstallPlatformAgent(c *gin.Context) {
	var p installPlatformAgentPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	name := p.Name
	if name == "" {
		name = defaultPlatformAgentName()
	}
	if err := installPlatformAgent(c.Request.Context(), db.DB, p.ID, name); err != nil {
		log.Printf("InstallPlatformAgent: %v (id=%s)", err, p.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "install failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"status":            "installed",
		"platform_agent_id": p.ID,
		"kind":              models.KindPlatform,
	})
}

// installPlatformAgent performs the idempotent, transactional install described
// in the file header. Separated from the gin handler so integration tests can
// exercise it directly against a real Postgres (the org-anchor migration cannot
// be proven with sqlmock).
func installPlatformAgent(ctx context.Context, database *sql.DB, platformID, name string) error {
	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// 1. Ensure the platform-agent row exists as a kind='platform' root.
	//    ON CONFLICT keeps it a platform root if it was pre-seeded; the row is
	//    tier 0 and never billed/provisioned as an ordinary workspace EC2.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, kind, tier, status, runtime, parent_id)
		VALUES ($1, $2, 'platform', 0, 'online', 'claude-code', NULL)
		ON CONFLICT (id) DO UPDATE SET kind = 'platform', parent_id = NULL
	`, platformID, name); err != nil {
		return fmt.Errorf("upsert platform agent: %w", err)
	}

	// 2. Capture the org's other current roots (everything at parent_id IS NULL
	//    except the platform agent itself). In a one-org tenant DB this is the
	//    single team root; the query tolerates 0 (already installed) or N.
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM workspaces WHERE parent_id IS NULL AND id <> $1`, platformID)
	if err != nil {
		return fmt.Errorf("select old roots: %w", err)
	}
	var oldRoots []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("scan old root: %w", err)
		}
		oldRoots = append(oldRoots, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate old roots: %w", err)
	}

	// 3 + 4. Re-parent each old root under the platform agent and move its
	//        org-anchor references in the same transaction. A non-root old root
	//        is kind='workspace', so it does not trip workspaces_platform_root_check.
	for _, root := range oldRoots {
		if _, err := tx.ExecContext(ctx,
			`UPDATE workspaces SET parent_id = $1, updated_at = now() WHERE id = $2`,
			platformID, root); err != nil {
			return fmt.Errorf("re-parent %s: %w", root, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE org_api_tokens SET org_id = $1 WHERE org_id = $2`, platformID, root); err != nil {
			return fmt.Errorf("migrate org_api_tokens for %s: %w", root, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE org_plugin_allowlist SET org_id = $1 WHERE org_id = $2`, platformID, root); err != nil {
			return fmt.Errorf("migrate org_plugin_allowlist for %s: %w", root, err)
		}
	}

	return tx.Commit()
}
