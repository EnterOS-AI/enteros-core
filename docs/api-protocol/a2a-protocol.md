# A2A Protocol (Inter-Workspace Communication)

Molecule supports two A2A (Agent-to-Agent protocol) transports: runtimes can
call a discovered peer **directly**, or callers can use the platform's
authenticated A2A proxy.

## How It Works

Every workspace is an A2A server. For direct peer transport, the platform
authorizes discovery and returns a reachable URL, then leaves the message path.
For Canvas, external inbound, queue fallback, and server-side delegation, the
platform proxy authenticates, authorizes, forwards the request, and relays the
response.

```
Business Core (A2A client)  ->  Developer PM (A2A server)
                                  (opaque to Business Core
                                   what's inside)
```

## Discovery Flow

How Business Core finds Developer PM's URL:

1. Business Core asks the platform: `GET /registry/discover/developer-pm-id`
   with its required `X-Workspace-ID` claim and its bearer when token-enrolled.
2. The platform validates the discovery credential and checks
   `CanCommunicate()` for the caller/target pair.
3. The platform returns the URL appropriate to the target and caller runtime:
   an external target's registered URL, or a Docker-internal URL for a local
   workspace target.
4. On a cache miss, the platform reads from Postgres and refreshes the cache.
5. Business Core sends A2A JSON-RPC message **directly** to Developer PM
6. Developer PM processes the task and responds

For this direct transport, the platform is involved only in authenticated URL
resolution. The separate proxy transport remains in the request and response
path as described below.

## Message Format

A2A uses JSON-RPC 2.0 over HTTP:

```json
{
  "jsonrpc": "2.0",
  "id": "task-123",
  "method": "message/send",
  "params": {
    "message": {
      "role": "user",
      "parts": [{ "kind": "text", "text": "Build the login feature" }],
      "messageId": "msg-456"
    }
  }
}
```

The receiving workspace:
1. Processes this as a task
2. Streams progress updates via SSE
3. Returns artifacts (files, structured data, text) when done

## On-Demand Discovery (Not Pushed)

Topology is **not** pushed to workspaces at startup. A workspace only queries the platform for another workspace's URL at the moment it decides to delegate to it.

**Why not push at startup:** The topology changes while the workspace is running — sub-workspaces get added, removed, come online and go offline. If you push at startup you'd need to also push every topology change to every affected workspace and keep them in sync. That's complex and fragile.

On-demand fits naturally with how agents work — an agent only needs to know about another workspace at the moment it decides to delegate, not before.

**Note:** While URL resolution is on-demand, the workspace does fetch peer Agent Cards on startup to build its system prompt (see [System Prompt Structure](../agent-runtime/system-prompt-structure.md)). The system prompt is rebuilt reactively when `AGENT_CARD_UPDATED` events arrive — but the actual A2A URL for sending messages is resolved on-demand at delegation time.

## Authentication Between Workspaces

**Direct peer transport:** The platform validates the discovery caller and
`CanCommunicate()` when workspace A calls `GET /registry/discover/:id`. That
route requires `X-Workspace-ID`; an enrolled workspace also presents its own
bearer. Once A has B's URL, a direct request to B's agent server does not pass
through the platform's HTTP A2A authenticator.

Direct transport therefore relies on the target network's trust boundary after
discovery. It is appropriate only when peer endpoints are isolated accordingly
(for example, a self-hosted Docker network controlled by one operator). Use the
platform proxy when the caller needs current source authentication and
hierarchy enforcement on each request.

**Known gap:** Once workspace A caches workspace B's URL, nothing stops A from calling B directly even after the hierarchy changes and A is no longer supposed to reach B. The cached URL remains valid until the container is restarted or the URL changes.

**Platform proxy transport:** `POST /workspaces/:id/a2a` is a separate path.
It authenticates workspace callers with a source-bound bearer and applies the
current hierarchy before dispatch. Verified control-plane sessions,
`ADMIN_TOKEN`, org tokens, and authenticated external inbound requests are
privileged non-workspace paths. A combined self-host/dev Canvas may use the
same-origin fallback only when control-plane session verification is not
configured; SaaS never accepts same-origin headers as authentication.

**Post-MVP fix — platform-issued tokens:** On discovery, the platform issues a short-lived signed token scoped to the specific caller/target pair. The target workspace validates the token on every A2A request. When the hierarchy changes, old tokens expire and new discovery attempts are blocked by `CanCommunicate()`.

## Task Lifecycle

Every A2A message creates a task with a defined lifecycle:

```
submitted → working → completed
                    → failed
                    → canceled
           → input-required → working (caller provides input)
```

### Full Flow

```
Caller sends message/send or message/sendSubscribe
      │
      ▼
Task created: status = submitted
      │
      ▼
Workspace starts processing: status = working
      │
      ├── needs clarification?
      │         │
      │         ▼
      │   status = input-required
      │   SSE event fires to caller
      │   caller sends follow-up message
      │         │
      │         ▼
      │   status = working (resumes)
      │
      ├── success
      │         │
      │         ▼
      │   status = completed
      │   SSE terminal event fires
      │   artifacts returned
      │
      └── error
                │
                ▼
          status = failed
          SSE terminal event fires
          error details returned
```

### Calling Patterns

Two patterns — synchronous for short tasks, streaming for long ones:

```python
# pattern 1 — synchronous (short tasks)
# caller blocks until terminal state
result = await a2a.send({
    "method": "message/send",
    "params": { "message": { ... } }
})
# returns when completed/failed — no streaming

# pattern 2 — streaming (long tasks)
# caller subscribes to SSE stream
async for event in a2a.subscribe({
    "method": "message/sendSubscribe",
    "params": { "message": { ... } }
}):
    if event["status"] == "working":
        # intermediate progress update
        print(event["message"])

    if event["status"] in ("completed", "failed", "canceled"):
        # terminal event — stream ends here
        result = event["artifacts"]
        break
```

No polling needed. The SSE stream includes a terminal event — the caller knows the task is done when it receives `completed`, `failed`, or `canceled`.

### Task ID

Every task gets an ID on creation, returned in the first SSE event or synchronous response:

```python
task_id = response["id"]

# caller can check status explicitly if needed
status = await a2a.get(f"/tasks/{task_id}")
```

### Cancellation

```python
# cancel an in-flight task
await a2a.send({
    "method": "tasks/cancel",
    "params": { "id": task_id }
})
# workspace receives cancel signal
# status → canceled
# SSE terminal event fires to all subscribers
```

The workspace handles cancellation via the `LangGraphA2AExecutor.cancel()` method, which uses LangGraph's interrupt mechanism:

```python
# workspace/a2a_executor.py
async def cancel(self, context: RequestContext, queue: EventQueue):
    await self.agent.ainterrupt(context.context_id)
    # status → canceled, SSE terminal event fires automatically
```

See [Workspace Runtime — A2A Server Wrapping](../agent-runtime/workspace-runtime.md#a2a-server-wrapping) for the full executor implementation.

### Artifacts

On completion, the task returns artifacts:

```json
{
  "status": "completed",
  "artifacts": [
    {
      "type": "text/plain",
      "content": "Page generated successfully"
    },
    {
      "type": "application/json",
      "content": { "page_path": "/kitchen-renovation-vancouver" }
    }
  ]
}
```

## Platform A2A Proxy

The canvas (browser) cannot reach Docker-internal agent URLs directly. The platform provides `POST /workspaces/:id/a2a` as a proxy:

1. The caller presents a workspace bearer, verified control-plane session, `ADMIN_TOKEN`, org token, or the target-bound external inbound secret. A combined self-host/dev Canvas may use same-origin only when control-plane session verification is unconfigured.
2. For workspace callers, the platform derives the source workspace from the bearer. `X-Workspace-ID`, when supplied, must match that identity.
3. The proxy enforces hierarchy and same-org access before resolving the target.
4. The proxy resolves the agent's host-accessible URL from Redis cache (falls back to DB).
5. If the request lacks a `jsonrpc` field, the proxy wraps it in a JSON-RPC 2.0 envelope with a generated UUID.
6. If `params.message.messageId` is missing, the proxy injects one (required by a2a-sdk).
7. The proxy forwards the request to the agent and returns the response.

Missing, revoked, tokenless legacy, forged-self, mismatched-header, and auth
datastore-error cases fail closed before dispatch. The only no-bearer Canvas
compatibility path is the CP-unconfigured same-origin case above. A raw direct
call to a discovered agent URL is outside the platform proxy authorization
boundary.

## Key Properties

- **Transport:** JSON-RPC 2.0 over HTTP — any language can implement it
- **Discovery:** Agent Cards at `/.well-known/agent-card.json`
- **On-demand:** Workspaces discover peers when needed, not at startup
- **Opaque execution:** The caller doesn't know (or care) what's inside the callee
- **Interoperable:** Any A2A-compliant agent from any framework can plug in
- **Source-bound proxy auth:** Workspace bearer ownership is the authoritative caller identity on the platform proxy
- **Fail-closed:** Invalid credentials and auth lookup failures never downgrade to canvas traffic

## Related Docs

- [Agent Card](../agent-runtime/agent-card.md) — The identity document used for discovery
- [Communication Rules](./communication-rules.md) — Who can communicate with whom
- [System Prompt Structure](../agent-runtime/system-prompt-structure.md) — How peer Agent Cards are used in prompts
- [Registry & Heartbeat](./registry-and-heartbeat.md) — How workspaces register URLs
- [Platform API](./platform-api.md) — The discovery endpoint
