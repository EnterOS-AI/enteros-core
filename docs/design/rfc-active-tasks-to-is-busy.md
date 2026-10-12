# RFC: Replace the `active_tasks` count with a self-healing `is_busy` signal

**Status:** proposed (awaiting CTO sign-off to build) · **Author:** CEO-assistant · **Date:** 2026-07-15

## 1. Motivation

`workspaces.active_tasks` is a demo-era mechanism that has quietly become
load-bearing infra, and it is both **meaningless as a count** and **fragile as a
signal**. Two facts, each verified in the current tree:

**The count carries no information — because the runtime is serial, not because
the cap is fixed.** `max_concurrent_tasks` is a *configurable* per-workspace
column (default `DefaultMaxConcurrentTasks = 1`,
core/`workspace-server/internal/models/workspace.go:13`; but its own doc comment
at `:270` notes "Leaders typically set 3", and both create paths persist it —
`workspace.go:724`, `org_import.go:191`). So the cap is not universally 1. What
*is* universal is that **no maintained runtime ever holds more than one turn in
flight**: the A2A handler is serialised on the live SDK turn
(runtime/`molecule_runtime/runtime_inbox.py:8`) and admission is gated on a single
boolean `turn_in_flight` (runtime/`molecule_runtime/a2a_executor.py:453`). A
second dispatched turn is rejected/queued, never run concurrently. Therefore the
runtime-reported `active_tasks` is only ever `0` or `1` **regardless of the
`max_concurrent_tasks` value**, and every consumer's arithmetic
(`active_tasks < max_concurrent`, `>= max_concurrent`, `== 0`) is already a
boolean in disguise. Collapsing to a boolean does not just simplify the signal —
it removes a `max_concurrent > 1` knob that already delivers no concurrency
against serial runtimes (a leader configured at 3 has the drain *over-dispatch*
three turns to a runtime that bounces two of them; see §2.2).

**The signal strands stuck-high.** `active_tasks` is a pure agent self-report:
the runtime increments it on turn-start and decrements it in a `finally` on
turn-end (core writes it verbatim, never computes it):

- runtime counter: runtime/`molecule_runtime/executor_helpers.py:241-248`
  (+ `shared_runtime.py:195-202`), set at `a2a_executor.py:367` (start) / `:674`
  (finally end), shipped in the heartbeat body
  (runtime/`molecule_runtime/heartbeat.py:234-241`).
- core writes it wholesale each beat: `active_tasks = $4`
  (core/`workspace-server/internal/handlers/registry.go:1460`).

When a turn crashes (MiniMax timeout, OOM, host overload) the `finally` decrement
never runs and the counter is pinned `>0` **forever**. This is a known, recurring
class — the JRS agent held `active_tasks=1` while dead for ~2.5h
(see `rfc-agent-liveness.md` §1), and the runtime comments cite stuck-at-1
bugs #1372 / #2026. The evidence that this is real: core carries an entire
**phantom-busy sweeper** whose only job is to un-stick the value
(as of scheduler P4 it is core/`workspace-server/internal/handlers/phantom_busy_sweeper.go`;
it was previously `internal/scheduler/scheduler.go:901-950`),
plus a Prometheus metric to measure the leak rate
(core/`workspace-server/internal/metrics/metrics.go:87-99`).

Because every consumer that reads `active_tasks > 0` as "busy" trusts that
stranding-prone value, one root cause surfaces as **four symptoms**:

| Symptom | Where | Caused by |
|---|---|---|
| Hibernate false-409 | core/`.../workspace_restart.go:546-568` | a stranded counter refuses a legitimate hibernate |
| Idle-digest "arms but never fires" (#94) | runtime/`.../main.py:703-705` (`if heartbeat.active_tasks > 0: continue`) | a stranded counter means the loop never sees idle |
| Phantom-busy sweeper + metric exist at all | core/`.../scheduler.go:901-950` | janitor for the stranding |
| Hibernate/queue wake race (#124) | ephemeral gate run 512020 | a queued A2A turn survives force-hibernate and re-wakes the ws |

The idle-digest point is the important one: the newer idle-prompt consolidation
feature is **not** a replacement for `active_tasks` — it is a *reader* of it
(runtime/`molecule_runtime/main.py:703`), and it is the mechanism's most visible
victim. Fixing `active_tasks` fixes #94 for free.

**Goal:** a single, boolean, **self-healing** `is_busy` signal that reflects
whether the agent is actually running a turn (including its tool calls), that
physically cannot strand, and that every consumer reads instead of the count.

## 2. Design

### 2.1 The signal: runtime-reported boolean, core-applied TTL

- **Runtime** reports a boolean `is_busy` in the heartbeat, sourced from the
  existing per-turn in-flight flag `turn_in_flight`
  (runtime/`molecule_runtime/runtime_inbox.py:134`, set around the whole turn in
  `a2a_executor.py`), which wraps the entire turn **including its tool calls** —
  this is the "robust to actual tool calls" property. It replaces the
  increment/decrement counter entirely (no more `finally`-decrement to miss).
- **Core** never trusts a bare `is_busy`. It computes the effective signal as:

  ```
  effective_busy = heartbeat.is_busy AND (now - last_heartbeat_at < BUSY_TTL)
  ```

  A crashed agent stops heartbeating, so within `BUSY_TTL` its `effective_busy`
  decays to false on its own — **no sweeper, no stranding**. This is the whole
  fix: the busy state can only be true while the agent is both claiming a turn
  *and* actively heartbeating.

- **Backstop (already exists):** the `rfc-agent-liveness.md` L3 stall-watchdog
  (busy **AND** `last_activity_at` stale → probe → restart) remains the recovery
  path for an agent that is genuinely wedged mid-turn. Under this RFC its
  predicate changes from `active_tasks > 0` to `effective_busy`
  (core/`workspace-server/internal/handlers/stall_watchdog.go:230`); its real
  discriminator (`last_activity_at`) is unchanged.

### 2.2 Consumer migration (mechanical — all four collapse to boolean)

| Consumer | Today | After |
|---|---|---|
| Hibernate 409 guard + atomic claim | `active_tasks > 0` / `= 0` (core/`.../workspace_restart.go:546-568,637-649`) | `effective_busy` / `NOT effective_busy` |
| a2a-queue drain gate | `payload.ActiveTasks < maxConcurrent` (core/`.../registry.go:2257`) | `NOT is_busy` (release the one queued turn) |
| Native cron scheduler | `active_tasks < max_concurrent_tasks` (core/`.../scheduler.go:367-377,417-453`) | `NOT effective_busy` (defer tick) |
| Idle-prompt loop | `heartbeat.active_tasks > 0` (runtime/`.../main.py:703`) | `heartbeat.is_busy` |
| Idle auto-hibernate | `active_tasks = 0 AND heartbeat stale` (core/`.../registry/hibernation.go:66-77`) | `NOT effective_busy AND heartbeat stale` (the staleness term already dominates) |
| Stall watchdog | `active_tasks > 0 AND last_activity stale` (core/`.../stall_watchdog.go:230`) | `effective_busy AND last_activity stale` |
| Request-nudge sweeper | `active_tasks = 0` (core/`.../request_nudge_sweeper.go:203`) | `NOT is_busy` |
| Wedged-agent monitor | `active_tasks > 0 + stale` (core/`.../registry/wedged_agent.go:95,183`) | `effective_busy + stale` — or **fold into** the L3 stall-watchdog (they overlap) |

### 2.3 Deletions

- **Phantom-busy sweeper** and its metric (core/`.../metrics.go:87-99`) —
  obsolete once busy self-heals via TTL. Keep the metric name reserved for one
  release as a canary that decay-to-false works.
  **Note (sequencing):** the scheduler-as-trigger-plugin RFC's P4 already
  *relocated* this sweeper out of the (now-deleted) `internal/scheduler` package
  into its own worker, `core/workspace-server/internal/handlers/phantom_busy_sweeper.go`
  (with `metrics.PhantomBusyResets()`), precisely because `active_tasks` is still
  load-bearing today. That relocation and this deletion are compatible in order:
  P4/P4b keep the sweeper while `active_tasks` exists; this RFC then removes
  `active_tasks` and the sweeper *together* once `is_busy` self-heals. Land this
  RFC after P4, and delete `phantom_busy_sweeper.go` (not `scheduler.go`, which is
  already gone).
- **`current_task`** string (heartbeat + column) — vestigial (already flagged in
  the workload-visibility RFC). Drop with `active_tasks`, or keep read-only for
  display if the canvas still wants "what is it doing" text (decide in §7).

### 2.4 #124 — force-hibernate purges the queue

Once `effective_busy` is trustworthy, the hibernate/queue race is a small,
correct fix: `POST /hibernate?force=true` must **purge or tombstone pending
A2AQueue items** for the workspace (in the same claim txn that stops the
container), so the drain loop cannot redeliver a queued turn and re-wake a
just-hibernated workspace. This unblocks the task #92 e2e coverage — the
force-on-busy assertion becomes stable because nothing re-wakes the ws.

## 3. Data model

- Heartbeat payload: replace `active_tasks int` with `is_busy bool`
  (core/`workspace-server/internal/models/workspace.go:116`;
  runtime/`molecule_runtime/heartbeat.py:234-241`).
- Column: `ALTER TABLE workspaces` — add `is_busy BOOLEAN DEFAULT false`; keep
  `active_tasks` for one release (dual-write, see §5) then drop it and the
  `(status, active_tasks)` partial index (replace with `(status, is_busy)` if the
  auto-hibernate query benefits). `last_heartbeat_at` / `last_activity_at` already
  exist.
- No new tables.

## 4. Config / defaults (override any)

| Knob | Default | Notes |
|---|---|---|
| `BUSY_TTL` (core) | 90 s | ≈ 3× heartbeat interval; the self-heal floor |
| heartbeat interval (runtime) | unchanged | `is_busy` rides the existing beat |

`BUSY_TTL` is deliberately short relative to the L3 stall-watchdog window (12 min)
— TTL prevents *decision-time* false-positives (hibernate/cron/idle), L3 handles
*genuine* mid-turn wedges.

## 5. Phasing (each = SOP PR → 2-genuine review → merge; cross-repo, compat-gated)

The rollout must survive a mixed fleet (old runtime images + new core, and vice
versa). Core reads both signals during the window.

- **B1 (core, compat-in):** core accepts `is_busy` if present, else derives it
  from `active_tasks > 0`. Add `is_busy` column, dual-read. No behavior change
  yet. Safe on old runtimes.
- **B2 (runtime):** runtime computes `is_busy` from `turn_in_flight` and sends
  **both** `is_busy` and `active_tasks` (dual-write) for the transition. Ship on a
  new image; re-provision fleet.
- **B3 (core, cut over):** consumers switch to `effective_busy` (TTL applied);
  add the force-hibernate queue purge (#124). Delete the phantom-busy sweeper.
- **B4 (cleanup):** once the fleet is all ≥B2 images, runtime stops sending
  `active_tasks`; core drops the column + index + `current_task`. Retire the
  reserved metric.

## 6. Interactions & non-goals

- **vs. `rfc-agent-liveness.md`:** complementary and additive — this RFC swaps the
  *point-in-time busy* signal that L3 already consumes; `last_activity_at` and the
  probe→restart machinery are unchanged. L3 remains the backstop for a wedged
  mid-turn agent that TTL alone wouldn't restart.
- **vs. the workload-visibility RFC (PR#4400/#4401):** that RFC kept
  `active_tasks` as a display "load gauge." At `max_concurrent=1` the gauge is a
  boolean, so `is_busy` supersedes it for display too; `current_task` was already
  deemed vestigial there. This RFC assumes the gauge is dropped — confirm in §7.
- **vs. the scheduler-as-trigger RFC:** the cron capacity gate
  (core/`.../scheduler.go:417-453`) moves to `effective_busy` regardless of
  whether the scheduler stays native or becomes a trigger plugin; the signal
  contract is the same either way.
- **Non-goals:** not adding real multi-turn concurrency (if we ever want
  `max_concurrent > 1`, that is a separate design that would reintroduce a count —
  called out in §7). Not changing heartbeat cadence or the activity-log path.

## 7. Open questions / risks

1. **`max_concurrent > 1` ever?** Collapsing to a boolean forecloses per-workspace
   turn parallelism. No maintained runtime supports it and none is planned; if
   that changes, `is_busy` would need to become `in_flight_count` again. Confirm
   we are comfortable closing that door for now.
2. **Source of `is_busy` on the runtime — the load-bearing correctness risk, not
   a footnote.** `turn_in_flight` is per-context and in-process; it must be set
   for **every** turn path (external A2A, self-sent idle-prompt, cron/self-scheduler
   trigger), not just external `message/send`, or a self-driven turn reads
   `is_busy=false` mid-run and a hibernate/cron-fire decision races it. This is the
   one place the swap can silently regress, so it must be **enforced by a test**
   asserting every executor turn entrypoint sets `turn_in_flight` (a negative
   control that fails if a new turn path forgets it) — not left to code review. If
   any path legitimately cannot set it, `is_busy` must OR that path in explicitly.
3. **`current_task` display:** drop entirely, or keep a read-only "what's it
   doing" string for the canvas? Leaning drop.
4. **`BUSY_TTL` value:** 90s assumes a ~30s beat. If any runtime beats slower,
   raise TTL so a slow-but-alive agent mid-turn isn't briefly read idle (which
   would only ever cause an *early* hibernate/cron-fire, not data loss — but still
   worth tuning to the real cadence).
5. **Rollout ordering:** B1 must land and deploy before B2 images reach the fleet,
   or a new-runtime `is_busy` is ignored by old core (fails safe — old core just
   keeps reading `active_tasks`, which B2 still dual-writes). Low risk, but the
   deploy order matters.
