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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/cpurl"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/crypto"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/db"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/models"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/orgtoken"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/providers"
	"git.moleculesai.app/molecule-ai/molecule-core/workspace-server/internal/provisioner"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	molcontracts "go.moleculesai.app/sdk/gen/go/molcontracts"
	"gopkg.in/yaml.v3"
)

// The concierge system-prompt template, the concierge MCP servers block, the
// concierge MCP fragment file (mcp_servers.yaml), the concierge runtime, the
// concierge declared model, and the concierge identity files function were
// REMOVED as part of RFC #2843 §10a (the concierge de-hardcode migration). The
// concierge's identity — system prompt, model, runtime, MCP wiring — is now
// delivered via the molecule-ai-workspace-template-platform-agent template
// (manifest.json workspace_templates entry) and applied like any other runtime
// template. ZERO concierge literals remain in core.
//
// A minimal {{CONCIERGE_NAME}} substitution is performed by
// applyConciergeProvisionConfig (see PR description for the substitution
// recommendation: option (a) — substitute, with the per-instance name; the
// template's prompts/concierge.md already has the placeholder where the name
// goes). The MCP env (conciergePlatformMCPEnv) is still concierge-specific
// because the env wiring is per-MCP-binary, not per-template.

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

// (the concierge identity files function removed in RFC #2843 §10a — the concierge's
// identity is now delivered via the platform-agent template's
// prompts/concierge.md + config.yaml + mcp_servers.yaml, applied like
// any other runtime template. See substituteConciergeName below for
// the only remaining per-instance identity step.)

// resolveConciergeAdminCredential returns the org-admin credential the concierge's
// management MCP authenticates with. WS-C: the concierge is an AGENT, so it carries
// a MANAGED org token (named, revocable, audited) rather than the tenant's raw
// break-glass ADMIN_TOKEN (the un-managed root — see wsauth AdminAuth tiers). An
// org token grants the identical admin surface, so the MCP works the same, but the
// concierge's standing admin is now revocable and shows up in the org-token list.
//
// Rotates on every provision: mints a fresh token, then revokes every PRIOR live
// concierge token for this workspace (keyed on a deterministic created_by marker),
// so the credential can't accumulate against the mint ceiling and a replaced box's
// token dies. Falls back to the raw ADMIN_TOKEN when there is no org anchor
// (self-host/local — MOLECULE_ORG_ID unset) or the mint fails: the concierge must
// never boot without an admin credential.
func resolveConciergeAdminCredential(ctx context.Context, workspaceID string) string {
	adminToken := os.Getenv("ADMIN_TOKEN")
	orgID := strings.TrimSpace(os.Getenv("MOLECULE_ORG_ID"))
	// A managed org token must anchor to a real org UUID (org_api_tokens.org_id is
	// a uuid column, so a non-UUID would fail the mint query). No/non-UUID org id —
	// self-host, local dev, or the cp-stub replay harness (MOLECULE_ORG_ID is
	// "harness-org-alpha") — means there is no org-token anchor: keep the break-glass
	// ADMIN_TOKEN and skip the mint entirely (no failed query, no noise).
	if _, err := uuid.Parse(orgID); err != nil {
		return adminToken
	}
	owner := "system:concierge:" + workspaceID
	plaintext, newID, err := orgtoken.Issue(ctx, db.DB, "concierge (auto-rotated)", owner, orgID, orgtoken.AuditLogRequestContext{})
	if err != nil {
		log.Printf("concierge %s: managed org-token mint failed (%v) — falling back to break-glass ADMIN_TOKEN", workspaceID, err)
		return adminToken
	}
	// Revoke every prior live concierge token for this workspace (all but the one
	// just minted) so exactly one stays live. Best-effort: a revoke failure just
	// leaves a stale token for the next provision to clean up.
	if toks, lerr := orgtoken.List(ctx, db.DB); lerr == nil {
		for _, t := range toks {
			if t.CreatedBy == owner && t.ID != newID {
				if _, rerr := orgtoken.Revoke(ctx, db.DB, t.ID, orgtoken.AuditLogRequestContext{}, owner); rerr != nil {
					log.Printf("concierge %s: revoke stale org-token %s: %v", workspaceID, t.ID, rerr)
				}
			}
		}
	}
	return plaintext
}

// conciergePlatformMCPEnv injects the env the platform MCP child reads at spawn
// (RFC §5.5/§5.6). orgAdminCred is the org-admin bearer — a managed org token on
// SaaS, else the raw ADMIN_TOKEN (see resolveConciergeAdminCredential); the
// platform URL is the in-cluster PLATFORM_URL (e.g. http://platform:8080). Existing
// values in env win, so an operator/CP override is never clobbered. No-op for a
// non-platform workspace. Best-effort: when orgAdminCred is empty the key is
// absent and management MCP calls fail closed; local operators must configure
// ADMIN_TOKEN rather than relying on unauthenticated bootstrap.
func conciergePlatformMCPEnv(env map[string]string, workspaceID, orgAdminCred string) {
	setIfAbsent := func(k, v string) {
		if v == "" {
			return
		}
		if _, ok := env[k]; !ok {
			env[k] = v
		}
	}

	// Platform-agent identity marker (runtime wheel:
	// platform_agent_identity.PLATFORM_AGENT_IMAGE_ENV). Historically only the
	// baked Dockerfile.platform-agent image set it; a concierge on a PLAIN
	// runtime image (hermes) had to infer platform-ness from "management MCP
	// present in the rendered runtime config" — a signal that only becomes
	// true MID-boot, so the runtime's boot-step emitter silently dropped
	// steps 1-4 (PLG/ID/RT/MCP idle keycaps on the Enter OS screen,
	// 2026-07-19). This function is kind-gated to the concierge, which IS the
	// platform agent regardless of which image it runs — stamp the marker so
	// every consumer of the identity gate is correct from process start.
	setIfAbsent("MOLECULE_PLATFORM_AGENT_IMAGE_BAKED", "1")
	// The management MCP's SELF default: install_plugin / get_conversation_history
	// fall back to MOLECULE_WORKSPACE_ID when workspace_id is omitted, making
	// "act on MY OWN workspace" the zero-config case (self-reprovision §5.2).
	// Without this the concierge's MCP env never carried its own id and the
	// SELF default failed closed with INVALID_ARGUMENTS on every live agent.
	setIfAbsent("MOLECULE_WORKSPACE_ID", workspaceID)
	setIfAbsent("MOLECULE_API_KEY", orgAdminCred)
	// The management-mode tool registry (mcp-server >=1.5.0,
	// src/tools/management/client.ts) authenticates with
	// MOLECULE_ORG_API_KEY — a distinct env from the connectivity-preflight
	// MOLECULE_API_KEY. The tenant ADMIN_TOKEN is a valid bearer for the
	// tenant-admin surface those tools call (same header shape as the
	// install/restart curls), so wire it under both names. Verified live on
	// the agents-team pilot: with only MOLECULE_API_KEY set, every
	// management tool returns AUTH_ERROR.
	setIfAbsent("MOLECULE_ORG_API_KEY", orgAdminCred)
	// MOLECULE_API_URL: prefer an explicit env, else the in-cluster platform URL.
	apiURL := os.Getenv("MOLECULE_API_URL")
	if apiURL == "" {
		apiURL = os.Getenv("PLATFORM_URL")
	}
	setIfAbsent("MOLECULE_API_URL", apiURL)
	setIfAbsent("MOLECULE_ORG_ID", os.Getenv("MOLECULE_ORG_ID"))

	// Authenticate the on-box plugin boot-install's gitea fetch (core#3065).
	// The concierge declares its management MCP as a gitea:// plugin
	// (molecule-ai-plugin-molecule-platform-mcp). That repo is PUBLIC, so the
	// box's git-native boot-install clones it ANONYMOUSLY — its per-host git
	// credential-helper fires only on a 401, which a public repo never returns, so
	// no token is ever sent to it (this is the token-on-public 401-poison fix).
	// The GIT_HTTP_* wiring below is therefore NOT needed for the mgmt-MCP; it is
	// kept as back-compat for OTHER, genuinely-private plugin/template fetches — a
	// private repo's 401 triggers the helper with these creds. Derived from the
	// read-only MOLECULE_TEMPLATE_REPO_TOKEN the box holds, in the basic-auth shape
	// the resolver uses (PAT as username + literal "x-oauth-basic" password).
	// setIfAbsent so an operator-supplied GIT_HTTP_* / persona token still wins.
	if tok := strings.TrimSpace(os.Getenv("MOLECULE_TEMPLATE_REPO_TOKEN")); tok != "" {
		setIfAbsent("GIT_HTTP_USERNAME", tok)
		setIfAbsent("GIT_HTTP_PASSWORD", "x-oauth-basic")
	}

	// Thread the plugin registry/SCM base to the box so its boot-install fetches
	// the management-MCP plugin (conciergePlatformMCPSource) from a CONFIGURABLE
	// host — a mirror / airgap / self-host Gitea — instead of a baked
	// git.moleculesai.app. The gitea:// source carries no host; the box resolves it
	// against this base. Fleet-wide default (defaultPluginRegistryBase), NOT a
	// per-tenant surface. setIfAbsent so an operator-supplied value on the box env
	// still wins. The repo is PUBLIC, so this needs no credential of its own — the
	// GIT_HTTP_* wiring above is for OTHER private plugin/template fetches.
	setIfAbsent(conciergePluginRegistryEnvVar, pluginRegistryBase())
}

// applyConciergeProvisionConfig is the provision-time hook for the platform
// agent. Called from prepareProvisionContext for EVERY provision of a
// kind='platform' workspace (create, restart, auto-recover). It is a no-op
// for ordinary workspaces.
//
// Post RFC #2843 §10a: the concierge's PROMPT + MCP-wiring identity (system
// prompt, agent-skills) is delivered via the molecule-ai-workspace-template-
// platform-agent template and applied like any other runtime template. This
// hook's responsibilities are (0) SEED the concierge's declared model so the
// provision passes the universal MISSING_MODEL gate (core#2594), (1) inject the
// platform-MCP env (org-admin token + platform URL + org id) and (2) the
// per-instance {{CONCIERGE_NAME}} substitution in the system-prompt.md.
//
// IMPORTANT (incident 2026-06-15, regression from #2919): the model is NOT
// delivered via the template. The MISSING_MODEL gate reads the stored MODEL
// workspace_secret at provision time — BEFORE any template config.yaml is
// fetched — so a model-less platform agent fails closed ("reached provisioning
// with no model set"). #2919 removed the model-seed on the theory the
// platform-agent template would supply it, but that template entry was never
// added to the manifest. The concierge is the platform-agent PRODUCT itself, so
// it carries an explicit declared model in code (core#2594) exactly as a
// template declares one — this is the SSOT-correct source, not a stopgap. The
// CI test TestApplyConciergeProvisionConfig_SeedsModel gates against re-removal.
//
// Returns the (possibly newly-allocated) configFiles map so the caller can
// rebind it — configFiles is nil on the auto-restart path, where this is the
// thing that introduces the substitution.
func (h *WorkspaceHandler) applyConciergeProvisionConfig(
	ctx context.Context,
	workspaceID, templatePath string,
	configFiles map[string][]byte,
	envVars map[string]string,
	name string,
) map[string][]byte {
	var kind, runtime string
	if err := db.DB.QueryRowContext(ctx,
		`SELECT COALESCE(kind, 'workspace'), COALESCE(runtime, '') FROM workspaces WHERE id = $1`, workspaceID).Scan(&kind, &runtime); err != nil {
		// Non-fatal: a missing row / probe error just means "treat as ordinary".
		return configFiles
	}
	if kind != models.KindPlatform {
		return configFiles
	}
	// The concierge runtime is per-org (P3b). Fall back to the KMS-resolved
	// platform default (conciergeDefaultRuntime: MOLECULE_DEFAULT_RUNTIME else the
	// const) when the row carries none (legacy rows / self-host seed before the
	// column was set).
	if strings.TrimSpace(runtime) == "" {
		runtime = conciergeDefaultRuntime()
	}

	// 0. Concierge model (core#2594). The platform agent carries an explicit,
	//    SSOT-declared model so it never relies on a silent default. The
	//    universal MISSING_MODEL gate (prepareProvisionContext) reads the stored
	//    MODEL secret at provision time — before any template config.yaml is
	//    fetched — so this MUST run here, not be deferred to the template.
	//    Seed-only: it respects a model the customer later picked.
	h.ensureConciergeModel(ctx, workspaceID, runtime, envVars)

	// 0b. Concierge LLM provider pin (companion to the model seed). The
	//    molecule-runtime wheel DERIVES a provider slug from the model id
	//    ("moonshot/kimi-k2.6" -> "moonshot" via _derive_provider_from_model),
	//    which is a model-PREFIX on the `platform` provider, NOT a provider
	//    NAME — so the claude-code adapter's _resolve_provider fail-closes
	//    ("provider='moonshot' but it is not in the providers registry") and
	//    the concierge boots configuration_status=not_configured: online but
	//    unable to run a single turn (last_outbound_at stays null).
	//
	//    The platform-agent template config.yaml's `provider: platform` field
	//    does NOT fix this on SaaS: the on-box /configs/config.yaml is the
	//    BAKED base-image config (the box reports the base claude-code image's
	//    8-entry provider registry, not the platform-agent template's 3), so
	//    the template `provider:` scalar never reaches the adapter. Seeding
	//    LLM_PROVIDER (env, highest precedence in the wheel's
	//    LLM_PROVIDER > YAML provider: > derive chain; injected via the
	//    workspace secret) is the robust pin — it survives restart and the
	//    regenerated config.yaml. Verified on prod: setting LLM_PROVIDER=platform
	//    flipped a stuck concierge from not_configured to ready + responding.
	//    Seed-only + gated to the platform-managed model namespace so it never
	//    overrides a BYOK/self-host concierge (see ensureConciergeProvider).
	h.ensureConciergeProvider(ctx, workspaceID, runtime, envVars)

	// 0c. Declare the concierge's management MCP as a PLUGIN (RFC:
	//    rfc-platform-mcp-as-plugin). The asset-channel mcp_servers.yaml does NOT
	//    reach the on-box /configs (the box runs the baked base-image config), so
	//    the concierge boots with no management MCP — generic Claude Code, no
	//    create_workspace. Routing it through the plugin channel (the path that
	//    reliably delivers skills) fixes that: declare it here so the post-online
	//    reconcile + boot-install wire molecule-platform-mcp via MCPServerAdaptor.
	//    This declaration runs ONLY on the kind=platform concierge (this function
	//    is kind-gated) → it is the primary entitlement gate for the privileged
	//    org-admin MCP; recordDeclaredPlugin fail-closes the same name for any
	//    non-platform workspace as defense-in-depth. Idempotent (upsert).
	if rec, skip := seedTemplatePlugins(ctx, workspaceID, []string{conciergePlatformMCPSource}); skip > 0 {
		log.Printf("Provisioner: concierge %s could not declare %q plugin (recorded=%d skipped=%d) — management MCP may be absent until next provision", workspaceID, conciergePlatformMCPSource, rec, skip)
	}

	// 1. Platform-MCP env (org-admin token + platform URL + org id + self id).
	//    WS-C: the concierge authenticates with a MANAGED, rotated org token
	//    (revocable/audited), not the raw break-glass ADMIN_TOKEN.
	conciergePlatformMCPEnv(envVars, workspaceID, resolveConciergeAdminCredential(ctx, workspaceID))

	// 2. Compose the concierge's /configs from its ACTUAL (switchable) runtime's
	//    NATIVE base config + the runtime-agnostic persona, grafted per that
	//    runtime's convention. This is the GENERAL fix for the #2027
	//    runtime-seed-mismatch abort (BUG: the single platform-agent template
	//    config.yaml pins `runtime: claude-code`, so a concierge whose row
	//    declares any OTHER runtime — including the default hermes — received a
	//    config whose top-level runtime contradicted the requested one; the
	//    provision aborted and the concierge got no persona). The concierge runs
	//    on the plain per-runtime image (selectImage: kind=platform →
	//    RuntimeImages[runtime]), so its config.yaml MUST be that runtime's own
	//    config, and the persona is grafted per the runtime's convention:
	//    claude-code reads system-prompt.md; every other runtime reads
	//    prompt_files: [prompts/concierge.md]. See composeConciergeRuntimeConfig.
	//
	//    The {{CONCIERGE_NAME}} substitution is applied to the delivered persona
	//    at provision time (the runtime's build_system_prompt does NOT template
	//    prompt files). Idempotent: a re-provision re-runs it; the result is
	//    stable (the placeholder is gone after the first substitution).
	if configFiles == nil {
		configFiles = map[string][]byte{}
	}
	if composed, err := h.composeConciergeRuntimeConfig(runtime); err != nil {
		// Base config unavailable (template-cache miss / a test without the
		// template tree). Fall back to the delivered config UNCHANGED and keep the
		// historical {{CONCIERGE_NAME}} substitution so we never regress the
		// claude-code path — but log loudly so a real cache miss is visible.
		log.Printf("Provisioner: concierge %s could not compose runtime-native config for runtime=%q (%v) — keeping delivered config + {{CONCIERGE_NAME}} substitution", workspaceID, runtime, err)
		if prompt, ok := configFiles["system-prompt.md"]; ok {
			configFiles["system-prompt.md"] = substituteConciergeName(prompt, name)
		}
	} else {
		configFiles["config.yaml"] = composed
		persona := substituteConciergeName(h.resolveConciergePersonaBytes(configFiles), name)
		if len(persona) > 0 {
			// Always land the persona at prompts/concierge.md (the path the
			// grafted prompt_files references for non-claude-code runtimes); ALSO
			// deliver system-prompt.md for claude-code (its runtime reads that file
			// by convention).
			configFiles[conciergePersonaPromptPath] = persona
			if runtime == claudeCodeRuntime {
				configFiles["system-prompt.md"] = persona
			}
		}
		log.Printf("Provisioner: concierge %s composed runtime-native /configs for runtime=%q (persona grafted per convention, name=%q, %d config file(s))",
			workspaceID, runtime, name, len(configFiles))
	}
	return configFiles
}

// conciergePersonaPromptPath is the /configs-relative path the concierge's
// runtime-agnostic persona (prompts/concierge.md, shipped by the platform-agent
// template) lands at. It is what the grafted config.yaml's prompt_files points to
// for every NON-claude-code runtime; claude-code reads system-prompt.md instead.
const conciergePersonaPromptPath = "prompts/concierge.md"

// conciergeBaseTemplateName maps the concierge's runtime to the on-disk template
// whose config.yaml is that runtime's NATIVE base config (runtime + provider
// registry the runtime's own adapter parses). The concierge runs on the plain
// per-runtime image (selectImage: kind=platform uses RuntimeImages[runtime]), so
// its /configs/config.yaml MUST be that runtime's config, not the historical
// claude-code-pinned platform-agent config. claude-code's baked base template is
// the "-default" variant; every other runtime's template dir == its name.
func conciergeBaseTemplateName(runtime string) string {
	runtime = strings.TrimSpace(runtime)
	if runtime == "" {
		// Resolve the ACTUAL default (env-aware), not the compiled-in const —
		// an operator's MOLECULE_DEFAULT_RUNTIME must drive the template pick.
		runtime = conciergeDefaultRuntime()
	}
	// The "-default" suffix is a claude-code-ONLY convention (its baked base
	// template dir is "claude-code-default"); every other runtime's template
	// dir == its runtime name. Previously keyed off defaultConciergeRuntime,
	// which broke the moment the default stopped being claude-code.
	if runtime == claudeCodeRuntime {
		return claudeCodeRuntime + "-default"
	}
	return runtime
}

// composeConciergeRuntimeConfig builds the concierge's /configs/config.yaml from
// its ACTUAL runtime's native base template, grafting the runtime-agnostic
// persona per that runtime's convention. This is the general fix for the #2027
// runtime-seed-mismatch abort that fired for every non-claude-code concierge.
//
// Returns the composed config bytes, or a non-nil error when the runtime's base
// config is unavailable (template-cache miss / tests without the template tree) —
// the caller then falls back to the delivered config unchanged.
//
// Transforms applied to the base config (yaml.v3 node edit, so the rest of the
// runtime-native config — provider registry, models, a2a, timeouts — is
// preserved):
//   - runtime_config.required_env -> []  (the concierge is platform-managed; its
//     LLM creds are injected server-side via LLM_PROVIDER=platform, so it needs
//     no tenant key. Without this a codex/claude-code base config whose
//     runtime_config.required_env lists OPENAI_API_KEY / an anthropic key would
//     trip the missingRequiredEnv preflight and abort the concierge.)
//   - prompt_files -> [prompts/concierge.md]  for every NON-claude-code runtime
//     (the persona is the sole system-prompt file; claude-code reads
//     system-prompt.md, delivered separately, so its prompt_files is untouched).
func (h *WorkspaceHandler) composeConciergeRuntimeConfig(runtime string) ([]byte, error) {
	base := conciergeBaseTemplateName(runtime)
	dir, err := resolveWorkspaceTemplatePath(h.configsDir, h.cacheDir, base)
	if err != nil {
		return nil, fmt.Errorf("resolve concierge base template %q: %w", base, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read concierge base config %q: %w", base, err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse concierge base config %q: %w", base, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("concierge base config %q is not a YAML mapping", base)
	}
	root := doc.Content[0]

	// Neutralize required_env — the concierge is platform-managed (no tenant key).
	if rc := yamlMappingGet(root, "runtime_config"); rc != nil && rc.Kind == yaml.MappingNode {
		yamlMappingSet(rc, "required_env", yamlEmptySeq())
	}
	// Graft the persona per the runtime's convention. claude-code reads
	// system-prompt.md (delivered separately); every other runtime reads the
	// prompt_files list, so the persona becomes its sole prompt file.
	if strings.TrimSpace(runtime) != "" && runtime != claudeCodeRuntime {
		yamlMappingSet(root, "prompt_files", yamlStringSeq(conciergePersonaPromptPath))
	}
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("marshal composed concierge config %q: %w", base, err)
	}
	// Self-host only: graft the platform-agent template's runtime-native
	// `schedules:` node onto the composed config so the concierge boots with the
	// template's schedules. Boot-safe: on ANY problem (SaaS, no template, no
	// schedules node, unparseable template, failed round-trip) we ship `out` —
	// the composed config WITHOUT schedules — unchanged.
	if withSched, grafted := h.graftConciergeSchedules(&doc, root); grafted {
		out = withSched
	}
	return out, nil
}

// graftConciergeSchedules grafts the platform-agent template's top-level
// `schedules:` node onto the composed concierge config root and returns the
// re-marshaled bytes. It is a ONE-TIME, GENERIC enablement: whatever runtime-
// native `schedules:` the platform-agent template carries survives concierge
// config composition — the schedule content is NEVER hardcoded in Go.
//
// Gate: only self-host deployments (SelfHostPlatformSeedEnabled — MOLECULE_ORG_ID
// unset) graft; on SaaS the concierge config stays byte-identical (grafted=false).
//
// The template's schedules are already runtime-native (cron/inline prompt), so
// this is a pure passthrough — renderTemplateSchedulesYAML is NOT called.
//
// Boot-safety (mirrors appendYAMLBlockChecked): after grafting, the whole doc is
// re-marshaled and re-parsed (round-trip guard). If the template dir is
// unresolvable, its config.yaml is missing/unparseable, it carries no schedules
// node, or the merged doc fails the round-trip, grafted=false is returned and the
// caller ships the composed config UNCHANGED — an unloadable config.yaml bricks
// boot, so a missing schedule is always preferred over a broken config.
func (h *WorkspaceHandler) graftConciergeSchedules(doc, root *yaml.Node) ([]byte, bool) {
	if !SelfHostPlatformSeedEnabled() {
		return nil, false // SaaS: concierge config stays byte-identical.
	}
	dir, err := resolveWorkspaceTemplatePath(h.configsDir, h.cacheDir, "platform-agent")
	if err != nil {
		return nil, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		return nil, false
	}
	var tdoc yaml.Node
	if err := yaml.Unmarshal(raw, &tdoc); err != nil {
		log.Printf("Provisioner: concierge schedules graft — platform-agent config.yaml unparseable (%v); shipping composed config WITHOUT schedules", err)
		return nil, false
	}
	if tdoc.Kind != yaml.DocumentNode || len(tdoc.Content) == 0 || tdoc.Content[0].Kind != yaml.MappingNode {
		return nil, false
	}
	sched := yamlMappingGet(tdoc.Content[0], "schedules")
	if sched == nil {
		return nil, false // template carries no schedules → no-op.
	}
	// Graft the node, then round-trip-guard the WHOLE merged doc.
	yamlMappingSet(root, "schedules", sched)
	merged, err := yaml.Marshal(doc)
	if err != nil {
		log.Printf("Provisioner: concierge schedules graft — re-marshal failed (%v); shipping composed config WITHOUT schedules", err)
		return nil, false
	}
	var probe map[string]interface{}
	if err := yaml.Unmarshal(merged, &probe); err != nil {
		log.Printf("Provisioner: concierge schedules graft — merged config.yaml fails to re-parse (%v); shipping composed config WITHOUT schedules (would brick boot)", err)
		return nil, false
	}
	return merged, true
}

// resolveConciergePersonaBytes returns the RAW (unsubstituted) concierge persona
// bytes — the runtime-agnostic prompts/concierge.md shipped by the platform-agent
// template. It prefers the persona already delivered in configFiles (the SaaS
// gitea asset channel populates prompts/concierge.md + system-prompt.md), and
// falls back to reading it from the on-disk platform-agent template cache (the
// local-docker path, where configFiles is nil and the template dir is copied to
// /configs). Returns nil when no persona source is available.
func (h *WorkspaceHandler) resolveConciergePersonaBytes(configFiles map[string][]byte) []byte {
	if b, ok := configFiles[conciergePersonaPromptPath]; ok && len(b) > 0 {
		return b
	}
	if b, ok := configFiles["system-prompt.md"]; ok && len(b) > 0 {
		return b
	}
	if dir, err := resolveWorkspaceTemplatePath(h.configsDir, h.cacheDir, "platform-agent"); err == nil {
		if b, rerr := os.ReadFile(filepath.Join(dir, conciergePersonaPromptPath)); rerr == nil && len(b) > 0 {
			return b
		}
	}
	return nil
}

// yamlMappingGet returns the value node for key in a YAML mapping node, or nil.
func yamlMappingGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlMappingSet sets key=val in a mapping node, replacing an existing value or
// appending a new key/value pair.
func yamlMappingSet(m *yaml.Node, key string, val *yaml.Node) {
	if m == nil || m.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		val)
}

// yamlEmptySeq returns an empty flow sequence node ([]).
func yamlEmptySeq() *yaml.Node {
	return &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Style: yaml.FlowStyle}
}

// yamlStringSeq returns a block sequence of string scalars.
func yamlStringSeq(items ...string) *yaml.Node {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, it := range items {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: it})
	}
	return seq
}

// conciergeNamePlaceholder is the {{CONCIERGE_NAME}} marker the template's
// prompts/concierge.md carries where the per-instance name goes. The runtime's
// build_system_prompt does NOT template prompt files, so applyConciergeProvisionConfig
// performs the substitution at provision time (RFC #2843 §10a).
const conciergeNamePlaceholder = "{{CONCIERGE_NAME}}"

// substituteConciergeName replaces every occurrence of the {{CONCIERGE_NAME}}
// placeholder in a system-prompt byte slice with the per-instance concierge
// name. Stable: if the placeholder is absent, the input is returned
// unchanged. No other templating is performed — keep this minimal.
func substituteConciergeName(prompt []byte, name string) []byte {
	if len(prompt) == 0 {
		return prompt
	}
	// Use a single allocation to avoid multiple string-to-byte conversions
	// on the hot path. strings.Replace is in the standard library and
	// handles the empty-name case safely (the placeholder is replaced
	// with "" — leaving the prompt with a blank first line. We
	// intentionally do NOT guard against empty name here: the caller
	// (defaultPlatformAgentName) guarantees a non-empty name; if it
	// somehow becomes empty, an empty first line is the louder failure
	// mode (visible in the agent's startup log) than a silent skip.
	// CR2 RC 11903 QF1004: strings.ReplaceAll (replaces all) replaces
	// the legacy strings.Replace(s, old, new, -1) "replace all" idiom
	// with the dedicated stdlib helper.
	return []byte(strings.ReplaceAll(string(prompt), conciergeNamePlaceholder, name))
}

// claudeCodeRuntime names the claude-code runtime for its TWO concierge-side
// conventions that are claude-code-SPECIFIC and not default-runtime-specific
// (they were previously conflated with defaultConciergeRuntime, which would
// break the moment the default changed): (a) claude-code reads
// system-prompt.md instead of a prompt_files list, and (b) its baked base
// template dir is the "-default" variant ("claude-code-default") while every
// other runtime's template dir == its runtime name.
const claudeCodeRuntime = "claude-code"

// defaultConciergeRuntime is the compiled-in FALLBACK runtime for the platform
// agent (concierge) when no per-org runtime is specified AND the generic KMS
// platform default env is unset. The platform de-bake (P3b) makes the concierge
// runtime a PARAMETER (threaded through installPlatformAgent + ensureConciergeModel),
// so a per-org concierge can run on codex / openclaw / hermes, not only the legacy
// 'claude-code' image variant. platformDefaultModelFallback is validated
// against the registry FOR THE CHOSEN RUNTIME before being seeded.
//
// SSOT FOLLOW (PR-6 concierge-follows): this const is now only the FALLBACK.
// conciergeDefaultRuntime resolves the runtime default from the generic KMS env
// MOLECULE_DEFAULT_RUNTIME (the same SSOT handlers.Create reads via
// bareCreateDefaultRuntime), falling back to this const when the env is unset.
//
// 'hermes' is the product default for fresh platform agents. Managed deploys can
// still override it through MOLECULE_DEFAULT_RUNTIME; local/self-host boots use
// this fallback when no operator value is present.
const defaultConciergeRuntime = "hermes"

// conciergeDefaultRuntime resolves the concierge's default runtime, honoring the
// generic KMS platform default env MOLECULE_DEFAULT_RUNTIME (injected at deploy
// from the KMS SSOT) over the compiled-in defaultConciergeRuntime fallback. This
// is the SAME env + the SAME known-runtime allowlist the bare-create default path
// uses (runtime_registry.go bareCreateDefaultRuntime), so the concierge follows
// the one platform-runtime SSOT instead of a second hardcoded literal.
//
// FAIL CLOSED on an override the concierge can't run on: the resolved runtime
// must pass the SAME container-backed guard the ensure/install endpoints apply
// to an explicit runtime (platformRuntimeAllowed) — NOT just isKnownRuntime.
// isKnownRuntime accepts external-like/mock meta-runtimes, which have no
// container for the concierge's provision path; a MOLECULE_DEFAULT_RUNTIME of
// e.g. 'external' or 'mock' would otherwise stamp an unprovisionable platform
// root through the empty-runtime (default) path that the explicit-runtime guard
// never sees (core#3496 review). An override that fails the guard is refused
// and we fall back to the compiled-in container-backed default.
func conciergeDefaultRuntime() string {
	if v := strings.TrimSpace(os.Getenv("MOLECULE_DEFAULT_RUNTIME")); v != "" {
		if ok, _ := platformRuntimeAllowed(v); ok {
			return v
		}
		log.Printf("Concierge: MOLECULE_DEFAULT_RUNTIME=%q is not a container-backed runtime the concierge can run on; falling back to %q for the platform-agent default", v, defaultConciergeRuntime)
	}
	return defaultConciergeRuntime
}

// platformDefaultModelFallback is THE single shared platform-default model — one
// value, not per-runtime. The authoritative SSOT remains Infisical
// /shared/controlplane/llm (MOLECULE_LLM_DEFAULT_MODEL), read from the control
// plane on SaaS tenants or from the operator env on self-hosted ones. This const
// is the LAST-RESORT fallback used ONLY when that SSOT value is unset (a CP
// reachable-but-unconfigured response, or a self-hosted/local boot with no
// operator env) — never a per-runtime hardcode. It is a platform-managed-routable
// id (every runtime's `platform` arm in providers.yaml lists it: claude-code,
// codex, openclaw AND hermes), so a fresh concierge on any runtime resolves it to
// the proxy, never to a tenant BYOK key. A genuine CP transport/auth FAILURE
// still fails closed (defaultResolveConciergeModel) — the fallback covers "SSOT
// unset", not "SSOT unreachable".
const platformDefaultModelFallback = "minimax/MiniMax-M2.7"

// conciergeModelResolver resolves the concierge's seed model at provision time.
// It is a variable so tests can stub it. The default implementation fetches the
// model authoritatively from the CP on SaaS tenants, reads the operator env on
// self-hosted tenants, and fails closed otherwise.
var conciergeModelResolver = defaultResolveConciergeModel

// defaultResolveConciergeModel returns the platform default model for a fresh
// concierge. In SaaS (MOLECULE_ORG_ID + ADMIN_TOKEN present) it asks the control
// plane at /cp/tenants/config for MOLECULE_LLM_DEFAULT_MODEL (which the CP in turn
// sources from the Infisical SSOT /shared/controlplane/llm). In self-hosted/local
// mode it reads the MOLECULE_LLM_DEFAULT_MODEL env (the operator's SSOT).
//
// When the SSOT value is UNSET — a CP reachable-but-unconfigured response, or a
// self-hosted boot with no operator env — it returns the ONE shared
// platformDefaultModelFallback (never a per-runtime literal). A genuine CP
// transport/auth FAILURE is NOT "SSOT unset": it still returns an error so
// ensureConciergeModel refuses to seed and the universal MISSING_MODEL gate fails
// closed rather than masking a CP outage with a fabricated default.
func defaultResolveConciergeModel(ctx context.Context) (string, error) {
	// Self-hosted / local dev: the operator's env is the SSOT.
	if os.Getenv("MOLECULE_ORG_ID") == "" || os.Getenv("ADMIN_TOKEN") == "" {
		if v := strings.TrimSpace(os.Getenv("MOLECULE_LLM_DEFAULT_MODEL")); v != "" {
			return v, nil
		}
		// SSOT unset on a self-hosted/local boot → the one shared fallback.
		return platformDefaultModelFallback, nil
	}

	cpModel, err := fetchCPDefaultModel(ctx)
	if err != nil {
		// CP unreachable / non-2xx / bad JSON is an OUTAGE, not "SSOT unset":
		// fail closed rather than fabricate a default.
		return "", fmt.Errorf("MISSING_MODEL: failed to fetch platform default model from CP: %w", err)
	}
	if cpModel == "" {
		// CP reachable but the SSOT carries no platform default → the one
		// shared fallback (in prod the CP boot fail-closes on an empty
		// selector, so this branch only fires on a dev/e2e CP).
		return platformDefaultModelFallback, nil
	}
	return cpModel, nil
}

// fetchCPDefaultModel asks the control plane for the tenant's config and returns
// the value of MOLECULE_LLM_DEFAULT_MODEL. It requires MOLECULE_CP_URL,
// ADMIN_TOKEN, and MOLECULE_ORG_ID env vars. A non-200 response or invalid JSON
// returns an error so the caller fails closed rather than seeding a stale/default
// model.
func fetchCPDefaultModel(ctx context.Context) (string, error) {
	orgID := os.Getenv("MOLECULE_ORG_ID")
	adminToken := os.Getenv("ADMIN_TOKEN")
	if orgID == "" || adminToken == "" {
		return "", fmt.Errorf("missing MOLECULE_ORG_ID or ADMIN_TOKEN")
	}

	// Single CP-URL seam (internal/cpurl): MOLECULE_CP_URL, else the managed
	// default. MOLECULE_CP_DEFAULT_URL lets an OSS deployer redirect it.
	base := cpurl.Base()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", base+"/cp/tenants/config", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("X-Molecule-Org-Id", orgID)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("cp returned %d", resp.StatusCode)
	}

	var cfg map[string]string
	if err := json.Unmarshal(body, &cfg); err != nil {
		return "", err
	}
	return strings.TrimSpace(cfg["MOLECULE_LLM_DEFAULT_MODEL"]), nil
}

// conciergeTemplateForRuntime returns the concierge's platform-agent template
// name. There is ONE runtime-agnostic concierge persona template — "platform-agent"
// — for EVERY runtime (claude-code, openclaw, codex, hermes, …). This is what
// installPlatformAgent stamps into workspaces.template so the asset fetcher pulls
// the concierge identity.
//
// SSOT COLLAPSE (tenant-agent BUG 1, P0): this used to map non-claude-code
// concierges to a per-runtime "<runtime>-platform-agent" name (e.g.
// "openclaw-platform-agent"). No such template is registered in manifest.json, so
// resolveTemplateIdentity fail-closed to an EMPTY identity and the concierge booted
// with NO persona (the 97-byte stub /configs/config.yaml). The concierge identity
// is runtime-agnostic (the SAME orchestrator persona regardless of the underlying
// runtime image), so it now resolves through the single "platform-agent" template
// entry (manifest.json workspace_templates) whose config.yaml + system-prompt.md +
// prompts/concierge.md serve every runtime via the control-plane materializer.
//
// The `runtime` parameter is retained for call-site compatibility; it no longer
// changes the template (kept named for readability at the stamp site).
func conciergeTemplateForRuntime(runtime string) string {
	_ = strings.TrimSpace(runtime) // runtime-agnostic: one concierge persona template for all runtimes
	return "platform-agent"
}

// ensureConciergeModel makes the platform agent's model explicit (core#2594).
// It (1) seeds the container model env for the current provision and (2)
// persists the MODEL workspace_secret so the read endpoint / canvas Config tab
// surface the resolved model. The model is the concierge's declared SSOT model,
// validated against the registry for its runtime. If validation fails (registry
// drift — a build bug caught by the model-registry CI test), it sets NOTHING:
// the downstream universal MISSING_MODEL gate then fails the provision CLOSED
// rather than letting the runtime pick an opaque default.
//
// runtime is the concierge's per-org runtime (P3b). The declared model is
// validated against the registry FOR THIS RUNTIME, so a codex/openclaw concierge
// is gated against codex/openclaw's model set, not claude-code's. An empty runtime
// falls back to the default.
func (h *WorkspaceHandler) ensureConciergeModel(ctx context.Context, workspaceID, runtime string, envVars map[string]string) {
	if strings.TrimSpace(runtime) == "" {
		runtime = conciergeDefaultRuntime()
	}
	// SEED-ONLY (CTO 2026-06-12: customer setting > platform default; the
	// concierge's model is changeable like any workspace, "anytime"). If a MODEL
	// secret already exists — whether the original seed or a model the customer
	// later picked in the canvas — RESPECT it: loadWorkspaceSecrets +
	// applyRuntimeModelEnv have already put it in envVars, so do nothing. Only
	// SEED the declared default when the concierge has no model at all (first
	// boot). Re-asserting on EVERY provision would silently revert the customer's
	// pick — exactly the platform-overriding-customer violation the SSOT
	// directive forbids.
	if existing, readErr := readStoredModelSecret(ctx, workspaceID); readErr != nil {
		log.Printf("Provisioner: concierge %s MODEL read failed (failing closed, NOT seeding): %v", workspaceID, readErr)
		return
	} else if existing != "" {
		// A model is already stored. RECONCILE a platform-managed (proxy) default
		// to the current SSOT so a platform-default bump propagates to an
		// already-seeded concierge instead of freezing at whatever the default was
		// at first boot (the one-shot-seed drift — e.g. a concierge seeded as
		// moonshot/kimi-k2.6 or minimax/MiniMax-M2 never picking up the
		// minimax/MiniMax-M2.7 SSOT). A non-platform (BYOK) model is a deliberate
		// CUSTOMER pick and is left untouched (CTO: customer setting > platform
		// default). EITHER WAY both canonical env names are re-asserted to the SAME
		// effective model so a stale frozen MOLECULE_MODEL from a prior provision
		// can never out-rank a fresh MODEL (runtime precedence is
		// MOLECULE_MODEL > MODEL) — the MODEL/MOLECULE_MODEL split fix.
		h.reconcileExistingConciergeModel(ctx, workspaceID, runtime, existing, envVars)
		return
	}

	// First boot — no model yet. Resolve the concierge's declared default from the
	// authoritative source (CP on SaaS, operator env on self-hosted), then validate
	// it against the registry for its runtime. If resolution fails or returns empty,
	// leave the model unset so the universal MISSING_MODEL gate fails closed rather
	// than seeding a stale or hardcoded default.
	model, err := conciergeModelResolver(ctx)
	if err != nil {
		log.Printf("Provisioner: concierge %s model resolution failed (failing closed, NOT seeding): %v", workspaceID, err)
		return
	}
	if model == "" {
		log.Printf("Provisioner: concierge %s model unresolved (failing closed, NOT seeding)", workspaceID)
		return
	}
	if ok, why := validateRegisteredModelForRuntime(runtime, model); !ok {
		log.Printf("Provisioner: concierge %s resolved model %q is NOT registered for runtime %q (%s) — leaving model unset; provision will fail closed", workspaceID, model, runtime, why)
		return
	}
	if ok, why := validateDerivedProviderInRegistry(runtime, model); !ok {
		log.Printf("Provisioner: concierge %s resolved model %q has no derivable registry provider for runtime %q (%s) — leaving model unset; provision will fail closed", workspaceID, model, runtime, why)
		return
	}

	// applyRuntimeModelEnv already ran with an empty payload model for the
	// concierge (no stored MODEL on first boot). Run the shared mapper again now
	// that the model is resolved so this provision gets both canonical names and
	// any runtime-specific contract (for example HERMES_DEFAULT_MODEL).
	applyRuntimeModelEnv(envVars, runtime, model)

	// Persist so GET /workspaces/:id/model returns it (Config tab visibility) and
	// the next provision's readStoredModelSecret takes the respect-existing path.
	if setErr := setModelSecret(ctx, workspaceID, model); setErr != nil {
		log.Printf("Provisioner: concierge %s persist MODEL secret failed: %v (env still seeded for this provision)", workspaceID, setErr)
	}
}

// reconcileExistingConciergeModel keeps an already-seeded concierge's model in
// sync with the platform SSOT WITHOUT clobbering a genuine customer choice.
//
// ensureConciergeModel is seed-once (it only seeds when no MODEL secret exists),
// which froze a concierge on whatever the platform default was at first boot — so
// a later SSOT bump (e.g. the moonshot/kimi-k2.6 → minimax/MiniMax-M2.7 default
// migration, or a future M2.x bump) never reached an existing concierge. This
// reconciler closes that drift:
//
//   - PLATFORM-MANAGED stored model (derives to the `platform` provider — i.e.
//     the platform default, not a BYOK pick): re-resolve the authoritative SSOT
//     default (CP on SaaS / MOLECULE_LLM_DEFAULT_MODEL env on self-host). When it
//     resolves to a DIFFERENT, registered + routable id, overwrite the stored
//     MODEL secret and the container env so the bump propagates. A resolver
//     error / empty / unchanged / unroutable result leaves the existing model in
//     place — never break a running concierge on a transient CP blip. (This is a
//     RECONCILE of the platform default, NOT an override of a customer pick, so
//     it honors the CTO "customer setting > platform default" directive.)
//   - NON-platform-managed stored model: a deliberate customer BYOK pick — respect
//     it untouched.
//
// In BOTH cases it re-asserts envVars["MODEL"] == envVars["MOLECULE_MODEL"] ==
// the effective model, eliminating the cross-provision MODEL/MOLECULE_MODEL split
// (a stale MOLECULE_MODEL frozen in a prior container out-ranking a fresh MODEL,
// since the runtime's precedence is MOLECULE_MODEL > MODEL).
func (h *WorkspaceHandler) reconcileExistingConciergeModel(ctx context.Context, workspaceID, runtime, existing string, envVars map[string]string) {
	// A non-platform (BYOK) model is a deliberate customer pick: respect it
	// untouched (loadWorkspaceSecrets + applyRuntimeModelEnv already put it in
	// envVars for this provision). This also covers a stale/unknown id that does
	// not derive to the platform provider — left to the customer-pick path rather
	// than auto-rewritten.
	if !conciergeModelIsPlatformManaged(runtime, existing) {
		return
	}
	resolved, err := conciergeModelResolver(ctx)
	if err != nil {
		log.Printf("Provisioner: concierge %s model reconcile skipped (resolver error, keeping %q): %v", workspaceID, existing, err)
		return
	}
	if resolved == "" || resolved == existing {
		return // SSOT unavailable or unchanged — nothing to reconcile.
	}
	if ok, why := validateRegisteredModelForRuntime(runtime, resolved); !ok {
		log.Printf("Provisioner: concierge %s SSOT model %q is NOT registered for runtime %q (%s) — keeping current %q", workspaceID, resolved, runtime, why, existing)
		return
	}
	if ok, why := validateDerivedProviderInRegistry(runtime, resolved); !ok {
		log.Printf("Provisioner: concierge %s SSOT model %q has no derivable registry provider for runtime %q (%s) — keeping current %q", workspaceID, resolved, runtime, why, existing)
		return
	}
	// Overwrite the frozen platform default to the SSOT. Re-run the shared mapper
	// so BOTH canonical names and any runtime-specific name that still carries the
	// old model are updated for this provision.
	applyRuntimeModelEnv(envVars, runtime, resolved)
	if setErr := setModelSecret(ctx, workspaceID, resolved); setErr != nil {
		log.Printf("Provisioner: concierge %s persist reconciled MODEL secret failed: %v (env still updated for this provision)", workspaceID, setErr)
	} else {
		log.Printf("Provisioner: concierge %s reconciled platform-managed model %q → SSOT default %q", workspaceID, existing, resolved)
	}
}

// readStoredModelSecret returns the decrypted MODEL workspace_secret.
// The second return value distinguishes the three observed states so the caller
// can fail closed rather than fail open on a transient error:
//
//   - (value, nil): a secret is stored and decrypted successfully → caller
//     respects the existing model and skips the seed.
//   - ("", nil): no row exists for this workspace/key → caller may re-seed
//     safely (this is the fresh-boot / cleared-secret case).
//   - ("", error): the row exists (or the read otherwise succeeded) but the
//     decryption failed → caller MUST NOT treat this as "unset" and MUST NOT
//     fall back to seeding the platform default. Returning "" without the
//     error used to silently overwrite the customer's model pick if the
//     secret store later recovered: the seed path would re-fire on the next
//     provision and the customer's choice would be lost without any error
//     surfaced. Sibling of core#3162 (which closed the same fail-open shape
//     on readStoredProviderSecret).
func readStoredModelSecret(ctx context.Context, workspaceID string) (string, error) {
	var stored []byte
	var version int
	err := db.DB.QueryRowContext(ctx,
		`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = 'MODEL'`,
		workspaceID).Scan(&stored, &version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("readStoredModelSecret scan: %w", err)
	}
	dec, derr := crypto.DecryptVersioned(stored, version)
	if derr != nil {
		return "", fmt.Errorf("readStoredModelSecret decrypt: %w", derr)
	}
	return string(dec), nil
}

// conciergeProvider is the provider-registry NAME the concierge's declared
// platform-managed model resolves to. The platform agent is always
// platform-managed (billing/audit flow through the platform LLM proxy), so the
// provider is unconditionally "platform" for the platform-managed model family.
const conciergeProvider = "platform"

// conciergeModelIsPlatformManaged reports whether the concierge's effective model
// is a platform-managed (proxy-billed) id for the given runtime — i.e. one whose
// registry derivation resolves to the `platform` provider. A concierge running a
// platform-managed model MUST have its provider pinned to `platform` explicitly;
// without the pin the runtime's wheel re-derives a slug-PREFIX (e.g. "moonshot"
// from "moonshot/kimi-k2.6", or "minimax" from "minimax/MiniMax-M2.7") that is
// NOT a registry provider name and the adapter fail-closes.
//
// Registry-derived (NOT a hardcoded model-id prefix) so it is runtime- and
// model-agnostic: the P3b default flipped from "moonshot/…" to "minimax/…" and a
// per-org concierge can run any registered platform model on any runtime. An empty
// model is treated as platform-managed (unresolved fresh/rebuilt-from-DB payload
// defaults to the platform family — see ensureConciergeProvider). A derive miss
// (unknown runtime/model) is NOT platform-managed: leave such a concierge alone
// (it resolves on its own or fails the create/provision gate elsewhere). Mirrors
// ensureCreatedWorkspaceProviderPin's IsPlatform gate.
func conciergeModelIsPlatformManaged(runtime, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		return true // unresolved → platform-managed family
	}
	m, err := providerRegistry()
	if err != nil || m == nil {
		// Registry unavailable (build-time gate owns it). Be conservative and do
		// NOT pin — an unverifiable model must not force `platform` auth routing.
		return false
	}
	prov, derr := m.DeriveProvider(runtime, model, nil)
	if derr != nil {
		return false // unknown/ambiguous — not provably platform-managed
	}
	return prov.IsPlatform()
}

// The management-MCP plugin the concierge declares. It wires the `molecule-mcp`
// server (MOLECULE_MCP_MODE=management — provision_workspace, list_workspaces, …;
// the management lifecycle verb is provision_workspace — create_workspace is a
// legacy/workspace-mode tool that is never on the concierge's management surface)
// into the Claude Code runtime via the plugin channel's MCPServerAdaptor,
// replacing the baked-image + asset-channel mcp_servers.yaml path that does NOT
// reach the on-box config (RFC: rfc-platform-mcp-as-plugin). Declaring it from
// the kind=platform-only applyConciergeProvisionConfig IS the primary
// entitlement gate (no user workspace runs this path); recordDeclaredPlugin adds
// a defense-in-depth refusal for this PRIVILEGED name on any non-platform
// workspace. The post-online reconcile + boot-install then install it.
//
// conciergePlatformMCPSource is the source-contract string recorded into
// workspace_declared_plugins and consumed by the box boot-install. It is NO
// LONGER a hand-built literal: it is sourced (by name) from the SDK
// native-plugins registry SSOT (plugin_registry.go), the install:concierge
// entry. Consuming the registry removes the hand-kept RepoPath/Ref/Source
// constants that previously duplicated the plugin repo's coordinates.
//
// The registry value is `gitea://molecule-ai/molecule-ai-plugin-molecule-platform-mcp#main`
// — the same gitea:// WIRE FORM the old literal used, kept deliberately:
//
//   - REGISTRY-CONFIGURABLE / provider-agnostic: the gitea:// scheme carries NO
//     host. The box resolver resolves gitea://owner/repo against a configurable
//     registry base — the platform default git.moleculesai.app OR the
//     MOLECULE_PLUGIN_REGISTRY this handler threads into the box env — so the host
//     is not hardcoded and a mirror/airgap install just sets that env.
//   - PINNED ref (#main): the gitea resolver rejects an unpinned spec in
//     production (PLUGIN_ALLOW_UNPINNED unset by default), so the ref must stay
//     set or the declaration records but fails to fetch → no management MCP.
//   - A bare name is NOT acceptable (parses to the `local` image-baked scheme).
//     The #ref does NOT affect PluginNameFromSource, so conciergePlatformMCPName
//     below still equals the derivation.
//
// mustNativePluginSource panics at startup if the registry drops this privileged
// plugin, rather than recording an empty source the box then can't fetch.
var conciergePlatformMCPSource = mustNativePluginSource(conciergePlatformMCPName)

// conciergePluginRegistryEnvVar names the box-facing env var that carries the
// SCM/registry base the plugin boot-install fetches plugin repos from. Setting it
// fleet-wide (or per self-host deployment) overrides the platform default WITHOUT
// touching any recorded source string — it is the single knob that makes concierge
// plugin sourcing provider-agnostic for mirror / airgap installs. This is a
// fleet-wide default, NOT a per-tenant SCM surface.
const conciergePluginRegistryEnvVar = "MOLECULE_PLUGIN_REGISTRY"

// defaultPluginRegistryBase is the SaaS platform SCM origin used when no registry
// override is configured.
const defaultPluginRegistryBase = "https://git.moleculesai.app"

// pluginRegistryBase resolves the registry/SCM base the box should fetch plugin
// repos from. Precedence: an explicit MOLECULE_PLUGIN_REGISTRY, then the gitea
// resolver's own MOLECULE_GITEA_BASE_URL (so a self-host that already configured
// its Gitea origin gets a consistent plugin registry for free — one knob, no
// drift), then the SaaS default. Always returns a non-empty value.
func pluginRegistryBase() string {
	if v := strings.TrimSpace(os.Getenv(conciergePluginRegistryEnvVar)); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("MOLECULE_GITEA_BASE_URL")); v != "" {
		return v
	}
	return defaultPluginRegistryBase
}

// conciergePlatformMCPName is the install NAME plugins.PluginNameFromSource
// derives from the gitea:// source above (the repo segment, no subpath). It is
// what gets written to workspace_declared_plugins.plugin_name and what the
// recordDeclaredPlugin entitlement gate matches on — so it MUST equal the
// derivation, not the human label "molecule-platform-mcp".
const conciergePlatformMCPName = "molecule-ai-plugin-molecule-platform-mcp"

// The platform concierge must surface mcp__molecule-platform__provision_workspace
// for the post-online fail-loud gate (core#3082) to consider the management MCP
// actually loaded. The Claude Code dispatcher formats every MCP tool as
// `mcp__<server>__<tool>`.
//
// VERB CORRECTION (2026-06-25): the concierge runs the platform MCP in
// MOLECULE_MCP_MODE=management. In that mode @molecule-ai/mcp-server's
// createServer() returns EARLY after registering only the management tools and
// NEVER calls registerWorkspaceTools — so the lifecycle verb on the concierge's
// management surface is `provision_workspace` in ALL published versions
// (1.1.1 → 1.6.1). `create_workspace` is a WORKSPACE-mode tool that has never
// shipped on the concierge's management surface; the previous contract required a
// phantom verb. This SSOT now matches the deployed producer + the live 1.6.1
// concierge (which loads provision_workspace and no create_workspace).
//
// SSOT (audit 2026-06-25): the gate tool id is COMPOSED from two building blocks,
// each sourced from the generated molecule-ai-sdk binding and pinned by
// TestSSOT_DegradeGateToolDerivesFromContract — there is NO standalone hardcoded
// full tool-id and NO independently-spelled verb. The gate and the test share
// the single `mcp__<server>__<verb>` formula and the same contract source.
//
// Note we match the TOOL id, not the plugin/server NAME: the heartbeat's
// loaded_mcp_tools list carries TOOL identifiers (mcp__<server>__<tool>); a name
// match would be a no-op false-green (the plugin is named
// "molecule-ai-plugin-molecule-platform-mcp" while its tools are
// "mcp__molecule-platform__*").

// conciergePlatformMCPServerName == contract.mcp_server_name (the mcp__<server>__
// prefix). conciergePlatformMCPRequiredTool == contract.required_tool (the verb).
const conciergePlatformMCPServerName = molcontracts.MCPServerName
const conciergePlatformMCPRequiredTool = molcontracts.RequiredTool

// conciergePlatformMCPProvisionWorkspaceTool is composed from the two contract-
// pinned constants above via the canonical mcp__<server>__<tool> formula. It
// resolves to mcp__molecule-platform__provision_workspace — the real lifecycle
// verb the management-mode concierge exposes.
const conciergePlatformMCPProvisionWorkspaceTool = "mcp__" + conciergePlatformMCPServerName + "__" + conciergePlatformMCPRequiredTool

// ensureConciergeProvider pins the concierge's LLM provider to `platform` (core
// companion to ensureConciergeModel). It guarantees the env-level provider pin
// that the runtime needs, independent of the template config.yaml (which is NOT
// delivered to the on-box /configs — the box uses the baked base-image config).
//
// SEED-ONLY, keyed on the LLM_PROVIDER secret (NOT MODEL) so an EXISTING
// concierge that already has a MODEL secret still receives the provider pin on
// its next provision, while a provider the customer later pinned in the canvas
// (which writes LLM_PROVIDER) is respected. GATED on the effective model's
// platform-managed namespace so it never forces `platform` onto a BYOK or
// self-hosted concierge running a non-proxy model.
func (h *WorkspaceHandler) ensureConciergeProvider(ctx context.Context, workspaceID, runtime string, envVars map[string]string) {
	if strings.TrimSpace(runtime) == "" {
		runtime = conciergeDefaultRuntime()
	}
	// Respect an explicit provider already set (customer canvas pick or a prior
	// seed): loadWorkspaceSecrets already injected it into envVars. Do nothing.
	//
	// Fail-CLOSED on decrypt/read error (core#3162): if the secret store returned
	// an error rather than a clean "" (the row exists but the value is unreadable
	// — transient DB blip, key-rotation drift, ciphertext corruption), we MUST NOT
	// fall through to the platform-provider pin below. Doing so would wedge a
	// transient decrypt failure into a fail-OPEN platform mis-pin on the next
	// provision — combined with a momentarily-empty MODEL, that could silently
	// route a BYOK/self-host concierge through the platform LLM proxy.
	if existing, readErr := readStoredProviderSecret(ctx, workspaceID); readErr != nil {
		log.Printf("Provisioner: concierge %s LLM_PROVIDER read failed (failing closed, NOT seeding): %v", workspaceID, readErr)
		return
	} else if existing != "" {
		return
	}

	// Effective model for this provision. In production envVars["MODEL"] is
	// ALWAYS populated before this runs — either by loadWorkspaceSecrets +
	// applyRuntimeModelEnv (an existing/customer model) or by ensureConciergeModel
	// just above (the fresh-boot seed) — so reading it here is sufficient and
	// avoids a redundant secret decrypt.
	model := strings.TrimSpace(envVars["MODEL"])
	// Pin platform when the model is platform-managed OR unresolved (empty). An
	// empty MODEL here is NOT a BYOK/self-host signal — those carry a stored
	// LLM_PROVIDER (handled by the early-return above) or an explicit non-platform
	// MODEL (skipped just below). Empty means an unresolved fresh/rebuilt-from-DB
	// payload, which defaults to the platform-managed family; skipping the pin
	// there left the concierge without LLM_PROVIDER, so the runtime could not drop
	// the inherited tenant CLAUDE_CODE_OAUTH_TOKEN and the agent 401'd against the
	// CP LLM proxy. Only a NON-empty non-platform model (an explicit BYOK pick)
	// resolves on its own; forcing `platform` there would mis-route auth and break
	// the agent.
	//
	// The platform-managed test is now registry-DERIVED (conciergeModelIsPlatformManaged)
	// rather than a hardcoded "moonshot/" model-id prefix, so the P3b minimax/
	// default — and any other registered platform model on any concierge runtime —
	// is correctly recognized.
	if !conciergeModelIsPlatformManaged(runtime, model) {
		return
	}

	envVars["LLM_PROVIDER"] = conciergeProvider
	if setErr := setProviderSecret(ctx, workspaceID, conciergeProvider); setErr != nil {
		log.Printf("Provisioner: concierge %s persist LLM_PROVIDER secret failed: %v (env still seeded for this provision)", workspaceID, setErr)
	} else {
		log.Printf("Provisioner: concierge %s pinned LLM_PROVIDER=%s for platform-managed model %q", workspaceID, conciergeProvider, model)
	}
}

// readStoredProviderSecret returns the decrypted LLM_PROVIDER workspace_secret.
// The second return value distinguishes the three observed states so the caller
// can fail closed rather than fail open on a transient error:
//
//   - (value, nil): a secret is stored and decrypted successfully → caller
//     respects the existing provider pin and skips the seed.
//   - ("", nil): no row exists for this workspace/key → caller may re-seed
//     safely (this is the fresh-boot / cleared-secret case).
//   - ("", error): the row exists (or the read otherwise succeeded) but the
//     decryption failed → caller MUST NOT treat this as "unset" and MUST NOT
//     fall back to seeding the platform provider. Returning "" without the
//     error used to wedge a transient decrypt failure into a fail-OPEN
//     platform-pin (see core#3162): combined with a momentarily-empty MODEL,
//     a BYOK/self-host concierge could be silently mis-routed onto the
//     platform LLM proxy on the next provision.
//
// `sql.ErrNoRows` is the canonical "no row" case and is mapped to ("", nil).
// Any other Scan error or a DecryptVersioned error is treated as the
// error-case and returned as ("", err).
//
// NOTE: this fix is scoped to the BYOK fail-open path (core#3162).
// `readStoredModelSecret` has the same shape but is intentionally out of scope
// per the issue body and PM's scope discipline (one item, don't bundle).
// Tracking separately.
func readStoredProviderSecret(ctx context.Context, workspaceID string) (string, error) {
	var stored []byte
	var version int
	err := db.DB.QueryRowContext(ctx,
		`SELECT encrypted_value, encryption_version FROM workspace_secrets WHERE workspace_id = $1 AND key = 'LLM_PROVIDER'`,
		workspaceID).Scan(&stored, &version)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("readStoredProviderSecret scan: %w", err)
	}
	dec, derr := crypto.DecryptVersioned(stored, version)
	if derr != nil {
		return "", fmt.Errorf("readStoredProviderSecret decrypt: %w", derr)
	}
	return string(dec), nil
}

// setProviderSecret persists (or clears, when provider == "") the LLM_PROVIDER
// workspace_secret. Mirrors setModelSecret (secrets.go). LLM_PROVIDER is the
// provider-slug pin the molecule-runtime wheel reads at highest precedence; it
// is injected into the container as an env var by loadWorkspaceSecrets, so it
// survives restarts and the regenerated on-box config.yaml.
func setProviderSecret(ctx context.Context, workspaceID, provider string) error {
	if provider == "" {
		_, err := db.DB.ExecContext(ctx,
			`DELETE FROM workspace_secrets WHERE workspace_id = $1 AND key = 'LLM_PROVIDER'`,
			workspaceID)
		return err
	}
	encrypted, err := crypto.Encrypt([]byte(provider))
	if err != nil {
		return err
	}
	version := crypto.CurrentEncryptionVersion()
	_, err = db.DB.ExecContext(ctx, `
		INSERT INTO workspace_secrets (workspace_id, key, encrypted_value, encryption_version)
		VALUES ($1, 'LLM_PROVIDER', $2, $3)
		ON CONFLICT (workspace_id, key) DO UPDATE
			SET encrypted_value = $2, encryption_version = $3, updated_at = now()
	`, workspaceID, encrypted, version)
	return err
}

// ensureCreatedWorkspaceProviderPin pins LLM_PROVIDER=platform on a freshly
// CREATED workspace (the Create handler / `create_workspace` management-MCP
// path) when, and ONLY when, its (runtime, model) derives to the closed
// `platform` provider via the registry.
//
// Why this exists (the create_workspace NOT_CONFIGURED bug): the platform/root
// concierge gets LLM_PROVIDER=platform from ensureConciergeProvider, but that
// helper runs ONLY on the kind=platform provision path. A CHILD workspace the
// concierge spawns via `create_workspace` goes through WorkspaceHandler.Create,
// which persists MODEL (setModelSecret) but — since the internal#718 P4 closure
// removed the unconditional setProviderSecret write — persists NO LLM_PROVIDER.
// For a platform-managed model id like "moonshot/kimi-k2.6" the on-box runtime
// re-derives the provider with its own slug-split (_derive_provider_from_model →
// "moonshot", a model-PREFIX, NOT a registry provider NAME), so the claude-code
// adapter fail-closes: "workspace config picks provider='moonshot' but it is not
// in the providers registry" → the child boots online but NOT_CONFIGURED. This
// is the exact symptom ensureConciergeProvider was added to cure for the root;
// children created via create_workspace need the same env-level pin.
//
// The pin is gated on the registry-DERIVED provider being `platform`
// (providers.Manifest.DeriveProvider → IsPlatform), NOT on a model-prefix string
// or on the parent being platform-managed. This:
//   - is parent-independent and not moonshot-specific (any platform-managed
//     model id whose registry derivation is `platform` gets the pin);
//   - leaves BYOK / OAuth / self-host children UNTOUCHED — their model derives to
//     a real provider entry (anthropic-oauth, minimax, kimi-coding, …) the
//     runtime resolves correctly on its own, so pinning would be wrong. Mirrors
//     ensureConciergeProvider's `IsPlatform` gate (it only ever pins `platform`).
//
// availableAuthEnv is the auth-env-var NAMES present in the create payload's
// secrets — the same disambiguation input DeriveProvider uses elsewhere — so a
// BYOK create that carries vendor keys derives to its real provider (not
// platform) and is correctly skipped. Best-effort + non-fatal: a derive miss or
// a persist error logs and returns; the workspace row stays consistent and the
// (unchanged) downstream provision validation still applies.
func ensureCreatedWorkspaceProviderPin(ctx context.Context, workspaceID, runtime, model string, secretKeys []string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	m, err := providerRegistry()
	if err != nil || m == nil {
		log.Printf("Create workspace %s: provider registry unavailable; cannot pin LLM_PROVIDER (non-fatal): %v", workspaceID, err)
		return
	}
	prov, derr := m.DeriveProvider(runtime, model, secretKeys)
	if derr != nil {
		// Unknown/ambiguous (runtime, model) — the create-boundary registry
		// gates already let this through (e.g. federated/unknown runtimes), and
		// a non-derivable model is not platform-managed by construction. No pin.
		return
	}
	if !prov.IsPlatform() {
		// BYOK / OAuth / self-host: the model derives to a real provider entry
		// the runtime resolves on its own. Pinning here would mis-route.
		return
	}
	if setErr := setProviderSecret(ctx, workspaceID, providers.PlatformProviderName); setErr != nil {
		log.Printf("Create workspace %s: failed to pin LLM_PROVIDER=%s for platform-managed model %q: %v (non-fatal)", workspaceID, providers.PlatformProviderName, model, setErr)
		return
	}
	log.Printf("Create workspace %s: pinned LLM_PROVIDER=%s for platform-managed model %q (create_workspace child config completeness)", workspaceID, providers.PlatformProviderName, model)
}

// EnsureSelfHostedPlatformAgent seeds the org's platform agent (the concierge,
// the org root) on a tenant that has no control plane to do it — i.e. self-hosted
// or local. In SaaS the CP calls InstallPlatformAgent at org-provision time; this
// is the no-CP equivalent. The CALLER gates this on SelfHostPlatformSeedEnabled()
// (MOLECULE_ORG_ID unset) — it runs UNCONDITIONALLY on self-host, every boot
// (core#3496: the org root always exists; the old MOLECULE_SEED_PLATFORM_AGENT
// opt-in flag is removed). SaaS tenants + CI harnesses set MOLECULE_ORG_ID and
// never reach here.
//
// Thin adapter over the ONE canonical lifecycle flow (platform_agent_flow.go):
// row-only (no ProvisionTrigger — the boot provision is phase 2,
// MaybeProvisionPlatformAgentOnBoot, which needs the provisioner that doesn't
// exist yet at this point in boot). Via the flow's decide step this is also
// self-healing: a healthy root no-ops ("exists"), a half-installed/failed root
// re-runs the idempotent install ("repaired") — re-anchoring org tokens and
// re-parenting drift the old exists-early-return silently ignored.
func EnsureSelfHostedPlatformAgent(ctx context.Context, database *sql.DB) error {
	out, err := ensurePlatformAgentFlow(ctx, database, ensureFlowOptions{
		// All defaults: name defaultPlatformAgentName(), runtime
		// conciergeDefaultRuntime() (MOLECULE_DEFAULT_RUNTIME else the compiled-in
		// Hermes fallback) —
		// a self-host operator who wants a different runtime sets the env or
		// switches it post-seed via the standard runtime-change path.
		//
		// SkipTombstoned: an unattended boot must never silently un-delete a
		// deliberately-removed concierge — reviving is an explicit user action
		// (the ensure endpoint / onboarding scene).
		SkipTombstoned: true,
	})
	if err != nil {
		return fmt.Errorf("self-host platform-agent seed: %w", err)
	}
	switch {
	case out.Skipped == "tombstoned":
		log.Printf("boot: platform-agent self-seed skipped — root %s is tombstoned (deliberate deletion respected; revive via the canvas repair/setup flow)", out.Action.targetID)
	case out.Action.status != "exists":
		log.Printf("boot: platform-agent self-seed %s %s (prior_status=%q)",
			out.Action.status, out.Action.targetID, out.PriorStatus)
	}
	return nil
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
//   - The CALLER gates this on SelfHostPlatformSeedEnabled() (MOLECULE_ORG_ID
//     unset — the removed MOLECULE_SEED_PLATFORM_AGENT flag is gone, core#3496)
//     AND the local Docker provisioner being active (prov != nil).
//     SaaS (cpProv) never reaches here.
//   - It looks up the kind='platform' root; if absent (seed failed) it no-ops.
//     If the container is already running (prov.IsRunning) it no-ops.
//   - Otherwise it kicks off ONE provision via the same path the restart
//     endpoint uses (WorkspaceHandler.RestartByID), which reads the row's
//     runtime ('claude-code' as seeded) + config and provisions accordingly.
//
// UNCONFIGURED-SKIP (core#3496 D2): a fresh self-host root with NO model signal
// (no MODEL workspace_secret, no MOLECULE_LLM_DEFAULT_MODEL env) would provision
// straight into a guaranteed MISSING_MODEL / MISSING_PLATFORM_PROXY abort on
// every boot. Instead of burning that failed provision, the kick-off is skipped
// and the root stays parked at 'offline' for the onboarding scene (or a
// Settings + explicit restart) to configure. Once ANY model signal exists the
// old behavior applies: a missing/wrong KEY still fails the provision loudly —
// that's a real error state the user must see. This never fatals and never
// loops: RestartByID is itself debounced/coalesced, and this runs exactly once
// at boot. Run it in a goroutine so a slow Docker pull doesn't delay the HTTP
// server coming up.
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
	// identity; if it's missing, re-declare the management MCP plugin in the DB
	// BEFORE restarting so the post-restart reconcile + boot-install see it, then
	// restart ONCE so the provision path re-seeds the overlay. This is what makes
	// the seed idempotent + self-applying on the EXISTING concierge (the
	// deterministic self-hosted id), not just new installs. IsRunning is the
	// authoritative liveness check; status is the cheap one.
	running, _ := prov.IsRunning(ctx, id)
	if running {
		if conciergeIdentityPresent(ctx, prov, id) {
			log.Printf("boot: platform-agent %s already running with concierge identity — skipping", id)
			return
		}
		log.Printf("boot: platform-agent %s running but MISSING concierge identity — re-declaring management MCP and restarting once to apply the system prompt + platform MCP", id)
		if rec, skip := seedTemplatePlugins(ctx, id, []string{conciergePlatformMCPSource}); skip > 0 {
			log.Printf("boot: concierge %s could not re-declare %q plugin (recorded=%d skipped=%d) — management MCP may be absent until next provision", id, conciergePlatformMCPSource, rec, skip)
		}
		go restartByID(id)
		return
	}
	if !platformAgentModelConfigured(ctx, database, id) {
		log.Printf("boot: platform-agent %s awaiting setup (no MODEL secret, no MOLECULE_LLM_DEFAULT_MODEL) — provision skipped; configure via the onboarding scene or Settings", id)
		return
	}
	log.Printf("boot: platform-agent %s not running (status=%s) — kicking off best-effort provision", id, status)
	go restartByID(id)
}

// conciergeIdentityPresent reports whether the running concierge container
// already carries the seeded identity (a substituted /configs/system-prompt.md).
// Used to decide whether a running-but-vanilla concierge needs a one-shot
// restart to pick up the overlay. Best-effort: on a probe error or an empty
// file it returns false (so the safe action — re-seed via restart — is taken).
//
// P3b: the check is the ABSENCE of the {{CONCIERGE_NAME}} placeholder, NOT the
// presence of the literal "Org Concierge". The old substring check broke the
// moment the concierge was renamed (MOLECULE_ORG_NAME → "<Org> Agent", never
// "Org Concierge") or ran on a non-claude-code template whose prompt never
// mentions that phrase: conciergeIdentityPresent would return false on a
// perfectly-seeded concierge → MaybeProvisionPlatformAgentOnBoot restarts it on
// EVERY boot (the boot-restart loop). The placeholder is the unambiguous,
// name-/runtime-agnostic signal of "identity NOT yet substituted": the template
// ships it literal and applyConciergeProvisionConfig's substituteConciergeName
// replaces it at provision. A file that still contains the literal placeholder
// has NOT had the per-instance overlay applied; any other non-empty file has.
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
	s := string(body)
	if strings.TrimSpace(s) == "" {
		// Empty / vanilla file — no identity delivered. Re-seed via restart.
		return false
	}
	// Identity is present iff the placeholder has been substituted away. A file
	// still carrying the literal {{CONCIERGE_NAME}} is an un-substituted (or
	// stub) prompt → not yet identity-bearing.
	return !strings.Contains(s, conciergeNamePlaceholder)
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

// platformRootConflictError is returned by installPlatformAgent when the caller
// asked to install a platform root under an id that differs from the org's
// CURRENT, ONLINE one. Honouring it would demote a live concierge and put a
// container-less row in its place (this install never provisions), destroying
// the org's concierge with no path back. The caller gets 409 and the id of the
// root that is actually serving, so it can either target that id or go through
// POST /admin/org/platform-agent/ensure, which repairs in place.
type platformRootConflictError struct {
	ExistingID  string
	RequestedID string
}

func (e *platformRootConflictError) Error() string {
	return fmt.Sprintf(
		"refusing to displace the ONLINE platform root %s with %s: this install is row-only "+
			"(it never provisions), so the org would be left with a concierge that has no container",
		e.ExistingID, e.RequestedID)
}

type installPlatformAgentPayload struct {
	// ID is the platform agent's workspace id (a deterministic uuidv5 the
	// control plane derives per org). Required.
	ID string `json:"id" binding:"required"`
	// Name is the display name; defaults to "Org Concierge" when omitted.
	Name string `json:"name"`
	// Runtime is the concierge's runtime (P3b). Optional; defaults to
	// conciergeDefaultRuntime() when omitted. A CP that wants an explicit runtime
	// passes it.
	Runtime string `json:"runtime"`
}

// InstallPlatformAgent handles POST /admin/org/platform-agent (AdminAuth).
//
// Idempotently installs the platform agent as the org root for THIS tenant. The
// control plane calls it at org-provision time (new orgs) and during the
// existing-org backfill rollout. Safe to call repeatedly.
//
// DEPRECATED-FOR-NEW-CALLERS (core#3496): this is the CP's CONTRACT-FROZEN
// row-only shim over the shared install — it deliberately keeps its historical
// semantics (caller-supplied id, always-upsert, NO decide step, NO provision
// trigger) byte-identical for the CP. New callers use POST
// /admin/org/platform-agent/ensure (the one canonical lifecycle flow). Once the
// CP migrates to /ensure (tracked CP-side), this endpoint is deleted. The only
// post-freeze addition is the platform-runtime guard below, which is
// additive-safe: the CP only ever sends container-backed runtimes.
func InstallPlatformAgent(c *gin.Context) {
	var p installPlatformAgentPayload
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	// Platform-runtime guard (shared with the ensure flow): an external-like /
	// mock / unknown runtime on the org root wedges it permanently — the
	// provision path silently no-ops for those. See platformRuntimeAllowed.
	if ok, why := platformRuntimeAllowed(p.Runtime); !ok {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": why, "code": "RUNTIME_UNSUPPORTED"})
		return
	}
	name := p.Name
	if name == "" {
		name = defaultPlatformAgentName()
	}
	if err := installPlatformAgent(c.Request.Context(), db.DB, p.ID, name, p.Runtime); err != nil {
		// A foreign id aimed at a LIVE concierge is a caller error, not a server
		// fault: answer 409 with the id that is actually serving. Additive-safe for
		// the frozen CP contract — the CP always sends the canonical id, which IS
		// the existing root's id, so this arm is unreachable for it.
		var conflict *platformRootConflictError
		if errors.As(err, &conflict) {
			log.Printf("InstallPlatformAgent: %v", conflict)
			c.JSON(http.StatusConflict, gin.H{
				"error":             conflict.Error(),
				"code":              "PLATFORM_ROOT_ONLINE",
				"platform_agent_id": conflict.ExistingID,
			})
			return
		}
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
func installPlatformAgent(ctx context.Context, database *sql.DB, platformID, name, runtime string) error {
	// P3b: the concierge runtime is a parameter. An empty runtime (legacy callers,
	// self-host seed) falls back to the KMS-resolved platform default
	// (conciergeDefaultRuntime: MOLECULE_DEFAULT_RUNTIME else the compiled-in
	// fallback). The
	// resolved runtime is what the INSERT below stamps into workspaces.runtime
	// ($3) and what conciergeTemplateForRuntime maps to the template ($4), so the
	// asset fetcher pulls the right platform-agent identity.
	if strings.TrimSpace(runtime) == "" {
		runtime = conciergeDefaultRuntime()
	}
	template := conciergeTemplateForRuntime(runtime)
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
	//
	//    ...but ONLY when the root it displaces is not a LIVE concierge. This
	//    install is row-only: it never provisions (see the handler's frozen
	//    contract), so demoting an ONLINE root and upserting a container-less one
	//    in its place leaves the org with a concierge that has no container and
	//    no path to getting one — the canvas resolves the new root, its status
	//    never reaches 'online', and the composer is disabled forever. One POST
	//    with a foreign id permanently destroys a working org concierge.
	//
	//    This is not hypothetical: it is what canvas/e2e/staging-concierge.spec.ts
	//    did to every staging E2E org from 2026-06-11 (when the downgrade above
	//    replaced the uniq_workspaces_one_platform_root COLLISION that used to
	//    reject a foreign id loudly) until this guard.
	//
	//    The CP is unaffected: it always supplies the canonical derived id, which
	//    equals the existing root's id, so `id <> $1` excludes it and neither the
	//    downgrade nor this guard ever fires. The guard's ONLY reachable arm is
	//    the destructive one it exists to stop.
	//
	//    LIVE = status IN ('online','degraded') — NOT 'online' alone. This guard
	//    originally probed `status = 'online'`, and the comment here enumerated the
	//    safe-to-repair states as "offline / failed / provisioning / removed",
	//    silently omitting 'degraded'. That omission was the whole bug: a DEGRADED
	//    concierge has a real, running container — it is merely failing its health
	//    probe — so displacing it with a row-only install (this endpoint never
	//    provisions) orphans that container and leaves the org with a concierge that
	//    has none. Exactly the destruction the guard exists to prevent, just in a
	//    narrower window.
	//
	//    ('online','degraded') is the codebase's canonical container-backed
	//    predicate, not a value invented here — see registry/cp_instance_reconciler.go
	//    :168 and :294, registry/healthsweep.go, registry/hibernation.go,
	//    registry/wedged_agent.go. Guard the PROPERTY (has a container that a row-only
	//    install would orphan), never one status literal that happens to be the state
	//    you saw while debugging.
	var liveRootID string
	err = tx.QueryRowContext(ctx, `
		SELECT id FROM workspaces
		WHERE kind = 'platform' AND parent_id IS NULL AND id <> $1
		  AND status IN ('online', 'degraded')
		LIMIT 1
	`, platformID).Scan(&liveRootID)
	switch {
	case err == nil:
		return &platformRootConflictError{ExistingID: liveRootID, RequestedID: platformID}
	case errors.Is(err, sql.ErrNoRows):
		// No live foreign root — the downgrade below is safe.
	default:
		return fmt.Errorf("probe existing platform root: %w", err)
	}

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
	//
	//    RUNTIME (P3b): the INSERT seeds the requested concierge runtime. The
	//    ON CONFLICT clause deliberately does NOT re-write `runtime` — re-running
	//    the install (CP backfill, idempotent re-call) must PRESERVE the row's
	//    current runtime, not revert a codex/openclaw concierge back to
	//    claude-code. The pre-P3b `runtime = 'claude-code'` on conflict (core#2496)
	//    is the exact clobber this de-bake removes.
	//
	//    STATUS (CR2 RC 14676): the ON CONFLICT clause likewise NEVER touches
	//    `status`. Install must PRESERVE the removed/tombstoned flag — a re-install
	//    of a REMOVED concierge (CP backfill, self-host re-seed, or the create/
	//    repair endpoint's reinstall step) must not silently un-delete it. Only a
	//    DELIBERATE repair of a removed root un-tombstones it, and that is done by
	//    EnsurePlatformAgent's explicit reviveRemovedPlatformAgent step AFTER this
	//    install, never as a side-effect of the upsert. (A brand-new row still
	//    seeds status='offline' via the INSERT's VALUES; only the conflict path
	//    leaves status untouched.)
	//
	//    TEMPLATE (tenant-agent BUG 1, P0): the concierge persona template is now
	//    RUNTIME-AGNOSTIC — a single 'platform-agent' entry serves every runtime
	//    (see conciergeTemplateForRuntime). The previous per-runtime CASE stamped
	//    '<runtime>-platform-agent' (e.g. 'openclaw-platform-agent') for a
	//    non-claude-code concierge; no such template is registered in manifest.json,
	//    so resolveTemplateIdentity fail-closed to an empty identity and the
	//    concierge booted with no persona. On conflict `runtime` is still PRESERVED
	//    (never reverted), but the template is now unconditionally 'platform-agent'
	//    for both a fresh INSERT ($4) and a reinstall — the (runtime, template) pair
	//    can no longer desync because there is exactly one concierge template.
	//
	//    BROADCAST (org-wide birth default): the concierge is the org's top
	//    orchestrator — the one workspace whose job is to fan a message out to its
	//    whole team — so it is BORN with broadcast_enabled=TRUE. This makes every
	//    newly-created org's concierge able to POST /broadcast out of the box, with
	//    no manual PATCH /workspaces/:id/abilities. It is scoped to THIS row (the
	//    kind='platform' org root); ordinary sub-agents are still created elsewhere
	//    (org import / team expand / bundle import) with the schema default FALSE,
	//    so broadcast stays an orchestrator-only ability. This is a birth default,
	//    NOT an auth-gate change: the POST /broadcast handler still re-validates the
	//    flag per request. Only the fresh-INSERT VALUES sets it; the ON CONFLICT
	//    clause deliberately does NOT touch broadcast_enabled — like status and
	//    runtime, a reinstall PRESERVES the row's current value so an operator who
	//    deliberately disabled broadcast via PATCH is never silently re-enabled.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspaces (id, name, kind, tier, status, runtime, parent_id, template, broadcast_enabled)
		VALUES ($1, $2, 'platform', 0, 'offline', $3, NULL, $4, TRUE)
		ON CONFLICT (id) DO UPDATE SET
			kind = 'platform',
			parent_id = NULL,
			template = 'platform-agent'
	`, platformID, name, runtime, template); err != nil {
		return fmt.Errorf("upsert platform agent: %w", err)
	}

	// 1b. Declare the privileged org-management MCP in the SAME transaction as
	// the concierge row. Provisioning reads workspace_declared_plugins to stamp
	// MOLECULE_DECLARED_PLUGINS; if this row is missing, first boot has no
	// desired plugin source and non-Claude runtimes come up without
	// mcp__molecule-platform__provision_workspace.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO workspace_declared_plugins (workspace_id, plugin_name, source_raw)
		VALUES ($1, $2, $3)
		ON CONFLICT (workspace_id, plugin_name)
		DO UPDATE SET source_raw = EXCLUDED.source_raw, updated_at = NOW()
	`, platformID, conciergePlatformMCPName, conciergePlatformMCPSource); err != nil {
		return fmt.Errorf("declare platform mcp plugin: %w", err)
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
