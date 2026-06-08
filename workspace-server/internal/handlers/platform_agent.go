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
	"path/filepath"
	"strings"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// conciergeSystemPrompt is the identity seeded into the platform agent's
// /configs/system-prompt.md. It makes the concierge BE the Org Concierge —
// the org root (kind='platform'), the user's universal A2A peer and default
// chat target — instead of booting as a generic claude-code coding assistant.
//
// Grounded in the RFC (docs/design/rfc-platform-agent.md §1-2): it IS the org,
// orchestrates the org via the platform MCP (the 87-tool org-admin surface) +
// a2a delegation, and routes destructive ops through human approval. The prompt
// is identity-only and works LOCALLY regardless of whether the platform MCP
// binary is present — the org-admin tools simply aren't available until the
// agent runs on the dedicated platform-agent image.
//
// %s is the concierge's display name (defaultPlatformAgentName()).
const conciergeSystemPromptTmpl = `# You are %s — the Org Concierge

You are the organization's **platform agent**: the single org-root agent
(kind=platform) that sits above every workspace. You are the user's one front
door to the whole organization — their universal peer and default chat target.
You are NOT a generic coding assistant; you are an **org orchestrator**.

## What you are

- **You are the org.** Every team and workspace in this organization lives under
  you in the agent hierarchy. When the user talks to the org, they talk to you.
- **You orchestrate; you don't do the work yourself.** Break a request down and
  delegate it to the right workspace(s). Spin up new workspaces/agents when the
  org doesn't yet have the right team.
- **You manage the org through tools, not guesswork.** You hold the
  platform-management MCP (the org-admin surface: list/create/delete workspaces,
  assign agents, set secrets, manage channels/schedules, delegate, chat with any
  agent). Always inspect real state with these tools before acting — never assume
  the org's shape from memory.

## How you work

1. **Recall first.** At the start of a conversation, recall prior context so you
   continue org work coherently across restarts.
2. **Understand the ask, then act.** For "spin up an SEO team that publishes
   weekly", that means: create the workspaces, assign the agents, wire the
   schedule — using the platform MCP — not a paragraph of instructions for the
   user to run by hand.
3. **Delegate via A2A.** Use list_peers to discover agents and delegate_task to
   hand work to them; coordinate their results back into one clear answer.
4. **Report back clearly.** Synthesize what the org did into a concise summary
   for the user; use send_message_to_user for progress on long-running work.

## Guardrails

- **Destructive operations are human-approved.** Deleting a workspace,
  deprovisioning, writing secrets, or minting org tokens go through the approvals
  subsystem — the platform returns a pending approval and the user decides. Never
  try to route around the gate.
- **Stay inside this org.** You can reach every workspace in your organization
  and only this organization; tenant isolation is enforced server-side.
- **Be honest about capability.** If the org-admin tools aren't available in this
  environment (e.g. a local/dev image without the platform MCP), say so plainly
  and fall back to A2A delegation + advising the user — do not fabricate results.

You have full org-management authority. Use it deliberately, on the user's
behalf, and keep them in the loop.
`

// conciergeMCPServersBlock is the YAML appended to the concierge's config.yaml
// so the runtime loads the org-admin platform MCP alongside the always-on a2a
// server. The Phase-2 extra-MCP merge (claude_sdk_executor.py
// _apply_extra_mcp_servers) reads this `mcp_servers:` list. The platform MCP
// authenticates purely from the container env (MOLECULE_API_KEY /
// MOLECULE_API_URL / MOLECULE_ORG_ID — wired by conciergePlatformMCPEnv), so no
// per-server env block is needed here.
//
// SELF-HOST CAVEAT: the local stack provisions the concierge on the ordinary
// `claude-code` image, which does NOT ship /opt/molecule-mcp-server. The
// dedicated `platform-agent` image (Dockerfile.platform-agent) does. The
// executor's _apply_extra_mcp_servers skips an entry whose command/script is
// absent, so declaring this block can never crash the agent or wedge the SDK
// init locally — the identity (system prompt) works everywhere; the org-admin
// MCP tools only light up on the platform-agent image.
const conciergeMCPServersBlock = `mcp_servers:
  - name: platform
    command: node
    args:
      - /opt/molecule-mcp-server/dist/index.js
`

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

// conciergeIdentityFiles returns the overlay config files that turn an ordinary
// claude-code workspace into the Org Concierge: the system-prompt.md identity
// and a config.yaml that declares the platform MCP. These are written on top of
// the workspace template at provision time (provisioner writes ConfigFiles AFTER
// CopyTemplateToContainer), so they survive restarts — every provision re-seeds
// the identity from the single source here.
//
// baseConfigYAML is the config.yaml the concierge would otherwise boot with
// (the template's, the freshly-generated one, or — on auto-restart — the live
// container's). We append the mcp_servers block only when it is not already
// present, so re-applying is idempotent and never duplicates the block. When
// baseConfigYAML is empty (we couldn't read a base) we overlay only the system
// prompt and leave config.yaml to the template — the identity still lands; the
// MCP simply isn't declared that cycle (the next provision with a readable base
// adds it).
func conciergeIdentityFiles(name string, baseConfigYAML []byte) map[string][]byte {
	files := map[string][]byte{
		"system-prompt.md": []byte(fmt.Sprintf(conciergeSystemPromptTmpl, name)),
	}
	if len(baseConfigYAML) > 0 && !strings.Contains(string(baseConfigYAML), "\nmcp_servers:") &&
		!strings.HasPrefix(string(baseConfigYAML), "mcp_servers:") {
		files["config.yaml"] = appendYAMLBlock(baseConfigYAML, conciergeMCPServersBlock)
	}
	return files
}

// conciergePlatformMCPEnv injects the env the platform MCP child reads at spawn
// (RFC §5.5/§5.6). The org-admin token is ADMIN_TOKEN on self-host; the platform
// URL is the in-cluster PLATFORM_URL (e.g. http://platform:8080). Existing
// values in env win, so an operator/CP override is never clobbered. No-op for a
// non-platform workspace. Best-effort: when ADMIN_TOKEN is unset (pure-local dev
// with AdminAuth fail-open) the key is simply absent and the MCP — which only
// runs on the platform-agent image anyway — is unauthenticated locally.
func conciergePlatformMCPEnv(env map[string]string) {
	setIfAbsent := func(k, v string) {
		if v == "" {
			return
		}
		if _, ok := env[k]; !ok {
			env[k] = v
		}
	}
	setIfAbsent("MOLECULE_API_KEY", os.Getenv("ADMIN_TOKEN"))
	// MOLECULE_API_URL: prefer an explicit env, else the in-cluster platform URL.
	apiURL := os.Getenv("MOLECULE_API_URL")
	if apiURL == "" {
		apiURL = os.Getenv("PLATFORM_URL")
	}
	setIfAbsent("MOLECULE_API_URL", apiURL)
	setIfAbsent("MOLECULE_ORG_ID", os.Getenv("MOLECULE_ORG_ID"))
}

// applyConciergeProvisionConfig is the provision-time hook that makes the
// platform agent boot as the concierge. Called from prepareProvisionContext for
// EVERY provision of a kind='platform' workspace (create, restart, auto-recover)
// so the identity + platform-MCP declaration are re-seeded each cycle and never
// drift. It is a no-op for ordinary workspaces.
//
// It (1) injects the platform-MCP env into envVars and (2) merges the concierge
// overlay files (system-prompt.md + a config.yaml carrying mcp_servers) into the
// returned configFiles map, which the provisioner writes on top of the template.
//
// Returns the (possibly newly-allocated) configFiles map so the caller can
// rebind it — configFiles is nil on the auto-restart path, where this is the
// thing that introduces the overlay.
func (h *WorkspaceHandler) applyConciergeProvisionConfig(
	ctx context.Context,
	workspaceID, templatePath string,
	configFiles map[string][]byte,
	envVars map[string]string,
	name string,
) map[string][]byte {
	var kind string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(kind, 'workspace') FROM workspaces WHERE id = $1`, workspaceID).Scan(&kind); err != nil {
		// Non-fatal: a missing row / probe error just means "treat as ordinary".
		return configFiles
	}
	if kind != models.KindPlatform {
		return configFiles
	}

	// 1. Platform-MCP env (org-admin token + platform URL + org id).
	conciergePlatformMCPEnv(envVars)

	// 2. Resolve the base config.yaml to append mcp_servers onto, in priority
	//    order: the in-memory configFiles (fresh provision), the template dir
	//    (apply-template provision), then the live container (auto-restart,
	//    configFiles == nil + templatePath == ""). Any miss falls through.
	var base []byte
	if configFiles != nil {
		base = configFiles["config.yaml"]
	}
	if len(base) == 0 && templatePath != "" {
		if b, err := os.ReadFile(filepath.Join(templatePath, "config.yaml")); err == nil {
			base = b
		}
	}
	if len(base) == 0 && h.provisioner != nil {
		if b, err := h.provisioner.ExecRead(ctx, configDirName(workspaceID), "/configs/config.yaml"); err == nil {
			base = b
		}
	}

	overlay := conciergeIdentityFiles(name, base)
	if configFiles == nil {
		configFiles = map[string][]byte{}
	}
	for k, v := range overlay {
		configFiles[k] = v
	}
	log.Printf("Provisioner: applied concierge identity overlay for platform agent %s (system-prompt + %d config file(s))", workspaceID, len(overlay))
	return configFiles
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
	// Already online AND a live container? Then it's running — but it may be a
	// concierge that pre-dates the identity overlay (booted as a vanilla
	// claude-code agent with no system-prompt.md). Probe for the concierge
	// identity; if it's missing, restart ONCE so the provision path re-seeds the
	// overlay. This is what makes the seed idempotent + self-applying on the
	// EXISTING concierge (the deterministic self-hosted id), not just new
	// installs. IsRunning is the authoritative liveness check; status is the
	// cheap one.
	running, _ := prov.IsRunning(ctx, id)
	if running {
		if conciergeIdentityPresent(ctx, prov, id) {
			log.Printf("boot: platform-agent %s already running with concierge identity — skipping", id)
			return
		}
		log.Printf("boot: platform-agent %s running but MISSING concierge identity — restarting once to apply the system prompt + platform MCP", id)
		go restartByID(id)
		return
	}
	log.Printf("boot: platform-agent %s not running (status=%s) — kicking off best-effort provision", id, status)
	go restartByID(id)
}

// conciergeIdentityPresent reports whether the running concierge container
// already carries the seeded identity (a non-empty /configs/system-prompt.md).
// Used to decide whether a running-but-vanilla concierge needs a one-shot
// restart to pick up the overlay. Best-effort: on a probe error or an empty
// file it returns false (so the safe action — re-seed via restart — is taken).
func conciergeIdentityPresent(ctx context.Context, prov localProvisionerIsRunning, id string) bool {
	reader, ok := prov.(interface {
		ExecRead(ctx context.Context, containerName, filePath string) ([]byte, error)
	})
	if !ok {
		// Can't probe — assume present to avoid a restart loop on a backend
		// that doesn't expose ExecRead.
		return true
	}
	body, err := reader.ExecRead(ctx, configDirName(id), "/configs/system-prompt.md")
	if err != nil {
		return false
	}
	return strings.Contains(string(body), "Org Concierge")
}

// localProvisionerIsRunning is the minimal slice of the local Docker
// provisioner that MaybeProvisionPlatformAgentOnBoot needs — the
// "is this workspace's container live?" probe. The boot helper additionally
// type-asserts for an optional ExecRead (conciergeIdentityPresent) to detect a
// running-but-vanilla concierge; keeping ExecRead off this interface keeps the
// unit-test fake minimal while still letting the real *Provisioner satisfy it.
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
