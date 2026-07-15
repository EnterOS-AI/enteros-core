# A2A communication

Molecule uses A2A JSON-RPC messages between workspaces, but there is no single
transport path. Current delivery can be direct, platform-proxied, queued, or
poll-driven depending on the caller, target runtime, and delivery mode.

## Message envelope

The common request is JSON-RPC 2.0:

```json
{
  "jsonrpc": "2.0",
  "id": "task-123",
  "method": "message/send",
  "params": {
    "message": {
      "role": "user",
      "parts": [{"kind": "text", "text": "Build the login feature"}],
      "messageId": "msg-456"
    }
  }
}
```

The selected workspace runtime owns execution and task-status semantics. Core
does not prescribe one agent framework or interrupt implementation.

## Delivery paths

### Platform proxy

`POST /workspaces/:id/a2a` is the main routing boundary for Canvas and
platform-mediated traffic. It:

- accepts a verified control-plane tenant-member session, org token, or
  `ADMIN_TOKEN` for Canvas traffic, whether or not it supplies an
  identity-workspace header; combined self-host/dev deployments retain a
  same-origin fallback only when control-plane session verification is
  unconfigured, and SaaS never treats `Origin` as authentication;
- derives a workspace caller from its live bearer and requires any supplied
  caller ID, including a self-call ID, to match that token owner;
- applies hierarchy and same-org communication checks;
- normalizes missing JSON-RPC/message IDs;
- enforces request/response size and forwarding limits;
- resolves the target's current URL;
- records activity and classifies delivery-confirmed response failures;
- queues work when the target is busy or uses a queue/poll delivery mode;
- performs backend-aware dead-target recovery checks.

Long Canvas turns can return `{ "status": "queued" }` after the synchronous
edge-safe budget while dispatch continues. The eventual response is delivered
through the Canvas event path. `queued` is therefore not itself a failure.

### Direct peer delivery

Runtime tools can discover an allowed peer and send A2A directly for the fast
path. The platform authenticates discovery and applies `CanCommunicate()` before
returning the URL, but the subsequent peer request does not pass through the
platform HTTP authenticator. Direct reachability and protection therefore
depend on the deployment network boundary; callers should rediscover instead
of treating a cached URL as continuing authorization.

### Durable queue and poll

Busy or poll-mode workspaces receive durable `a2a_queue` rows. The target polls
with its workspace credential, and callers can query a queue row only when they
are the recorded caller, the target, or hold accepted tenant-admin authority.
Authorization failures use non-inferable not-found behavior.

`POST /workspaces/:id/delegate` and the delegation ledger add orchestration and
terminal-status tracking on top of these delivery primitives.

### External inbound

External agents expose a public inbound endpoint. Platform-to-external forwards
carry the target workspace's `platform_inbound_secret`. An external caller that
needs to reach a platform-hosted workspace uses:

```text
POST /workspaces/:id/a2a/inbound
Authorization: Bearer <platform_inbound_secret>
```

The handler reads the target's secret, compares it in constant time, strips the
consumed credential and any caller-supplied workspace identity, then reuses the
normal proxy. Missing enrollment or a bad secret fails closed. The platform can
mint/heal the secret on its outbound path and asks the caller to retry while the
external runtime picks it up.

## Discovery

`GET /registry/discover/:id` checks whether the caller may communicate with the
target before returning a usable address. Local container peers may receive a
container-network URL; platform callers use a platform-reachable URL. Addresses
can change after restart or relocation, so callers should rediscover rather
than persist backend hostnames.

Discovery authorization is not a substitute for the current credential checks
on proxy, queue, or external inbound routes.

## Task and delegation status

A runtime can expose A2A task states such as submitted, working,
input-required, completed, failed, and canceled. Molecule's delegation ledger
separately tracks dispatch/delivery/execution status so a caller can distinguish
transport acceptance from terminal agent work.

Cancellation is supported only when the target runtime advertises and
implements an interrupt primitive. A response body may contain text or
structured artifacts; Core treats the agent as opaque and applies transport
limits rather than interpreting framework internals.

## Security boundaries

- Current tenant-admin credentials authorize Canvas calls with or without an
  identity-workspace header. Combined self-host/dev Canvas traffic has the
  narrow CP-unconfigured same-origin fallback described above. Other public
  proxy callers need either a live workspace bearer or the target-bound
  external-inbound credential; Core derives workspace-token ownership
  server-side and rejects any mismatched `X-Workspace-ID`, including a forged
  self-call.
- A workspace bearer must match the caller workspace; it cannot be used to
  impersonate another caller ID.
- Reserved system-caller prefixes are accepted only from trusted in-process
  call sites and are rejected from public headers.
- Cross-org and hierarchy checks run before workspace-to-workspace forwarding.
- External agent traffic requires the per-workspace inbound secret.
- Agent URLs and provider identifiers are routing data, not authentication.

Implementation authority: `workspace-server/internal/handlers/a2a_proxy.go`,
`workspace-server/internal/handlers/a2a_queue.go`,
`workspace-server/internal/handlers/delegation.go`,
`workspace-server/internal/wsauth/platform_inbound.go`, and the route groups in
`workspace-server/internal/router/router.go`.

Related: [Communication rules](./communication-rules.md),
[Registry and heartbeat](./registry-and-heartbeat.md), and
[Workspace runtime](../agent-runtime/workspace-runtime.md).
