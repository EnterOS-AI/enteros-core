# Enter OS Light API

An Enter OS Light agent sleeps between its scheduled runs. While it is
**parked**, its agent pod is stopped (status `hibernated`) and the control plane
runs each of its schedules as a Kubernetes CronJob: every fire starts a
short-lived run pod from the agent's own image and a copy of its volume, which
runs the schedule's prompt as one turn, reports the outcome to core and exits.
Opening the agent wakes its pod for chat; parking it again hands the schedules
back to the CronJobs.

Light v1 runs **hermes** agents on the **Kubernetes** substrate only.

This page is the REST contract for clients that drive Light (the canvas, the
owner's own apps). Every route is under `/workspaces/{id}` and returns JSON.

## States

`light_state` on the workspace (also on every workspace object of
`GET /workspaces` and `GET /workspaces/{id}`, `null` for an ordinary agent;
changes are broadcast as `WORKSPACE_UPDATED` with `{"light_state": ...}`):

| Value | Meaning |
|---|---|
| `null` | Not a Light agent. Everything behaves as before Light. |
| `open` | Light agent, awake. Its CronJobs are paused (or not written yet). |
| `parking` | Being parked: core is handing the schedules to the control plane and stopping the pod. |
| `parked` | Asleep; the CronJobs run the schedules. |
| `opening` | Being opened: the CronJobs are paused and the pod is waking. |

While an agent is `parking` or `parked`, nothing wakes it implicitly: chat,
restarts and every other wake path answer `409` (see
[What other endpoints answer](#what-other-endpoints-answer-for-a-parked-agent)).

How an agent moves between them:

```
            enable               park (202)             pod stopped, CronJobs resumed
  null ────────────▶ open ───────────────▶ parking ─────────────────────────────▶ parked
   ▲                  ▲ ▲                    │                                       │
   │                  │ └────────────────────┘  refused after it started, the agent  │
   │                  │                         was busy, or opened after 3 minutes  │
   │                  │     pod up                       open (202)                  │
   │                  └────────────── opening ◀─────────────────────────────────────┘
   └──── disable (from any state)
```

Opening a park stuck in `parking` for three minutes leads to `open` when its
pod is still up, and to `opening` when the pod was already stopped. While the
pod is still stopping (`status: hibernating`) open answers `409`; the monitor
finishes that stop five minutes into the park.

`park` and `open` answer `202 Accepted` as soon as the step that can be
refused is done; the rest finishes in the background. Poll
`GET /workspaces/{id}/light` (every few seconds) until `light_state` is
`parked` or `open`. A park usually completes within a minute (the agent's pod
needs about 30 s to stop); an open takes as long as the agent's boot. If a
background step fails, `last_error` says why, and core retries it (see
[What core does in the background](#what-core-does-in-the-background)).

### When the agent is ready

Enable restarts the agent, and an open boots it. Until its new pod has
registered, the agent cannot be parked and its schedules cannot be read or
edited. The agent is ready when `GET /workspaces/{id}` shows `status`
`online` (or `degraded`) **and** a non-empty `url`: a restart clears `url`,
and only the new pod's registration writes it again. A heartbeat from the pod
a restart is stopping no longer moves `status` from `provisioning` to
`online` (before 2026-11-18 it could, so `online` with an empty `url` was a
pod that was going away).

A pod that has just registered can still take a few seconds before it
serves. Calls made in that window are refused as retryable, never as
failures:

- `POST /light/park` waits up to a minute for the agent to answer, then
  answers `409 LIGHT_AGENT_NOT_READY` with `Retry-After` (nothing is changed).
- The schedule routes (`/workspaces/{id}/schedules...`) answer
  `503 {"code": "WORKSPACE_NOT_READY"}` with `Retry-After` while the agent
  has no registered URL or does not answer.

Cloudflare, in front of every tenant, replaces the body of a `502` or `504`
with its own page, so no refusal a client must act on uses those statuses. A
`5xx` whose body is not JSON is a proxy's page, not core's answer: retry an
idempotent call (every Light route but `run`, every `GET`).

## Authentication

Every route sits behind `WorkspaceAuth`: the owner's session, an org API
token, the tenant admin token, or a workspace token of `{id}`. Anything else is
`401`.

- **Management** routes (`enable`, `disable`, `park`, `open`, `run`) refuse
  the workspace's own tokens with `403 LIGHT_SELF_FORBIDDEN`: a run pod holds
  the agent's run token and must not manage its own agent.
- `POST /light/runs` accepts **only** the workspace's own token (the run pod's
  run token) and answers anyone else `403 LIGHT_RUN_TOKEN_REQUIRED`.
- Reads (`GET /light`, `GET /light/runs`) accept any `WorkspaceAuth`
  credential.

**Known limit.** A workspace that uses the platform LLM carries the tenant admin
token as its LLM key (a pre-existing design of the platform-LLM proxy), so its
run pods hold an owner-level credential and the `403` above does not stop them.
Tracked in
[molecule-core#5269](https://git.moleculesai.app/molecule-ai/molecule-core/issues/5269).

## Errors

Every error body has core's shape:

```json
{"error": "<a sentence for a human>", "code": "<UPPER_SNAKE_CODE>"}
```

plus named extras where listed (`light_state`, `status`, `active_tasks`,
`is_busy`, `runtime`, `field`, `run_id`, `key`, `invalid`). Branch on `code`,
never on `error`.

Common to every route:

| Status | `code` | When |
|---|---|---|
| 400 | `LIGHT_BAD_REQUEST` | `{id}` is not a canonical UUID; a bad query parameter or body |
| 404 | `WORKSPACE_NOT_FOUND` | no such workspace, or it was deleted |
| 500 | `LIGHT_FAILED` | core could not complete the step; safe to retry |

Common to routes that call the control plane (`enable`, `park`, `open`, `run`,
`disable`). `disable` never answers the `404` or the `501`: a tenant with no
namespace, or one that cannot run Light, has no Light objects to delete, so
disable goes ahead.

| Status | `code` | When |
|---|---|---|
| 404 | `LIGHT_TENANT_NOT_FOUND` | the control plane has no tenant namespace for the org |
| 501 | `LIGHT_UNSUPPORTED` | the tenant cannot run Light (no control plane, or not the Kubernetes substrate) |
| 502 | `LIGHT_FAILED` | the control plane answered with an unexpected error |
| 503 | `LIGHT_BACKEND_UNAVAILABLE` | the control plane did not answer; nothing was changed by the failed step. `Retry-After` gives seconds |

A `409` or `404` the control plane gives is passed through with its own `code`
and message; its body carries only the extras the control plane sent (`key`,
`run_id`), never `status`, `active_tasks` or `light_state`.

Management routes answer `409 LIGHT_TRANSITION_IN_PROGRESS` while another
Light operation on the same agent is running (an owner call, a workspace
delete, or a background step: a park's pod stop and resume, a refresh, a
failure-streak check); retry after a few seconds.

Times are RFC 3339 in UTC.

## `GET /workspaces/{id}/light`

The agent's Light state.

**200**

```json
{
  "workspace_id": "be7540cf-d160-4e2a-9b1c-0123456789ab",
  "light_state": "parked",
  "light_state_at": "2026-11-13T09:00:04Z",
  "refresh_pending": false,
  "pending_actions": [],
  "last_error": null,
  "schedules": [
    {
      "name": "daily-report",
      "cron": "0 9 * * *",
      "timezone": "America/Vancouver",
      "prompt": "Write the daily report.",
      "enabled": true,
      "suspended": false,
      "suspended_reason": null,
      "suspended_at": null,
      "consecutive_failures": 0,
      "cronjob": "light-be7540cfd160-1a2b3c4d",
      "next_run_at": "2026-11-13T16:00:00Z"
    }
  ]
}
```

- `light_state`: `null` for an ordinary agent; `light_state_at` is when it
  last changed.
- `schedules`: the set the agent had at its **last park**. `enabled` is the
  agent's own on/off flag for the schedule (what its schedule list says).
  `suspended`, `suspended_reason` and `suspended_at` are core's suspension
  record (`suspended_at` is when it was last suspended, kept after a resume);
  a suspended schedule does not fire even when `enabled` is true. While the
  agent is open, its live schedules are at `GET /workspaces/{id}/schedules`.
- `next_run_at`: the schedule's next fire time after now, RFC 3339 in UTC: its
  `cron` in its `timezone` (an empty `timezone` is UTC), parsed as the schedule
  grid parses crons (five fields). It is the schedule's own minute (a run the
  control plane starts a minute early still begins its task at this time) and
  does not depend on `light_state`, so an open agent's paused schedules still
  show when they would run. `null` when the schedule is off (`enabled` false),
  suspended, or its `cron` or `timezone` does not parse.
- `pending_actions`: work queued for the agent's next open/park cycle, each
  `{"kind", "source", "queued_at"}`. `refresh_pending` is true when a `refresh`
  is queued (a secret, plugin or config change made while the agent was parked
  or being parked: core opens the agent briefly and parks it again so its runs
  pick the change up).
- `last_error`: the last failure of a Light step, `{"code", "message", "at"}`,
  or `null`. An open clears it when it claims the agent, and a completed park
  clears it except for the two codes that hold back the next refresh (below).
  The codes:

| `code` | Meaning |
|---|---|
| `LIGHT_AGENT_BUSY` | a park was undone because the agent was working; it is `open` |
| `LIGHT_PARK_INCOMPLETE` | stopping the pod or resuming the CronJobs failed; still `parking`, retried |
| `LIGHT_PARK_FAILED` | the pod failed while it was being parked; it is `open` |
| `LIGHT_OPEN_FAILED` | the pod failed to start while opening; it is `open` (restart it) |
| `LIGHT_OPEN_INCOMPLETE` | an open has paused the CronJobs and not woken the agent yet. Seen briefly during every open; if the open was interrupted, core resumes the CronJobs two minutes later and clears it |
| `LIGHT_REFRESH_OPEN_FAILED` | a refresh open did not bring the agent up; it was parked again on its previous configuration, and the refresh is retried 10 minutes later. Kept until the next open |
| `LIGHT_ACTION_FAILED` | a queued action failed during a refresh cycle; retried 10 minutes later. Kept until the next open |
| any refusal code of `park` or `open` | a park after its claim, or a park or refresh core started itself, was refused with that code (for example `LIGHT_RUNTIME_UNSUPPORTED` when the runtime was changed, `LIGHT_TENANT_SUSPENDED` from a resume) |

## `POST /workspaces/{id}/light/enable`

Makes the agent a Light agent. No body.

Core stores two workspace secrets, `MOLECULE_LIGHT=1` (the runtime leaves its
own scheduler off; the control plane refuses to park an agent without it) and
`MOLECULE_CONTINUOUS=false` (the 24/7 switch off, so an idle open agent can be
parked), sets `light_state` to `open`, and restarts the agent so the new env
applies (a hibernated agent is woken instead). It records the agent's 24/7
setting as it was, so disable can put it back. Before the first enable it asks
the control plane whether the tenant can run Light at all, so that a tenant
that cannot is refused here rather than at the first park.

**200**

```json
{"status": "enabled", "light_state": "open", "restart": "started"}
```

| Field | Values |
|---|---|
| `status` | `enabled`; `unchanged` when the agent already was a Light agent |
| `light_state` | the state after the call |
| `restart` | `started` (restart begun), `woken` (a hibernated agent is waking), `refresh_queued` (a parking or parked agent had lost a Light secret: restored, and picked up by a refresh), `none` (both secrets were already in place, or the agent's pod is stopping: `status: hibernating`) |

The restart is asynchronous: watch the workspace's `status`.

**Errors** (besides the common ones)

| Status | `code` | When |
|---|---|---|
| 403 | `LIGHT_SELF_FORBIDDEN` | called with the workspace's own token |
| 409 | `LIGHT_AGENT_PAUSED` | the workspace is paused; resume it first |
| 409 | `LIGHT_TRANSITION_IN_PROGRESS` | another Light operation is running |
| 422 | `LIGHT_RUNTIME_UNSUPPORTED` | not a hermes agent; `runtime` names its runtime |
| 422 | `LIGHT_PLATFORM_AGENT` | the org's platform agent cannot be a Light agent |

A refusal writes nothing.

## `POST /workspaces/{id}/light/park`

Parks an open Light agent. No body.

Core checks the agent and reads its schedules from the agent itself (the same
list as `GET /workspaces/{id}/schedules`, up to 8 MiB). An agent that does not
answer (it is restarting: no registered URL yet, a transport failure, or a
`5xx`) is tried again for up to a minute, 1, 2, 4 and then 8 seconds apart;
between tries core reads the workspace again, and stops waiting when it was
deleted, is no longer online, or took a task. Then core claims `parking`,
reads the schedules again (the set it sends), mints a new run token, and hands
the set to the control plane, which writes one CronJob per schedule. Then, in the background, it stops the agent's pod and
resumes the CronJobs. A schedule core suspended after failed runs stays
suspended unless this park is the owner's (this call resumes every suspended
schedule) or the schedule's definition changed.

**202**

```json
{
  "light_state": "parking",
  "schedules": [
    {"name": "daily-report", "cron": "0 9 * * *", "timezone": "America/Vancouver",
     "enabled": true, "suspended": false, "cronjob": "light-be7540cfd160-1a2b3c4d"}
  ],
  "removed": ["old-schedule"]
}
```

- `schedules`: what this park sent. `enabled` is the flag the CronJob got:
  `false` for a schedule the agent has off, and for one whose suspension this
  park kept (then `suspended` is `true`). An owner's park sends no suspension.
  (`GET /light` shows the agent's own flag in `enabled` and core's record in
  `suspended`.)
- `removed`: schedules the control plane held from an earlier park that the
  agent no longer has; their CronJobs are deleted.

Other answers:

- `200 {"status": "unchanged", "light_state": "parked"}`: already parked.
- `202` with empty `schedules` and `removed`: the agent was already `parking`
  and its pod is no longer up (a park stuck in its last step); core retries
  that step in the background (once the pod has stopped, it resumes the
  CronJobs, retrying for up to two minutes).
- Calling park on a `parking` agent whose pod is still up (a park a core
  restart interrupted) re-runs the whole park; any refusal then returns the
  agent to `open` with `last_error`.

**Errors.** A refusal **before the park starts** changes nothing: the agent
stays as it was and `last_error` is not touched.

| Status | `code` | When |
|---|---|---|
| 403 | `LIGHT_SELF_FORBIDDEN` | called with the workspace's own token |
| 409 | `LIGHT_NOT_ENABLED` | not a Light agent (`light_state` is `null`) |
| 409 | `LIGHT_TRANSITION_IN_PROGRESS` | the agent is `opening` (`light_state` says so), or another Light operation is running |
| 409 | `LIGHT_AGENT_NOT_AWAKE` | the agent is not online; `status` gives its status. It is not reachable until it is online again |
| 409 | `LIGHT_AGENT_NOT_READY` | the agent did not answer its schedule list for a minute (it is starting or restarting); `status` gives its status, `Retry-After` the seconds to wait. Retry |
| 409 | `LIGHT_AGENT_BUSY` | the agent is working; `active_tasks` and `is_busy` say how |
| 409 | `LIGHT_RESTART_REQUIRED` | the agent's `MOLECULE_LIGHT` secret is missing; call enable again |
| 413 | `LIGHT_TOO_LARGE` | the agent's schedules are over 8 MiB |
| 422 | `LIGHT_SCHEDULES_UNREADABLE` | the agent answered, but not with a list of schedules (another status than `200` below `500`, malformed JSON, or no `schedules` list). Not retried: fix the agent |
| 422 | `LIGHT_RUNTIME_UNSUPPORTED` | not a hermes agent; `runtime` names its runtime |
| 501 | `LIGHT_UNSUPPORTED` | the tenant has no control plane |
| 500 | `LIGHT_FAILED` | core could not read the agent or its Light secret, or claim the park |

A refusal **after the park started** returns the agent to `open` (reachable
again, its CronJobs still paused) and records the code in `last_error`:

| Status | `code` | When |
|---|---|---|
| 413 | `LIGHT_TOO_LARGE` | the schedules are over 8 MiB (core's read after the claim), or over the control plane's size limit |
| 422 | `LIGHT_SCHEDULES_UNREADABLE` | the read after the claim got something that is not a list of schedules |
| 409 | `LIGHT_AGENT_NOT_READY` | the agent stopped answering between the two reads (10 s of tries after the claim); `Retry-After` as above |
| 422 | `LIGHT_INVALID` | some schedules are invalid; `invalid` lists `{"schedule", "field", "reason"}` for each |
| 409 | `LIGHT_ENV_UNSUPPORTED` | the agent's env has a value the control plane cannot copy; `key` names it |
| 409 | `LIGHT_RESTART_REQUIRED` | the agent runs without `MOLECULE_LIGHT=1`; restart it, then park |
| 409 | `LIGHT_AGENT_NOT_AWAKE` | the control plane found no running agent to copy the env from (no `status` field) |
| 422 | `LIGHT_RUNTIME_UNSUPPORTED` | the control plane refused the runtime |
| 404 | `LIGHT_TENANT_NOT_FOUND` | see the common errors |
| 501, 502, 503 | `LIGHT_UNSUPPORTED`, `LIGHT_FAILED`, `LIGHT_BACKEND_UNAVAILABLE` | see the common errors |
| 500 | `LIGHT_FAILED` | core failed a step (reading its stored schedules, the run token) |

`404 WORKSPACE_NOT_FOUND` after the park started means the workspace was
deleted meanwhile; core deletes whatever the park wrote to the control plane.

**After the 202.** If the agent turns out to be working (a task that started
around the claim: its heartbeat still reports the task while `parking`, and any
activity of its own after the claim counts, as defined under
[Auto-park](#what-core-does-in-the-background), such as a reply; what its runs
write does not), the park is undone before the pod is stopped: the agent goes
back to `open` and `last_error` is `LIGHT_AGENT_BUSY`. If stopping
the pod or resuming the CronJobs fails, the agent stays `parking`, `last_error`
says why (`LIGHT_PARK_INCOMPLETE`, or the control plane's code such as
`LIGHT_TENANT_SUSPENDED`), and core keeps retrying.

**A stuck park.** When a park cannot finish (its resume keeps failing), call
`POST /light/open` once the agent has been `parking` for three minutes: it
takes the agent back to `open` (or `opening`, when its pod was already
stopped). While its pod is still stopping (`status: hibernating`), open keeps
answering `409` until the monitor finishes the stop, five minutes into the
park. `POST /light/disable` also works from any state.

## `POST /workspaces/{id}/light/open`

Opens a parked agent for chat or editing. No body.

Core first records the open (`last_error` reads `LIGHT_OPEN_INCOMPLETE` for a
moment), then pauses the CronJobs, while the agent is still parked: runs that
had not started yet are cancelled (recorded as `cancelled`, listed in the
answer) and runs already going finish. Then it claims `opening` and wakes the
agent's pod.

**202**

```json
{"light_state": "opening", "cancelled": ["light-be7540cfd160-1a2b3c4d-29384756"]}
```

`light_state` becomes `open` once the pod is up. Opening a park stuck in
`parking` for three minutes (see [park](#post-workspacesidlightpark)) answers
`202` with `light_state` `opening` when its pod was stopped, or `open` when
its pod is still up.

An agent already `open` or `opening` answers
`200 {"status": "unchanged", "light_state": "<state>"}`, and the owner takes it
over: if a refresh had opened it, it now stays open until the auto-park idle
window (30 minutes without activity) has passed from this call.

**Errors** (besides the common ones)

| Status | `code` | When |
|---|---|---|
| 403 | `LIGHT_SELF_FORBIDDEN` | called with the workspace's own token |
| 409 | `LIGHT_NOT_ENABLED` | not a Light agent |
| 409 | `LIGHT_TRANSITION_IN_PROGRESS` | the agent is being parked (`light_state` is `parking`): open it once it is parked, or, if the park cannot finish, once it has been parking for three minutes with its pod up or stopped (a pod still stopping, `hibernating`, is finished by the monitor five minutes into the park); also while another Light operation is running |

Any refusal leaves the agent as it was. When the pause may already have run
(it failed, `503`/`502`/`404`, or the claim after it failed, `500`/`409`), core
resumes the CronJobs of the still-parked agent, and if that fails too, the
monitor resumes them two minutes later. A `500` before the pause (core could
not record the open) changes nothing.

An open agent parks itself again after 30 minutes without activity of its own (see
[Background](#what-core-does-in-the-background)).

## `POST /workspaces/{id}/light/run`

Runs one schedule of a parked agent now, outside its cron times. The schedule
runs even when it is off or suspended (to try a fix).

```json
{"schedule": "daily-report"}
```

**201** `{"run_id": "light-be7540cfd160-1a2b3c4d-m1763000000"}`. The run
appears in `GET /light/runs` once its pod reports `started`, or once the jobs
poll (every 5 minutes) sees it running; a run still waiting (for quota, or an
image pull) is not listed yet.

| Status | `code` | When |
|---|---|---|
| 400 | `LIGHT_BAD_REQUEST` | the body is not exactly one `{"schedule": "<name>"}` object: unknown keys, anything after the object, or over 64 KiB |
| 403 | `LIGHT_SELF_FORBIDDEN` | called with the workspace's own token |
| 404 | `LIGHT_SCHEDULE_NOT_FOUND` | the parked agent has no such schedule |
| 409 | `LIGHT_NOT_PARKED` | the agent is not parked; `light_state` gives its state (`null` for an ordinary agent) |
| 409 | `LIGHT_RUN_ACTIVE` / `LIGHT_RUN_EXISTS` | that schedule already has a run going, or one started this second; `run_id` names it |
| 409 | `LIGHT_AGENT_AWAKE` | the agent's pod is still running; retry in a few seconds |
| 409 | `LIGHT_TENANT_SUSPENDED` | the tenant is suspended for billing |
| 409 | `LIGHT_TRANSITION_IN_PROGRESS` | another Light operation is running |
| 422 | `LIGHT_INVALID` | no schedule named; `invalid` says so |

## `POST /workspaces/{id}/light/disable`

Turns a Light agent back into an ordinary agent, from any Light state. No body.

Core first deletes the control plane's Light objects (the CronJobs, any run
still going, the run Secret); only when that succeeds does it revoke the run
tokens (including one a park minted but never recorded), remove
`MOLECULE_LIGHT`, put the 24/7 setting back exactly as it was before enable
(the workspace's own `MOLECULE_CONTINUOUS` value, or no workspace value at
all), clear `light_state` and `last_error`, drop the stored schedules and
queued actions, and restart the agent (or wake it, when parked). The run
history (`GET /light/runs`) is kept. A tenant that cannot run Light, or has no
tenant namespace, has nothing to delete: that counts as deleted.

**200**

```json
{
  "status": "disabled",
  "light_state": null,
  "deleted": {"cronjobs": 1, "jobs": 0, "secrets": 1},
  "restart": "woken"
}
```

`{"status": "unchanged", "light_state": null}` when the agent was not a Light
agent. `restart` is `started`, `woken` or `none` (a paused agent stays paused).

**Errors** (besides the common ones): `403 LIGHT_SELF_FORBIDDEN`,
`409 LIGHT_TRANSITION_IN_PROGRESS`. A control-plane failure (`503`, `502`)
leaves everything as it was.

## `POST /workspaces/{id}/light/runs` (run pods)

The run ledger write. A run pod calls it with the agent's run token, once
before its turn (`phase: "started"`) and once after it (`phase: "finished"`).
Clients other than run pods never call it.

```json
{
  "run_id": "light-be7540cfd160-1a2b3c4d-29384756",
  "schedule": "daily-report",
  "phase": "finished",
  "status": "succeeded",
  "started_at": "2026-11-13T09:00:07Z",
  "finished_at": "2026-11-13T09:03:41Z",
  "exit_code": 0,
  "output": "Report sent.",
  "error": ""
}
```

| Key | Rule |
|---|---|
| `run_id` | the run's Job name: printable ASCII, no spaces, at most 128 bytes |
| `schedule` | the schedule's name: 1 to 128 bytes, no control characters |
| `phase` | `started` (with `status: "running"`) or `finished` (with `succeeded`, `failed` or `timed_out`) |
| `started_at` | required, RFC 3339 |
| `finished_at` | RFC 3339 or null (core uses its own clock when a finished report has none) |
| `exit_code` | optional; a 32-bit integer |
| `output`, `error` | optional; `output` keeps its last 64 KiB, `error` its first 4 KiB; NUL characters and invalid UTF-8 are stored as U+FFFD |

The body is at most 256 KiB. Unknown keys are ignored. Core upserts on
`(workspace_id, run_id)`:

- `started` inserts a `running` row, or refreshes one that is still running.
  When the row is already finished (for example a run that opening the agent
  cancelled) core answers `409 LIGHT_RUN_FINISHED` and the run must not start.
- `finished` records the outcome once. A repeated `finished` is a `200` no-op,
  and a finished run never goes back to `running`. A `failed` or `timed_out`
  outcome is checked against the schedule's failure streak right away, or,
  when a Light operation holds the agent at that moment, by the next jobs poll
  (within 5 minutes).

**200** `{"ok": true}`

| Status | `code` | When |
|---|---|---|
| 400 | `LIGHT_INVALID_REPORT` | the body is not one JSON object, or a key breaks its rule; `field` names it |
| 403 | `LIGHT_RUN_TOKEN_REQUIRED` | not the workspace's own token |
| 409 | `LIGHT_NOT_ENABLED` | the workspace is not a Light agent |
| 409 | `LIGHT_RUN_FINISHED` | a `started` for a run already finished; `run_id` names it |

## `GET /workspaces/{id}/light/runs`

The agent's runs, newest first.

| Query | Meaning |
|---|---|
| `limit` | 1 to 100, default 20; larger values are capped at 100 |
| `schedule` | only this schedule's runs |
| `before` | the previous page's `next_before`; or an RFC 3339 time (runs recorded strictly earlier) |

**200**

```json
{
  "runs": [
    {
      "run_id": "light-be7540cfd160-1a2b3c4d-29384756",
      "schedule": "daily-report",
      "status": "succeeded",
      "source": "report",
      "scheduled_for": "2026-11-13T09:00:00Z",
      "started_at": "2026-11-13T09:00:07Z",
      "finished_at": "2026-11-13T09:03:41Z",
      "exit_code": 0,
      "output": "Report sent.",
      "error": "",
      "created_at": "2026-11-13T09:00:07.512Z",
      "activity_count": 12,
      "activity_path": "/workspaces/be7540cf-d160-4e2a-9b1c-0123456789ab/activity?run_id=light-be7540cfd160-1a2b3c4d-29384756"
    }
  ],
  "next_before": null
}
```

- `status`: `running`, `succeeded`, `failed`, `timed_out` or `cancelled`
  (cancelled by opening the agent).
- `source`: `report` when the run pod reported it; `job` when core recorded it
  from the run's Kubernetes Job (a pod that never reported: killed, out of
  memory, image pull failure, deadline). A Job that ran out its deadline, or
  exited 124, is `timed_out`.
- `activity_count` / `activity_path`: the activity rows the run wrote (the
  activity its runtime reports, its messages to the owner, its A2A traffic)
  and the path that lists them.
- `next_before`: pass it back as `before` for the next page; `null` on the last
  page. It is opaque: do not parse it.

**Errors** (besides the common ones): `400 LIGHT_BAD_REQUEST` for a `limit`
that is not a positive integer, a `before` that is neither a `next_before` nor
an RFC 3339 time, or a `schedule` over 128 bytes.

## A run's activity

Every request a run pod makes to core carries `X-Molecule-Run-Id: <run_id>`.
When the request is authenticated with a workspace token, the activity rows it
writes are tagged with that run (rows of the run's own workspace, and the
receiving side of its A2A messages). List them with the existing activity route:

```
GET /workspaces/{id}/activity?run_id=<run_id>
```

A `run_id` that breaks the run-id rule is `400`. A header on a request made with
any other credential is ignored.

## What other endpoints answer for a parked agent

While an agent is `parking` or `parked`:

| Endpoint | Answer |
|---|---|
| `POST /workspaces/{id}/a2a` (chat, delegation, channels, webhooks) | `409 LIGHT_AGENT_PARKED` |
| `POST /workspaces/{id}/restart`, `/pause`, `/resume` | `409 LIGHT_AGENT_PARKED` |
| `POST /registry/register`, `/registry/update-card` | `409 LIGHT_AGENT_PARKED`, nothing written |
| `POST /registry/heartbeat` | `409 LIGHT_AGENT_PARKED`. While `parking` with the pod still up, core records only `active_tasks` and `is_busy` from it (so a park sees a task in flight); nothing else, and nothing at all while `parked` |
| A2A messages queued before the agent parked | stay queued until it is open |
| secret, model and plugin changes that restart the agent | accepted; a `refresh` is queued instead of a restart |

The body:

```json
{
  "error": "This agent is a Light agent and is parked. Open it to chat.",
  "code": "LIGHT_AGENT_PARKED",
  "light_state": "parked"
}
```

(`LIGHT_AGENT_PARKING` and "...is being parked..." while `parking`.)

For a Light agent in **any** state, `POST /workspaces/{id}/hibernate` and
`POST /workspaces/{id}/switch-provider` answer `409 LIGHT_ENABLED` (park it
instead; disable Light before switching the provider).

## What core does in the background

A monitor in core, every 30 s:

- **Auto-park.** An `open` agent that is online, idle (no task, not busy),
  open for 30 minutes (`LIGHT_AUTO_PARK_IDLE_MINUTES`) and without activity of
  its own for as long is parked. Its own activity is what moves the agent's
  `last_activity_at`: a message to it, its reply (the clock restarts when it
  finishes a turn, however long the turn took), its A2A traffic, and the
  activity its runtime reports to core (status updates, memory writes). Rows
  core inserts on its own (notifies to the owner, delegation records, the MCP
  bridge's tool-call rows, broadcasts) do not count, and neither does what its
  runs write; a turn in progress keeps the agent from parking through its
  heartbeat (`active_tasks`, `is_busy`) whatever it logs. Auto-park exists so
  schedules cannot stay paused by mistake, and it keeps core's schedule
  suspensions. A refused auto-park records its code in `last_error` and is not
  retried for 10 minutes. The monitor's parks wait only five seconds for an
  agent that does not answer (`LIGHT_AGENT_NOT_READY`), so one restarting
  agent does not hold up the sweep.
- **Refresh.** A change that would restart a parked agent (a secret, its
  model, a plugin), including one made while a park is under way, queues a
  `refresh`. Core opens the agent, and parks it again once it is online, idle
  and has had no activity of its own for a minute, so later runs carry the
  change. At most two agents are in a refresh at a time; one whose refresh
  open failed, or that you paused, does not count. A refresh-opened agent
  whose pod fails, or that is not online after 15 minutes in `opening` or
  `open`, is parked again on its previous configuration
  (`LIGHT_REFRESH_OPEN_FAILED`) and retried 10 minutes later; one you paused
  is left as you left it. When you open an agent that a refresh opened, it is yours: it stays
  open for the full idle window.
- **Interrupted steps.** A park or open that a core restart or a control-plane
  outage left half-way is finished: a park whose pod is still up is re-run
  after two minutes (as the same owner, auto or refresh park); a pod that is
  slow to stop is stopped again after three minutes, and a stop stuck half-way
  is finished after five; an open whose wake never ran is woken again; an
  agent that failed while opening goes back to `open` with `LIGHT_OPEN_FAILED`
  (or, when a refresh opened it, is parked again as above); an open
  interrupted after its pause has its CronJobs resumed two minutes later.
- **Runs that never reported** (every 5 minutes). A run pod killed before it
  could report (out of memory, image pull failure, deadline, no room in the
  tenant's quota) is recorded from its Kubernetes Job with `source: "job"`.
- **Failed schedules.** When a schedule's last three runs all failed or timed
  out, core suspends it and tells the owner in the agent's chat (or, when the
  agent may not message the user, through a `WORKSPACE_UPDATED` event naming
  the schedule, `{"light_schedule_suspended": "<name>"}`). `GET /light` shows
  `suspended` and `suspended_reason`. The agent is parked when this happens,
  so to resume the schedule: open the agent, fix the cause (or change the
  schedule), then park it yourself (`POST /light/park`): your park resumes
  every suspended schedule, and a changed schedule is resumed by any park.
  `POST /light/run` still runs a suspended schedule once, to try a fix.
- **Retention.** Finished runs older than 30 days are deleted (hourly).

Each sweep acts on at most five agents (a refresh sweep on at most two) and
starts no agent once it has run 90 seconds, so a slow control plane delays the
rest to the next pass; the jobs poll reads ten agents a pass until it has read
them all. The monitor reports itself as `light-monitor` in
`GET /admin/liveness`.

## Deleting a Light agent

`DELETE /workspaces/{id}` deletes the agent's control-plane Light objects
before it stops any compute; it waits up to 10 seconds for a Light operation
on the agent to finish first, and a park that finds its workspace deleted
deletes what it wrote. If the deletion fails, the delete answers `500` like
any other teardown failure, and the workspace keeps `light_state`; the monitor
retries the deletion until the objects are gone.
