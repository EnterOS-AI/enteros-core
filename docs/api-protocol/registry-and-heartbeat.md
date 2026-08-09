# Registry and heartbeat

The registry records where a workspace can be reached and its latest health
snapshot. Postgres is authoritative; Redis holds short-lived liveness and URL
caches.

## Registration

A newly started workspace calls `POST /registry/register` with its workspace
identity, advertised URL, Agent Card, and runtime capability fields. The exact
request contract is defined by `internal/models` and
`internal/handlers/registry.go`.

Registration is not an unauthenticated URL update. Existing workspaces must
present a valid credential for the workspace, and the boot path uses its scoped
registration credential. The handler rejects an attempt to register another
workspace's ID or replace its URL without the required proof.

On success the handler updates the workspace row, refreshes routing caches,
sets the liveness marker, writes/broadcasts the relevant lifecycle event, and
returns the current token/config material the runtime is allowed to receive.
`runtime=external` can enter `awaiting_agent` until the external process
registers.

## Heartbeat

The workspace runtime normally posts `POST /registry/heartbeat` on an
approximately 30-second cadence. Current payload fields include health and UI
state such as error rate, sample error, active task count, uptime, and current
task. Runtime capability fields are also reported where applicable.

For a `kind=platform` workspace, management-MCP health is fail closed:

- `mcp_server_present` reports that the management server is declared;
- `loaded_mcp_tools` reports tools the runtime's enumeration probe observed;
- after the grace window, absence of the required create-workspace tool can
  degrade the concierge even when the server was declared.

`loaded_mcp_tools` is a PRODUCER SELF-REPORT and its count is not a callability
claim (core#5137). The runtime probe spawns each declared MCP server as its own
stdio subprocess under a private client and lists that subprocess's tool
schemas; nothing on that path touches the surface the model dispatches from, and
the two can be in different namespaces — a concierge has reported 54
`mcp__molecule-platform__*` ids while every model dispatch on the fleet was
`mcp__molecule__*`.

Core therefore publishes a second, consumer-derived field next to it, on
`GET /workspaces/:id`:

- `mcp_surface` — how many of those reported ids sit in a namespace this
  workspace's model has actually been observed dispatching from, derived from
  core's own turn record (`activity_logs.tool_trace` and the tool-use
  `agent_log` summaries). Read `dispatch_corroborated_count`, not
  `len(loaded_mcp_tools)`, when the question is "what can the model call".
- `verdict` carries its own strength: `dispatch_observed:*` (core saw a real
  dispatch in that namespace), `contradicted:*` (core has dispatch records and
  NONE match the advertised inventory — the 54/zero state), or `unknown:*`
  (nothing to compare yet). `null` means core has not classified the row; it is
  not a verdict.

`mcp_surface` deliberately does not gate anything. It re-labels the
`workspace.online` event's `readiness_evidence` and reports; it is not a term of
the promotion predicate in either polarity.

Heartbeat values overwrite the latest snapshot in Postgres. Long-term task and
request observability belongs in the tracing/activity systems, not a heartbeat
history table.

## Status and liveness

Each successful register or heartbeat refreshes `ws:<workspace-id>` with
`db.LivenessTTL`, currently 180 seconds. This tolerates several missed
heartbeats during a busy runtime turn. Do not duplicate the duration in another
implementation; use the constant in `internal/db/redis.go`.

Health is backend aware:

- Redis expiry drives passive offline detection;
- local container workspaces are checked through Docker;
- control-plane-backed workspaces use the control-plane running-state API;
- external workspaces are considered stale after their heartbeat-age window,
  180 seconds by default;
- the A2A proxy can perform a reactive backend check after a forwarding error.

Paused, hibernating, hibernated, provisioning, and removed states are protected
from being overwritten by a late health sweep. A dead or stale active workspace
is marked offline, cache keys are cleared, an event is broadcast, and the
configured recovery path may restart it.

Self-reported error rate can move an active workspace between `online` and
`degraded`. Platform workspaces also apply the management-MCP checks above.

## Discovery and relocation

`GET /registry/discover/:id` applies communication authorization and resolves
the target's currently usable URL. Local peers may receive a container-network
URL, while browser/system proxy paths use a platform-reachable URL. Cache misses
fall back to Postgres.

When compute moves, a successful authenticated registration updates the durable
URL and invalidates the old cache. Callers must discover or proxy through the
platform rather than persisting a provider address.

The legacy `forwarded_to` column can still be read for existing data, but
current create, reparent, and delete flows do not populate redirect chains.

## Implementation authority

- register/heartbeat: `workspace-server/internal/handlers/registry.go`;
- request and response models: `workspace-server/internal/models/`;
- Redis TTL/cache helpers: `workspace-server/internal/db/redis.go`;
- active health sweeps: `workspace-server/internal/registry/healthsweep.go`;
- route wiring: `workspace-server/internal/router/router.go`.

Related: [A2A protocol](./a2a-protocol.md),
[Workspace provisioning](../architecture/provisioner.md), and
[Database schema](../architecture/database-schema.md).
