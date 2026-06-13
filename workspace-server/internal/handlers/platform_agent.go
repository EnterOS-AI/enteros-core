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

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
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
   for the user. **Acknowledge first, then work:** the moment you pick up a
   request that will take more than a few seconds, FIRST send a one-line
   acknowledgement + your plan with the send_message_to_user tool (e.g. "On it —
   I'll do X then Y, back shortly"), THEN start the work. For long tasks,
   drop a brief progress note when a phase finishes. Never go silent for
   minutes — a user with no acknowledgement assumes the agent is stuck.
   (core#2724: the concierge prompt is the one workspace-server surface
   the runtime MCP preamble in workspace-runtime PR #129 doesn't reach;
   the parallel platform_instruction seed migration
   20260613081005_platform_instructions_ack_first_seed covers the
   rest of the org.)

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
- **Never run secret operations against your own workspace.** Secret writes and
  deletes auto-restart the target workspace; when the target is you, the
  platform tears down YOUR box mid-turn. If asked to test or demonstrate the
  approval flow, use create_approval / create_request (no side effects). If
  those tools are unavailable, use a naturally gated operation such as
  mint_org_token (it returns a pending approval the user can deny) — never a
  secret write — or say plainly that you lack a no-side-effect approval tool
  and ask how to proceed. Never improvise a demo with a destructive or
  state-changing operation.

You have full org-management authority. Use it deliberately, on the user's
behalf, and keep them in the loop.
`

// conciergeMCPServersBlock is the YAML appended to the concierge's config.yaml
// so the runtime loads the org-admin platform MCP alongside the always-on a2a
// server. The Phase-2 extra-MCP merge (claude_sdk_executor.py
// _apply_extra_mcp_servers) reads this `mcp_servers:` list.
//
// Entry shape pins the REAL image contract (agents-team pilot RCA,
// 2026-06-10 — the previous block pointed at a /opt/molecule-mcp-server
// path the image never shipped):
//   - command `molecule-platform-mcp` — Dockerfile.platform-agent symlinks
//     the npm-installed @molecule-ai/mcp-server bin under this UNAMBIGUOUS
//     name. The package's own bin name (`molecule-mcp`) COLLIDES with the
//     runtime wheel's Python a2a inbox bridge at /usr/local/bin/molecule-mcp,
//     which wins on PATH — the pilot's second-stage failure (2026-06-10):
//     the config resolved to the Python bridge and the agent got a duplicate
//     a2a server instead of the management registry.
//   - env MOLECULE_MCP_MODE=management — the SAME binary serves the
//     21-tool workspace a2a registry by default; only management mode
//     registers the org-admin tools (list_workspaces et al). Without it
//     the concierge gets a duplicate a2a server and zero admin tools.
//
// Auth comes from the container env (MOLECULE_API_KEY / MOLECULE_API_URL /
// MOLECULE_ORG_ID — wired by conciergePlatformMCPEnv); MCP-host env merges
// over process env, so the mode flag composes with those.
//
// SELF-HOST CAVEAT: the local stack provisions the concierge on the ordinary
// `claude-code` image, which does NOT ship the molecule-platform-mcp bin. The
// executor's _apply_extra_mcp_servers skips an entry whose command is
// absent, so declaring this block can never crash the agent or wedge the SDK
// init locally — the identity (system prompt) works everywhere; the org-admin
// MCP tools only light up on the platform-agent image.
const conciergeMCPServersBlock = `mcp_servers:
  - name: platform
    command: molecule-platform-mcp
    env:
      MOLECULE_MCP_MODE: management
`

// conciergeMCPFragmentFile is the standalone overlay fragment carrying the
// SAME declaration as conciergeMCPServersBlock. Written UNCONDITIONALLY by
// conciergeIdentityFiles — unlike the config.yaml append, it does not depend
// on resolving a base config. On the SaaS restart-provision path all three
// base resolutions miss (no in-memory configFiles, no templatePath, no
// exec-readable container), so the appended block silently never shipped and
// the concierge booted without its admin MCP (the pilot's TOOLS-FAIL).
// The runtime executor merges /configs/mcp_servers.yaml after config.yaml;
// older runtimes ignore the extra file — strictly additive.
const conciergeMCPFragmentFile = "mcp_servers.yaml"

// conciergeRuntime is the runtime the platform agent (concierge) always runs as
// — installPlatformAgent hardcodes it (kind='platform' rows insert runtime
// 'claude-code'). conciergeDeclaredModel is validated against the registry for
// THIS runtime at provision time.
const conciergeRuntime = "claude-code"

// conciergeDeclaredModel is the platform agent's OWN declared model — a
// deliberate part of the platform-agent product spec, mirroring the claude-code
// template's `runtime_config.model` SSOT. It is NOT a generic "platform default
// for user workspaces": the CTO SSOT directive (2026-05-22,
// feedback_workspace_model_required_no_platform_default_dynamic_credential_intake)
// forbids the platform from defaulting a USER workspace's model — model is
// required user input there. The concierge is the platform-agent product itself
// (installed by the platform, not a user), so it carries an explicit declared
// model exactly as a template declares one.
//
// core#2594: before this, the concierge had NO stored model. It ran kimi ONLY
// because the provision path's MOLECULE_LLM_DEFAULT_MODEL env fail-open injected
// MOLECULE_MODEL; with that fail-open removed, a model-less concierge would
// silently drop to the runtime's hardcoded `anthropic:claude-opus-4-7` fallback
// (molecule_runtime/config.py _picked_model_from_env). Storing the model
// explicitly (a) makes GET /workspaces/:id/model — and the canvas Config tab —
// show the resolved model instead of blank, and (b) lets the provision path fail
// CLOSED (no opaque substitution) for everything else. The value matches the
// prod MOLECULE_LLM_DEFAULT_MODEL the concierge already runs on, so this is
// behavior-preserving. A CI test asserts it stays registered for the runtime.
const conciergeDeclaredModel = "moonshot/kimi-k2.6"

// SelfHostedPlatformAgentID is the deterministic platform-agent id used when no
// control plane is present to derive a per-org id (self-hosted / local). There
// is one platform agent per self-hosted tenant, so a fixed namespaced uuidv5 is
// sufficient and stable across restarts.
var SelfHostedPlatformAgentID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("molecule:self-hosted:platform-agent")).String()

// defaultCreateParentID resolves the parent a NEW workspace should nest under
// when the caller didn't pass one. Order:
//  1. The org's platform-agent root (kind='platform'), if exactly one — the
//     intended home (core#2609).
//  2. FALLBACK (core#2697): when the org has NO platform-agent (e.g. a tenant
//     provisioned with only a plain root workspace — JRS had just its SEO Agent
//     at parent_id NULL), nest under the SOLE non-removed root workspace if there
//     is exactly one. Without this, new workspaces scatter at bare root as
//     siblings of that root agent, and approval/discovery treat each NULL-parent
//     row as its own org root, breaking hierarchy + delegation routing.
//
// The root fallback fires ONLY for the ZERO-platform case. When MULTIPLE
// platform agents exist (ambiguous), we preserve the original fail-soft
// behavior and return "" WITHOUT falling back to a root — picking a root there
// would silently change the intended ambiguous-platform semantics (CR2 #2783).
// Returns "" when: >1 platform; or 0 platform with 0/>1 roots — preserving
// bootstrap/self-host multi-root behavior. The DURABLE fix is guaranteeing every
// org has a platform-agent at provision; this is the safe runtime fallback.
func defaultCreateParentID(ctx context.Context) string {
	// Count platform-agent roots directly (LIMIT 2 distinguishes 0 / 1 / >1).
	plats := queryUpToTwoIDs(ctx,
		`SELECT id FROM workspaces WHERE COALESCE(kind, 'workspace') = 'platform' AND status != 'removed' LIMIT 2`)
	if len(plats) == 1 {
		return plats[0] // the platform-agent root (core#2609)
	}
	if len(plats) >= 2 {
		return "" // ambiguous platform — fail soft, do NOT fall back to a root
	}
	// Exactly ZERO platform agents → nest under the sole plain root if unambiguous.
	roots := queryUpToTwoIDs(ctx,
		`SELECT id FROM workspaces WHERE parent_id IS NULL AND status != 'removed' LIMIT 2`)
	if len(roots) == 1 {
		return roots[0]
	}
	return ""
}

// queryUpToTwoIDs runs an id-selecting query (with its own LIMIT 2) and returns
// up to two ids; on any error it returns an empty slice (fail-soft — defaulting
// the parent is best-effort and must never fail the create).
func queryUpToTwoIDs(ctx context.Context, query string) []string {
	rows, err := db.DB.QueryContext(ctx, query)
	if err != nil {
		return nil
	}
	defer rows.Close()
	ids := make([]string, 0, 2)
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

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
		// Always-shipped fragment: declares the platform MCP regardless of
		// whether a base config.yaml was resolvable (see
		// conciergeMCPFragmentFile). Idempotent — fixed content, re-seeded
		// every provision cycle, never touches config.yaml.
		conciergeMCPFragmentFile: []byte(conciergeMCPServersBlock),
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
	// The management-mode tool registry (mcp-server >=1.5.0,
	// src/tools/management/client.ts) authenticates with
	// MOLECULE_ORG_API_KEY — a distinct env from the connectivity-preflight
	// MOLECULE_API_KEY. The tenant ADMIN_TOKEN is a valid bearer for the
	// tenant-admin surface those tools call (same header shape as the
	// install/restart curls), so wire it under both names. Verified live on
	// the agents-team pilot: with only MOLECULE_API_KEY set, every
	// management tool returns AUTH_ERROR.
	setIfAbsent("MOLECULE_ORG_API_KEY", os.Getenv("ADMIN_TOKEN"))
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

	// 0. Concierge model (core#2594). The platform agent carries an explicit,
	//    SSOT-declared model so it never relies on a silent default — neither the
	//    (now-removed) MOLECULE_LLM_DEFAULT_MODEL env fail-open nor the runtime's
	//    hardcoded anthropic:claude-opus-4-7 fallback. Seed the container env for
	//    THIS provision AND persist the MODEL workspace_secret so GET /model (the
	//    canvas Config tab) shows the resolved model. Self-healing: a pre-existing
	//    concierge with no stored model gets it on its next provision cycle.
	h.ensureConciergeModel(ctx, workspaceID, envVars)

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
		if b, err := h.provisioner.ExecRead(ctx, provisioner.ContainerName(workspaceID), "/configs/config.yaml"); err == nil {
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

// ensureConciergeModel makes the platform agent's model explicit (core#2594).
// It (1) seeds the container model env for the current provision and (2)
// persists the MODEL workspace_secret so the read endpoint / canvas Config tab
// surface the resolved model. The model is the concierge's declared SSOT model,
// validated against the registry for its runtime. If validation fails (registry
// drift — a build bug caught by the CI test), it sets NOTHING: the downstream
// universal MISSING_MODEL gate then fails the provision CLOSED rather than
// letting the runtime pick an opaque default.
func (h *WorkspaceHandler) ensureConciergeModel(ctx context.Context, workspaceID string, envVars map[string]string) {
	// SEED-ONLY (CTO 2026-06-12: customer setting > platform default; the
	// concierge's model is changeable like any workspace, "anytime"). If a MODEL
	// secret already exists — whether the original seed or a model the customer
	// later picked in the canvas — RESPECT it: loadWorkspaceSecrets +
	// applyRuntimeModelEnv have already put it in envVars, so do nothing. Only
	// SEED the declared default when the concierge has no model at all (first
	// boot). Pre-fix this function re-asserted conciergeDeclaredModel on EVERY
	// provision, silently reverting the customer's pick (e.g. kimi-for-coding →
	// moonshot/kimi-k2.6) — exactly the platform-overriding-customer violation
	// the SSOT directive forbids.
	if existing := readStoredModelSecret(ctx, workspaceID); existing != "" {
		return // explicit model already set; never overwrite the customer's choice
	}

	// First boot — no model yet. Seed the concierge's declared default.
	model := conciergeDeclaredModel
	if ok, why := validateRegisteredModelForRuntime(conciergeRuntime, model); !ok {
		log.Printf("Provisioner: concierge %s declared model %q is NOT registered for runtime %q (%s) — leaving model unset; provision will fail closed", workspaceID, model, conciergeRuntime, why)
		return
	}
	if ok, why := validateDerivedProviderInRegistry(conciergeRuntime, model); !ok {
		log.Printf("Provisioner: concierge %s declared model %q has no derivable registry provider for runtime %q (%s) — leaving model unset; provision will fail closed", workspaceID, model, conciergeRuntime, why)
		return
	}

	// Seed the container env (precedence MOLECULE_MODEL > MODEL in the runtime).
	// applyRuntimeModelEnv already ran with an empty payload model for the
	// concierge (no stored MODEL on first boot), so set both canonical names
	// here so this provision actually runs the seeded default.
	envVars["MOLECULE_MODEL"] = model
	envVars["MODEL"] = model

	// Persist so GET /workspaces/:id/model returns it (Config tab visibility).
	if setErr := setModelSecret(ctx, workspaceID, model); setErr != nil {
		log.Printf("Provisioner: concierge %s persist MODEL secret failed: %v (env still seeded for this provision)", workspaceID, setErr)
	}
}

// readStoredModelSecret returns the decrypted MODEL workspace_secret, or "" when
// none is stored (or on any read/decrypt error — treated as "unset" so a
// transient miss re-seeds rather than wedges). Used by ensureConciergeModel to
// decide seed-vs-respect.
func readStoredModelSecret(ctx context.Context, workspaceID string) string {
	var stored []byte
	var version int
	if err := db.DB.QueryRowContext(ctx,
		`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = 'MODEL'`,
		workspaceID).Scan(&stored, &version); err != nil {
		return ""
	}
	dec, err := crypto.DecryptVersioned(stored, version)
	if err != nil {
		return ""
	}
	return string(dec)
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
	body, err := reader.ExecRead(ctx, provisioner.ContainerName(id), "/configs/system-prompt.md")
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

	// 0. If a different platform root already exists, downgrade it to 'workspace'
	//    so it no longer blocks the partial unique index uniq_workspaces_one_platform_root.
	//    It will then be picked up as an old root and re-parented under the new
	//    platform agent (defense-in-depth: covers pathological cases where a
	//    platform root was created with a non-canonical id, e.g. test fixtures
	//    or a prior failed migration).
	if _, err := tx.ExecContext(ctx, `
		UPDATE workspaces SET kind = 'workspace', updated_at = now()
		WHERE kind = 'platform' AND parent_id IS NULL AND id <> $1
	`, platformID); err != nil {
		return fmt.Errorf("downgrade existing platform root: %w", err)
	}

	// 1. Ensure the platform-agent row exists as a kind='platform' root.
	//    ON CONFLICT keeps it a platform root if it was pre-seeded; the row is
	//    tier 0 and never billed/provisioned as an ordinary workspace EC2.
	//    Status starts as 'offline' — there is no container yet; 'online' is a
	//    green-dot lie until the first heartbeat (core#2508).
	//
	//    parent_name_uniq collision: the platform-agent name is deterministic
	//    and unique per org by construction (e.g. "Org Concierge"). A pre-
	//    existing root with the same name is a data inconsistency; we fail loud
	//    rather than silently rename/reparent, which could orphan billing or
	//    provisioning state. The integration tests use unique names per fixture
	//    to avoid cross-test collision (CR-A RC 10610).
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, kind, tier, status, runtime, parent_id)
		VALUES ($1, $2, 'platform', 0, 'offline', 'claude-code', NULL)
		ON CONFLICT (id) DO UPDATE SET kind = 'platform', runtime = 'claude-code', parent_id = NULL
	`, platformID, name); err != nil {
		return fmt.Errorf("upsert platform agent: %w", err)
	}

	// 2. Capture the org's other current roots (everything at parent_id IS NULL
	//    except the platform agent itself). In a one-org tenant DB this is the
	//    single team root; the query tolerates 0 (already installed) or N.
	//    FOR UPDATE prevents a root created mid-install from escaping re-parent
	//    (core#2508).
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM workspaces WHERE parent_id IS NULL AND id <> $1 FOR UPDATE`, platformID)
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
		// org_plugin_allowlist has UNIQUE(org_id, plugin_name). A plain UPDATE
		// collides 23505 when N>1 old roots allowlisted the SAME plugin.
		// INSERT…SELECT…ON CONFLICT DO NOTHING deduplicates; DELETE leftovers
		// cleans the old-root rows (core#2508).
		//
		// Column-list matches the actual schema (026_org_plugin_allowlist.up.sql):
		//   id, org_id, plugin_name, enabled_by, enabled_at
		// id is auto-populated by gen_random_uuid(); enabled_by is NOT NULL but
		// preserved from the old root row (same admin who enabled it);
		// enabled_at is preserved verbatim (no audit-time rewrite on
		// re-parenting — the original "when this was enabled" stays stable).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO org_plugin_allowlist (org_id, plugin_name, enabled_by, enabled_at)
			SELECT $1, plugin_name, enabled_by, enabled_at
			FROM org_plugin_allowlist
			WHERE org_id = $2
			ON CONFLICT (org_id, plugin_name) DO NOTHING
		`, platformID, root); err != nil {
			return fmt.Errorf("migrate org_plugin_allowlist for %s: %w", root, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM org_plugin_allowlist WHERE org_id = $1`, root); err != nil {
			return fmt.Errorf("delete old org_plugin_allowlist for %s: %w", root, err)
		}
	}

	return tx.Commit()
}
