# Build Order

> **Historical implementation plan.** This file records the original scaffold
> order and names several retired layouts and runtime-specific choices. It is
> not a current setup guide. Use [Local Development](local-development.md) and
> the checked repository structure instead.

The core loop to prove first: **workspace registers -> canvas shows it -> heartbeat keeps it alive -> workspace goes offline -> canvas shows it gray.**

Once that loop works end to end, you have the skeleton of the entire product.

## Step 1: Infrastructure

Write `docker-compose.yml` that starts Postgres, Redis, and Langfuse. Verify all services start cleanly. Write `infra/scripts/nuke.sh` and `infra/scripts/setup.sh`.

## Step 2: Database Migrations

Write the SQL migration files:

1. `001_workspaces.sql` — full workspaces table (includes `parent_id`, heartbeat columns, `forwarded_to` — all core workspace state in one migration)
2. `002_agents.sql` — agents table
3. `003_events.sql` — structure_events table + indexes
4. `004_secrets.sql` — workspace_secrets table
5. `005_canvas_layouts.sql` — canvas_layouts + canvas_viewport tables
6. `006_workspace_config_memory.sql` — workspace config and memory key-value tables

See [Database Schema](../architecture/database-schema.md) for the full table definitions.

## Step 3: Platform API Skeleton

Initialize Go module, install `gin`, write `cmd/server/main.go`, set up router with CORS middleware. Connect to Postgres and Redis. Verify the server starts.

## Step 4: Registry Endpoints

Implement `POST /registry/register`, `POST /registry/heartbeat`, and `POST /registry/update-card` in Go. Write Redis TTL logic. Enable Redis keyspace notifications. Subscribe to expiry events and log them.

See [Registry & Heartbeat](../api-protocol/registry-and-heartbeat.md) for the full flow.

## Step 5: Workspace Runtime Layer

Write `main.py`, `config.py`, `heartbeat.py`, `a2a_executor.py`. Create a minimal LangGraph-based workspace that just echoes responses. Wrap it with `a2a-sdk` (`A2AStarletteApplication`) to create the A2A server. Verify:

- Agent Card is served at `/.well-known/agent-card.json`
- Heartbeat POSTs reach the platform

See [Workspace Runtime](../agent-runtime/workspace-runtime.md) for the file structure.

## Step 6: Canvas Skeleton

Initialize Next.js 15 project with React Flow and Zustand. Create the canvas page with a basic React Flow setup. Create `WorkspaceNode` component that renders a node from an Agent Card. Implement initial hydration (`GET /workspaces` + WebSocket connect). Edges render automatically from `parent_id` hierarchy.

## Step 7: WebSocket Live Updates

Implement WebSocket handler in Go (`handlers/socket.go`). Implement Redis pub/sub broadcaster (`events/broadcast.go`). Implement `useSocket.ts` in the canvas.

Verify: when a workspace registers, the node appears on the canvas in real time. When it goes offline, it turns gray.

## Step 8: First Real Workspace Config

Write `workspace-configs-templates/seo-agent/` with a real `config.yaml`, `system-prompt.md`, and at least one skill. Deploy it as a workspace container.

Verify end-to-end: container starts -> registers -> appears on canvas -> heartbeat -> stays green.

## Step 9: Hierarchy & Communication

Implement `GET /registry/:id/peers` and `POST /registry/check-access` with the `CanCommunicate()` hierarchy check. Implement workspace nesting on the canvas (drag-into to nest). Verify: edges render automatically from parent/child relationships.

See [Communication Rules](../api-protocol/communication-rules.md) for the access model.

## Step 10: Bundle Export/Import

Implement `bundle/exporter.go` to serialize a running workspace into a bundle JSON. Implement `bundle/importer.go` to provision a workspace from a bundle. Add `BundleDropZone` to canvas.

Test round-trip: export -> delete workspace -> import -> workspace reappears.

## Related Docs

- [Architecture](../architecture/architecture.md) — System overview
- [Platform API](../api-protocol/platform-api.md) — Endpoints to build
- [Workspace Runtime](../agent-runtime/workspace-runtime.md) — Runtime layer to build
- [Canvas UI](../frontend/canvas.md) — Frontend to build
- [Communication Rules](../api-protocol/communication-rules.md) — Hierarchy-based access
