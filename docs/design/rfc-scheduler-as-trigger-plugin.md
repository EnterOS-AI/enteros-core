# RFC: Scheduler as a trigger daemon plugin (decouple the cron engine from core)

**Status:** Implemented — additive delivery + CI gates merged (all code phases through P5; delivery linchpin core#4408). The legacy `workspace_schedules` table DROP (P4b) and the operational rollout remain owner-gated (fleet runtime pins, prod backfill `?apply=true`, `E2E_SCHEDULER_CHECK` flip) — tracked in [issue #4411](https://git.moleculesai.app/molecule-ai/molecule-core/issues/4411). Author: CEO Assistant (on CTO direction 2026-07-14).
**Delivery state (2026-07-15):** all code phases through P5 are **merged** (P0 cron lib: core `internal/cronspec` + runtime#298; P1/P2/P3a-c: runtime#300 + SDK; P3-live: core#4398; P4 loop retirement: core#4399; P5 delivery: core#4408 `60fdebf6`); what remains is P4b (store retirement), a handful of P3 re-points (§8A P3), and the **operational rollout — tracked in [issue #4411](https://git.moleculesai.app/molecule-ai/molecule-core/issues/4411)** (owner-gated: fleet pins, prod backfill `?apply=true`, `E2E_SCHEDULER_CHECK` flip).
**Related:** [`rfc-platform-mcp-as-plugin.md`](rfc-platform-mcp-as-plugin.md) (same "core capability → plugin channel" move for the management MCP), [`rfc-decouple-config-skill-delivery.md`](rfc-decouple-config-skill-delivery.md) (the persisted-volume asset channel this reuses), ADR-005 (SDK-owns-adapter socket; core carries zero runtime-behaviour code).
**Repos touched:** molecule-core (thin), molecule-ai-workspace-runtime (daemon socket + host), molecule-ai-sdk (contract + template).

## 1. Summary & the decision requested

At drafting time we shipped a mature native scheduler — but it lived **inside** the workspace-server core: a single core-wide `robfig/cron` loop (`internal/scheduler/scheduler.go`, 1207 lines — deleted by core#4399) polled the `workspace_schedules` table every 30 s and fired A2A turns into every workspace. This RFC moves the **trigger engine** out of core into a per-workspace **`kind: trigger` daemon plugin** that runs in the workspace runtime, owns the cron clock, and injects `self-scheduler` turns locally. Core keeps only a thin schedule store + the existing CRUD API and **defers firing** to any workspace that runs the plugin.

This serves all four drivers the CTO named: **decouple core** (ADR-005, formerly numbered ADR-004 — the scheduler is the last big core-baked runtime behaviour), **third-party extensibility** (anyone can author a trigger plugin), **plugin-model consistency** (skills/MCP/channels are plugins; the scheduler shouldn't be special), and **per-workspace portability** (schedules travel on the workspace volume, not a central table).

**Decided (CTO, 2026-07-14):** kind = **`trigger`** (a scheduler is the first *trigger* type; webhook/event pokes are another — one kind, one inject lane, one governance path); state ownership = **Option A, the daemon owns everything** (retire `workspace_schedules`; Canvas/admin/webhooks move to a runtime-exposed API — staged delivery, but A is the destination). Remaining open items are in §10.

### 1.1 A correctness fix this move delivers: scheduled turns become *system* self-turns, not fake *user* messages

> **Historical (delivered):** the fix below SHIPPED — the trigger daemon stamps `source_type=self-scheduler` (runtime#300), and the core engine this section critiques was deleted in core#4399 (2026-07-15). Kept as the design rationale.
Today core fires the scheduled turn as an A2A message with `role: "user"` and **no `source_type`** (`scheduler.go:408`; there's an abandoned `// "system:scheduler" was invalid — source_id is UUID` workaround comment right beside it). So every cron tick **impersonates the user**. That is not an oversight — it is the direct symptom of the engine living in Go core, *outside* the runtime's autonomous-self-turn taxonomy (`kernel.py`: `KIND_IDLE/self-idle`, `KIND_DELEGATION_RESULT`, `KIND_HARVESTER`, `KIND_LIFECYCLE_WAKE`, … all stamped via `autonomous_metadata()` so they are loop-guard-governed; scheduler is the lone holdout). Consequences: (1) the agent reads a timer as "the user asked"; (2) the turn **bypasses the autonomous-loop runaway guard** that protects idle turns; (3) it pollutes the D3 user-origin marker (the task-queue provider prioritises user-origin asks — a scheduled turn falsely tagged user jumps that queue). Moving the trigger into the runtime routes it through `kernel.autonomous_metadata(KIND_SCHEDULER)` → `source_type = self-scheduler` → a correctly-attributed, guard-governed **system self-turn**. "Make it a plugin" and "make it system, not user" are the *same fix*.

## 2. What existed before the move (grounding, not proposal — written 2026-07-14; the engine below was deleted by core#4399)

- **Engine:** `workspace-server/internal/scheduler/scheduler.go` (now deleted) — 30 s poll, per-schedule 5-field cron + timezone, `maxConcurrent=10`, phantom-busy sweep, DB-op deadlines, 3-layer liveness, auto-disable after 3 consecutive SDK errors, `stale` after 3 empty runs.
- **Store:** core Postgres `workspace_schedules` (5 migrations). Columns split cleanly into **definition** (`name, cron_expr, timezone, prompt, enabled, source`) and **engine-state bookkeeping** (`next_run_at, last_run_at, run_count, last_status, last_error, consecutive_empty_runs, consecutive_sdk_errors`).
- **API:** 7 Canvas routes (`schedules.go`: List/Create/Update/Delete/RunNow/History + peer Health), admin health/orphan-reap (`admin_schedules_health.go`), template seeding (`template_schedules.go` + `org.go` `orgImportScheduleSQL`).
- **Content:** 11 org-template agents ship `schedules:` YAML + Markdown prompt bodies; seeded with `source='template'` (additive-only upsert; `source='runtime'` edits survive re-provision).
- **Delivery today:** core → `ProxyA2ARequest` (idle) or `EnqueueA2A` (busy, serial drain, schedule-ID idempotency) → workspace runtime.
- **The runtime receiving half already exists but is unused:** `KIND_SCHEDULER` → `source_type="self-scheduler"` (`kernel.py`, `a2a_executor.py`) — defined, classified as a routine self-turn, governed by the autonomous-loop guard, **but has zero producers**. Cron ticks originate platform-side.

### 2.1 The three hard couplings (why this isn't a lift-and-shift)
1. **Core-wide singleton loop, not per-workspace** — one query across all workspaces, global concurrency ceiling. A per-workspace daemon inverts the model.
2. **Writes core-owned tables** — the phantom-busy sweep (`UPDATE workspaces SET active_tasks=0`) and `activity_logs`; the capacity check reads `workspaces.active_tasks`. The scheduler doubles as a task-lifecycle janitor.
3. **Busy-buffering uses in-process `EnqueueA2A`** (serial drain + idempotency) with no external API.

**Key insight:** couplings #2 and #3 are artifacts of the scheduler being a *separate core process reaching into* the workspace. Run the trigger **inside the runtime** and they dissolve: the runtime already knows its own idle state (`heartbeat.active_tasks`, the signal the idle-digest loop gates on) and already serializes its own turn queue. The phantom-busy janitor exists only to repair cross-process staleness that in-process code never creates.

## 3. Goals / non-goals

**Goals**
- The cron clock + fire loop run in a **per-workspace daemon plugin**, not core.
- **Third parties can author trigger plugins** against an SDK-owned contract.
- Schedules **travel with the workspace** (portability), seeded via the existing template→persisted-volume reconcile channel.
- **Zero double-fire** during migration; workspaces without the plugin keep working unchanged.
- **Trigger turns are system self-turns** (`source_type=self-scheduler`, loop-guard-governed), never `role: "user"` — fixing the impersonation described in §1.1.
- Preserve every invariant in §7.

**Non-goals**
- Re-designing the schedule *semantics* (cron grammar, timezone, prompt body) — unchanged.
- Changing the Canvas UX. The 7 routes stay; only their backing owner may move (§6).
- Building second-accurate firing. The runtime idle/tick cadence is minutes-grained and that's fine.
- A marketplace listing for the scheduler plugin (later; see rfc-marketplace-delivery).

## 4. Target architecture

```
                        ┌─────────────────────── workspace runtime container ──────────────────────┐
  template repo ──seed──▶ /configs/…/schedules  (persisted volume, reconcile-on-boot)              │
                        │        │                                                                  │
                        │        ▼                                                                  │
                        │  ┌─ trigger daemon plugin (kind: trigger) ─┐   supervised subprocess      │
   core workspace_      │  │  • reads schedule grid from volume       │   (DaemonSupervisor:         │
   schedules  ──sync?──▶│  │  • owns cron clock (own parser)          │    spawn/backoff/breaker)    │
   (thin store, CRUD)   │  │  • fires ONLY when idle (active_tasks=0) │                              │
                        │  │  • injects self-scheduler turn ──────────┼──▶ local A2A lane ──▶ agent  │
                        │  └──────────────────────────────────────────┘   (generalized from         │
                        │             claims provides_native_scheduler      kind:channel socket)     │
                        └───────────────────┬──────────────────────────────────────────────────────┘
                                            │ heartbeat: provides_native_scheduler=true
                                            ▼
                        core scheduler loop  ──  NativeSchedulerCheck(wsID) → skip poll-and-fire
                        (HISTORICAL — cutover control during P1–P3; BOTH deleted in core#4399)
```

The plugin is a supervised subprocess (the daemon lifecycle — spawn, exponential backoff, 10-fast-failure circuit breaker, SIGTERM→SIGKILL teardown — is production-ready and kind-agnostic today). It runs the clock and, on a due schedule, injects a `self-scheduler` turn through a local A2A lane. Core's `NativeSchedulerCheck` seam **made** the central loop defer for any workspace advertising native scheduling — the incremental cutover control during P1–P3 (both the loop and the seam were deleted in core#4399; the fire path is now 100% plugin-owned).

## 5. The seams that must be built (the honest cost)

The daemon *lifecycle* exists; the *inbound-to-agent* half is hard-wired to `kind: channel`. Six concrete gaps, each a scoped build item:

| # | Gap | Fix | Repo |
|---|---|---|---|
| G1 | **No inject lane for non-channel daemons.** The local A2A socket + capability token are handed only to `kind: channel` specs; a `kind: trigger` daemon has no socket, no token, no `message/send` lane. | Generalize `ChannelEventSocketManager` to a **kind-independent local A2A lane** with a `source_type` allow-list (a trigger plugin may stamp only `self-scheduler`). This is the crux enabler. | runtime |
| G2 | **A daemon can't claim `provides_native_scheduler`.** It's an *adapter* capability today (reported via heartbeat); a plugin has no way to flip it, so core would double-fire. | Let a loaded trigger plugin set the capability (manifest-declared or daemon-reported → folded into the heartbeat capability payload). | runtime + core |
| G3 | **No durable per-workspace schedule state for a daemon.** Daemons aren't pointed at the persisted mailbox volume. | Hand the daemon a durable state dir on the persisted volume (schedule grid + engine bookkeeping). | runtime |
| G4 | **No cron parser in the runtime.** `cron_expr` math lives only in Go (`ComputeNextRun`). | Daemon brings a cron evaluator; extract Go `ComputeNextRun` to a shared lib (still needed by core CRUD validation) and mirror its semantics in an SDK-owned cron contract so both sides agree. | sdk + core |
| G5 | **No scheduler-kind template or manifest schedule block.** Templates exist only for channel/skill/mcp; the manifest can't express a cron grid. | Add a `kind: trigger` SDK scaffold + a `contributes.schedules` (or equivalent) manifest block, SSOT-validated. | sdk |
| G6 | **No daemon health/liveness contract.** A circuit-broken scheduler goes silently `failed` until next boot. | Surface daemon state to the agent/platform (a "scheduler up/last-tick" signal) so a dead trigger is observable, matching today's `schedules/health`. | runtime + core |

`kind: trigger` is already schema-legal (`kind` is an open string, "so new kinds are additive") — no schema change needed to *name* it; the work is the runtime behaviour behind it.

## 6. THE decision: schedule state ownership

Where does the schedule definition + bookkeeping live after the engine moves? Three options, with the blast radius the investigation measured:

**Option A — Daemon owns state on the volume (max portability).**
Schedule grid + bookkeeping live on the workspace's persisted volume; the daemon is authoritative. Canvas CRUD, admin-health, and the webhook `next_run_at=NOW()` trigger re-point to a runtime-exposed API (proxied through the platform). *Pro:* schedules truly travel with the workspace; cleanest ADR-005 (SDK-owns-adapter)/portability story. *Con:* biggest surface — 7 Canvas routes + admin + webhooks + org-import all move; the `workspace_schedules` table is retired.

**Option B — Core keeps the store; daemon owns only the clock (min blast radius).**
`workspace_schedules` + all CRUD/webhooks/admin/seeding stay exactly as they are. The daemon reads its workspace's rows via a small core read-API and does the firing locally; core stops firing via `NativeSchedulerCheck`. *Pro:* smallest change, every invariant untouched, Canvas/webhooks/admin work verbatim. *Con:* state does **not** travel (still central Postgres) — fails the portability driver; least "plugin-like" (the plugin is a firing agent, not a self-contained capability).

**Option C — Volume is authoritative, core mirrors (portability + working UI). [recommended]**
The schedule *definition* travels on the volume (seeded via the existing reconcile-on-boot channel, editable by the daemon). Core keeps a **thin read-through mirror** of `workspace_schedules` that the daemon syncs, so Canvas/admin/webhooks keep working against core unchanged while the volume is the source of truth. *Pro:* portability + third-party + zero Canvas/webhook rewrite. *Con:* a sync path to keep consistent (volume ⇄ core mirror), the classic dual-write care.

**CTO decision: Option A is the destination — the daemon owns everything, `workspace_schedules` is retired, and Canvas/admin/webhooks move to a runtime-exposed API.** Delivery is still **staged** (A is where we land, not a big-bang): the phasing in §8 proves the hardest new seam (local inject for a non-channel daemon) with core state untouched *first*, then moves storage + the API surface, so no single PR moves all seven routes + webhooks + seeding at once. The staging is a rollout tactic; unlike the earlier B/C options, we do **not** leave state in core as a permanent mirror — the end state is volume-authoritative with a runtime API, and the core table is deleted in P4.

## 7. Invariants to preserve (regression contract)

1. **Canvas 7 routes + JSON shapes** stay stable (List/Create/Update/Delete/RunNow/History + peer Health).
2. **Seeding contract**: `source=template|runtime`, additive-only upsert, `(workspace_id,name)` uniqueness, runtime edits survive re-provision, FK-CASCADE cleanup on workspace delete.
3. **Webhook event-trigger** (`webhooks.go` sets `next_run_at=NOW()` on named schedules) — schedules are event-poked, not purely time-based; the daemon must honor an out-of-band "fire now."
4. **Per-template caps** (`maxTemplateSchedules=100`, cron ≤128 chars, prompt ≤16 KiB, config ≤1 MiB) — hostile-template DoS bounds.
5. **No scheduled CI** — the `lint_schedule_budget` zero-cron ratchet stays green; exercise the new scheduler via `pull_request`/`push`/`workflow_dispatch`, never `on.schedule`.
6. **No double-fire** — `NativeSchedulerCheck` must gate core firing before any workspace's daemon goes live.
7. **Busy behaviour** — a schedule that comes due while the agent is mid-turn must not be dropped; in-process the runtime turn queue serializes it (replacing core's `EnqueueA2A` buffer).
8. **Auto-disable / stale semantics** (3 SDK errors → disable; 3 empty → stale) carry into the daemon.
9. **System provenance** — every trigger-fired turn carries `source_type=self-scheduler` (loop-guard-governed) and is **never** delivered as `role: "user"`. A regression test asserts a scheduled turn is classified as a routine self-turn and does not appear as a user-origin task-queue row (§1.1).

## 8. Phasing (each an independently reviewable increment)

- **P0 — shared cron lib (G4).** Extract Go `ComputeNextRun` to a tiny shared package (core CRUD still needs it); write the SDK cron contract; land a Python cron evaluator that matches it byte-for-byte on a shared fixture set. *No behaviour change.*
- **P1 — the inject lane (G1) + native-scheduler claim (G2).** Generalize the local A2A lane to a `source_type`-allow-listed, kind-independent socket; let a trigger plugin claim `provides_native_scheduler`. Prove a subprocess can deliver a `self-scheduler` turn to the agent and that core defers. *Behind a flag; no schedule content yet.*
- **P2 — the trigger daemon + SDK scaffold (G3, G5, G6).** The daemon: run the clock, fire-when-idle **as a `self-scheduler` system turn**, honor webhook pokes, durable bookkeeping on the volume, health surfaced. SDK `kind: trigger` template. To de-risk the *storage* move independently of the *firing* move, P2's daemon may transitionally read the existing core rows (read-only) while the inject path is proven on a canary; core defers for that workspace via `NativeSchedulerCheck`; assert fire-parity with the old core loop **and** that the turn now arrives `source_type=self-scheduler`, not `role:user`.
- **P3 — storage move to the volume (reach Option A).** Schedule definition + bookkeeping become volume-authoritative (seeded via the reconcile-on-boot channel, written by the daemon). Stand up the **runtime-exposed schedule API**; re-point Canvas's 7 routes, admin health/orphan-reap, and the webhook event-poke at it (through the platform proxy). `workspace_schedules` reads/writes cease.
- **P4 — retire core.** Once every scheduled workspace carries the plugin (see P5): delete `scheduler.go`'s loop **and** drop the `workspace_schedules` table + its migrations/handlers (Option A end state — no permanent mirror). Keep only `ComputeNextRun` as the shared cron lib (P0). *P4's loop-retirement landed in #4399; the table drop is P4b, gated on the P5 delivery + backfill being universal.*
- **P5 — per-workspace delivery (the linchpin P4 assumed).** P4 retired the loop on the premise that "the plugin is present," but **nothing installed it** — no template, runtime builtin, or repo declared it, so post-#4399 scheduled turns fire nowhere fleet-wide. Delivery is **per-workspace** (CTO, 2026-07-14 — a scheduler is a plugin, and plugins are per-workspace; it is *not* baked into every runtime image): a workspace that has (or gains) a schedule **declares** the plugin into `workspace_declared_plugins` → it installs via `MOLECULE_DECLARED_PLUGINS` at boot or the transition-to-online reconcile, and an already-running workspace is **hot-armed** without a restart. Three artifacts: the plugin repo (**P5a**), core delivery + backfill (**P5b**), the runtime hot-start endpoint (**P5b-rt**), proven by an autonomous-fire e2e (**P5c**). This is what makes P4's precondition true.

## 8A. Delivery checklist (Definition of Done)

Each phase is DONE only when its **Build · Tests/CI · e2e · Cleanup · Docs** rows are all checked. Cross-cutting DoD at the end applies to every phase.

### P0 — Shared cron lib (G4)
- **Build:** extract Go `ComputeNextRun` + cron parse/validate into a standalone shared package (`internal/cronspec`); author the SDK cron contract (5-field grammar, timezone semantics, next-run algorithm) as the SSOT; land a Python cron evaluator (vendored/pinned) in the runtime that mirrors it.
- **Tests/CI:** a **shared cross-language fixture set** (`cron_expr × timezone × base_time → expected next_run`) asserted **identically** by Go and Python (the equivalence gate — a drift here is a silent mis-fire); `check-schemas-in-sync` drift gate extended to the cron contract; Go+Python unit CI green.
- **e2e:** none (no behaviour change).
- **Cleanup:** repoint all 4 core call sites (`schedules.go`, `template_schedules.go`, `admin_schedules_health.go`, `org_import.go`) at the shared package; delete the inline copy in `scheduler.go`.
- **Docs:** cron contract documented in `sdk/contracts/` + PROVENANCE; note in testing-strategy that cron math is now a shared tier-4 lib.

### P1 — Inject lane (G1) + native-scheduler claim (G2) — **DELIVERED (runtime#300, which superseded runtime#299)**
- **Build:** generalize `ChannelEventSocketManager` to a kind-independent local A2A lane with a `source_type` allow-list (a `trigger` plugin may stamp **only** `self-scheduler`); wire a trigger plugin's `provides_native_scheduler` claim into the heartbeat capability payload; core `NativeSchedulerCheck` reads it. Behind a flag.
- **Tests/CI (runtime):** a non-channel daemon opens the lane and injects a `self-scheduler` turn; **negative controls** — a daemon CANNOT forge `role:user`, a channel `source`, or a user-origin marker, and the allow-list rejects any non-`self-scheduler` `source_type`; the injected turn is governed by `should_halt` (loop guard).
- **Tests/CI (core):** `NativeSchedulerCheck` true ⇒ `tick()` skips fire for that workspace (state-machine test, both arms).
- **e2e:** ephemeral-CP gate sub-step (flag-gated) asserts the lane arms on a canary; distinct fail arms (no-arm-line vs armed-never-injected).
- **Cleanup:** none (nothing retired yet).
- **Docs:** document the trigger inject lane + `source_type` allow-list; update the autonomous-self-turn taxonomy doc to add the scheduler producer.

### P2 — Trigger daemon + SDK scaffold (G3, G5, G6) — **DELIVERED (sdk#·runtime re-vendor), except the live e2e**
- **Build:** ✅ **DONE.** Reference trigger daemon `templates/trigger/scheduler.py` — cron clock (real `molecule_runtime.cronspec`, lazy-imported), `self-scheduler` turn via the trigger client, durable bookkeeping on the persisted volume (atomic temp-file+`os.replace`), health heartbeat file, per-tick error isolation (one bad cron never wedges the daemon or its siblings). SDK `kind: trigger` template (`plugin.yaml` daemon + `schedules.yaml` grid) scaffolds + validates.
  - **Design decision — idle-gating delegated to the executor, not the daemon.** The RFC drafted `fire-only-when-idle` as a daemon `active_tasks==0` check. Instead the daemon fires unconditionally when due and the **executor's routine-self drop-not-queue** governs idleness (a `self-scheduler` turn that arrives mid-turn is dropped, never queued/interrupting). This is the same idle-gate the idle-digest and cron self-pings already use — one gate, no daemon-side TOCTOU race. Consequence: a schedule due while the agent is busy is dropped for that tick (fires next due time), not buffered as the old core `EnqueueA2A` loop did; consistent with the §1.1 routine-self taxonomy.
  - **Not yet built (fold into P3, they need the runtime schedule API):** webhook event-poke honoring; auto-disable@3-SDK-errors / stale@3-empty counters (the old core engine-state bookkeeping) — deferred until the grid is volume-authoritative and the daemon owns that state.
- **Tests/CI (runtime + SDK):** ✅ **DONE.** Pure decision core (`trigger_schedule.evaluate_tick`) unit-tested — arm-without-boot-fire (negative control), fire-once-and-rearm, not-yet-due, disabled-disarm, bad-cron isolation; **integration vs the real `cronspec` engine** — hourly arm→fire→rearm, no thundering catch-up, DST spring-forward gap skipped. Trigger **client** contract tested — autonomous `self-scheduler` provenance (never `role:user`), client-supplied `source` stripped, capability-absent known-safe, post-delivery failure → `DeliveryUnknown`/never-replay. Scaffold generates + validates + its tests pass. SSOT drift fixed: `TRIGGER_*` constants relocated to the SDK source, vendor gate byte-identical.
- **e2e (the big sub-step) — ⏳ REMAINING:** seed a schedule → canary workspace WITH the plugin → short fire interval → assert **(a)** trigger fires, **(b)** turn arrives `self-scheduler`, NOT `role:user`, **(c)** core defers (no double-fire), **(d)** parity. Four distinct fail arms: no-arm / armed-never-fired / fired-wrong-provenance / double-fire. Needs a live canary + the P3 schedule source.
- **Cleanup — ⏳ REMAINING:** `KIND_SCHEDULER` / `A2A_SOURCE_SELF_SCHEDULER` now have a real producer (the daemon) — update the "unwired stub / zero producers" code comments once the plugin ships with a maintained runtime.
- **Docs:** ⏳ SDK trigger-plugin authoring guide + runtime daemon reference still to write; update `project_maintained_runtime_set` when the plugin ships.

### P3 — Storage → volume + runtime API (reach Option A) — **P3-live DELIVERED (core#4398: Canvas CRUD/RunNow re-point + DB→volume `MigrateToVolume`; ScheduleTab e2e core#4397); webhook/admin/History+Health re-points + template seeding REMAINING**
- **Build:** ✅ **P3a DONE — the volume-authoritative store.** SDK `schedule` contract (`contracts/schedule/`) is the grid SSOT — definition-only entries, caps (100 / cron ≤128 / prompt ≤16384 bytes), valid/invalid fixtures. Runtime `molecule_runtime/schedule_store.py` is the validated write side: CRUD on the volume grid file, every write checked against the vendored schema + byte/count caps + `cronspec.validate` (unschedulable cron rejected at *write* time), atomic `os.replace`, `load()` re-validates so an out-of-band corrupt grid never loads silently, `replace_all` for seeding. Grid contract vendored byte-identical + drift-gated.
  - ✅ **P3b DONE — the runtime schedule API.** `molecule_runtime/internal_schedules.py`: `/internal/schedules*` List/Create/Update/Delete/Health over the store, mounted beside the other `/internal/*` platform-forward routes with the same `platform_inbound` forward-auth. Store validation → 400, unknown → 404, unconfigured state dir → 503; Health reads the daemon's health file (grid armed-count fallback pre-first-tick). API + daemon share one `MOLECULE_TRIGGER_STATE_DIR`.
  - ✅ **P3c DONE — RunNow / History / webhook-poke over a file IPC.** `POST /internal/schedules/{name}/run` enqueues a poke (202; 404 unknown, 409 disabled); the webhook event-poke is the same call. `evaluate_tick(poked=…)` fires a poked+enabled schedule immediately regardless of cron and re-arms so it never double-fires; the daemon consumes pokes (deferring only undeliverable ones) and appends a bounded run log that `GET …/history` (optionally per-name) reads. The whole poke→deliver→clear→history loop is covered by a `run_once` scaffold test.
  - ✅ **State-dir injection DONE.** `molecule_runtime/trigger_state.resolve_trigger_state_dir()` is the one resolver both sides use — `MOLECULE_TRIGGER_STATE_DIR` override else `<configs_dir>/schedules` on the persisted volume. The schedule API defaults to it; the channel-events injector sets it into every trigger daemon's env, so the API and daemon provably share the durable per-workspace grid + health + history + poke files (tested).
  - ✅ **P3-live DONE (core#4398, merged 2026-07-15).** Canvas List/Create/Update/Delete/RunNow are proxied to the runtime's volume-backed `/internal/schedules*` API whenever the workspace advertises the `scheduler` capability (`schedules_proxy.go` — `scheduleBackendIsVolume`, incl. the `cron`↔`cron_expr` shape mapping to the existing Canvas JSON contract; `SCHEDULE_VOLUME_PROXY_DISABLED` kill-switch forces the legacy DB path). The DB→volume **data migration** shipped as `POST /admin/workspaces/:id/schedules/migrate-to-volume` (`MigrateToVolume` — copies `source='runtime'` rows, idempotent; see `docs/guides/selfhost-schedule-migration.md`). Canvas `ScheduleTab` e2e landed as core#4397.
  - ⏳ **REMAINING:** re-point Canvas **History + Health** (still read `activity_logs` / `workspace_schedules`), **admin health/orphan-reap** (`admin_schedules_health.go`, still DB), and the **webhook event-poke** (`webhooks.go` still writes `next_run_at=NOW()` to the DB) at the runtime API. **Template seeding to the volume is an open design seam:** core still seeds a template's `config.yaml` `schedules:` block into the legacy DB only (`template_schedules.go` / `org.go`); the runtime's reconcile-on-boot seeding (runtime#303) covers a trigger plugin's *shipped* `schedules.yaml`, not the workspace template's `config.yaml` grid.
- **Tests/CI:** ✅ **P3a store suite** (`tests/test_schedule_store.py`, 11) — every valid/invalid **contract fixture** partition holds (store can't drift from the contract), byte-cap enforced on UTF-8 bytes (multibyte negative control), count/dup caps, unschedulable-cron rejected, CRUD round-trip, corrupt-grid rejected, `load` re-validates persisted entries, `replace_all` atomic (failed validation leaves prior grid intact). ⏳ **REMAINING:** Canvas contract tests (7 routes, **identical JSON shapes**); seeding invariants (`source=template|runtime`, additive-only, runtime edits survive re-provision); webhook poke still fires; **portability** — schedules survive re-provision/move; **data-migration** — existing `workspace_schedules` rows migrate to the volume idempotently with zero loss.
- **e2e:** full-SaaS gate — create → schedule via Canvas API → fires → survives restart → visible in `ScheduleTab`.
- **Cleanup:** core CRUD / seeding / webhooks stop **writing** `workspace_schedules` (reads may linger one release for rollback).
- **Docs:** update `TEMPLATE_ASSET_DELIVERY.md` (schedules travel on the volume); admin runbook (health via runtime API); Canvas API doc; **self-host migration guide** for the DB→volume move.

### P4 — Retire core — **loop retirement MERGED (core#4399, 2026-07-15): `internal/scheduler` deleted, `NativeSchedulerCheck` removed, the fire path is 100% plugin-owned; P4b (retire the `workspace_schedules` store) PENDING — [issue #4411](https://git.moleculesai.app/molecule-ai/molecule-core/issues/4411) item 5**
- **Build:** delete `scheduler.go`'s loop + its `main.go` wiring; drop the `workspace_schedules` table (down-migration) + reduce/delete `schedules.go`, `template_schedules.go`, `admin_schedules_health.go` to the proxy (or remove); remove the phantom-busy sweep **after proving nothing else relied on it** (else relocate the `workspaces.active_tasks` repair to a proper owner); remove the `EnqueueA2A` scheduler-specific usage; delete the abandoned `//"system:scheduler" invalid` comment.
- **Tests/CI:** delete `scheduler_test.go`, `native_scheduler_test.go`, `scheduler_integration_test.go` + the `handlers-postgres-integration.yml` scheduler steps; **anti-regression test** that the deleted scheduler artifacts stay deleted (the SOP-removal pattern); precondition gate — **verify every maintained runtime carries the plugin before deleting**.
- **e2e:** the P2/P3 gates now run against the plugin-only path (core loop gone); no double-fire arm is now vacuous (core can't fire) — assert that explicitly.
- **Cleanup:** purge dead constants (`priorityTask` dup, `pollInterval`, `batchLimit`), unused `events.EventCron*`, `metrics.TrackPhantomBusyReset`; drop scheduler from the core tier-3 package list.
- **Docs:** mark **this RFC IMPLEMENTED**; update **ADR-005 (SDK-owns-adapter, formerly ADR-004)** ("scheduler decoupled — core carries zero scheduler runtime code"); `workspace-runtime.md`; `testing-strategy.md` (remove `scheduler` from the workspace-server packages table); CHANGELOG; retire the old scheduler docs.

### P5 — Per-workspace delivery (closes the P4 linchpin) — **ALL MERGED via core#4408 (`60fdebf6`, 2026-07-15), incl. the P5b backfill endpoint + P5c e2e sub-step 10d (gate default-off)**
- **Build:**
  - ✅ **P5a DONE — the plugin artifact.** `molecule-ai-plugin-scheduler` repo (`plugin.yaml` `kind: trigger` + daemon `scheduler`, empty `schedules.yaml` — it ships the *daemon*, not preset schedules), tagged **v0.1.0**. `scheduler.py`/`trigger_schedule.py`/`channel_sdk.py` are byte-identical to the SDK `templates/trigger` scaffold, enforced by an in-repo **drift gate** (`scripts/check_scaffold_drift.py` regenerates via `init_plugin(...)` and diffs; CI installs the SDK from git so it carries the `trigger` scaffold kind the published wheel lacks).
  - ✅ **P5b-rt DONE — runtime hot-start (runtime#308).** `DaemonSupervisor.supervise`/`ChannelEventSocketManager.add_specs` add a newly-installed daemon mid-process; a `DaemonRuntime` holder makes `ensure_daemons()` idempotent (cold at boot, warm via `POST /internal/daemons/reload`, `platform_inbound`-authed). So a running workspace arms a just-declared scheduler **without a restart**.
  - ✅ **P5b core delivery (core#4408).** `scheduler_plugin.go`: `ensureSchedulerPluginDeclared` (idempotent upsert of the pinned `gitea://…#v0.1.0` source), `armSchedulerPlugin` (best-effort reload forward — non-fatal, reconcile-on-online is the durable net), `ensureAndArmSchedulerPlugin` (declare sync + arm async). Hooked into `schedules.go` Create and template seeding (`template_schedules.go`, when ≥1 schedule seeds). `POST /admin/schedules/backfill-plugin` (AdminAuth, **dry-run by default**, `?apply=true` to declare+arm) remediates workspaces stranded by #4399.
- **Tests/CI:** ✅ P5a scaffold-drift gate + daemon tests green in the plugin repo; P5b-rt hot-start unit + integration tests (supervise idempotency, holder cold/warm-add/warm-noop, `add_specs` binds only new lanes) — 129 runtime tests green; P5b core `scheduler_plugin_test.go` — **pinned source declared exactly** (typo = silent no-install, the load-bearing guard), source shape well-formed, arm no-ops without a callback URL (negative control), backfill dry-run is read-only (negative control, no INSERT).
- **e2e — P5c (core#4408):** `test_staging_full_saas.sh` **step 10d** (mirrors idle step 10c) — with `E2E_SCHEDULER_CHECK=on` the ephemeral workspace declares the plugin, boot-installs + arms it, then a `* * * * *` schedule is created via the **tenant API** and the daemon must **autonomously fire** it (`schedule-history.json` + `"fired schedule"` log). Reachable, distinct fail arms: create-non-2xx (capability/routing gap) vs never-fired with grid/health/history diagnostics naming the broken leg. Defaults **OFF** until the ephemeral runtime pin carries plugin boot-install + the trigger scaffold, so it can't red a gate on a capability the image doesn't yet have.
- **Cleanup:** the delivery is additive — nothing retired here; it is the *precondition* for P4's `workspace_schedules` drop (P4b).
- **Docs:** this block; `project_scheduler_trigger_plugin_no_default_delivery` memory; the plugin repo README. ⏳ SDK trigger-plugin authoring guide still references the scaffold (task #39).

### Cross-cutting DoD (every phase)
- **No double-fire** demonstrated at each cutover point (`NativeSchedulerCheck` gates before any daemon goes live).
- **No scheduled CI** introduced — `lint_schedule_budget` zero-cron ratchet stays green; exercise via `pull_request` / `push` / `workflow_dispatch`.
- **Contracts in sync** across core/runtime/sdk — every drift gate green (cron contract, plugin-manifest, mcp-plugin-delivery).
- **Negative-controlled tests** — every provenance/security/gate assertion proven to fail when the property is broken (never a vacuous green).
- **Docs + memory current** — consolidated idle-prompt design doc, this RFC's status, and `project_scheduler_as_trigger_plugin_rfc` memory updated each phase.
- **Rollback path** — each phase reversible (P1/P2 behind a flag; P3 keeps core reads one release; P4 only after the plugin is universal).

## 9. Risks

- **Double-fire during cutover** — mitigated by making `NativeSchedulerCheck` the hard gate; a workspace's daemon must not fire until core confirms defer (order: claim capability → core observes → daemon arms).
- **Inject-lane security** — generalizing the channel socket must keep the `source_type` allow-list tight (a trigger plugin may stamp `self-scheduler` and nothing else; it must not be able to forge user-origin or channel provenance).
- **Sync consistency (Option C)** — dual-write volume⇄mirror; treat the volume as authoritative and the mirror as derived (rebuildable), never the reverse.
- **Third-party trust** — a trigger plugin can wake the agent on a timer; the autonomous-loop guard (`should_halt`) still governs, and per-template caps bound frequency. Marketplace trust tiers apply when it's listed.

## 10. Decisions

**Settled (CTO, 2026-07-14):**
- **Kind = `trigger`** — a scheduler is the first trigger type; the webhook event-poke and future condition/hook triggers share the kind, the inject lane, and the loop-guard governance.
- **State ownership = Option A** — daemon owns everything; `workspace_schedules` retired in P4; Canvas/admin/webhooks move to a runtime API (staged per §8).
- **Trigger turns are system self-turns** (`source_type=self-scheduler`), never `role:user` (§1.1).

**Still open:**
1. **Inject-lane breadth (G1)** — build the local A2A lane as a *general* trigger lane now (a `source_type` allow-list so a future webhook/file trigger plugs into the same socket), or scope it to the scheduler self-turn first and generalise later. The `trigger` framing leans general; the lean-increment principle leans scoped-first. *Leaning: general lane, scheduler as the first + only user shipped in P2.*
2. **Where the native-scheduler claim is declared** — manifest (static, inspectable at load) vs daemon-reported via heartbeat (dynamic). *Recommend manifest.*
3. **The webhook event-poke under `trigger`** — today it's a core `next_run_at=NOW()` write. Under Option A + `kind: trigger`, does the event-poke become its own trigger sub-type delivered to the daemon, or a runtime-API call the platform makes when the webhook fires? (Affects G1 breadth.)
