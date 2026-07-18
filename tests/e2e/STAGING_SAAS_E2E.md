# Staging SaaS E2E runbook

The staging E2E workflows provision disposable resources through
`https://staging-api.moleculesai.app` on the current local-Docker backend,
exercise the deployed tenant stack, and tear everything down through the
control plane. The workflow YAML files are authoritative for triggers, cadence,
runner labels, and timeout budgets.

## Main coverage

- `.gitea/workflows/e2e-staging-saas.yml`: control-plane/tenant API, workspace
  creation, registration, A2A, memory, delegation, and cleanup.
- `.gitea/workflows/e2e-staging-canvas.yml`: current Canvas workspace-panel
  interactions against a real staging org.
- `.gitea/workflows/e2e-staging-external.yml`: external-runtime registration,
  heartbeat, and delivery.
- `.gitea/workflows/e2e-staging-reconciler.yml`: backend reconciliation and
  recovery.
- `.gitea/workflows/e2e-workspace-lifecycle-staging.yml`: restart,
  pause/resume, hibernate/wake, and A2A survival.
- `.gitea/workflows/staging-tenant-cd.yml`: exact-image staging rollout plus a
  hard provision/management-MCP/A2A/lifecycle gate and rollback chain.

The shared shell harness is `tests/e2e/test_staging_full_saas.sh`; individual
workflows select the required mode. The active local-Docker lanes in
`e2e-staging-saas.yml` that use these shell harnesses keep EXIT/INT/TERM cleanup
traps. Destructive teardown is enabled only after a successful create returns a
valid creation-returned org ID. Cleanup requires that ID to match the exact slug
before DELETE, then normally validates the DELETE response's purge and org
identities, the exact completed purge audit, and that
`/cp/admin/tenants/<slug>/boot-events?limit=1` returns HTTP 404. That endpoint
resolves the exact tenant identity before listing events; the admin roster is
used only to rediscover the exact slug/ID pair already published by a successful
create and is not absence proof. A missing run ID or verified creation identity
makes the safety net visibly inconclusive and authorizes no roster request or
DELETE. If the synchronous delete response is lost during local-Docker network
detach, cleanup can recover only from a completed purge audit for the same
creation-returned slug/org ID recorded no earlier than that DELETE attempt, plus
the same exact structured HTTP 404 absence proof; a missing, stale, or malformed
audit remains a hard failure. A generic Actions runner does not directly
enumerate provider resources, so its logs must never
describe that unperformed scan as a pass. Other staging workflow families retain
separate teardown contracts; `molecule-ai/internal#639` tracks the remaining
convergence.

The Go staging suites use `e2eSlug(<tag>)`, which embeds `GITHUB_RUN_ID` and a
five-digit uniqueness suffix in CI. Their primary path remains the test's
fail-closed `t.Cleanup` exact delete. A process timeout or runner SIGKILL cannot
run `t.Cleanup`, so every credential-bearing Go job invokes the shared
`tests/e2e/lib/go_e2e_run_teardown.sh` safety net with only the exact tags that
job creates. The helper accepts only a full
`e2e-<tag>-<run-id>-<six-hex>` match, obtains the matching org ID from the
staging roster, and delegates deletion and proof to `cp_purge_receipt.sh`.
Cleanup failure remains visible; the automatic main-push janitor is only a
delayed final backstop after its conservative 90-minute age floor.

The full-SaaS scheduler checks are volume-authoritative. Step 10f resolves the
workspace under test, accepts daemon-readiness evidence only from that exact
container's `schedule-health.json`, and requires both explicit-ID and omitted-ID
self-mode creates to appear on its own `schedules.yaml` grid. A UUID response is
the only signal that permits a bounded create retry because it proves the
request reached the retired database path while capability propagation was
stale; name-keyed volume responses and visible grid entries switch to poll-only
to avoid duplicate creates. On failure, the harness prints the target's bounded
grid, health, armed-state, and history snapshots before the wider container
context. A sibling workspace heartbeat is never target readiness evidence.

## Credential

Load staging `CP_ADMIN_API_TOKEN` from Infisical environment `staging`, path
`/shared/controlplane-admin`, and expose it to a local harness as
`MOLECULE_ADMIN_TOKEN` only for that process. Do not copy the value into this
runbook, a committed env file, or a persistent credential bundle.

```bash
source ~/.molecule-ai/ops.sh
E2E_INFRA_BACKEND=local-docker \
  MOLECULE_ADMIN_TOKEN="$(mol_secret CP_ADMIN_API_TOKEN /shared/controlplane-admin staging)" \
  bash tests/e2e/test_staging_full_saas.sh
```

`E2E_KEEP_ORG=1` is a local debugging escape hatch. Never set it in CI.

## Completion criteria

A run is complete only when the throwaway tenant/workspaces reach the asserted
states, a real A2A response is observed, and cleanup proves the exact completed
purge audit plus an HTTP 404 from the exact-tenant boot-events endpoint. This is
control-plane purge evidence, not a direct provider-resource scan. A successful
API create, image push, or pin update by itself is not an end-to-end pass.
