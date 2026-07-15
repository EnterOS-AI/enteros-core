# System architecture

Molecule Core is the tenant workspace control plane and browser Canvas. It
coordinates independently managed agent runtimes through authenticated HTTP,
WebSocket, registry, and A2A contracts.

```text
Canvas (Next.js)  <-- HTTP / WebSocket -->  Workspace server (Go / Gin)
                                              |            |
                                           Postgres      Redis
                                              |
                                   configured lifecycle backend
                                              |
                                   workspace runtime / agent
```

## Boundaries

- Postgres domain tables own durable current state.
- Redis supports liveness, cache, and fanout; it is not a durable replay store.
- `workspaces.parent_id` owns hierarchy and feeds peer authorization.
- Canvas visual collapse is presentation only.
- Local and control-plane lifecycle implementations sit behind shared
  dispatchers; deployment topology is environment-specific.
- Runtime parsing, prompt assembly, MCP entry points, and runtime-specific boot
  behavior belong to the workspace-runtime and workspace-template repositories.
- `structure_events` records selected lifecycle history, not every state change.

## Sources of truth

| Concern | Source |
|---|---|
| Route authentication and API wiring | `workspace-server/internal/router/router.go` |
| Durable schema | `workspace-server/migrations/` |
| Hierarchy and peer access | `workspace-server/internal/registry/` |
| Lifecycle backend selection | `workspace-server/internal/handlers/workspace_dispatchers.go` |
| Runtime/template catalog | `manifest.json` |
| Canvas behavior | `canvas/src/` and Canvas tests |
| Active CI/deployment automation | `.gitea/workflows/` |

See the [Core technical reference](./molecule-technical-doc.md), [Platform
API](../api-protocol/platform-api.md), [Communication
Rules](../api-protocol/communication-rules.md), and [Event Log](./event-log.md).
