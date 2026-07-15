# Workspace-runtime boundary

The workspace-runtime package is maintained in the separate
[`molecule-ai-workspace-runtime`](https://git.moleculesai.app/molecule-ai/molecule-ai-workspace-runtime)
repository and published through the Molecule Gitea package registry. Core does
not contain a second executable runtime implementation.

## Ownership

The runtime repository owns:

- `config.yaml` parsing and validation;
- prompt assembly;
- runtime MCP entry points, registration, heartbeat, and poll delivery;
- adapter-specific session execution;
- selected-skill loading/reload behavior; and
- the runtime/platform contract schemas mirrored into consumers.

Core owns authenticated workspace state, hierarchy, lifecycle dispatch,
registry APIs, A2A proxying, memory proxies, and server-stamped external
connection payloads.

## Reproducibility

Workspace templates install a pinned runtime version. Core's `manifest.json`
pins each template repository to an immutable commit. A runtime change reaches
a deployed template only through the reviewed release, template-pin update, and
environment promotion chain; a merge to runtime `main` alone is not proof that
every template or tenant is using it.

## Current compatibility notes

- `PARENT_ID` is preserved as a legacy provisioning environment field; current
  checked-in runtimes discover hierarchy through platform state and peer APIs.
- The retired `shared_context` parent-file injection model is not supported.
- Configuration, prompt, plugin, and selected-skill-list changes require a
  restart unless the current runtime code explicitly implements a narrower
  reload path.
- Runtime/plugin capabilities must be negotiated or verified against the exact
  installed version; Core docs must not maintain a copied adapter matrix.

See [Runtime config boundary](./config-format.md), [System-prompt assembly
boundary](./system-prompt-structure.md), and [Runtime / platform / plugin
responsibilities](../architecture/runtime-platform-plugin-responsibilities.md).
