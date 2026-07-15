# RFC: Agent workload visibility (supervisor/HR resource management)

**Status:** DRAFT — design note, needs CTO sign-off on the contract shape before build.
**Repos touched:** molecule-ai-workspace-runtime (workload source), molecule-core (peer surface + MCP), molecule-ai-sdk (contract).
**Related:** [`rfc-agent-liveness.md`](rfc-agent-liveness.md), the idle-consolidation digest kernel (runtime), ADR-004 (SDK-owns-adapter socket; core carries zero runtime-behaviour code).

## Problem

A supervisor/HR agent that assigns work has no good view of how loaded its peers are. Two symptoms motivated this note:

1. **`current_task` is mis-modelled.** The `workspaces` row carries `active_tasks` (an integer count, 0..`max_concurrent_tasks`) *and* `current_task` (a single string). `active_tasks` is the real "is this agent busy, and how loaded" gauge — it gates A2A dispatch drain, hibernation, discovery, the wedged-agent detector, and the request-nudge sweeper. `current_task` is written in the same heartbeat, zeroed by the same phantom-busy sweep, and **gates nothing** — it is only broadcast/displayed. When `active_tasks > 1`, the singular `current_task` can only show one of the N in-flight items, so it reads like a task list but is a lossy "last thing started" label.

2. **No workload summary for supervisors.** `GET /peers` (and the `list_peers` MCP tool) already expose each peer's raw `active_tasks` count, but not a decision-useful view: what each agent is working on, whether it is idle vs. saturated, queue depth. HR/supervisor agents can't allocate work by load.

## Non-goals

- Do **not** expand `current_task` into a list on the `workspaces` row. That just adds heartbeat payload and more denormalized state to drift. The authoritative task list already lives in the runtime.
- Not a scheduler/cron concern; orthogonal to the scheduler-as-trigger-plugin RFC.

## Proposal

### 1. Deprecate `current_task`; keep `active_tasks`
**`current_task` retires** — it is a singular string that gates nothing (only displayed), so remove it once nothing on the canvas depends on it. The "what is it working on" need is served by (2) instead.

**`active_tasks` stays.** It is not a separable feature from the workload summary — it *is* the "running" count, and it is load-bearing: five gates read it (A2A drain capacity `max_concurrent − active_tasks`, hibernation refusal, wedged-agent detection, stall-watchdog, idle-nudge) and the idle-consolidation digest fires only when it is `0`. `get_peer_workload` exposes this count (plus `queued`/`idle`) to supervisors; it does not remove any gate's need for it. The `workload` summary is therefore **additive** — `workload.running` mirrors `active_tasks`; `active_tasks` remains the canonical field the gates read.

**Optional future consolidation (not in scope):** the standalone `active_tasks` column could later be folded so `workload.running` is its single home, migrating the five gates + the hot per-heartbeat drain path to read it there. That is pure SSOT cleanup with **zero behavior change** (the count still exists, just relocated) and hot-path churn — noted as a follow-up, not proposed here.

### 2. Runtime-owned workload summary (SSOT)
The runtime already owns its authoritative task queue (origins + states) and assembles the idle-consolidation digest from it. It reports a small structured **workload summary** in/alongside its heartbeat — counts by state, not a denormalized string:

```
workload: {
  running: int,        # turns in flight now (== active_tasks)
  queued:  int,        # A2A-queue depth waiting for capacity
  idle:    bool,       # active_tasks == 0
  titles:  [string]    # <=3 short titles of current/next items (bounded)
}
```

Core stores/relays it as opaque runtime-reported state (same trust model as `active_tasks` today — the runtime is the source of truth for its own tasks; core never infers workload from denormalized columns).

### 3. Expose to supervisors via a dedicated `get_peer_workload` MCP tool
**Decision (CTO 2026-07-15): a dedicated `get_peer_workload` tool**, not a `list_peers` extension. Rationale: `list_peers` is a lightweight discovery/roster call that many agents make often; folding a richer, more expensive workload payload into it would tax every caller. A separate tool keeps the roster cheap and lets a supervisor pull workload deliberately when it is actually allocating work.
- `get_peer_workload` returns `{peer_id, name, workload}` for the peers the caller may see, workload as in (2).
- Access governed by the existing hierarchy/`CanCommunicate` rules — same visibility set as `list_peers`.
- `list_peers` stays as-is (may keep its existing raw `active_tasks` field for back-compat; not expanded).

### 4. Surface in the idle-consolidation digest
Fold the same summary into the idle digest so an HR/supervisor agent passively receives fleet workload each idle cycle ("peer A: 3 running / 5 queued; peer B: idle"), enabling load-based assignment without polling. Reuses the digest machinery that already exists.

## Why this shape

- **SSOT:** the runtime's task queue is the one source; core relays, never re-derives. Matches ADR-004 and how the idle digest / schedule grid are already owned.
- **No fake task list:** avoids resurrecting `current_task` as a pseudo-array.
- **Reuses existing seams:** `list_peers` for pull, the idle digest for push.

## Open questions (for CTO)
1. Contract home: does `workload` live in the heartbeat payload contract (SDK) or a separate report? (Rec: extend the heartbeat contract — it already carries `active_tasks`.)
2. ~~`list_peers` extension vs. dedicated `get_peer_workload` tool.~~ **DECIDED 2026-07-15: dedicated `get_peer_workload` (keep `list_peers` cheap).**
3. `titles` — include short task titles, or counts-only for the first cut? (Rec: counts-only v1; titles behind a follow-up once privacy/verbosity is considered.)
4. `current_task` removal timing — deprecate now, remove after canvas migration.

## Rollout
Additive and dark-safe: the `workload` field is optional; peers that don't report it simply omit it, and `list_peers`/digest degrade to the current `active_tasks`-only view. No migration needed. Build staged: (a) runtime reports summary, (b) core relays + `list_peers` carries it, (c) digest folds it in, (d) deprecate `current_task`.
