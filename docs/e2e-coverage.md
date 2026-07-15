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
