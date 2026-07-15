# E2E Testing

End-to-end test scripts live under `tests/e2e/` and exercise the platform against a real Postgres + Redis. Every script is shellcheck-clean and shares helpers from `tests/e2e/_lib.sh` + `tests/e2e/_extract_token.py`.

## Current entry points

| Script | Purpose | Prerequisites |
|---|---|---|
| `test_api.sh` | Core platform API contract used by the required API workflow. | Platform, Postgres, and Redis; no external model required. |
| `test_priority_runtimes_e2e.sh` | Canonical runtime completion smoke; the mock arm is CI-load-bearing and live-model arms are opportunistic. | Platform plus matching provider credentials for any live arms. |
| `test_local_provision_lifecycle_e2e.sh` | Local Docker provision, restart, authenticated A2A, and cleanup lifecycle. | Local Docker and the supported `ADMIN_TOKEN` setup. |
| `test_poll_mode_e2e.sh` | Authenticated poll-mode send, queue, cursor, and ownership behavior. | Platform and test database. |
| `test_staging_full_saas.sh` | Production-shaped staging tenant boot and end-to-end validation. | Staging control-plane admin credential; never point it at production. |

For the maintained cross-runtime developer smoke outside `tests/e2e/`, use
`scripts/test-all-runtimes-a2a-e2e.sh`. The older one-off team, adapter, and
Hermes-plugin demo scripts were removed because they depended on the retired
in-core runtime tree and sent tokenless A2A requests.

## Auth Prerequisites (Phase 30)

After Phase 30.1, the following routes require `Authorization: Bearer <token>` once a workspace has any live token on file (legacy workspaces are grandfathered):

- `POST /registry/heartbeat`
- `POST /registry/update-card`

After Phase 30.6, discovery caller identity is endpoint-specific:

- `GET /registry/discover/:id` requires `X-Workspace-ID`.
- `GET /registry/:id/peers` uses the path `:id` as caller identity and does not
  require `X-Workspace-ID`.

For both routes, enrolled workspace callers present a matching bearer; admin,
org, and verified control-plane session credentials are also accepted. Legacy
workspaces with no live token remain bootstrap-compatible, but authentication
datastore errors fail closed with `503`.

The A2A send and queue-status routes are stricter:

- `POST /workspaces/:id/a2a` requires a workspace bearer, verified human credential, or the verified external-inbound path.
- `GET /workspaces/:id/a2a/queue/:queue_id` applies the same authentication before checking row ownership.
- A workspace bearer determines the source identity. An optional `X-Workspace-ID` must match it.
- Tokenless legacy callers and authentication datastore errors fail closed.
- The no-bearer same-origin Canvas fallback exists only in combined self-host/dev when CP session verification is unconfigured; SaaS tests must use a verified session or bearer.

The scripts handle this in one of three explicit ways:

1. A workspace actor sends its own bearer and matching `X-Workspace-ID`.
2. A human-originated local request uses the `ADMIN_TOKEN` written by
   `scripts/dev-start.sh` (the common helper can load just that value from
   `.env`).
3. A dedicated combined-tenant contract test may opt into the narrow
   same-origin Canvas fallback with `E2E_ALLOW_SAME_ORIGIN_FALLBACK=1`; ordinary
   shell E2Es must not synthesize browser identity from `Origin` alone.

Registration-focused tests still call `POST /registry/register`, extract the
one-time `auth_token` through `_extract_token.py`, and thread that bearer
through heartbeat, discovery, activity, and A2A calls. SaaS tests always use a
real tenant/session/workspace credential; they never rely on the local fallback.

`test_comprehensive_e2e.sh` registers each workspace **immediately after creation** so the provisioner's auto-register doesn't race the test's explicit register. `test_activity_e2e.sh` re-registers a detected-already-online agent to capture a fresh bearer token.

## Running Locally

```bash
# Quickest local platform check (the test safely reads ADMIN_TOKEN from the
# repository .env written by scripts/dev-start.sh when it is not exported):
cd workspace-server && go build ./cmd/server && ./server &
bash tests/e2e/test_api.sh

# Runtime smoke (loads the local dev ADMIN_TOKEN from .env when needed):
bash scripts/test-all-runtimes-a2a-e2e.sh
```

Use each script's header as the source of truth for required services,
credentials, target guards, and cleanup behavior. Do not infer current pass
counts from this document; checks are added frequently.

## What CI runs

- `.gitea/workflows/e2e-api.yml` boots Postgres, Redis, the platform, and its
  local mock upstreams. It runs the API, keyless-feature, user-task,
  notification, channel, priority-runtime, poll-mode, and upload suites.
- `.gitea/workflows/ci.yml` runs ShellCheck across `tests/e2e/*.sh` and the
  selected operational scripts, plus the no-live-infrastructure Bash contract
  tests.
- Staging workflows own real tenant, Canvas, external-runtime, reconciler, and
  lifecycle validation. Their target guards and required credentials are part
  of each workflow/script contract.

## Adding a New E2E Check

1. Source `tests/e2e/_lib.sh` for auth, token extraction, cleanup, and shared assertions.
2. Choose the actor explicitly: workspace bearer for agent traffic, or a
   verified human/admin credential for Canvas-originated traffic. Never add a
   tokenless public A2A call.
3. Keep each check idempotent — the comprehensive script is expected to be re-runnable on the same DB.
4. Run `shellcheck tests/e2e/your_script.sh` locally before pushing.

## Related Docs

- [Local Development](./local-development.md)
- [Platform API](../api-protocol/platform-api.md) — route reference incl. auth requirements
