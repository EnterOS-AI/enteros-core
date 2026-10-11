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
