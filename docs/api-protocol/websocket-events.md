# WebSocket Events

The canvas subscribes to the platform's WebSocket at `/ws` and receives real-time lifecycle and activity events as JSON messages. The event taxonomy is defined in `workspace-server/internal/events/types.go`.

## Authentication

Authentication completes before the HTTP connection upgrades:

- Canvas clients use a verified control-plane tenant-member session cookie, an
  org-scoped token, or `ADMIN_TOKEN`. Because browser WebSocket constructors
  cannot set `Authorization`, Canvas offers both
  `molecule-auth.<hex-encoded-token>` and the non-secret `molecule-ws` sentinel
  in `Sec-WebSocket-Protocol`. The server uses the credential-bearing offer for
  authentication, then selects and echoes only `molecule-ws`, so browser
  negotiation succeeds without reflecting the secret.
- Workspace agents send `X-Workspace-ID` plus
  `Authorization: Bearer <workspace-token>`. The bearer must belong to that
  exact workspace.

Anonymous connections are rejected. A workspace bearer cannot subscribe to the
global Canvas stream.

## Message Format

Every WebSocket message has this structure:

```json
{
  "event": "EVENT_TYPE",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": { ... }
}
```

## Event Reference

### WORKSPACE_PROVISIONING

Workspace is being spun up. Canvas shows a spinner on the node.

```json
{
  "event": "WORKSPACE_PROVISIONING",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "name": "Vancouver SEO Agent",
    "tier": 1,
    "config": "seo-agent"
  }
}
```

### WORKSPACE_ONLINE

First heartbeat received, or workspace returned from offline.

```json
{
  "event": "WORKSPACE_ONLINE",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "url": "http://ws-abc-123:8000",
    "agent_card": {
      "name": "Vancouver SEO Agent",
      "version": "1.0.0",
      "skills": ["generate-seo-page", "audit-seo-page"],
      "capabilities": { "streaming": true }
    }
  }
}
```

### WORKSPACE_OFFLINE

Heartbeat TTL expired. Node turns gray.

```json
{
  "event": "WORKSPACE_OFFLINE",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:01:00Z",
  "payload": {}
}
```

### WORKSPACE_PROVISION_FAILED

Provisioning timed out or errored. Node turns red with retry button.

```json
{
  "event": "WORKSPACE_PROVISION_FAILED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:03:00Z",
  "payload": {
    "reason": "provisioning timeout -- no heartbeat received"
  }
}
```

### WORKSPACE_DEGRADED

Workspace is online but experiencing errors. Node shows warning indicator.

```json
{
  "event": "WORKSPACE_DEGRADED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:05:00Z",
  "payload": {
    "error_rate": 0.87,
    "sample_error": "anthropic API rate limit exceeded"
  }
}
```

### WORKSPACE_REMOVED

User deleted the workspace. Node removed from canvas.

```json
{
  "event": "WORKSPACE_REMOVED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:10:00Z",
  "payload": {
    "cascade_deleted": 0
  }
}
```

### AGENT_REPLACED

AI model swapped inside a workspace.

```json
{
  "event": "AGENT_REPLACED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "old_model": "anthropic:claude-sonnet-4-6",
    "new_model": "openai:gpt-4o"
  }
}
```

### AGENT_CARD_UPDATED

Workspace republished its Agent Card (new skill added, description changed, capabilities changed). The platform broadcasts this to all peer workspaces (siblings, children, parent) so they can rebuild their system prompts.

```json
{
  "event": "AGENT_CARD_UPDATED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "agent_card": {
      "name": "Vancouver SEO Agent",
      "version": "1.1.0",
      "skills": ["generate-seo-page", "audit-seo-page", "monitor-rankings"],
      "capabilities": { "streaming": true }
    }
  }
}
```

Hierarchy changes do not have dedicated `WORKSPACE_EXPANDED`,
`WORKSPACE_COLLAPSED`, or `WORKSPACE_MOVED` events. A team is the current set
of workspace rows linked by `parent_id`; clients create or reparent those rows
through the HTTP API and rehydrate current topology from `GET /workspaces`.
The Canvas `collapsed` flag only hides or shows descendants and is persisted in
`canvas_layouts` through `PATCH /workspaces/:id`.

### AGENT_ASSIGNED

A new AI agent assigned to a workspace (first time or after removal).

```json
{
  "event": "AGENT_ASSIGNED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "agent_id": "agent-xyz-789",
    "model": "anthropic:claude-sonnet-4-6"
  }
}
```

### AGENT_REMOVED

Agent removed from a workspace (workspace becomes empty).

```json
{
  "event": "AGENT_REMOVED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "agent_id": "agent-xyz-789",
    "reason": "user removed"
  }
}
```

### AGENT_MOVED

Agent moved from one workspace to another.

```json
{
  "event": "AGENT_MOVED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "agent_id": "agent-xyz-789",
    "from_workspace_id": "ws-abc-123",
    "to_workspace_id": "ws-def-456"
  }
}
```

### TASK_UPDATED

Agent's current task changed (via heartbeat). WebSocket-only — not persisted to `structure_events`.

```json
{
  "event": "TASK_UPDATED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "current_task": "Analyzing quarterly report",
    "active_tasks": 2
  }
}
```

Canvas shows the current task as an amber banner on the workspace node and side panel header. Only broadcast when the task actually changes (not on every heartbeat).

### ACTIVITY_LOGGED

New activity log entry created (A2A communication, webhook-triggered task ingress, agent log, error). WebSocket-only — not persisted to `structure_events` (stored in `activity_logs` table instead).

```json
{
  "event": "ACTIVITY_LOGGED",
  "workspace_id": "ws-abc-123",
  "timestamp": "2026-03-30T12:00:00Z",
  "payload": {
    "activity_type": "a2a_receive",
    "method": "message/send",
    "summary": "message/send → ws-abc-123",
    "status": "ok",
    "source_id": "ws-def-456",
    "target_id": "ws-abc-123",
    "duration_ms": 1500
  }
}
```

Canvas ActivityTab uses this event as a refresh hint. The event is informational — the full activity details (request/response bodies) are fetched via `GET /workspaces/:id/activity`.

## Subscribers

Both canvas clients and workspace agents subscribe to the same WebSocket endpoint (`/ws`):

| Subscriber | Identifies via | Receives | Purpose |
|------------|---------------|----------|---------|
| Canvas client | Verified control-plane session cookie, org bearer token, or `ADMIN_TOKEN`; no `X-Workspace-ID` | All tenant events | UI updates |
| Workspace agent | `X-Workspace-ID` plus a bearer token issued to that workspace | Filtered — only events about reachable peers | System prompt rebuilds |

The platform filters server-side using `CanCommunicate()` — each workspace only receives events about workspaces it can talk to.

The tenant-wide Canvas feed is an admin surface. A same-origin or allowlisted
`Origin` header is not authentication, and an unauthenticated upgrade is
rejected. Same-origin browser clients do not need a custom header: the browser
includes the Canvas session cookie in the WebSocket handshake, and the tenant
platform verifies that session with the control plane before upgrading.
Per-workspace tokens cannot open the tenant-wide feed.

## Event Flow

```
Event-producing operation occurs
      |
      v
Platform records the event in structure_events, or uses BroadcastOnly
for event families whose authoritative data lives elsewhere
      |
      v
Platform publishes to Redis pub/sub (events:broadcast)
      |
      v
WebSocket handler receives from Redis
      |
      v
WebSocket pushes JSON to subscribers (filtered per workspace)
      |
      +-> Canvas clients: update Zustand state -> React Flow re-renders
      +-> Workspace agents: rebuild system prompt if peer changed
```

## Related Docs

- [Canvas UI](../frontend/canvas.md) — How events drive the UI
- [Event Log](../architecture/event-log.md) — Persistent event storage
- [Registry & Heartbeat](./registry-and-heartbeat.md) — Events from registration
- [Provisioner](../architecture/provisioner.md) — Events from provisioning
- [Communication Rules](./communication-rules.md) — Hierarchy-based peer broadcasting
