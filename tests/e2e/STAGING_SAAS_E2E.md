# Staging SaaS E2E runbook

The staging E2E workflows provision disposable resources through
`https://staging-api.moleculesai.app`, exercise the deployed tenant stack, and
tear everything down. The workflow YAML files are authoritative for triggers,
cadence, runner labels, and timeout budgets.

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
workflows select the required mode. Every path that creates a tenant must keep
an EXIT/INT/TERM cleanup trap and treat leftover resources as a failure.

## Credential

Load staging `CP_ADMIN_API_TOKEN` from Infisical environment `staging`, path
`/shared/controlplane-admin`, and expose it to a local harness as
`MOLECULE_ADMIN_TOKEN` only for that process. Do not copy the value into this
runbook, a committed env file, or a persistent credential bundle.

```bash
source ~/.molecule-ai/ops.sh
export MOLECULE_ADMIN_TOKEN="$(mol_secret CP_ADMIN_API_TOKEN /shared/controlplane-admin staging)"
bash tests/e2e/test_staging_full_saas.sh
unset MOLECULE_ADMIN_TOKEN
```

`E2E_KEEP_ORG=1` is a local debugging escape hatch. Never set it in CI.

## Completion criteria

A run is complete only when the throwaway tenant/workspaces reach the asserted
states, a real A2A response is observed, and cleanup verifies no resources are
left behind. A successful API create, image push, or pin update by itself is not
an end-to-end pass.
