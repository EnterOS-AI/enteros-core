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
	return installPlatformAgent(ctx, database, SelfHostedPlatformAgentID, "Org Concierge")
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
		name = "Org Concierge"
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
