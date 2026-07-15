---
title: "How Molecule AI's A2A Protocol Works: Peer-to-Peer Agent Communication"
description: "A technical deep-dive into Molecule AI's A2A v1.0 implementation — JSON-RPC, SSE, authenticated discovery, direct peer calls, and the platform proxy."
date: 2026-04-23
slug: a2a-protocol-deep-dive
og_title: "How Molecule AI's A2A Protocol Works"
og_description: "Agent communication with JSON-RPC, SSE, authenticated discovery, direct peer calls, and an authenticated platform proxy."
canonical: https://docs.molecule.ai/blog/a2a-protocol-deep-dive
---

*Meta description (160 chars): Protocol-native A2A in production — JSON-RPC,
SSE, authenticated discovery, direct peer calls, and a source-bound platform
proxy.*

---

Most A2A explainers stop at the message format. This one goes further: you'll
see exactly what a message looks like on the wire, how authenticated registry
discovery works, when runtimes call peers directly, and when traffic uses the
platform proxy.

If you're evaluating agent platforms, this is the layer that determines whether A2A is a feature or a constraint.

## The Protocol Layer

A2A v1.0 is built on JSON-RPC 2.0. Every message between agents is a valid JSON-RPC request or response, which means it works with any HTTP client and any JSON library in any language.

The `message/send` call — the core primitive — takes a target agent ID and a task payload:

```json
{
  "jsonrpc": "2.0",
  "method": "message/send",
  "params": {
    "message": {
      "message_id": "msg_01hx3k...",
      "task_id": "task_01hx3k...",
      "role": "user",
      "content": {
        "kind": "text",
        "text": "Run the security audit on the payment service workspace"
      }
    },
    "target_agent_id": "ws_01hx3k...",
    "metadata": {}
  },
  "id": 1
}
```

The `task_id` is client-generated and idempotent — if you send the same `task_id` twice, Molecule AI treats the second call as a duplicate and returns the cached response rather than re-executing. This is how you get at-least-once delivery without building your own deduplication layer.

## Platform Proxy Routing

Here's the part that matters architecturally.

When an agent uses `POST /workspaces/:id/a2a`, the platform proxy does three
things:

1. **Authenticates** the caller and source-binds identity. For a workspace
   caller, the bearer owner is authoritative; an optional `X-Workspace-ID`
   claim must match it.
2. **Looks up** the target workspace's current URL from the registry
3. **Forwards** the message to the target and relays its HTTP response

The platform remains in this proxy transport's HTTP request and response path.
It parses and may enrich the JSON-RPC envelope, while the target agent handles
the task semantics. Peer runtimes may instead use authenticated discovery and
call the returned URL directly, in which case the platform leaves the message
path after discovery.

```
Agent A                          Platform                          Agent B
   |                                  |                                |
   |-- POST /workspaces/:id/a2a ----->|                                |
   |  { target: ws_B, content: ... }  |                                |
   |                                  |-- POST http://agent-b:3001 -->|
   |                                  |  (normalized/enriched JSON-RPC)|
   |                                  |                                |
   |                                  |<-- HTTP response --------------|
   |<-- relayed A2A response ----------|                                |
   (the platform relays Agent B's HTTP response)
```

The proxy's role is an authenticated relay: it verifies the sender, applies the
current hierarchy, resolves the address, and passes through the target's
response.

### JSON-RPC Wrapping

The platform wraps your message in a JSON-RPC envelope before forwarding:

```json
{
  "method": "message/send",
  "params": {
    "message": {
      "message_id": "msg_01hx3k...",
      "content": { "kind": "text", "text": "Run the security audit" }
    }
  },
  "id": 1
}
```

The `params.metadata` field carries non-JSON-RPC extensions — `run_id` for grouping parallel calls, `source_workspace_id` for audit attribution, and any custom key-value pairs your integration needs to propagate. The platform preserves `metadata` end-to-end.

## Agent Discovery: Register Once, Message Anyone

Agents don't need a pre-configured address book. They register with the platform and the platform resolves addresses on demand.

Registering looks like this:

```python
import requests, os, time, threading

PLATFORM = os.environ["PLATFORM_URL"]
WORKSPACE_ID = os.environ["WORKSPACE_ID"]
AUTH_TOKEN = os.environ["AUTH_TOKEN"]

resp = requests.post(
    f"{PLATFORM}/registry/register",
    json={
        "id": WORKSPACE_ID,
        "url": os.environ["AGENT_URL"],
        "agent_card": {
            "name": "Security Auditor",
            "skills": ["security", "audit", "python"]
        }
    },
    headers={"Authorization": f"Bearer {AUTH_TOKEN}"}
)

token = resp.json()["token"]  # per-workspace bearer, not a shared key

def heartbeat():
    while True:
        requests.post(
            f"{PLATFORM}/registry/heartbeat",
            json={"workspace_id": WORKSPACE_ID, "error_rate": 0.0,
                  "active_tasks": 0, "uptime_seconds": 0},
            headers={"Authorization": f"Bearer {token}"}
        )
        time.sleep(30)

threading.Thread(target=heartbeat, daemon=True).start()
```

The response includes a per-workspace bearer token scoped to exactly this workspace — it cannot be used to access any other workspace, even if the token is intercepted.

When Agent A wants to message Agent B, it calls `GET /registry/discover/:id` with Agent B's workspace ID. The platform returns Agent B's current URL and a snapshot of its agent card. Agent A then POSTs directly to that URL. Discovery is a single API call, not a permanent channel.

```json
// GET /registry/discover/ws_01hx3k...
{
  "agent_card": {
    "name": "Security Auditor",
    "skills": ["security", "audit", "python"]
  },
  "url": "http://audit-workspace:3001",
  "last_seen": "2026-04-23T14:32:01Z"
}
```

The `last_seen` timestamp tells you whether the target is online. Agents that haven't sent a heartbeat in 90 seconds are marked offline — messages to them return a `workspace_offline` error rather than hanging.

## Authentication at Discovery Time

Every discovery call requires `X-Workspace-ID`; token-enrolled workspace
callers also present their matching bearer. Legacy no-token discovery remains
bootstrap-compatible, but authentication datastore errors fail closed.
Platform-proxied A2A requires a source-bound workspace bearer or a verified
human/external-inbound credential. For workspace callers, an optional
`X-Workspace-ID` must match the bearer owner. The platform enforces this before
routing — not as a client-side convention or optional middleware setting.

The `CanCommunicate(callerID, targetID)` check runs before any message is forwarded:

```python
def CanCommunicate(caller_id: str, target_id: str) -> bool:
    # Same workspace — always allowed
    if caller_id == target_id:
        return True

    # Same non-root-parent siblings — allowed
    if share_non_root_parent(caller_id, target_id):
        return True

    # Ancestor/descendant in either direction, at any depth — allowed
    if is_ancestor_of(caller_id, target_id):
        return True
    if is_ancestor_of(target_id, caller_id):
        return True

    # Unrelated roots and disjoint subtrees — denied
    return False
```

Self-calls are in the same trust boundary. Cross-workspace communication
requires either the same non-root parent or an ancestor/descendant relationship
at any depth. Unrelated roots and disjoint subtrees are denied.

This is enforced in `workspace-server/internal/registry/access.go`. The Go implementation is the authoritative reference — the Python pseudocode above reflects the logic, not the production code.

## SSE Streaming for Long-Running Tasks

Agentic tasks are not always short. When an agent starts a task that takes minutes, you need to track progress without polling.

Molecule AI's A2A implementation supports Server-Sent Events for task progress. The caller receives a stream of `progress` events followed by a final `task_complete` or `error`:

```
event: progress
data: {"run_id":"run_01hx3k...","progress":0.25,"message":"Scanning 140 services..."}

event: progress
data: {"run_id":"run_01hx3k...","progress":0.60,"message":"Running CVE check on 23 packages..."}

event: task_complete
data: {"run_id":"run_01hx3k...","result":{"kind":"text","text":"3 critical CVEs found. Patch recommendation ready."}}
```

The `run_id` groups parallel calls — when an agent fires multiple tool calls simultaneously, each call gets a separate `run_id` so you can track them independently while seeing the full execution tree.

## Redis Key Resolution: How the Platform Tracks Agents

Behind the discovery API, Molecule AI uses Redis for agent registry state:

```
workspace:{id}:url        -> "http://audit-workspace:3001"
workspace:{id}:card       -> {"name":"Security Auditor","skills":[...]}
workspace:{id}:heartbeat  -> "2026-04-23T14:32:01Z" (TTL: 90s)
workspace:{id}:org        -> "org_01hx3k..."
```

The 90-second TTL on the heartbeat key is what drives the offline detection. When the heartbeat loop stops — because the agent crashed, was paused, or lost network — the key expires and the platform stops routing messages to that workspace.

This is the same Redis pub/sub used for the WebSocket event bus. When an agent's heartbeat key expires, the platform broadcasts a `WORKSPACE_OFFLINE` event over Redis, the WebSocket hub picks it up, and the canvas updates the agent's status in real time. The agent then gets auto-restarted by the provisioner.

The full cycle: heartbeat TTL expires → `WORKSPACE_OFFLINE` broadcast → canvas updates → provisioner restarts container → agent re-registers → discovery works again. No manual intervention required.

## Why This Matters for Your Architecture

The peer-to-peer model has concrete implications for teams building on Molecule AI.

**Latency:** Direct peer mode pays the discovery hop and then calls the target
URL directly. Proxy mode adds a platform relay on every request and response;
it is the appropriate path when the caller cannot reach the peer URL or needs
the proxy's current authorization and queue behavior.

**Privacy:** Direct peer mode leaves the tenant platform message path after
discovery. Proxy mode parses and normalizes the JSON-RPC envelope, may inject
metadata, records activity, forwards the request, and relays the response. Do
not treat the proxy as operator-blind; use direct transport and appropriate
end-to-end content protection when that is a compliance requirement.

**Scalability:** Postgres is the registry source of truth and Redis caches
liveness and URLs. Direct traffic does not load the proxy after discovery;
proxied traffic does, so capacity planning must include the tenant
workspace-server relay and any queue fallback.

**Auditability:** Authenticated A2A traffic that reaches routing is attributed
using the server-verified caller identity. Workspace attribution comes from the
bearer owner, not from trusting `X-Workspace-ID`; a supplied claim is accepted
only when it matches.

## LangGraph Is Shipping A2A — Here's the Difference

LangGraph's A2A PRs (#6645, #7113) are real and close to landing (Q2-Q3 2026 GA). The protocol layer is solid — message format, transport, capability negotiation. What they're building is what Molecule AI shipped in Phase 30.

The gap is governance:

| | Molecule AI | LangGraph (projected) |
|---|---|---|
| JSON-RPC message format | ✅ Production | ✅ In review |
| Agent discovery | ✅ On-demand | ✅ In review |
| Routing modes | ✅ Direct after discovery + authenticated proxy | ⚠️ Proxy in path |
| Per-workspace auth tokens | ✅ Phase 30 | ❌ Not in current PRs |
| Optional `X-Workspace-ID` source binding | ✅ Protocol-level | ❌ Not in current PRs |
| `CanCommunicate` access model | ✅ Production | ❌ Not in current PRs |
| Cross-network federation | ✅ Phase 30 | ❌ Not in current PRs |
| Org-scoped delegation attribution | ✅ Phase 33 | ❌ Not in current PRs |

Molecule AI's A2A implementation is production-ready today. The governance features that make A2A safe for enterprise — workspace scoping, immutable audit trails, cross-network federation — are already live. If you need those capabilities, you don't have to wait for LangGraph's roadmap.

## Get Started

To register an external agent, follow the [External Agent Registration Guide](/docs/guides/external-agent-registration). The A2A protocol spec with full JSON-RPC reference is at [docs/api-protocol/a2a-protocol.md](/docs/api-protocol/a2a-protocol).

For the MCP server that wraps the platform API: `npx @molecule-ai/mcp-server`.

If you're building a multi-agent workflow and want to understand how the pieces fit together, the [workspace runtime docs](/docs/agent-runtime/workspace-runtime) cover the adapter model and how external agents integrate.
