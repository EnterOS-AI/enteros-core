# RFC: On-demand workspaces — a "24/7" switch, sleep/wake, and a light executor

**Status:** Proposed. Step 1 runtime changes in review: molecule-ai-workspace-runtime#422, #423.
**Related:** [`rfc-scheduler-as-trigger-plugin.md`](rfc-scheduler-as-trigger-plugin.md) (schedules live in each workspace's trigger daemon), runtime#420, runtime#421, core#5237.
**Repos touched:** molecule-ai-workspace-runtime, molecule-ai-workspace-template-hermes, molecule-core (workspace-server + canvas).

## 1. Summary

Every workspace runs as a 24/7 agent today:
- the runtime wakes it 300 s after each turn whenever its digest has changed;
- every turn, whatever triggered it, continues one long hermes session.

This RFC adds a per-workspace switch, `continuous`, which defaults to on. When it is off, a workspace runs only for messages it receives and schedules it fires, and each scheduled run or peer request starts its own bounded session.

Two later steps build on the switch:
- the workspace sleeps when idle and wakes for a message or its next schedule;
- a light executor runs simple scheduled jobs without the full agent harness.

The switch is where the LLM calls are saved; an idle pod makes no calls. Sleep/wake saves compute and memory.

## 2. Measurements (prod, 2026-10-29/30)

LLM calls by workspace come from CP `GET /cp/admin/orgs/<slug>/llm-usage?window=24h`, read at 10-30 17:42Z. The window effectively ends at 12:37Z, when the shared MiniMax key's weekly quota hit 0 (`GET api.minimax.io/v1/token_plan/remains`: `current_weekly_status=2`). Every platform-managed agent was stopped until 11-02 00:00Z. The window is therefore 18.9 h.

| Workspace(s) | LLM calls in 18.9 h | What shipped |
|---|---|---|
| minori Site Fix `0e89e97e` (claude-code) | 4,834 | nothing: `minori/Minori-Skincare` last commit 10-20 |
| jrs (concierge + seo-agent) | 3,714 | commits on open PR branches; nothing merged (merge cap 0) |
| hongming (concierge + Personal Branding) | 2,867 | nothing: Personal Branding waits on Zernio auth; the concierge re-sent the auth link every 15 min |
| dinecall-ai (concierge + SEO) | 2,712 | one PR: #319, a 2-line change, 509 calls and 51.1M prompt tokens for the unit |
| amcan (concierge + Social) | 1,842 | nothing: Social has all schedules disabled |
| minori (concierge, SEO, Social) | 480 | — |
| **Total** | **16,449** (≈ 20.9K/day) | |

Size of a call:
- Site Fix averaged 124K cached prompt tokens per call (600M over 4,834 calls).
- A fresh hermes session already carries 27.9K tokens: the system prompt plus about 56 tool schemas (staging, 10-30).

## 3. Where the calls come from

| Driver | Mechanism | Source |
|---|---|---|
| Idle digest | Fires `idle_fire_after_seconds` = 300 s after a turn whenever the digest has changed. There is no backoff and no daily cap. The prompt says to keep "pulling the next item until the backlog AND your long-term goals are exhausted", and a standing goal never is. `MOLECULE_IDLE_FIRE_SECONDS` has a floor of 1, so it has no "off" value. | runtime `idle_digest/contract.py:166`, `:236`, `:285`; `idle_digest/controller.py` `tick()` |
| Delegation echo | The heartbeat re-injected every inbound peer request as a "delegation result" and told a child to delegate a summary back to its parent. Example: jrs seo-agent, 10:19–12:37 on 10-30, got 15 requests from its parent and 17 such self-wakes. | runtime `heartbeat.py` `_check_activity_delegations`; fixed in runtime#422 |
| One session for every trigger | hermes keys every turn on `workspace:<id>`: chat, peer requests, schedule ticks and self-wakes alike. A tick therefore resumes the day's history, and the session resets only after 1440 idle minutes. | template-hermes `executor.py:2571`, `:510` |
| Restart turns | Each restart injects a restart-context turn. jrs seo-agent restarted 5 times between 10:36 and 11:50 on 10-30. | activity feed |

## 4. Design

### 4.1 The switch: `continuous`

- **On** (the default) keeps today's behavior.
- **Off** changes these things:
  1. **No idle self-wake.** Runtime `MOLECULE_CONTINUOUS=false` arms neither the idle digest nor the legacy static idle loop (runtime#423).
  2. **Inputs still run.** User chat, peer delegations and schedule fires all still run. An agent whose work comes from a schedule (for example DineCall SEO's `*/30` tick) keeps working.
  3. **One session per run.** The hermes `_derive_chat_id` returns a per-run key for scheduled fires (`source_type=self-scheduler`) and for peer requests, so each run starts from the fixed overhead instead of the day's history. Canvas chat keeps `workspace:<id>`. A unit that spans several runs carries its state in files or the repo, as DineCall SEO already does.
  4. **Bounded runs.** The existing turn cap (`MOLECULE_MAX_TURN_SECONDS`) bounds each run.
  5. **No self-made schedules.** Schedule-creating and schedule-enabling tools refuse when the turn's `source_type` starts with `self-`. On 10-30 the jrs concierge created and enabled `zh-guardrail-r4-unit` on its worker during a self-directed turn.
- **Core:** `GET`/`PUT /workspaces/:id/continuous` `{"continuous": false}`, modeled on `SetModel` (`workspace-server/internal/handlers/secrets.go`):
  - stored as the `workspace_secrets` row `MOLECULE_CONTINUOUS`;
  - injected into the pod env at provision like `MODEL`, with a restart on change;
  - not approval-gated, because it is not a credential.
- **Canvas:** a "Runs 24/7" toggle next to the model picker.
- **Which workspaces turn it off:** concierges and every worker driven by schedules or delegation. A concierge with the switch off still answers chat at once; it just never wakes itself. Only an agent meant to work its own backlog with no schedule needs the switch on.

### 4.2 Sleep and wake

What already exists:
- manual hibernate;
- auto-wake on an inbound A2A message, answering 503 with Retry-After while the workspace boots (`workspace-server/internal/handlers/a2a_proxy.go:1258`, #711).

What changes:
- **Idle means no activity, not an old heartbeat.** The monitor's condition `last_heartbeat_at < now() - hibernation_idle_minutes` (`internal/registry/hibernation.go:76`) is never true for a live agent, because the runtime heartbeats every 30 s even when idle (runtime `heartbeat.py:210`; core `registry.go` Heartbeat writes `last_heartbeat_at = now()`). Use a `last_active_at`, stamped when a turn ends or a message is delivered.
- **A setter for `hibernation_idle_minutes`.** No API sets it today. Default it (e.g. 30) when `continuous` is off.
- **Wake for the next schedule.** Schedules live in the pod's trigger daemon (`internal/handlers/schedules_proxy.go:24`), so a sleeping workspace cannot fire them. When core hibernates a workspace, it reads the daemon's next due time over the runtime's `/internal/schedules` API and stores it as `wake_at`. The existing 2-minute hibernation monitor wakes any workspace whose `wake_at` has passed, and the daemon fires the due schedule on boot.
- **No restart turn after a wake from sleep.** The restart context rides on the next real turn instead.
- **Concierges stay awake** so chat is answered immediately.

### 4.3 Light executor

- **Per schedule:** `executor: light`, `tools: [...]` (an allow-list) and `max_steps`.
- **Where it runs:** inside the same workspace image and env, so the credentials and installed plugins/MCP servers are the same, but outside the main agent session.
- **What it sends the model:** only the job prompt and the allow-listed tool schemas. It runs up to `max_steps` steps and posts its result once, to the canvas or the parent.
- **What it is for:** report, check and post jobs. Code-changing units keep the full harness.

## 5. Rollout

1. **runtime#422** (the echo fix). It covers every workspace once the runtime release reaches the fleet image.
2. **runtime#423 plus the core and canvas switch.** Turn it off on concierges and on schedule- or delegation-driven workers.
3. **Per-run hermes sessions**, behind the switch.
4. **Sleep/wake.**
5. **Light executor.**

Measure each step with the 24 h soak: LLM calls per workspace per day, plus the artifacts each workspace shipped.

## 6. Risks

- **Per-run sessions drop the chat history between runs.** Work that spans runs must persist its state. This applies only when the switch is off.
- **Cold starts.** `/workspace` is not persisted, so a worker that clones and builds pays that cost on every wake.
- **Missed schedules** if `wake_at` is wrong. The daemon's catch-up on boot is the backstop.
- **A sleeping worker answers 503 with Retry-After while it boots.** A synchronous `delegate_task` with a 600 s budget can time out during a cold start, so sleeping workers should be called with async delegation.

## 7. Open questions

1. Should the self-made-schedule refusal (4.1 item 5) apply to every workspace, not only those with the switch off?
2. Should new workspaces default to on or off?
3. With per-run sessions, may a scheduled run execute at the same time as a canvas chat turn (two hermes sessions), or should runs stay serialized per workspace?
