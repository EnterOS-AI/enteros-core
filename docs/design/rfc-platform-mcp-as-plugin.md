# RFC: Deliver `molecule-platform-mcp` as an entitlement-gated MCP plugin

- **Status:** **Proposed — ready for CTO sign-off** (was Draft). Open questions resolved to decisions (§6), rollout made concrete and gated (§5), image-retirement scoped. Updated 2026-06-23.
- **Author:** devops-engineer (agent)
- **Date:** 2026-06-18
- **Related:** `rfc-platform-agent.md` §5.7, RFC #2843 (delivery decoupling),
  `MCPServerAdaptor` (runtime issue #847), marketplace/entitlement design,
  core PR #3044 (provider-pin seed, orthogonal), template PR #5 (inert).

## 1. Problem

The concierge (org-root `kind=platform` agent) is supposed to be the *Org
Concierge*: a privileged agent that manages the platform through a management
MCP (`molecule-platform-mcp`, 80+ org-admin tools incl. `create_workspace`,
`list_workspaces`). In production today it is **not** that agent.

Observed on prod tenant `test3` (2026-06-18), the concierge introspects as:

- System prompt: `"You are a Claude agent, built on Anthropic's Claude Agent SDK."` — the **generic** default, not the concierge persona.
- MCP servers: **`a2a` only** — the management MCP is **not wired**.
- `create_workspace` / `list_workspaces`: **absent**.

Asked to "spawn a test agent," it used Claude Code's **built-in `Task`
sub-agents** in `/workspace` (~17k tokens) instead of calling `create_workspace`
to create a Molecule workspace. It is **vanilla Claude Code**, not the concierge.

### Root cause

The concierge identity + MCP wiring are delivered via the **asset/baked
channel** (template `config.yaml` with `prompt_files` + `mcp_servers.yaml`,
optionally a baked `molecule-platform-agent` image). On SaaS this channel does
**not** land: the on-box `/configs/config.yaml` is a **218-byte CP-regenerated
stub** —

```yaml
name: <wsid>
runtime: claude-code
a2a: {port: 8000, streaming: true}
model: 'moonshot/kimi-k2.6'
provider: 'platform'
runtime_config: {model: 'moonshot/kimi-k2.6', provider: 'platform'}
```

— with **no `prompt_files`** (→ generic prompt) and **no `mcp_servers`** (→ no
management MCP). No `concierge.md` or `mcp_servers.yaml` reaches the box. The CP
regenerates this stub on every (re)provision/restart, so even a correct template
`config.yaml` is overwritten.

Meanwhile the **plugin channel works**: the post-online reconcile reliably
installs declared plugins (e.g. the `seo-all` skill) into `/configs`.

## 2. Proposal

Deliver `molecule-platform-mcp` as an **entitlement-gated MCP-server plugin**,
declared by the platform-agent template and installed dynamically post-online —
**not** via the asset relay or a baked image.

This is not new machinery. The runtime already ships **`MCPServerAdaptor`**
(`molecule_runtime/plugins_registry/builtins.py`, issue #847), promoted to a
first-class adaptor after four MCP plugins shipped it (`molecule-firecrawl`
#512, `molecule-github-mcp` #520, `molecule-browser-use` #553, `mcp-connector`).
It deep-merges a plugin's `settings-fragment.json` `mcpServers` block into
`/configs/.claude/settings.json` — exactly where the Claude Code SDK reads MCP
servers.

### Plugin shape

`molecule-platform-mcp` ships a `settings-fragment.json`:

```json
{
  "mcpServers": {
    "molecule-platform": {
      "command": "npx",
      "args": ["-y", "@molecule-ai/mcp-server"],
      "env": {
        "MOLECULE_MCP_MODE": "management",
        "MOLECULE_API_URL": "${MOLECULE_API_URL}",
        "MOLECULE_ORG_API_KEY": "${MOLECULE_ORG_API_KEY}"
      }
    }
  }
}
```

The secret (`MOLECULE_ORG_API_KEY`) is **referenced, never embedded** — core
keeps injecting it into the container env via `conciergePlatformMCPEnv`. The
concierge persona prompt ships as the plugin's rule/`SKILL.md`-style content (or
remains the one small identity asset, per the RFC #2843 carve-out for
`config.yaml` + prompts).

### Wiring

- The platform-agent template declares `molecule-platform-mcp` in
  `workspace_declared_plugins`.
- The post-online reconcile resolves `MCPServerAdaptor` and installs it.
- `command: npx -y @molecule-ai/mcp-server` launches on demand → **no baked
  binary**, so the special `molecule-platform-agent` image is no longer required:
  the standard runtime image **for the concierge's CONFIGURED runtime** (claude-code
  by default, but switchable — see §3.4) + this plugin = concierge.
- **Runtime-agnostic delivery is a requirement, not an assumption.** `MCPServerAdaptor`
  must wire the management MCP into whatever the configured runtime reads for MCP
  servers (claude-code's `settings.json`; the equivalent for codex/hermes/etc.) —
  so a concierge on any runtime gets `create_workspace`. (The baked image can never
  be runtime-agnostic; the plugin can.)

## 3. Why this is the right channel

1. **Uses the delivery path that works** (plugins) instead of the one that
   doesn't (asset/baked stub).
2. **Consistent with platform direction** — `feedback_skills_are_plugins_dynamic_install`:
   plugins install dynamically post-boot; the asset relay is for small
   identity/config only. An MCP server is a *capability*, so it belongs in the
   plugin channel.
3. **Retires a maintenance burden** — no special concierge image to build, pin,
   repin (a large share of recent incident toil), and MCP updates ship via the
   registry without an image rebuild.
4. **Required for runtime-switchable platform agents (correctness, not just
   cleanup).** A platform agent is NOT claude-code-specific — its runtime is
   *switchable* (claude-code is only the current default; codex/hermes/openclaw
   are first-class). The baked `molecule-platform-agent` image is built **FROM the
   claude-code runtime image**, so it structurally **binds the concierge to
   claude-code** and cannot serve a codex/hermes concierge. Only the
   runtime-agnostic plugin model (MCP wired per the configured runtime via
   `MCPServerAdaptor`) supports a switchable-runtime concierge. The image isn't
   just redundant — for any non-claude-code platform agent it is **wrong**.

## 4. Security — the load-bearing constraint

The management MCP holds the org-admin token and can create/delete workspaces.
It **MUST** be installable **only** on the org-root `kind=platform` concierge,
**never** on a user workspace. A normal public plugin-install path would be a
privilege-escalation hole.

Requirements:
- **Entitlement gate:** the platform-mcp plugin is installable only for the
  org-root concierge (enforced server-side at install/reconcile, keyed on
  `kind=platform` + org-root, not client-asserted). Ties into the
  marketplace/entitlement design.
- **Secret separation:** the org-admin token stays a core-injected container
  env var; the plugin only references it.
- **Audit:** install of the privileged plugin is logged like any org-admin
  action.

## 5. Migration / rollout — concrete, gated sequence

**Already shipped** (the plugin path exists end-to-end — only the cutover + retirement remain):
- `molecule-platform-mcp` plugin (settings-fragment + `MCPServerAdaptor`) — built, in the registry.
- Org-root-only **entitlement gate** (server-side, keyed on `kind=platform` + org-root) — shipped.
- Platform-agent provisioner **declares** it (`seedTemplatePlugins` in `applyConciergeProvisionConfig`).
- Verified: a fresh concierge can install it + gain `create_workspace`.
- Readiness gate (the original step 3): the management-MCP gate has a 90s warmup grace (`managementMCPUnloadedGrace`, core#3082), so the post-online install window doesn't false-red.

**The one blocker before the image can be retired:** the plugin is a **private Gitea repo** fetched at boot, and that fetch has been **flaky** (404 on missing/over-scoped token, gitea hang — #3065/#3108). The baked image is today's *safety net* against exactly that. Retiring it without hardening the fetch trades an intermittent degradation for a hard one.

**Sequence (staging-first, each step gated on the prior):**
1. **Make the failure visible** — land the #3164 instrumentation (runtime PR #171: `platform_mcp_diag` in the heartbeat) so the CP can see *which* path fails (image-fallback vs plugin-fetch) without box SSH. **Already up for review.**
2. **Harden the plugin fetch** — retry-with-backoff + a **fail-LOUD** signal when the management-MCP plugin fetch fails on a `kind=platform` agent, so a concierge can never come up silently "online-but-no-MCP." Closes the safety-net dependency the image currently provides.
3. **Prove plugin-only on staging** — provision a `kind=platform` agent on the **plain runtime image for its configured runtime** (claude-code by default; ALSO smoke at least one non-claude-code runtime, e.g. codex, to prove switchability) (no platform-agent image) + the plugin; confirm `create_workspace` on a FRESH provision and the staging E2E ("Platform Boot", "Concierge Creates Workspace") green.
4. **Cut over provisioning** — flip `kind=platform` image selection to the plain runtime image (retire `resolvePlatformAgentImage` / the `-platform-agent` variant); the plugin becomes the sole MCP delivery.
5. **Re-provision existing concierges** → plugin-only; verify each online + `create_workspace` via real-chat.
6. **Retire the image** — drop the `publish-platform-agent` job; remove the dual-path code (`on_platform_agent_image`, `MOLECULE_PLATFORM_AGENT_IMAGE_BAKED`, the baked-binary branch of `mcp_server_present`). Keep a documented offline/self-host build recipe (§6 Q4) but publish/provision nothing.
7. Keep PR #3044 (provider-pin) — orthogonal (responsiveness, not capability).

**Acceptance:** a fresh `kind=platform` agent on the plain image + plugin surfaces `create_workspace`; staging E2E green; no `molecule-platform-agent` image referenced in the provision path.

## 6. Decisions (resolved for sign-off)

1. **Entitlement mechanism** → **core-side org-root-only gate** (already shipped — server-enforced, keyed on `kind=platform` + org-root). The marketplace entitlement broker is the future general path, NOT a blocker for this one privileged plugin.
2. **Persona prompt** → **out of scope here.** This RFC fixes the *MCP* (capability). The concierge *system prompt* is an independent failure (the runtime reads `/configs/system-prompt.md` but the template ships `prompts/concierge.md` — a naming/delivery mismatch) tracked as a companion fix. Retiring the image does not regress the prompt — the baked/asset channel never reliably delivered it on SaaS anyway.
3. **`npx` vs pre-bundled** → **`npx -y @molecule-ai/mcp-server`** (the current settings-fragment shape). Revisit pre-bundling only if first-turn cold-start is measured as a real problem.
4. **Image deprecation** → **RETIRE** the `molecule-platform-agent` image (CTO directive, 2026-06-23): platform agent = the standard image **for its configured runtime** (claude-code by default, switchable to codex/hermes/etc.) + the entitlement-gated plugin. The baked image is claude-code-bound and cannot serve other runtimes (§3.4), so it is incompatible with switchable platform agents regardless. A documented offline/self-host build recipe may be kept for air-gapped use, but nothing in the SaaS provision path references the image.

## 7. Non-goals

- The general core↔runtime provider-derivation drift (tracked separately,
  template-claude-code issue #143).
- The CP config-regeneration-to-stub behavior — this RFC routes *around* it for
  the MCP; whether to also stop the stub clobbering `prompt_files` is the
  companion system-prompt fix (§6 D2), tracked separately.

## 8. Decision requested (CTO sign-off)

Sign-off requested to adopt, as the committed architecture:

- **(a)** plugin-only delivery of the management MCP — a platform agent is a standard workspace of its **configured runtime** (claude-code by default, but switchable to codex/hermes/etc.) + the entitlement-gated `molecule-platform-mcp` plugin; **no baked image** (the baked image is claude-code-bound and cannot serve other runtimes — §3.4 — so it is structurally incompatible with switchable platform agents);
- **(b)** **retirement** of the `molecule-platform-agent` image per the gated §5 sequence — staging-first, and *blocked on the plugin-fetch hardening (§5 step 2)* so we never trade an intermittent failure for a hard one;
- **(c)** the resolved decisions in §6.

On sign-off, the fleet executes §5 in order. Step 1 (the #3164 `platform_mcp_diag` instrumentation, runtime PR #171) is already up for review and is the empirical gate that tells us whether prod failures today are image-fallback or plugin-fetch — informing the cutover.

**Why now:** the dual image+plugin delivery is the direct root of the recurring #3164 fragility (silent image-fallback → no `molecule-platform-mcp` binary → MCP fails to start → concierge can't `create_workspace` → staging E2E red). Collapsing to plugin-only removes the image-resolution failure mode, the `MOLECULE_PLATFORM_AGENT_IMAGE_BAKED` env-marker gating, and the build/publish/cross-account-pull maintenance burden — with no loss of the privilege boundary (it moves to the already-shipped org-root entitlement gate).

**Sign-off:** ☐ Approved as written ☐ Approved with changes ☐ Hold — _________________ (CTO) — Date: __________
