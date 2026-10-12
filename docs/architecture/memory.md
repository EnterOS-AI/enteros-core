# Memory architecture

Molecule's memory rule is that sharing follows explicit organizational and
authorization boundaries. Physical placement is provider-dependent; memory
isolation must not rely on the retired assumption that every org has its own
EC2 host.

## Distinct surfaces

| Surface | Core routes | Purpose |
|---|---|---|
| Scoped agent memory | `/workspaces/:id/memories` | Durable `LOCAL`, `TEAM`, and `GLOBAL` records with platform authorization |
| Workspace key/value memory | `/workspaces/:id/memory` | Structured per-workspace values with optional TTL |
| Activity recall | `/workspaces/:id/session-search` | Recent activity plus supported memory recall |
| Memory v2 plugin | `/workspaces/:id/v2/memories` | Capability-negotiated FTS/semantic/TTL/pin behavior supplied by the configured plugin |

These surfaces are not interchangeable. The existence of a response field or
plugin capability does not prove that the built-in runtime currently produces
it. In particular, `source_workspace_id` is optional plugin provenance; Core's
built-in path does not populate it and Canvas does not render a peer badge from
it.

## Scope boundary

- `LOCAL` belongs to one workspace.
- `TEAM` follows the platform's team namespace/access contract.
- `GLOBAL` is wider but retains explicit write authorization.

The active namespace resolver and memory handlers are authoritative. Do not
infer memory access solely from a visual Canvas relationship.

## Plugin boundary

The Memory v2 service declares supported behavior through its health/capability
contract. Core adapts its proxy/tool surface to those capabilities. Whether the
plugin is a sidecar, separate service, or disabled is an environment decision,
not a fixed EC2 co-location contract.

Wire schema: [`memory-plugin-v1.yaml`](../api-protocol/memory-plugin-v1.yaml).
Core implementation: `workspace-server/internal/memory/` and the memory
handlers under `workspace-server/internal/handlers/`.

## Memory versus skills

Memory stores durable facts and recallable context. Skills package repeatable
procedure. A product may promote a durable workflow into a reviewed skill, but
documentation must not present that promotion as an automatic producer unless
the current runtime implements and tests it.

See [Communication Rules](../api-protocol/communication-rules.md), [Platform
API](../api-protocol/platform-api.md), and [Runtime config
boundary](../agent-runtime/config-format.md).
