# Marketplace template/plugin delivery RFC — moved

> **The full RFC moved to the private internal repository on 2026-07-18.**
>
> The canonical document is
> **`Molecule-AI/internal/rfcs/marketplace-delivery.md`**.
>
> Why moved: the RFC contains unshipped marketplace strategy, entitlement and
> encryption design, and historical account-specific rollout material. Core's
> Git history still contains the original June 2026 draft.
>
> **If you are updating the design, edit the internal RFC, not this stub.**

**Status:** moved; the marketplace broker design is deferred.

## Public implementation summary

The shipped foundation separates a workspace's template identity/assets from
its runtime engine:

- `workspaces.template` is persisted across create, restart, and provision
  paths.
- `resolveTemplateIdentity` maps an explicit manifest-allowlisted template to
  its pinned repository identity before falling back to the runtime default.
- `PATCH /workspaces/:id/template` is admin-gated and rejects unknown template
  names.
- Template config/prompts use the separate `TemplateAssets` provision channel;
  declared skills are reconciled as plugins.
- Core's required `template-delivery-e2e` provisions a fresh template workspace
  and verifies config, prompts, and declared-plugin delivery before merge.

The entitlement broker, per-seller artifact encryption, third-party publishing,
billing integration, and marketplace UI described by the original RFC are not
implemented or scheduled. This stub is not an authorization to build or operate
those deferred systems.

## Related public implementation surfaces

- `workspace-server/internal/handlers/runtime_registry.go`
- `workspace-server/internal/handlers/workspace_crud.go`
- `workspace-server/internal/provisioner/template_assets.go`
- `.gitea/workflows/template-delivery-e2e.yml`
- `docs/design/rfc-decouple-config-skill-delivery.md`
