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
stdio subprocess under a private client and lists that subprocess's tool schemas,
so **it can only ever prove the server is healthy**. The surface the model is
finally offered is assembled later, out of that inventory, and the two can
diverge without the inventory changing at all. On 2026-08-05 a concierge
reported 54 management tools loaded while its model could call none of them,
because hermes' tiered disclosure (`tool_search`) had replaced the individual
MCP tools with three bridge tools and deferred the real schemas — measured
`tool_search=off → subset CONVERGES`, `tool_search=on → 60/60 DIVERGES`. The
runtime template fixed that unconditionally on 2026-08-06.

Two spellings exist for the same tool and they are **not comparable raw**. The
probe composes ids from the server name as declared
(`mcp__molecule-platform__…`); hermes sanitises each name component with
`[^A-Za-z0-9_] → _` before registering, so the model is only ever offered
`mcp__molecule_platform__…`. Any comparison must fold both sides first — the
runtime's `canonical_tool_id` is the SSOT for that fold.

The authoritative runtime-side answer rides the heartbeat's identity-gate
payload:

- `model_facing_tools` — what the model was actually offered, post-assembly;
- `loaded_not_model_facing` — the runtime's own set-difference, already folded
  through `canonical_tool_id` because only the runtime sees both spellings. A
  NON-EMPTY value is the degraded signal, and it needs no spelling knowledge
  from core.

**Core does not consume it yet, for two concrete reasons** (recorded so this
reads as a plan rather than an oversight):

1. `models.HeartbeatPayload` has no `loaded_not_model_facing` field, so core
   cannot receive it — the string does not occur anywhere in core's `main`.
2. Nothing emits it yet. Over 168h of fleet logs it appears **zero** times,
   against a control (`mcp_tools_ready`) appearing 475 times in the same query
   shape — so the zero is real absence, not a broken selector.

Order of work: runtime ships the field → core adds the payload field → core
gates on NON-EMPTY. Arming the gate first would be a signal armed ahead of its
producer.

Core additionally publishes an INDEPENDENT consumer-side cross-check on
`GET /workspaces/:id`:

- `mcp_surface` — how many reported ids sit in a namespace this workspace's
  model has actually been observed dispatching from, derived solely from
  `activity_logs.tool_trace` (written only by core's `extractToolTrace` as it
  ingests a turn). The tool-use `agent_log` summaries are deliberately NOT read:
  `POST /workspaces/:id/activity` accepts an arbitrary summary from the
  authenticated workspace, so reading them would let a workspace manufacture its
  own corroboration. Read `dispatch_corroborated_count`, not
  `len(loaded_mcp_tools)`, when the question is "what can the model call".
- `verdict` carries its own strength and is either an **observation**
  (`dispatch_observed:namespace_corroborated`) or an **admission of ignorance**
  (`unknown:no_inventory_reported`, `unknown:no_dispatch_record`,
  `unknown:advertised_not_yet_exercised`). `null` means core has not classified
  the row; it is not a verdict.

  There is deliberately **no fault verdict**. Dispatch records are existential:
  an observed dispatch proves reachability, but no amount of non-observation
  proves unreachability — a concierge simply may not have needed the verb, which
  on this fleet is the ordinary case (168h: 611 `mcp__molecule__*` dispatches,
  zero management-MCP dispatches). A verdict reading as a fault would be a label
  stronger than its evidence and, being the common case, would be learned as
  noise. Contradiction requires comparing the inventory to the *offered* surface,
  which is what `loaded_not_model_facing` does.

- `corroborated_namespaces` is **monotonic** and `dispatch_corroborated_count` is
  derived from it, not from the read window. Corroboration is an existence claim
  ("this model has dispatched from namespace X"), so it must not flap as older
  dispatches age out behind ordinary chatter. Non-corroboration is windowed and
  re-derived each beat. A sticky corroborated verdict means *was reachable at
  least once*, not *is reachable now* — `first_corroborated_at` carries the age
  of the claim so that distinction stays visible.

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
