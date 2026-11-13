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

`light_state` on the workspace:

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

## Authentication

Every route sits behind `WorkspaceAuth`: the owner's session, an org API
token, the tenant admin token, or a workspace token of `{id}`. Anything else is
`401`.

- **Management** routes (`enable`, `disable`) refuse the workspace's own
  tokens with `403 LIGHT_SELF_FORBIDDEN`: a run pod holds the agent's run
  token and must not manage its own agent.
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

plus named extras where listed (`light_state`, `runtime`, `field`, `run_id`,
`key`, `invalid`). Branch on `code`, never on `error`.

Common to every route:

| Status | `code` | When |
|---|---|---|
| 400 | `LIGHT_BAD_REQUEST` | `{id}` is not a canonical UUID; a bad query parameter |
| 404 | `WORKSPACE_NOT_FOUND` | no such workspace, or it was deleted |
| 500 | `LIGHT_FAILED` | core could not complete the step; safe to retry |

Common to routes that call the control plane:

| Status | `code` | When |
|---|---|---|
| 501 | `LIGHT_UNSUPPORTED` | the tenant cannot run Light (no control plane, or not the Kubernetes substrate) |
| 503 | `LIGHT_BACKEND_UNAVAILABLE` | the control plane did not answer; nothing was changed by the failed step. `Retry-After` gives seconds |
| 502 | `LIGHT_FAILED` | the control plane answered with an unexpected error |

Management routes also answer `409 LIGHT_TRANSITION_IN_PROGRESS` while another
Light operation on the same agent is running; retry after a few seconds.

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
      "cronjob": "light-be7540cfd160-1a2b3c4d"
    }
  ]
}
```

- `light_state`: `null` for an ordinary agent; `light_state_at` is when it
  last changed.
- `schedules`: the set the agent had at its **last park**, with core's
  suspension record. While the agent is open, its live schedules are at
  `GET /workspaces/{id}/schedules`.
- `pending_actions`: work queued for the agent's next open/park cycle, each
  `{"kind", "source", "queued_at"}`. `refresh_pending` is true when a `refresh`
  is queued (a secret, plugin or config change made while the agent was parked:
  core opens the agent briefly and parks it again so its runs pick the change
  up).
- `last_error`: the last failure of a background Light step,
  `{"code", "message", "at"}`, or `null`.

## `POST /workspaces/{id}/light/enable`

Makes the agent a Light agent. No body.

Core stores two workspace secrets, `MOLECULE_LIGHT=1` (the runtime leaves its
own scheduler off; the control plane refuses to park an agent without it) and
`MOLECULE_CONTINUOUS=false` (the 24/7 switch off, so an idle open agent can be
parked), sets `light_state` to `open`, and restarts the agent so the new env
applies (a hibernated agent is woken instead). It records the agent's 24/7
setting as it was, so disable can put it back.

**200**

```json
{"status": "enabled", "light_state": "open", "restart": "started"}
```

| Field | Values |
|---|---|
| `status` | `enabled`; `unchanged` when the agent already was a Light agent |
| `light_state` | the state after the call |
| `restart` | `started` (restart begun), `woken` (a hibernated agent is waking), `refresh_queued` (a parked agent had lost a Light secret: restored, and picked up by a refresh), `none` (both secrets were already in place) |

The restart is asynchronous: watch the workspace's `status`.

**Errors** (besides the common ones)

| Status | `code` | When |
|---|---|---|
| 403 | `LIGHT_SELF_FORBIDDEN` | called with the workspace's own token |
| 409 | `LIGHT_AGENT_PAUSED` | the workspace is paused; resume it first |
| 409 | `LIGHT_TRANSITION_IN_PROGRESS` | another Light operation is running |
| 422 | `LIGHT_RUNTIME_UNSUPPORTED` | not a hermes agent; `runtime` names its runtime |
| 422 | `LIGHT_PLATFORM_AGENT` | the org's platform agent cannot be a Light agent |
| 501 | `LIGHT_UNSUPPORTED` | the tenant cannot run Light (checked before anything is written) |

## `POST /workspaces/{id}/light/disable`

Turns a Light agent back into an ordinary agent, from any Light state. No body.

Core first deletes the control plane's Light objects (the CronJobs, any run
still going, the run Secret); only when that succeeds does it revoke the run
tokens, remove `MOLECULE_LIGHT`, put the 24/7 setting back exactly as it was
before enable (the workspace's own `MOLECULE_CONTINUOUS` value, or no
workspace value at all), clear `light_state`, drop the stored schedules and
queued actions, and restart the agent (or wake it, when parked). The run
history (`GET /light/runs`) is kept.

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
`409 LIGHT_TRANSITION_IN_PROGRESS`. A `503` leaves everything as it was.

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
| `exit_code`, `output`, `error` | optional; `output` keeps its last 64 KiB, `error` its first 4 KiB |

Unknown keys are ignored. Core upserts on `(workspace_id, run_id)`:

- `started` inserts a `running` row, or refreshes one that is still running.
  When the row is already finished (for example a run that opening the agent
  cancelled) core answers `409 LIGHT_RUN_FINISHED` and the run must not start.
- `finished` records the outcome once. A repeated `finished` is a `200` no-op,
  and a finished run never goes back to `running`.

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
  memory, image pull failure, deadline).
- `activity_count` / `activity_path`: the activity rows the run wrote (its
  tool calls, messages, A2A traffic) and the path that lists them.
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
| `POST /registry/heartbeat`, `/registry/register`, `/registry/update-card` | `409 LIGHT_AGENT_PARKED`, nothing written |
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

## Deleting a Light agent

`DELETE /workspaces/{id}` deletes the agent's control-plane Light objects
before it stops any compute. If that step fails, the delete answers `500` like
any other teardown failure, and the workspace keeps `light_state` so the
leftover objects can be found and deleted later.
