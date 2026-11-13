# RFC: Enter OS Light runs (isolated, scheduled copies of one agent)

Status: implemented (core: Enter OS Light parts 1 and 2; control plane:
molecule-controlplane#3064; runtime v0.4.95; hermes `26d78cb`).
Related: #5238 (on-demand workspaces RFC). That RFC's step 5, a "light
executor" inside the agent's own pod, is refined here into an isolated pod per
scheduled fire.

The REST contract for clients is `docs/api-protocol/light-api.md`. This page is
the design behind it.

## What a Light agent is

A Light agent is one stored definition (an ordinary hermes agent: its volume
with skills, files, references and config; its env, MCP servers and
credentials) plus its schedules. Each scheduled fire runs a fresh, isolated
copy of that definition in its own short-lived pod, so five schedules due
together run as five copies at once. Nothing runs between fires.

The owner's requirements: scheduled tasks only; a run gets the agent's
material and shared memory; it starts at its fire time; schedules due
together run on separate copies of one agent; their data is isolated; fast and
resource friendly.

## Life of one run

1. **Park.** The owner sets the agent up as a normal agent, then parks it.
   Core hands its schedules to the control plane, which snapshots the agent's
   env into one Secret and writes one CronJob per schedule. The agent's pod is
   stopped; its volume is kept.
2. **Fire.** The cluster's CronJob controller creates a Job at the schedule's
   minute, in its time zone. No platform service is called to start it.
3. **Copy.** An init container mounts the agent's volume read-only and copies
   it into a private emptyDir, leaving out conversation history, logs and
   credentials files, and writes the run's token.
4. **Run.** The main container boots the image's normal start script with the
   runtime in once mode: no registration, no server, no heartbeat. It sends the
   schedule's prompt through the engine's executor once and waits for the turn
   to settle.
5. **Report.** It reports `started` and `finished` to core's run ledger, with
   the final text, and exits with the outcome as its exit code.
6. **Gone.** The pod and its private volumes are deleted.

| What | Where a copy gets it | Who sees what a run changes |
|---|---|---|
| Skills, prompt files, references, plugins | copied from the agent's volume at start | nobody (deleted at exit) |
| Env, MCP servers, credentials | the Light Secret written at park | read-only |
| Platform identity | the agent's id and the run token | the same for every copy |
| Working files, conversation | an empty private directory, a fresh session | nobody |
| Shared memory | core, through the usual tools | every copy, by design |
| Results | the run ledger, messages to the owner, shared memory | the owner |

## States and transitions (core)

`workspaces.light_state`: `NULL` (not Light) | `open` | `parking` | `parked`
| `opening`.

- **Park** (`open → parking → parked`): checks (hermes, online, idle, the
  `MOLECULE_LIGHT=1` secret), claim `parking`, read the grid through the
  schedules proxy, render (a schedule core suspended goes out `enabled:false`
  unless the owner parks or its definition changed), mint a run token, C4
  `PUT`, store the set; then hibernate the agent through the ordinary path and
  C4 `resume`, retried while the control plane answers `LIGHT_AGENT_AWAKE`
  (the pod's 30 s grace). Any refusal before the pod stops returns to `open`.
- **Open** (`parked → opening → open`): C4 `pause` first, while every wake
  path still refuses the agent; record the runs the pause cancelled; claim
  `opening`; wake. The monitor flips `opening → open` once the pod is up.
- **Run now**: C4 `run` on a parked agent.
- **Disable**: C4 `DELETE` first, then revoke the run tokens and clear the
  Light state, secrets, stored schedules and queued work.

**Closing the wake paths.** While `parking` or `parked`, nothing may wake the
agent behind the control plane's back. Each wake path has a guard (a read of
`light_state` that produces a `409`, failing open on a database error) and,
for safety, every write that wakes, re-provisions or revives a row carries
`COALESCE(light_state, '') NOT IN ('parking', 'parked')`, so a parked row
cannot be claimed even when the read failed. An AST test pins every
wake/restart/provision call site so a new one is reviewed for Light.

**Run tokens.** API-kind workspace tokens, minted at each park. The previous
one is retired (runs of the last park may still hold it) and revoked by the
monitor once no run is unfinished; the one before it is revoked at the park.
At most two are live.

**Queued work.** A change that would restart a parked agent (secret, model,
plugin) is queued in `light_pending_actions` as a `refresh` instead. The
monitor opens the agent (`light_opened_by = 'refresh'`), applies any queued
action that has an applier, and parks it again as soon as it is online and
idle; the park's env snapshot completes the refresh. The table's `kind` is
free text with a JSONB payload, so a later kind (for example a schedule edit
made while parked) needs an applier, not a schema change.

## Monitor (core)

Every 30 s: finish or continue interrupted parks and opens; auto-park an open
agent idle for 30 minutes; open at most two parked agents with queued work at
a time; delete the Light objects a workspace delete could not. Every 5
minutes: read each agent's jobs listing, record runs that never reported
(killed, out of memory, image pull, deadline, quota), suspend a schedule whose
last three terminal runs failed and tell the owner, revoke a retired run token
once no run holds it. Hourly: delete finished runs older than 30 days.

## Contracts

- **C1, run pod env** (control plane renders; runtime and hermes read):
  `MOLECULE_RUN_MODE=once`, `MOLECULE_RUN_SCHEDULE`, `MOLECULE_RUN_PROMPT`,
  `MOLECULE_RUN_ID` (the Job name), `MOLECULE_RUN_DEADLINE_SECONDS` (default
  3600), `MOLECULE_WORKSPACE_TOKEN` (the run token), `MOLECULE_LIGHT=1`, and
  the agent's env snapshot minus boot-only keys. With `MOLECULE_LIGHT=1` the
  runtime never starts the agent's own scheduler daemon.
- **C2, copy step**: `molecule-runtime-light-copy --src /mnt/agent --dst
  /mnt/run --token-env MOLECULE_WORKSPACE_TOKEN`, with the engine's leave-out
  list from `MOLECULE_LIGHT_COPY_EXCLUDE_FILE`.
- **C3, run ledger**: `POST /workspaces/{id}/light/runs` with the run token;
  `started` then `finished`; core upserts on `(workspace_id, run_id)` and a
  finished run never goes back to running. **C3b**: every request a run pod
  makes carries `X-Molecule-Run-Id`; core tags the activity rows it writes.
- **C4, core ↔ control plane**: `PUT/DELETE /cp/workspaces/{id}/light`,
  `POST .../resume`, `.../pause`, `.../run`, `.../schedules/{name}/suspend`,
  `GET .../jobs`; Kubernetes only (Docker answers 501).
- **C5, core Light state and API**: `docs/api-protocol/light-api.md`.

## Limits

| Limit | Why | Instead |
|---|---|---|
| A message does not reach a parked agent | waking it would start a second, non-isolated lane | open the agent to chat |
| Edits need the agent open | schedules, skills and secrets live on its volume and in its env | open, edit, park (a queued edit while parked can ride the refresh cycle later) |
| No files carried between runs | a run's volumes are deleted | shared memory, or send the result |
| Every copy has the agent's identity | one id and one run token | run-scoped tokens later |
| A platform-LLM agent's run pods hold the tenant admin token | its LLM key is the admin token (#5269) | fix in the LLM proxy's credential |
| Provider refusals count as succeeded | hermes reports them as reply text | hermes classifies them as failed (follow-up) |
| Kubernetes only | CronJobs hold the clock | Docker tenants are refused |
