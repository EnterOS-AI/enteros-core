# Runtime, platform, template, and plugin responsibilities

Molecule keeps transport/orchestration, runtime-specific execution, image
packaging, and installable capabilities in separate repositories. Checked code
is authoritative; older design RFCs do not override these boundaries.

## Ownership map

| Layer | Owns | Current authority |
|---|---|---|
| Core platform | Workspace rows, auth, hierarchy, registration/heartbeat, A2A proxy/queue, plugin source delivery, lifecycle dispatch | `molecule-core/workspace-server` |
| Shared runtime | Common config, A2A client/server, heartbeat, tools, adapter socket, plugin adapter framework | repository `molecule-ai-workspace-runtime`; Python distribution `molecules-workspace-runtime` |
| Runtime template | Runtime executable, concrete adapter module, Docker image, smoke tests, release pin | the Claude Code, Hermes, OpenClaw, or Codex template repository |
| Plugin | Skills, rules, setup, MCP settings fragment, and optional per-runtime adapter | plugin source tree plus runtime `plugins_registry` |
| Control plane | Provider/backend selection, image pins, tenant/workspace provisioning, fleet lifecycle | `molecule-controlplane` |

The repository name and Python distribution name are intentionally different.
Use `molecules-workspace-runtime` in package installation commands and
`molecule-ai-workspace-runtime` when referring to the source repository.

## Template adapter socket

The shared runtime does not contain every CLI executor. A template supplies the
concrete adapter selected by its `ADAPTER_MODULE`; the adapter implements the
contract in `molecule_runtime.adapter_base`. Common execution helpers remain in
the shared distribution so fixes can be released once and consumed by all four
maintained templates.

Adding a runtime therefore requires a real template/image and adapter, not just
a new string in Core documentation. It must pass the template's image smoke,
shared-runtime contract tests, registration/heartbeat, A2A, and staging pin
verification.

## Runtime to platform status

Registration and heartbeat use a runtime-independent payload. Core consumes
the same fields regardless of the concrete adapter. For a `kind=platform`
workspace, the management-MCP gate distinguishes:

- `mcp_server_present`: the management server is declared/wired;
- `loaded_mcp_tools`: tools actually observed by the running runtime.

Declaring a server is not proof that it loaded. After the grace window, Core
degrades a concierge that does not report the required management tool. Any new
status field must have a producer wired through the real template boot path and
an end-to-end test; an unused serializer field is not an implementation.

## Plugins

Core resolves `local`, `github`, or `gitea` sources and delivers a staged plugin
tree to the workspace. The installed shared runtime then resolves its adapter:

1. curated `molecule_runtime/plugins_registry/<plugin>/<runtime>.py`;
2. plugin-shipped `adapters/<runtime>.py`;
3. raw-drop fallback with a warning.

`AgentskillsAdaptor` handles skills, rules, prompt fragments, and setup hooks.
`MCPServerAdaptor` currently merges a Claude-compatible
`settings-fragment.json`; support for another runtime requires a concrete
adapter for that runtime. Do not document a universal per-runtime MCP renderer
unless the current runtime package implements and tests it.

See [Plugin shapes](../plugins/agentskills-compat.md) and
[Plugin sources](../plugins/sources.md).

## Release and deployment boundary

The shared runtime release publishes a versioned Python package. Template
automation bumps the exact runtime version, builds and smoke-tests an image,
publishes its digest, and updates the staging pin. A persisted pin affects new
or reprovisioned workspaces; moving already-running workspaces requires a wired
redeployer and must not be inferred from a successful pin update.

Core's merge pipeline and the control plane own deployment orchestration. A
documentation change, package publication, or image push alone is not proof
that a running workspace was replaced.
