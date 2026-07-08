# RFC: Deliver `molecule-platform-mcp` as an entitlement-gated MCP plugin

- **Status:** Proposed — ready for CTO sign-off (arch + entitlement change)
- **Author:** devops-engineer (agent)
- **Date:** 2026-06-18 (revised 2026-06-23 — runtime-agnostic / plugin-SSOT reframe per CTO review)
- **Related:** `rfc-platform-agent.md` §5.5/§5.7, `rfc-decouple-config-skill-delivery.md` §10a,
  RFC #2843, `MCPServerAdaptor` (runtime #847), marketplace/entitlement design,
  core #3044 (provider-pin, orthogonal), #3159 (live staging manifestation), #3164 (fragility).

## 1. Problem

The concierge (org-root `kind=platform` agent) is meant to be the *Org Concierge*: a privileged
agent that manages the platform via a management MCP (`molecule-platform-mcp`, 80+ org-admin tools
incl. `create_workspace`). In production today it is **not** that agent.

Observed on prod `test3` (2026-06-18): system prompt is the generic Claude default (not the concierge
persona); MCP servers = `a2a` only (management MCP **not wired**); `create_workspace`/`list_workspaces`
**absent**. Asked to "spawn a test agent," it used Claude Code's built-in `Task` sub-agents instead of
`create_workspace`. It is vanilla Claude Code, not the concierge.

### Root cause
The concierge identity + MCP wiring are delivered via the **asset/baked channel** (template
`config.yaml` + optional baked `molecule-platform-agent` image). On SaaS that channel does **not**
land: `/configs/config.yaml` is a 218-byte CP-regenerated stub (no `prompt_files` → generic prompt;
no `mcp_servers` → no management MCP), regenerated on every (re)provision. Meanwhile the **plugin
channel works**: the post-online reconcile reliably installs declared plugins (e.g. `seo-all`).

## 2. Proposal

Deliver `molecule-platform-mcp` as an **entitlement-gated MCP-server plugin**, declared by the
platform-agent template and installed dynamically post-online — **not** via the asset relay or a
baked image.

Not new machinery: the runtime already ships `MCPServerAdaptor`
(`molecule_runtime/plugins_registry/builtins.py`, #847), promoted to first-class after several early
MCP-plugin proposals (#512/#520/#553). The registry standardizes on the `molecule-ai-plugin-<name>`
repo convention (e.g. `molecule-ai-plugin-image-gen`, `molecule-ai-plugin-molecule-platform-mcp`).
**Today** the adaptor renders a plugin's MCP into `/configs/.claude/settings.json` (the claude-code
path only); the revision makes that rendering **per-runtime** (§2b).

### Plugin shape
The plugin's canonical shape is the **runtime-agnostic MCP descriptor** in §2b (the SSOT). The
claude-code adapter *renders* that descriptor into a `settings-fragment.json` — the claude rendering,
**not** the cross-runtime contract. The secret (`MOLECULE_ORG_API_KEY`) is **referenced, never
embedded** — core injects it into the container env via `conciergePlatformMCPEnv`.

### Wiring
- The platform-agent template declares `molecule-platform-mcp` in `workspace_declared_plugins`.
- The post-online reconcile resolves the per-runtime adapter and installs it.
- `npx -y @molecule-ai/mcp-server` launches on demand → **no baked binary**; the standard runtime
  image for the concierge's *configured* runtime (claude-code by default, switchable) + this plugin
  = concierge.
- **Runtime-agnostic delivery is a requirement, not an assumption** (§2b, §3.4).

## 2b. The single source of truth: the **plugin declaration** (not a separate `mcp_servers:` list)

**SSOT = the plugin.** An MCP server is delivered *as a plugin*; capabilities are declared **once**,
in the plugin list (`config.yaml: plugins:` / DB `workspace_declared_plugins`). The plugin *package*
carries a runtime-agnostic MCP descriptor + per-runtime adapters; the shape adapter renders it into
the runtime's native MCP config. There is **no separate top-level `mcp_servers:` list** — that would
be a second, competing declaration of the same thing.

> **Correction (CTO review):** an earlier draft proposed `config.yaml: mcp_servers:` as the SSOT.
> That is redundant — `config.yaml` already declares plugins, and the MCP *is* a plugin. A standalone
> `mcp_servers:` list is the "two delivery paths never reconciled" the docs audit flagged (it came
> from `rfc-platform-agent §5.5`, pre-plugin). The plugin declaration supersedes it.

**A) Now.** The plugin list already exists (`WorkspaceConfig.plugins: list[str]`, `config.py:350`).
On SaaS the box gets the 218-byte stub (neither `plugins` nor `mcp_servers` populated). The always-on
`a2a` MCP is a runtime *builtin* (injected directly, not a plugin/config). The management MCP today
rides the claude-only `settings-fragment.json` → `.claude/settings.json`. The `rfc-platform-agent
§5.5` `extra_mcp_servers`/`mcp_servers:` proposal is the redundancy trap to retire, not adopt.

**B) Changes (fix the plugin package — do NOT add a new list).** Declare the MCP as a plugin
(`plugins: [molecule-platform-mcp]`, already true via the template). Add, *inside the plugin package*,
a runtime-agnostic MCP descriptor (`name`/`command`/`args`/`env`, secret referenced) + per-runtime
adapters (`adapters/<runtime>.py`). `settings-fragment.json` becomes the claude adapter's *output*.

**C) Read (one plugin, N renderings).** Plugin declared → reconcile resolves the per-runtime shape
adapter → it materializes the descriptor into the active runtime's native MCP config:

| Runtime | Native MCP config |
|---|---|
| claude-code | `/configs/.claude/settings.json` → `mcpServers` (today's `settings-fragment.json` = this adapter's output) |
| codex | `~/.codex/config.toml` → `[mcp_servers]` |
| gemini-cli | `~/.gemini/settings.json` |
| hermes | `platforms.*` stanza / entry-point |

The server launches on demand and authenticates purely from container **env** — referenced in the
descriptor, injected by core, never embedded.

> **Identity-gate corollary:** "is the management MCP wired?" must be answered by **asking the active
> adapter**, not by reading `/configs/.claude/settings.json`. Today's claude-only check
> (`platform_agent_identity.py`) fail-closes a codex/hermes concierge **offline** even when its MCP is
> correctly wired — this is the live **#3159** staging failure.

## 3. Why this is the right channel

1. Uses the delivery path that works (plugins) instead of the one that doesn't (asset/baked stub).
2. Consistent with platform direction (`feedback_skills_are_plugins_dynamic_install`): an MCP server
   is a *capability* → plugin channel.
3. Retires a maintenance burden — no special image to build/pin/repin; MCP updates ship via the
   registry without an image rebuild.

### 3.4 Required for runtime-switchable platform agents — correctness, not cleanup
A platform agent is **NOT** claude-code-specific — its runtime is **switchable** (claude-code is only
the current default; codex/hermes/openclaw are first-class). The baked `molecule-platform-agent`
image is built **FROM the claude-code image**, so it structurally binds the concierge to claude-code
and cannot serve a codex/hermes concierge. Only the runtime-agnostic plugin model supports a
switchable-runtime concierge. **For any non-claude-code platform agent the image is not just
redundant — it is *wrong*.**

## 4. Security — the load-bearing constraint

The management MCP holds the org-admin token and can create/delete workspaces. It **MUST** be
installable **only** on the org-root `kind=platform` concierge, never on a user workspace.

- **Entitlement gate:** installable only for the org-root concierge, enforced server-side at
  install/reconcile, keyed on `kind=platform` + org-root (not client-asserted). *(Already shipped.)*
- **Secret separation:** the org-admin token stays a core-injected container env var; the plugin only
  references it.
- **Audit:** install of the privileged plugin is logged like any org-admin action.

## 5. Migration / rollout — concrete, gated sequence

**Already shipped (claude-code path only):** the plugin (settings-fragment + `MCPServerAdaptor`),
the org-root entitlement gate, the provisioner declaration (`seedTemplatePlugins`), verification that
a fresh **claude-code** concierge installs it + gains `create_workspace`, and the 90s warmup grace
(`managementMCPUnloadedGrace`, #3082).

> **The revision's CORE work — runtime-agnostic delivery — is NOT done yet.** Everything above is the
> claude-code path. The actual lego fix is still to build, and is the prerequisite for step 3:
> - **Per-runtime adapter rendering** — `MCPServerAdaptor` renders the §2b descriptor into each
>   runtime's native MCP config (it currently ignores its `runtime` arg and always writes
>   `.claude/settings.json`).
> - **Generalize the identity gate** — ask the active adapter "is the management MCP wired?" instead
>   of reading `.claude/settings.json` (today's read fail-closes codex/hermes offline = #3159).
> - **Update the delivery contract** — the molecule-ai-sdk MCP-plugin delivery contract pins the
>   runtime-specific render surfaces as SSOT; core consumes it through generated SDK bindings.
> - **Proven by §5b** (per-runtime render tests + local docker MCP-visibility harness).

**Blocker before the image can retire:** the plugin repo fetch must be reliable. As of 2026-06-23 the
repo `molecule-ai/molecule-ai-plugin-molecule-platform-mcp` is **public** (verified anon HTTP 200), so
this plugin's fetch is token-free and sidesteps the `gitea://` private-repo hang (#3108). Auth-gated
fetch applies only to future *private* plugins.

**Sequence (staging-first; each step gated on the prior):**
1. **Make the failure visible — via the OBS system.** Land #3164 instrumentation (runtime PR #171:
   `platform_mcp_diag`). PR #171 ships it on the heartbeat; the companion emits it as a **boot-event
   into `org_instance_boot_events`** so it's queryable alongside `image_pull`/`workspace_ready`.
2. **Harden the plugin fetch** — retry-with-backoff + fail-LOUD on a `kind=platform` agent.
3. **Prove plugin-only on staging** — provision a `kind=platform` agent on the plain runtime image
   (claude-code, **and** smoke a non-claude runtime e.g. codex to prove switchability) + the plugin;
   confirm `create_workspace` on a FRESH provision; staging E2E green.
4. **Cut over provisioning** — flip `kind=platform` *image selection* to the plain runtime image
   (retire `resolvePlatformAgentImage`). Scope guard: this retires the *image-selection* path only —
   **not the `kind=platform` field**.
5. **Re-provision existing concierges** → plugin-only; verify each via real-chat.
6. **Retire the image code** — drop `publish-platform-agent`; remove the image-specific dual-path code
   (`resolvePlatformAgentImage`, `on_platform_agent_image`, `MOLECULE_PLATFORM_AGENT_IMAGE_BAKED`, the
   baked-binary branch).
7. **Clean up + verify no-image, no-fallback (final)** — a fresh `kind=platform` provision references
   zero platform-agent image and **no fallback path remains**; E2E green with the plugin as sole MCP
   delivery.
8. Keep PR #3044 (provider-pin) — orthogonal.

> **Rollback:** through step 5, cutover is reversible (flip image-selection back + re-provision).
> After step 6 the safety net is gone — recovery rests on step 2's fail-loud signal + redeploying the
> prior image tag. Do not execute step 6 until step 3's per-runtime proof + a clean prod soak; keep
> the last image tag pullable one release cycle.

> **Do NOT retire `kind=platform` / `WORKSPACE_KIND.Platform`.** Only the *image* retires. The field
> is load-bearing: the **canvas** uses it to hide the concierge from the org-map graph (`Canvas.tsx:89`,
> `Toolbar.tsx:58`) and render it as the undeletable org-root (`ConciergeShell.tsx`,
> `canvas-topology.ts`, `socket.ts`); entitlement + provisioning special-handling key on it too.

**Acceptance:** a fresh `kind=platform` agent on the plain image + plugin surfaces `create_workspace`;
staging E2E green; no `molecule-platform-agent` image and no image-fallback branch anywhere in the
provision path.

## 5b. Local testability — no cloud wait

The entire correctness of this RFC is reproducible **locally in minutes**; staging e2e is the final
smoke, not the dev loop. The bug class (#3159/#3164) is runtime+plugin logic — no provisioned cloud
box needed to reproduce.

**Why these bugs reached staging:** (a) the delivery-contract test is claude-only (no per-runtime
render test → `MCPServerAdaptor` ignoring `runtime` was never caught); (b) the
"does the concierge see `create_workspace`" probe only runs LIVE (`E2E_REQUIRE_LIVE=1` on push/cron →
hours; `=0` on PRs → can false-green) so the real check lands after merge.

- **Addition 1 — per-runtime render unit tests (seconds).** Parametrized: assert `MCPServerAdaptor`
  writes the right native config per runtime — incl. "codex → `config.toml`, **NOT**
  `.claude/settings.json`." Extends `tests/test_mcp_plugin_delivery_contract.py`.
- **Addition 2 — local docker MCP-visibility harness (minutes).** `docker run` the runtime image + a
  fixture config declaring the plugin → probe `loaded_mcp_tools` for
  `mcp__molecule-platform__create_workspace`, per runtime. Zero-agent pre-check: launch the server,
  `tools/list` over an MCP handshake.

Only the **provisioning plumbing** (EC2 cloud-init, tunnel, ECR, Neon, org-slug DNS) needs cloud.
Both additions ship **with** this revision — the lego model isn't done until a non-claude runtime
surfaces `create_workspace` locally; they are the regression guard against a future #3159-class escape.

## 6. Decisions (resolved for sign-off)

1. **Entitlement mechanism** → core-side org-root-only gate (shipped). Marketplace broker is the
   future general path, not a blocker here.
2. **Persona prompt** → out of scope. This RFC fixes the *MCP*; the system prompt is an independent
   failure (`/configs/system-prompt.md` vs template `prompts/concierge.md`), companion fix.
3. **`npx` vs pre-bundled** → `npx -y @molecule-ai/mcp-server`; revisit pre-bundling only if cold-start
   is measured as a real problem.
4. **Image deprecation** → RETIRE (CTO 2026-06-23). Platform agent = standard image for its configured
   runtime + the entitlement-gated plugin. A documented offline/self-host build recipe may be kept;
   nothing in the SaaS provision path references the image.
5. **Naming convention** → noted, not renamed here. Repos are consistent at `molecule-ai-plugin-<name>`,
   but the *name* suffix is not (some carry a redundant `molecule-`: `…-molecule-platform-mcp`; some
   don't: `…-image-gen`). Renaming the platform plugin is out of scope (churns registry + template
   declarations); tracked separately. Also fix the stale `builtins.py:446` comment, which cites
   proposal-era names matching no current repo.

## 7. Non-goals

- The core↔runtime provider-derivation drift (template-claude-code #143).
- The CP config-regeneration-to-stub behavior — routed *around* for the MCP; the system-prompt clobber
  is §6 D2, tracked separately.
- The broader core `if runtime == "claude-code"` string-compares for LLM-auth/config
  (`workspace_provision.go` — `runtimeUsesAnthropicNativeProxy`, model-normalization, session-volume).
  Same "ask the adapter, don't compare the literal" theme, but out of this RFC's MCP scope — tracked
  separately.

## 8. Decision requested (CTO sign-off)

Sign-off to adopt as committed architecture:
- **(a)** plugin-only delivery of the management MCP — a platform agent is a standard workspace of its
  *configured* runtime + the entitlement-gated `molecule-platform-mcp` plugin; no baked image (the
  baked image is claude-code-bound — §3.4).
- **(b)** retirement of the `molecule-platform-agent` image per the gated §5 sequence — staging-first,
  blocked on the plugin-fetch hardening (§5 step 2).
- **(c)** the resolved decisions in §6.
- **(d)** the **runtime-agnostic correctness requirement** (§2b/§3.4) — the plugin wires the MCP per
  the configured runtime via per-runtime adapter rendering, with the identity gate + delivery contract
  generalized, proven **per-runtime locally** (§5b). First-class architecture, not a follow-up; the §5
  "DONE" items are the claude-only path and do not by themselves satisfy (d).

On sign-off, the fleet executes §5 in order. Step 1 (PR #171) is already up and is the empirical gate
telling us whether prod failures are image-fallback or plugin-fetch.
