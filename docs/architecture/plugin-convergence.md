# Plugin convergence

**SSOT for how a commit to a plugin repository reaches a running workspace.**

Scope: git-backed plugins (`gitea://`, `github://`) declared or installed on a
workspace. Covers the detect → queue → apply → materialize loop, what each
stage guarantees, and the gaps that remain. Anything that claims a plugin is
"up to date" must be reconcilable with this document.

Origin: core#4977, where a client-owned plugin pinned to `#main` served
five-hour-old code to a running workspace and only converged on restart.

---

## 1. The loop

```
   commit pushed to a plugin repo
              │
              ▼
   ┌──────────────────────┐   plugins/drift_sweeper.go
   │ 1. DETECT            │   every DriftSweepInterval (1h)
   │    tip != installed  │   selects DriftEligibleQuery
   └──────────┬───────────┘
              ▼
   ┌──────────────────────┐   plugin_update_queue, status='pending'
   │ 2. QUEUE             │   partial unique index: one pending row
   └──────────┬───────────┘   per (workspace, plugin)
              ▼
   ┌──────────────────────┐   handlers/plugin_drift_applier.go
   │ 3. APPLY             │   every DriftApplyInterval (10m),
   │    stage + deliver   │   ≤ driftApplyMaxPerTick entries
   └──────────┬───────────┘
              ▼
   ┌──────────────────────┐   bytes actually on the box
   │ 4. MATERIALIZE       │   → re-pin installed_sha
   └──────────────────────┘
```

Worst-case convergence for a branch-pinned plugin is therefore
**~1h (detect) + ~10m (apply)**. There is no push-on-commit path; see §6.

Each stage is a plain in-process loop started in `cmd/server/main.go`. **No
stage is agent-mediated.** That is deliberate: before core#4977 the only
consumer of the queue was the concierge's `plugin-auto-update` schedule
(cron `0 3 * * *`), so convergence depended on an LLM agent waking up and
choosing to call an admin endpoint — and on a tenant whose schedules were
never seeded, nothing called it at all. Convergence is a correctness property.

---

## 2. `tracked_ref`: which plugins are eligible

`workspace_plugins.tracked_ref` decides whether the sweeper looks at a row.
It is derived from the source's `#fragment` by `trackFromSource`:

| Source fragment | `tracked_ref` | Swept? | Rationale |
|---|---|---|---|
| `#tag:v1.0` | `tag:v1.0` | yes | immutable pin, sweeper-owned |
| `#sha:abc…` | `sha:abc…` | yes | immutable pin, sweeper-owned |
| `#main` | `ref:main` | yes | moving branch tip |
| `#v0.2.1` | `ref:v0.2.1` | yes | bare tag name — see below |
| `#973a35b7…` (bare 40-hex) | `ref:973a35b7…` | yes | bare commit SHA — see below |
| *(no fragment)* | `none` | no | nothing upstream to chase |
| `local://…` | `none` | no | no upstream at all |

All three `ref:` cases are observed in production. A bare tag name and a bare
commit SHA are both immutable in practice, so they resolve to a constant SHA
and never report drift; the cost is one cheap `--depth=1` resolve per sweep
that can never find anything. Classifying them as `tag:`/`sha:` up front would
avoid that resolve, but the derivation is mirrored in the backfill migration,
so any such rule must change in **both** places or the DB and the Go code
diverge. Not worth that coupling for one avoided fetch per hour; recorded here
so the behaviour is intentional rather than accidental.

**Why `ref:` and not `branch:`.** `#main` and `#v0.2.1` are syntactically
identical; classifying them would require a forge lookup. `ref:` is honest
about that. A bare tag stored this way resolves to a constant SHA and so simply
never reports drift — one cheap fetch instead of a missed update.

**`ref:` is a storage encoding, not a git ref.** `driftSpecForTrackedRef`
unwraps it back to a bare `#main` before any spec reaches a resolver. Sending
`#ref:main` to a forge would look up a ref literally named `ref:main`, fail,
and be logged-and-skipped — a sweeper that looks busy while converging nothing.

> **Historical note (core#4977).** `trackFromSource` used to return `none` for
> anything lacking a literal `tag:`/`sha:` **prefix**. Nothing in production
> writes that prefixed form — real sources are `#main` and `#v0.2.1` — so every
> row landed on `none`, and the sweeper's `WHERE tracked_ref != 'none'` matched
> **zero rows**. It had never swept anything. Immutable tag pins masked this:
> their content cannot drift, so only a branch-pinned plugin surfaced it.
> Migration `20260731000000_workspace_plugins_backfill_ref_tracking` re-derives
> `tracked_ref` for rows written before the fix.

---

## 3. Apply, and what "materialized" means

`applyQueuedDrift` (`handlers/admin_plugin_drift.go`) is the single
implementation. Both `POST /admin/plugin-updates/:id/apply` and the drainer
call it — there is deliberately no second copy.

Delivery has two modes:

- **push** — bytes copied into the container. Materialized immediately.
- **pull** — docker-less tenants return `errNoPushTarget`. **No bytes are
  copied; the RESTART is the delivery**, because it makes the boot installer
  fetch the new commit.

So on a docker-less tenant:

```
materialized = deliveredByPush || restartDispatched
```

### The deferral rule

The self-brick guard (`applyRestartAfterDrift`) **defers** the restart for a
`kind=platform` concierge in its fragile lifecycle window — an unconditional
restart there could brick the org-root box on a bad ref with nothing left to
recover it.

On a pull-delivery tenant, a deferred restart means **nothing reached the box**.
`installed_sha` is therefore **not** advanced.

This is load-bearing. Advancing it would leave the box with the old tree while
the database claims the new SHA; the next sweep compares new-vs-new, finds no
drift, and goes quiet permanently. The box would be indefinitely stale while
every signal reported converged. Leaving the old SHA means the sweeper
re-detects and re-queues, so the drift stays **visible** until the concierge is
deliberately restarted.

A failed re-pin demotes `materialized` to false for the same reason: never
claim convergence on the strength of a write that did not land.

The queue entry is marked `applied` either way, so the drainer does not
re-fetch the same entry every tick; the sweeper re-queues a fresh row hourly.

---

## 4. Operational contract

| Knob | Value | Where |
|---|---|---|
| Detection interval | 1h | `plugins.DriftSweepInterval` |
| Apply interval | 10m | `handlers.DriftApplyInterval` |
| Applies per tick | 25 | `driftApplyMaxPerTick` |
| First-drain settle | 2m | `handlers.DriftApplyStartupDelay` |
| Per-fetch deadline | 60s | `plugins.ResolveRefDeadline` |

The per-tick cap is a **restart budget**: each apply usually restarts a
workspace, so an unbounded drain after a fan-out would bounce the fleet at
once. Excess entries are picked up on the next tick.

The settle delay exists for the same reason. The sweeper runs immediately at
startup — safe, it only detects — but the applier must not, or a server boot
would restart up to a full budget of workspaces within seconds of coming up.

### First deploy after the backfill

Expect a one-time burst. The backfill makes every previously-`none` row
sweepable, so the first sweep queues drift for **every** branch-pinned plugin
whose tip has moved since it was installed, fleet-wide. Those then roll at
≤25 per 10 minutes, 2 minutes after the server settles.

That is intended — it is the fleet converging for the first time — but it does
mean the first post-deploy hour is the one time this system restarts many
workspaces at once. Watch `GET /admin/plugin-updates-pending` drain, and note
that tag/SHA-pinned plugins resolve to a constant and will *not* queue.

Per-entry failures are isolated — one unreachable upstream must not stop the
rest of the queue. A failed entry stays `pending` and is retried next tick.

### Inspecting state

```sql
-- what the sweeper considers eligible (its own predicate)
SELECT plugin_name, tracked_ref, installed_sha FROM workspace_plugins
 WHERE tracked_ref != 'none' AND installed_sha IS NOT NULL;

-- outstanding drift
SELECT workspace_id, plugin_name, current_sha, latest_sha, status
  FROM plugin_update_queue WHERE status = 'pending';
```

`GET /admin/plugin-updates-pending` exposes the same queue over HTTP.

**A tenant where every `tracked_ref` is `none` is the core#4977 signature** —
the sweeper is selecting nothing and is vacuously green.

---

## 5. Testing posture

The defect class here is *a guard that covers nothing*, so the tests are built
to fail when coverage disappears:

- **Real Postgres, not sqlmock, for anything cross-statement.** sqlmock never
  evaluates a `WHERE` clause, so it cannot detect a predicate that matches zero
  rows — it would have passed against the broken sweeper for its entire life.
  `TestIntegration_PluginRefTracking_*` and `TestIntegration_DriftApplier_*`
  execute `plugins.DriftEligibleQuery`, the constant the sweeper itself runs.
- **Observe effects, not statements.** Whether `installed_sha` was re-pinned is
  checked through the `driftRecordFn` seam. sqlmock cannot serve that role: an
  unexpected `INSERT` only returns an error, and the call site treats a record
  failure as non-fatal, so the write would fire unnoticed.
- **Assert the production ref shapes** (`#main`, `#v0.2.1`), never only the
  injected `tag:`-prefixed form — testing the prefixed form is what would have
  passed while production stayed broken.

CI: `.gitea/workflows/handlers-postgres-integration.yml`, selected by
`-run ^TestIntegration_`. Its path profile (`.gitea/scripts/detect-changes.py`,
`handlers-postgres`) includes `internal/plugins/`, `internal/router/`, and
`cmd/server/` so a change that unwires the applier still triggers the gate.

---

## 6. Known gaps

1. **No push-on-commit.** `POST /admin/plugin-fragment-changed` exists and is
   routed, but **no producer calls it** — there are no webhooks on any plugin
   repo, first-party or customer-owned, and no CI notify step. Until one
   exists, convergence latency is the §4 interval, not seconds.
2. **Private plugin repos cannot be fetched.** The runtime's per-host
   credential layer is sound, but `gitea_read_token` resolves
   `MOLECULE_TEMPLATE_REPO_TOKEN` / `GITEA_TOKEN` first — both on the RFC#523
   forbidden-env denylist. A correctly scoped per-org identity delivered as
   `MOLECULE_GIT_TOKEN__<HOST>` is the intended fix. Note the explicit scope
   constraint in the CP's `template_repo_creds.go`: the platform-wide template
   token must **not** be extended to read customer-owned repos.
3. **A deferred concierge never self-heals.** By design — it converges on its
   next deliberate restart. The drift stays visible in the queue until then.
