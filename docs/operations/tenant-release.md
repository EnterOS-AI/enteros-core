# Tenant releases

Code validation, release qualification, and production inventory have separate
verdicts. A missing production tenant is an inventory incident, not a code or
release failure.

## Staging qualifies the artifact

`staging-tenant-cd` builds on the published candidate, advances staging, rolls its
fleet, verifies runtime images, and tests fresh provisioning, agent tool calls,
and workspace lifecycle. Production requires a completed successful run and all
seven staging jobs, including the rollback audit. The staging pin must identify
that exact full commit, its provenance must bind to that run, and the registry
must still serve the same digest. A merely published image cannot be promoted.
Same-image staging retries refresh their run provenance, so a successful retry
can qualify even when a previous attempt wrote the pin before failing.

## One production action

Run **promote-prod-tenant-pin** on `main`. When production is enabled
(`PROD_AUTO_DEPLOY_DISABLED=false` and `PROD_TENANT_PIN_PROMOTE_ARMED=true`), that
single dispatch:

1. Resolves and verifies the current staging-qualified digest.
2. Records the previous production pin and discovers the production fleet.
3. Updates the production database pin and boot image secret by immutable digest.
4. Rolls existing Kubernetes targets and verifies each serving replica's digest
   and build SHA.
5. Restores the previous production pin and fleet image if promotion or rollout
   fails. The attempted release remains failed even if recovery succeeds.

While frozen or unarmed, the same action produces a read-only plan. Merging code
never deploys production. A retry reconciles the boot secret and retries the
fleet even when the database pin already matches the candidate.
The image publisher does not dispatch production operations. Its legacy
`:latest` alias identifies a CI-green build; production uses the qualified
digest and does not deploy from that alias.

This driver supports the current Kubernetes production fleet and one production
control-plane database. It refuses a different substrate or multiple production
endpoints before mutation. `PROD_CP_BASE_URLS` defaults to the production API;
legacy staging entries are excluded from all production writes. The
`CP_PROD_NAMESPACE`, `CP_PROD_DB_POD`, `CP_PROD_DB_USER`, and `CP_PROD_DB_NAME`
variables identify that database for the shared census resolver.

## Inventory and rollout outcomes

Discovery completes before any tenant is changed. An absent workload is reported
as `DRIFT:no-workload`, counted as `missing`, and excluded from release targets.
It neither becomes a successful rollout nor makes other tenants' rollouts fail.
Zero-replica deployments remain suspended. A production environment with no
active targets can still receive the pin for future provisions; its report
explicitly shows zero tenants rolled.

Unreadable or ambiguous inventory fails before mutation. An existing target that
disappears, fails to become ready, serves the wrong digest, or reports the wrong
build remains a release failure. Recovery restores the previous production pin;
it does not recover tenant data or recreate missing tenants. Pin/secret and
workload recovery are both attempted even if one fails. Recovery failures
remain visible and require investigation.

Run **redeploy-tenants-on-main** for a read-only, strict inventory audit (the
filename is retained for existing dispatch callers). Missing workloads make that
audit red, independently of release success. The live **pin-provenance-guard**
audit likewise runs only on manual dispatch from `main`; its push and PR runs
validate code without accessing production. The merge train retains required
code checks and reports these production outcomes without using them as code
merge prerequisites.
