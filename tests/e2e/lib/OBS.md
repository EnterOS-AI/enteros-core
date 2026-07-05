# E2E observability (`lib/obs.sh`) — debug a failed e2e from the dashboard

`lib/obs.sh` makes every e2e emit a **structured event per step** to Loki, tagged
so one run renders as a filterable **timeline** in Grafana. A failed e2e is then
debuggable by opening one dashboard — see *which* step failed, its duration, and
the error — instead of digging through raw runner/CP logs.

## What it emits

Each step emits one Loki log line. **Stream labels** (low-cardinality, for
filtering/grouping): `job="e2e"`, `env`, `test`, `run_id`, `step`, `status`.
The **JSON line** carries the rich, queryable fields:

| field | meaning |
|---|---|
| `ts` | UTC ISO-8601 timestamp |
| `run_id` | the run id == `E2E_RUN_ID` == the `e2e-` org slug (the universal link the leak reaper + `run_footprint.sh` use) |
| `test`, `env`, `git_sha`, `host` | run identity |
| `step` | `preflight`, `org_create`, `provision`, `tenant_health`, `canvas`, `concierge_online`, `mcp_tools`, `create_team_a2a`, `create_team_verify`, `assign_work`, `teardown`, `zero_leftover_verify`, `leak_verify`, `run` |
| `status` | `running` \| `pass` \| `fail` \| `timeout` \| `skip` \| `leak` \| `kept` |
| `duration_secs` | step wall-clock (numeric, unwrappable in LogQL) |
| `error` | on failure: the error message (secret-redacted, ≤600 chars) |
| `http_code`, `body`, `org_id`, `slug`, `leak_resource_type`, `leak_owner_op`, … | per-step context |

`status=run` is the per-run **summary** event (overall status, total duration,
`steps_pass`/`steps_fail`) — the row in the "Recent runs" table.

## Design contract (load-bearing)

* **Fail-soft.** A down/slow Loki must never fail or slow an e2e. Every function
  returns 0; the push has a short timeout and a circuit-breaker (one failed push
  trips `_OBS_DOWN` so a down Loki costs *at most one* timeout for the whole run).
* **`set -euo pipefail` safe**, **no hard deps** beyond `curl` (JSON built in pure
  bash — no python3/jq), **secret-safe** (`_obs_redact` scrubs Bearer/JWT/long-hex
  before ship; never pass a raw admin/tenant token as context).

## Config (env)

| var | default | notes |
|---|---|---|
| `OBS_ENABLED` | `1` | `0` disables emission |
| `OBS_LOKI_URL` | `http://localhost:3102` | local obs Loki (`molecule-obs-loki`). CI/staging: point at the obs Loki |
| `OBS_LOKI_TENANT` | unset | optional `X-Scope-OrgID` (multi-tenant Loki) |
| `OBS_LOKI_BEARER` | unset | optional auth bearer — **fetch via Infisical**, never hardcode |
| `E2E_RUN_ID` | generated | run id / slug seed |
| `OBS_ENV` | guessed from `MOLECULE_CP_URL` | `staging`/`prod`/`local` |
| `OBS_GIT_SHA` | `GITHUB_SHA` or `git rev-parse` | |

## How to use it (integration)

```bash
source "$(dirname "$0")/lib/obs.sh"
obs_init "my_test_name"
obs_step_start org_create
... do the step ...
obs_step_end   org_create pass "" "org_id=$ORG_ID"     # or: fail "msg" "http_code=$c"
# attribute a failure/skip raised in fail()/skip_loud()/a timeout to the live step:
fail() { echo "❌ $*" >&2; [ -n "${OBS_RUN_ID:-}" ] && obs_fail_current fail "$*"; exit 1; }
# leak verify (run_footprint integration):
obs_leak container mol-ws-abc "executeOrgPurge:purgeInfra"
obs_run_end "$overall_status"      # from the EXIT trap
```

`test_staging_concierge_creates_workspace_e2e.sh` is the reference integration
(every step instrumented; `fail`/`skip_loud`/timeouts + the teardown leak path
all feed the timeline). `lib/test_obs_lib_unit.sh` is the offline regression test.

## How to view

* Dashboard **`E2E Runs — per-step timeline, durations & failures`** (uid `e2e-runs`),
  version-controlled at `operator-config:obs/grafana/dashboards/e2e-runs.json` and
  auto-provisioned into the local Grafana (folder **Operator**).
* Local Grafana: `http://localhost:3002` (local dev creds `admin`/`admin`).
* Deeplink to a run: `http://localhost:3002/d/e2e-runs?var-run_id=<RUN_ID>&from=now-24h&to=now`
* Panels: ① selected-run step timeline + errors, ② per-step durations, ③ step
  results table, ④ recent failures across all runs, ⑤ recent run summaries.

## Obs stack reachability (audited 2026-06-29) — and the access gaps

The observability stack was relocated off the Hetzner obs box (5.78.196.20) to the
**local PC** per the self-host pivot. Current state:

| component | where | reachable | shipping e2e data? |
|---|---|---|---|
| **Loki** | `localhost:3102` (`molecule-obs-loki`, healthy) | ✅ | ✅ now (via `obs.sh`). Was **empty** — `molecule-obs-vector` ships only Vector's *own* internal logs; there is no promtail/alloy scraping CP/tenant/concierge stdout |
| **Grafana** | `localhost:3002` (11.4.0) | ✅ direct (`admin`/`admin`) | renders the `e2e-runs` dashboard |
| **GlitchTip** (Sentry-compatible) | `localhost:8000` | ✅ up | ❌ nothing sends to it; prod `SENTRY_DSN` is a placeholder (off) |
| **Langfuse** | — | ❌ not running locally | ❌ LLM/agent (concierge/MiniMax/A2A) traces not captured; needs langfuse up + `LANGFUSE_*` in the tenant runtime |

**Access gap — grafana MCP returns 401.** The grafana MCP is still wired to
`GRAFANA_URL=https://obs.moleculesai.app` (the **decommissioning Hetzner** box)
with a `glsa_` service-account token that now 401s; the **local** Grafana is up
but unwired. **Fix:**

1. In local Grafana (`http://localhost:3002`) → Administration → Service accounts →
   create a `viewer`/`editor` account → **Add token**.
2. Store it in Infisical `prod` `/shared/observability` (e.g. `GRAFANA_LOCAL_SA_TOKEN`)
   — do not print/commit it.
3. Update the grafana MCP entry in `~/.claude.json` (and the
   `molecule-mcp-grafana` `.mcp.json`): `GRAFANA_URL=http://localhost:3002`,
   `GRAFANA_SERVICE_ACCOUNT_TOKEN=<the new token>`. Restart Claude Code.

Until then, query Loki/Grafana directly (curl `localhost:3102` / `localhost:3002`)
— both are healthy; only the *MCP credential* is stale.
