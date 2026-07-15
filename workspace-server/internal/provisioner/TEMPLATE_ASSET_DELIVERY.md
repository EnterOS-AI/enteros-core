# Template delivery (config/prompts assets; skills as plugins)

> Status: the core-side asset contract is live as of RFC #2843 (PRs #2845 and
> #2857). The control plane chooses the provider-specific materialization
> transport; this document does not prescribe its secret store or compute
> backend.
> Scope: how core separates **non-secret** template assets (`config.yaml`,
> `system-prompt.md`, `prompts/**`) from bootstrap credentials, while
> `agent-skills/**` are installed later through the plugin pipeline.

## Why this exists

In the former AWS deployment, config/prompts/skills were bundled into the
`molecule/workspace/<id>/config` secret. That coupled non-sensitive,
sometimes-large assets (e.g. the ~716 KiB `seo-all` skill package) to a
secret-sized transport, so skills were silently dropped
(`agent_card.skills == []`) and a missing/stale config secret left a workspace
on a stub `/configs` forever. See RFC #2843 for the full root-cause.

The durable rule is provider-neutral: **the secret channel carries only
secrets.** Config and prompts ride a separate, generic, non-secret
**template-asset channel**. Skills stay in the template repo but are declared
as `gitea://.../agent-skills/<skill>#<ref>` plugins and installed dynamically
after the workspace is online.

## Separate delivery paths

| Channel | Carries | Store | Size bound |
|---|---|---|---|
| **Secrets** | provider/runtime bootstrap credentials | deployment-owned secret bootstrap channel | Provider-specific; never the asset budget |
| **Assets** | `config.yaml`, `system-prompt.md`, `prompts/**` | Gitea template archive → `template_assets` → workspace data volume | 16 MiB core transport DoS guard |
| **Plugins** | declared `agent-skills/**` subpaths | Gitea plugin resolver → post-online install/reconcile | Plugin pipeline limits |

The asset channel is **never** a secrets transport; the secrets channel is
**never** an asset transport.

## How delivery works

1. **Identity.** At boot, `initTemplateRepoByName()` (package init in
   `workspace_provision.go`) populates `templateRepoByName` from the runtime
   `manifest.json` `workspace_templates` block — `runtime -> {repo, ref}`.
   `templateIdentityForRuntime(runtime)` returns `"<owner>/<repo>@<ref>"`, or
   `("", false)` for runtimes with no template (`external`/`kimi`/`mock`).
   The map is **reset and re-populated every call**, so dropping a template
   from the manifest drops its identity (reconcile-on-boot semantic).

2. **Fetch.** `giteaTemplateAssetFetcher.Load(ctx, identity)` issues
   `GET {baseURL}/api/v1/repos/<owner>/<repo>/archive/<ref>.tar.gz`, gunzips,
   stream-extracts, strips the archive's top-level dir, and returns only
   **allowlisted** asset paths (`config.yaml`, `system-prompt.md`, `prompts/**`).
   It sends an Authorization header only when an optional read-only token is
   configured; public template repos use an unauthenticated request. Traversal
   entries are rejected, and the consumer enforces the aggregate 16 MiB bound.

3. **Send separately.** `collectCPConfigFiles` (`cp_provisioner.go`) calls the
   fetcher only when `cfg.TemplateAssetFetcher != nil && cfg.TemplateIdentity
   != ""`, re-gates each path through `IsCPTemplateAssetPath`, and routes assets
   into the **`TemplateAssets`** field (16 MiB bound), separate from the
   compatibility `ConfigFiles` field. The control plane then materializes the
   union through its selected provider backend. Agent-owned paths
   (`MEMORY.md`, `USER.md`, `CLAUDE.md`, `.claude/sessions/**`, `/workspace`)
   are **rejected** by the allowlist and can never be clobbered by a fetch.

4. **Reconcile on provision/restart.** `buildProvisionerConfig` is the shared
   config builder used by **both** first-provision and the restart/auto-heal
   path (`workspace.go` → `provisionWorkspaceAuto` → `workspace_provision_shared.go`
   → `buildProvisionerConfig`). So a stub/missing/partial `/configs` self-repairs
   to the template's current assets on every core-driven restart, not just first
   provision.

## Fail-closed / fail-open contract

- Any transport/extract/parse error from `Load` returns a non-nil error and the
  provision **aborts** (no silent regression to stub `/configs`).
- Managed SaaS selects the Gitea fetcher even when
  `MOLECULE_TEMPLATE_REPO_TOKEN` is unset; public repos are fetched without an
  Authorization header. Self-hosted mode selects an explicit no-op fetcher and
  uses its local TemplatePath/ConfigFiles path instead.

## Configuration (env)

| Var | Required | Default | Notes |
|---|---|---|---|
| `MOLECULE_TEMPLATE_REPO_TOKEN` | only for private template repos | unset (public fetch) | **read-only**, per-identity Gitea PAT scoped to the template repos. NOT a founder PAT. |
| `MOLECULE_GITEA_BASE_URL` | no | `https://git.moleculesai.app` | override for staging / a Gitea mirror; the `/api/v1/...` suffix is appended by the fetcher. |

## Runbook: private-template token

1. For private templates only, mint a **read-only** Gitea PAT
   with repo-read scope on the template repos:
   `molecule-ai/molecule-ai-workspace-template-{claude-code,hermes,openclaw,codex,seo-agent}`.
   Do **not** use a founder PAT or a workspace-admin token.
2. Store it in Infisical SSOT and project it to the workspace-server env as
   `MOLECULE_TEMPLATE_REPO_TOKEN`. Roll **staging first**, validate, then prod.
3. Validate in staging: provision a workspace, assert config/prompts arrived,
   assert declared skills installed through the plugin pipeline, restart, and
   assert both assets and plugin state reconcile correctly.

## Runbook: add a new template

1. Add asset paths plus any `agent-skills/**` plugin subpaths to the template,
   declare each skill as a plugin source in config, and add a
   `workspace_templates` entry (`name`, `repo`, `ref`) in `manifest.json`.
2. For a private repo, ensure the read-only PAT's scope includes it.
3. No core allowlist change is needed unless introducing a new asset namespace.

## Related

- RFC: `docs/design/rfc-decouple-config-skill-delivery.md`
- Code: `internal/provisioner/gitea_template_assets.go`,
  `internal/handlers/runtime_registry.go`,
  `internal/provisioner/cp_provisioner.go` (`collectCPConfigFiles`,
  `IsCPTemplateAssetPath`).
- Sibling cleanup tracked by RFC §10a: de-hardcode the concierge identity into a
  platform-agent template delivered via this same channel.
