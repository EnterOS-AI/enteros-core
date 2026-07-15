# Molecule Core technical reference

This page is a current-state map of the checked-in `molecule-core` repository.
It deliberately points to executable sources and focused references instead of
duplicating version numbers, route counts, deployment vendors, or runtime
matrices that change independently.

## Product boundary

Molecule Core is the tenant-side control plane and Canvas for heterogeneous
agent workspaces. A workspace is a durable organizational role. Current
hierarchy is stored in `workspaces.parent_id`; hierarchy changes create or
reparent workspace rows rather than expanding or collapsing a workspace into a
different resource.

Core coordinates workspaces but does not implement every agent runtime. Runtime
execution, prompt assembly, plugin loading, and runtime-specific boot behavior
belong to the maintained workspace-runtime and workspace-template repositories.
Core's `manifest.json` is the checked-in source of truth for the runtime choices
it exposes.

## Checked-in components

| Component | Responsibility | Source of truth |
|---|---|---|
| Workspace server | Authenticated HTTP/WebSocket APIs, lifecycle, registry, hierarchy, A2A proxying, activity, approvals, memory surfaces, secrets, files, bundles, and backend dispatch | `workspace-server/` |
| Canvas | Browser UI for current workspace state, hierarchy, communication, configuration, and operational panels | `canvas/` |
| Postgres | Authoritative durable domain state | `workspace-server/migrations/` and repository code |
| Redis | Liveness, cache, and event fanout where configured; not the durable workspace source of truth | `workspace-server/internal/` |
| Provisioning adapters | Local and control-plane lifecycle backends selected by the configured dispatcher | `workspace-server/internal/handlers/workspace_dispatchers.go` and `workspace-server/internal/provisioner/` |
| Runtime catalog | Runtime identifiers and user-visible metadata Core currently accepts | `manifest.json` |

Deployment topology is environment-specific. Do not infer a universal cloud
vendor, host shape, co-location guarantee, or network path from this repository.
The active `.gitea/workflows/` files and environment configuration define how a
given revision is built, tested, and shipped.

## Authoritative state

- `workspaces` owns current workspace lifecycle, runtime, role, and hierarchy
  fields.
- `canvas_layouts` and `canvas_viewport` own presentation state.
- `activity_logs`, schedules, secrets, approvals, and memory tables own their
  respective domains.
- `structure_events` is append-only selected lifecycle/audit history. It is not
  a complete event source and cannot reconstruct all current state.
- Redis entries are operational state and caches, not replacements for the
  durable domain tables.

See [Database Schema](./database-schema.md) and [Event Log](./event-log.md).

## Hierarchy and communication

`workspaces.parent_id` is both the org-chart relationship and the input to
hierarchy-aware peer discovery and communication checks. Ancestor/descendant
pairs at any depth and siblings sharing the same non-root parent are reachable;
unrelated roots and disjoint subtrees are not allowed merely because they share
a tenant.

Canvas `collapsed` state only hides or shows descendants. The former destructive
team expand/collapse handlers and their dedicated event claims are retired.
Deletion remains a separate explicit lifecycle operation.

See [Communication Rules](../api-protocol/communication-rules.md), [Platform
API — Team hierarchy](../api-protocol/platform-api.md#team-hierarchy), and
[Canvas](../frontend/canvas.md).

## Authentication and authorization

There is no repository-wide unauthenticated API rule. The router groups routes
under workspace, admin, org-token, and verified-session middleware according to
the operation. Infrastructure-bearing workspace changes such as `tier`,
`parent_id`, `runtime`, `workspace_dir`, and compute selection require admin or
verified control-plane authority; a workspace bearer can perform only the
allowed self-maintenance fields.

The authoritative route/middleware wiring is
`workspace-server/internal/router/router.go`. See [Platform
API](../api-protocol/platform-api.md) and [Admin Token
Scope](../adr/ADR-001-admin-token-scope.md).

## Lifecycle and provisioning

Workspace creation, restart, pause/resume, removal, provider changes, imports,
and bulk lifecycle operations must route through the shared backend dispatchers.
Those dispatchers select the configured local or control-plane implementation;
tier does not select the deployment vendor.

Provisioning acceptance and a created database row are not proof that a runtime
became healthy. Registration and heartbeat establish the live workspace state,
and deployment verification must follow the relevant CI and runtime health
surfaces.

See [Provisioner](./provisioner.md), [Backends](./backends.md), and [Registry &
Heartbeat](../api-protocol/registry-and-heartbeat.md).

## Runtime and prompt boundary

Core stores and forwards supported workspace configuration, supplies platform
and hierarchy context, and exposes authenticated APIs. The workspace-runtime
package owns the accepted `config.yaml` parser, prompt assembly, runtime MCP
entry points, and narrow selected-skill reload behavior.

Changing configuration, prompt files, plugins, or the selected skill list
requires a workspace restart unless the runtime-owned contract explicitly says
otherwise. The retired `shared_context` parent-file injection model is not a
current Core/runtime contract.

See [Runtime config boundary](../agent-runtime/config-format.md) and [System
prompt assembly boundary](../agent-runtime/system-prompt-structure.md).

## Registry, A2A, and events

Workspaces register, heartbeat, publish Agent Cards, discover reachable peers,
and exchange authenticated A2A traffic through the current registry and proxy
surfaces. Delivery may be push or poll depending on the runtime and workspace
contract. A queued poll-mode task is not by itself a delivery failure.

WebSocket messages are live fanout, not a durable replay protocol. Only the
documented subset is also recorded in `structure_events`; activity and current
task data use their own storage.

See [Registry & Heartbeat](../api-protocol/registry-and-heartbeat.md), [A2A
Protocol](../api-protocol/a2a-protocol.md), [WebSocket
Events](../api-protocol/websocket-events.md), and [Event Log](./event-log.md).

## Memory, secrets, and bundles

Core exposes scoped agent-memory and key/value workspace-memory surfaces, but
runtime/plugin ownership determines which agent operations produce or consume
those records. Optional provenance fields must not be presented as populated by
the built-in path unless a current producer exists.

Secret routes are authenticated. Values use AES-GCM when
`SECRETS_ENCRYPTION_KEY` is configured; production boot fails closed without a
valid key, while non-production mode can still write version-0 plaintext for
local compatibility. Lifecycle provisioning injects the resulting values.
Bundles do not include secrets, Memory v2 records, activity/chat history,
arbitrary workspace files, or provider/container state. Import provisions
asynchronously and assigns fresh workspace IDs.

See [Memory](./memory.md), [Secrets Key Custody](./secrets-key-custody.md), and
[Bundle boundary](../agent-runtime/bundle-system.md).

## Current compatibility fields and orphaned surfaces

- `PARENT_ID` provisioning environment injection remains for legacy external
  images; current checked-in runtimes use platform hierarchy and peer APIs.
- `workspaces.forwarded_to` remains a compatibility column with no current
  producer; removed-workspace handlers do not advertise a live redirect.
- The audit-ledger read endpoint has no current runtime producer. Its presence
  is not evidence that current agent actions are being written to that table.

These are compatibility facts, not product promises. Retire or reactivate them
through a tested product decision rather than documentation alone.

## Validation and change discipline

Before changing this reference:

1. Verify route claims against `workspace-server/internal/router/router.go`.
2. Verify state/event claims against migrations, handlers, and the event
   taxonomy.
3. Verify runtime claims against `manifest.json` and the current runtime
   repository rather than a copied matrix.
4. Verify UI claims against Canvas code and tests.
5. Verify deployment claims against active Gitea workflows and terminal run
   results; a merged PR alone is not production evidence.

Historical issue and PR narratives may explain why a guard exists, but must be
labelled historical when the referenced handler, route, runtime, or deployment
shape has been retired.
