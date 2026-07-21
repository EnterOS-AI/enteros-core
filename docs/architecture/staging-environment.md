# Staging environment

Staging is reached through the same domain-based access model as production:

- control-plane API: `https://staging-api.moleculesai.app`;
- tenant hosts: the staging domains returned by the control plane;
- SCM and Actions: `https://git.moleculesai.app`;
- secrets: Infisical at `https://key.moleculesai.app`, using the `staging`
  environment and the documented secret path.

There is no Railway, Vercel, operator-host, AWS ECR, or GitHub deployment step
in the current staging runbook.

## Merge-to-staging pipeline

The active tenant pipeline is `.gitea/workflows/staging-tenant-cd.yml`. On an
eligible merge to `main`, it:

1. waits for the exact `staging-<sha>` tenant image in the internal registry;
2. advances the staging tenant-image pin in the control plane;
3. rolls the stateless staging tenant platform containers while preserving
   workspace runtime/session volumes;
4. in parallel with the tenant roll, uses the guarded `local-deploy` daemon to
   pre-pull and exact-`RepoDigests` verify every pinnable runtime selected by
   the control-plane runtime catalog and promoted-pin projections;
5. runs the real staging provision, management-MCP, A2A, and lifecycle gate
   only after both the fleet roll and runtime-image readiness are green;
6. keeps or rolls back the pin/fleet according to that hard gate.

The pipeline is serialized and does not write the production pin or `latest`
tag. A successful staging run is not a production deployment.

Related workflows cover Canvas, external runtime, SaaS APIs, reconciler,
template delivery, and workspace lifecycle. Workflow files are the authority
for triggers and exact test coverage; do not maintain a second cadence table in
prose.

## Credentials

Automation obtains the staging `CP_ADMIN_API_TOKEN` from Infisical path
`/shared/controlplane-admin` in the staging environment. Runtime/default
selectors are read from their documented staging paths. Credentials must be
masked and fetched on demand; they do not belong in a repository secret bundle
or local committed environment file.

Local E2E invocation exports the fetched value as `MOLECULE_ADMIN_TOKEN` for the
duration of the test. Cleanup remains mandatory; keeping a staging org for
inspection is an explicit local-only action and must never be set in CI.

## What proves staging works

A green image build or pin write alone is insufficient. The staging gate must
show:

- the exact image is pullable and the deployed build identity matches;
- every promoted pinnable runtime digest is already present and verified on the
  local-Docker provisioner daemon before a workspace create starts;
- a throwaway tenant/workspace reaches the expected state;
- the selected runtime registers and serves a real A2A turn;
- management-MCP tools are present and callable where required;
- restart, pause/resume, and hibernate/wake behave through the configured
  backend;
- cleanup leaves no tenant or workspace resources behind.

Pin changes affect new or reprovisioned workspaces. Do not claim an
already-running workspace changed images unless the redeployer is wired and the
running instance/digest is verified.
