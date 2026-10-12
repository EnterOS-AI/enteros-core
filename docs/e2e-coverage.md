# E2E coverage map

Active workflow files under `.gitea/workflows/` are the executable source of
truth for triggers, jobs, providers, and schedules. Required status contexts are
declared in `.gitea/required-contexts.txt`. Do not copy either into this page as
a fixed matrix: provider migrations and protected checks change independently.

## Main coverage families

| Workflow family | Boundary exercised |
|---|---|
| `e2e-api.yml` and local lifecycle workflows | Local registry, workspace lifecycle, A2A, and API contracts |
| `e2e-chat.yml` | User message through runtime response and Canvas-visible activity |
| `e2e-peer-visibility.yml` | `parent_id` hierarchy, peer discovery, and communication authorization |
| `e2e-staging-saas.yml` | Control-plane-backed staging lifecycle lanes selected by the workflow's current provider configuration |
| `e2e-staging-canvas.yml` | Browser/Canvas staging behavior |
| external/runtime-specific staging workflows | Registration, heartbeat, delivery, and runtime integration contracts |
| merge-queue and required-context tests | Exact protected-status accounting and merge gating |

Some files retain explicitly dormant compatibility jobs for a retired provider.
A dormant job or historical comment is not evidence that the provider is active.
Read the actual trigger and `if:` expressions before reporting coverage.

## Chat browser isolation

`e2e-chat.yml` starts Chromium in a Docker bridge network namespace, using
`start-playwright-server.sh`. The installed Playwright version selects the
matching browser image and protocol package. The browser connects through
`PLAYWRIGHT_WS_ENDPOINT`; Playwright forwards only loopback destinations to the
test client. Canvas, the platform, and per-test HTTP/WebSocket servers therefore
keep their existing URLs, origins, CSP, and authentication behavior.

This isolates Chromium from other jobs creating and deleting host interfaces,
which caused `ERR_NETWORK_CHANGED` bursts against static assets (core#5106).
The job still fails on any static-asset network error, with zero test retries.
Server startup failure is fatal; it never falls back to a host-network browser.
The browser container uses a per-run name, is removed in `always()` cleanup,
and is covered by the existing two-hour stale-container sweep.

Local Playwright runs need no container unless `PLAYWRIGHT_WS_ENDPOINT` is set.

## Required-check rule

A check can be protected only if it emits a terminal result for every relevant
PR. Workflows with path/provider conditions need an always-emitted gate or
explicit no-op success; otherwise branch protection waits for a context that
never appears.

Never bypass a failing required E2E with an admin merge. Fix the defect, repair
the check, or change protection through the reviewed branch-protection process.

## Verification checklist

1. Confirm the exact head SHA.
2. Read the active workflow trigger and job conditions.
3. Inspect the run endpoint and every job conclusion.
4. Distinguish an intentional skip/dormant provider from an executed E2E.
5. Validate the user/runtime-visible result where the workflow claims to deploy
   or promote.
