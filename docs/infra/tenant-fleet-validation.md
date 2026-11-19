# Tenant fleet rollout validation

`scripts/deploy/redeploy-tenant-fleet-k8s.sh` joins the control-plane substrate
census to Deployments by org ID. A missing workload, empty unasserted fleet, or
rollout with no eligible tenants fails the deployment. Investigate lifecycle and
backup records before reconciling a missing tenant; do not suppress the census
failure merely to complete a rollout.

After Kubernetes reports rollout completion, verification captures the names and
actual image IDs of ready tenant containers in nonterminating Running pods. Both
the digest check and each pod's `/buildinfo` check use this snapshot. This matters
on rollback: Kubernetes can still list the superseded pod first while it drains.
Every serving replica must match the target. Missing image IDs, mixed digests,
unreadable pods, and mismatched build information fail verification.

The roller only looks for a tenant in one namespace. That is the namespace the
census records as `org_substrates.instance_id`, or `enteros-<slug>` when that
column is empty. A tenant found nowhere else fails as `FAIL:no-workload`, and an
API error such as a missing role fails as `FAIL:lookup-error`. Each pod's
`/buildinfo` is read through the API server's pod proxy, not `exec`. So the
credential needs, in each tenant namespace only, Deployments
get/list/watch/patch, Pods list and `pods/proxy` get. The proxy can reach only
pods that the tenant NetworkPolicy lets the API server reach, and a `/buildinfo`
read that cannot connect fails verification.

In staging those grants are RoleBindings in an explicit list of tenant
namespaces. There is no cluster-wide grant and no policy that generates grants
per namespace. The staging lane declares that list in `ROLLER_SCOPE_NAMESPACES`.
Only a declared scope can skip anything. A tenant outside it is reported as
`skipped-out-of-scope`, and nothing is called in its namespace.

Every tenant inside the declared scope must first pass `kubectl auth can-i patch
deployments -n <ns>`, and so must every tenant when no scope is declared, as in
production. Anything but a clean "yes" fails the tenant, because a deleted
namespace and a missing binding both answer "no".

A run fails if no tenant was in scope. It also fails unless every namespace in
`REQUIRED_TENANT_NAMESPACES`, and every declared scope entry, ends rolled and
verified (in `--dry-run`, planned). Verified means through `/buildinfo` when a
git sha is known. When none is known, it means the imageID digest of every
serving pod, which is what a revert to a pin recorded without a sha relies on.
The default required namespace is the staging canary,
`enteros-staging-canary-k8s`. The production lane requires none and declares no
scope.

Run the local regression suites with Bash:

```sh
bash scripts/deploy/tests/test-k8s-fleet-roller.sh
bash scripts/deploy/tests/test-staging-fleet-k8s-wiring.sh
```

These cover draining pods, multiple serving replicas, stale or missing image
identity, census drift, and the staging workflow's census and credential wiring.
The staging CD workflow additionally provisions a fresh tenant and checks that
its build matches the promoted candidate. A successful existing-fleet rollout
alone cannot prove fresh provisioning consumes the current pin.

CI reads `K8S_FLEET_KUBECONFIG` from Infisical `/shared/controlplane` separately
for `prod` and `staging`. It is a base64 kubeconfig with the cluster CA and the
environment's control-plane service-account credential. Rotate the Kubernetes
token source and its Infisical projection together. Do not substitute a runner's
deployment-only service account or distribute an operator's admin kubeconfig.

Production's deployment freeze still permits the read-only census audit. Pin
provenance and census failures remain blocking while frozen; green requires
valid deployment evidence and reconciled inventory, not disabling those checks.
