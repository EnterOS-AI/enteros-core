#!/usr/bin/env python3
"""lint_staging_tenant_cd_gate_chain — the staging tenant fleet roll cannot be
un-gated.

The gated staging tenant CD lives at .gitea/workflows/staging-tenant-cd.yml:

    await-image -> advance-pin -> redeploy-fleet -------------> e2e-smoke
                              -> runtime-image-readiness ------>

It is enforced INTRA-workflow by Gitea `needs:` (a red upstream job skips every
downstream job). The pin advances before the local-docker staging fleet rolls to
the candidate image. In parallel, runtime-image-readiness pre-pulls every
promoted pinnable runtime digest on the exact local-deploy daemon; e2e-smoke
cannot start until both paths are green, so its first workspace provision never
spends the control plane's 90-second request budget on a 700MiB-1.3GiB cold
pull.
rollback-pin must always run and restore the old pin + fleet image whenever any
candidate-path job fails or is skipped. CP-host redeploys are provider-owned and
must not be wired into this tenant-image CI path. That gate runs on
`push` to main (post-merge), so it cannot itself be a pre-merge
branch-protection context — but the INVARIANTS that make it a real gate CAN be
checked statically, pre-merge, and made merge-blocking via `ci / all-required`.

This lint is that mechanical guard. It fails the build if anyone:

  1. breaks the gate chain — advance-pin must `needs:` await-image,
     redeploy-fleet and runtime-image-readiness must each directly `needs:`
     advance-pin, and e2e-smoke must directly `needs:` both. Direct edges keep
     an `if: always()` bridge from converting a red gate into green input, and
     every candidate-path job must retain the default success-only condition; OR
  2. breaks rollback coverage — rollback-pin must `needs:` every candidate-path
     job and run under `if: always()`; OR
  3. lets e2e-smoke run without binding E2E_EXPECT_TENANT_BUILD_SHA on normal
     push runs — without it, the staginge2e candidate-build guard silently skips
     and can validate the pre-roll fleet; OR
  4. adds the `continue-on-error` key to any gating job (advance-pin,
     e2e-smoke, redeploy-fleet, runtime-image-readiness, rollback-pin) OR to any
     step inside one — expressions can resolve true at run time, and true rolls
     a failed step up to a SUCCESS job status (Gitea Quirk #10 / mc#1982), which
     would let a downstream `needs:`-dependent job run despite a real failure;
     OR
  5. moves or weakens runtime-image-readiness: it must run the canonical
     checkout -> local-deploy guard -> readiness consumer shape on exactly the
     `local-deploy` runner with staging/Infisical inputs, with no workflow/job
     execution overrides that can redirect or neutralize that shape; OR
  6. reintroduces provider-specific CP deploy wiring (Railway CLI/tokens or the
     legacy reload-cp-candidate job) into this tenant-image CI path. Preparing
     the selected provider's runtime cache is not a CP deploy/reload.

The dependency checks are behavior-based (they parse the YAML `needs:` graph).
The one daemon-mutating readiness job is intentionally name- and shape-pinned so
the rollback result mapping and exact local-deploy boundary cannot drift.

SSOT: the gate shape lives in staging-tenant-cd.yml. This lint encodes the
contract; if the gate is ever intentionally restructured, update both. Mirrors
molecule-controlplane/.gitea/scripts/lint_deploy_gate_chain.py.
"""

import os
import sys

try:
    import yaml
except ImportError:
    print("FAIL: PyYAML not available", file=sys.stderr)
    sys.exit(2)

DEFAULT_WORKFLOW = ".gitea/workflows/staging-tenant-cd.yml"
PINNED_CHECKOUT_ACTION = "actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd"
READINESS_JOB = "runtime-image-readiness"
READINESS_GUARD_COMMAND = "bash scripts/deploy/require-local-deploy-daemon.sh"
READINESS_ACTION_COMMAND = "bash scripts/deploy/prepare-staging-runtime-images.sh"
READINESS_ACTION_ENV = {
    "CP_BASE_URL": "https://staging-api.moleculesai.app",
    "INFISICAL_BASE": "https://key.moleculesai.app",
    "INFISICAL_ENV": "staging",
    "INFISICAL_CLIENT_ID": "${{ secrets.INFISICAL_CI_CLIENT_ID }}",
    "INFISICAL_CLIENT_SECRET": "${{ secrets.INFISICAL_CI_CLIENT_SECRET }}",
    "INFISICAL_PROJECT_ID": "${{ secrets.INFISICAL_CI_PROJECT_ID }}",
}
READINESS_JOB_ALLOWED_KEYS = {"name", "needs", "runs-on", "timeout-minutes", "steps"}
FORBIDDEN_WORKFLOW_ENV_KEYS = {
    "BASH_ENV",
    "ENV",
    "PATH",
    "DOCKER_HOST",
    "DOCKER_CONTEXT",
    "DOCKER_TLS_VERIFY",
    "DOCKER_CERT_PATH",
    "MOLECULE_PROD_DOCKER_HOST",
    "INFISICAL_BASE",
    "INFISICAL_ENV",
}


def workflow_path():
    return os.environ.get("STAGING_TENANT_CD_PATH", DEFAULT_WORKFLOW)


# job_key -> the job_key it must directly depend on. A transitive bridge could
# use `if: always()` and convert a red upstream into a green dependency, so only
# direct edges preserve failure propagation.
REQUIRED_EDGES = [
    ("advance-pin", "await-image"),
    ("redeploy-fleet", "advance-pin"),
    (READINESS_JOB, "advance-pin"),
    ("e2e-smoke", "redeploy-fleet"),
    ("e2e-smoke", READINESS_JOB),
]

# Jobs whose failure MUST skip everything downstream — so continue-on-error
# (which masks a failure as success) is forbidden on them.
GATING_JOBS = [
    "await-image",
    "advance-pin",
    "redeploy-fleet",
    READINESS_JOB,
    "e2e-smoke",
    "rollback-pin",
]
SUCCESS_ONLY_JOBS = ["advance-pin", "redeploy-fleet", READINESS_JOB, "e2e-smoke"]
ROLLBACK_NEEDS = [
    "advance-pin",
    "redeploy-fleet",
    READINESS_JOB,
    "e2e-smoke",
]
FORBIDDEN_JOB_KEYS = ["reload-cp-candidate"]
FORBIDDEN_CI_MARKERS = [
    "@railway/cli",
    "railway ",
    "railway\t",
    "RAILWAY_",
    "reload-staging-controlplane.sh",
]


def load_workflow(path):
    with open(path) as f:
        doc = yaml.safe_load(f)
    if not isinstance(doc, dict):
        raise ValueError(f"{path}: not a mapping")
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        raise ValueError(f"{path}: no jobs: block")
    return doc, jobs


def needs_of(job):
    n = job.get("needs")
    if n is None:
        return []
    if isinstance(n, str):
        return [n]
    if isinstance(n, list):
        return [x for x in n if isinstance(x, str)]
    return []


def has_continue_on_error(item):
    return isinstance(item, dict) and "continue-on-error" in item


def always_if(job):
    value = job.get("if", "")
    return isinstance(value, str) and value.strip() == "always()"


def steps_of(job):
    steps = job.get("steps")
    return steps if isinstance(steps, list) else []


def run_command_of(step):
    if not isinstance(step, dict):
        return ""
    run = step.get("run")
    return run.strip() if isinstance(run, str) else ""


def runner_labels_of(job):
    labels = job.get("runs-on")
    if isinstance(labels, str):
        return [labels]
    if isinstance(labels, list):
        return [label for label in labels if isinstance(label, str)]
    return []


def run_defaults_of(scope):
    if not isinstance(scope, dict):
        return {}
    defaults = scope.get("defaults")
    if not isinstance(defaults, dict):
        return {}
    run_defaults = defaults.get("run")
    return run_defaults if isinstance(run_defaults, dict) else {}


def env_of(scope):
    if not isinstance(scope, dict):
        return {}
    env = scope.get("env")
    return env if isinstance(env, dict) else {}


def values_contain(value, markers):
    if isinstance(value, str):
        return [m for m in markers if m in value]
    if isinstance(value, dict):
        hits = []
        for v in value.values():
            hits.extend(values_contain(v, markers))
        return hits
    if isinstance(value, list):
        hits = []
        for v in value:
            hits.extend(values_contain(v, markers))
        return hits
    return []


def main():
    workflow = workflow_path()
    if not os.path.isfile(workflow):
        print(
            f"FAIL: {workflow} is missing — the gated staging tenant CD "
            f"workflow must exist."
        )
        return 1
    try:
        workflow_doc, jobs = load_workflow(workflow)
    except Exception as e:
        print(f"FAIL: cannot parse {workflow}: {e}")
        return 1

    fails = []

    # 1. Every gating job must exist.
    for jk in GATING_JOBS:
        if jk not in jobs:
            fails.append(
                f"gate job `{jk}` is missing from {workflow} — the "
                f"await-image→advance-pin→redeploy-fleet→e2e-smoke "
                f"chain is broken."
            )

    # 2. The chain edges must be direct so an always-run bridge cannot mask red.
    for downstream, upstream in REQUIRED_EDGES:
        if downstream not in jobs or upstream not in jobs:
            continue  # already reported missing above
        if upstream not in needs_of(jobs[downstream]):
            fails.append(
                f"`{downstream}` does not directly `needs:` `{upstream}` "
                f"in {workflow} — a red `{upstream}` would NOT skip "
                f"`{downstream}`, so the gate can be bypassed."
            )

    for jk in SUCCESS_ONLY_JOBS:
        job = jobs.get(jk)
        if isinstance(job, dict) and "if" in job:
            fails.append(
                f"`{jk}` must use the default success-only condition in {workflow}; "
                f"a job-level `if` can override failed-needs skipping."
            )

    # 3. Rollback must cover every post-pin path and must always run.
    rollback = jobs.get("rollback-pin")
    if isinstance(rollback, dict):
        direct_needs = set(needs_of(rollback))
        missing = [need for need in ROLLBACK_NEEDS if need not in direct_needs]
        if missing:
            fails.append(
                f"`rollback-pin` does not directly `needs:` {missing} in "
                f"{workflow} — it would not have every candidate-path result "
                f"available and may fail to restore the old staging pin/fleet image."
            )
        if not always_if(rollback):
            fails.append(
                f"`rollback-pin` must use `if: always()` in {workflow} so it "
                f"runs after failed or skipped candidate-path jobs."
            )

    # 4. Runtime readiness is a deliberately narrow cross-provider boundary:
    # read CP projections, then mutate only the exact local-deploy daemon cache.
    readiness = jobs.get(READINESS_JOB)
    if isinstance(readiness, dict):
        extra_job_keys = sorted(set(readiness) - READINESS_JOB_ALLOWED_KEYS)
        if extra_job_keys:
            fails.append(
                f"`{READINESS_JOB}` execution schema has forbidden key(s) "
                f"{extra_job_keys} in {workflow}; job-level env/defaults/if/"
                f"container/strategy can redirect or neutralize the daemon guard."
            )
        if runner_labels_of(readiness) != ["local-deploy"]:
            fails.append(
                f"`{READINESS_JOB}` must run on exactly `local-deploy` in "
                f"{workflow}; got {runner_labels_of(readiness)}."
            )
        if readiness.get("timeout-minutes") != 30:
            fails.append(
                f"`{READINESS_JOB}` timeout-minutes must be exactly 30 in "
                f"{workflow} (20-minute script budget plus runner overhead)."
            )
        steps = steps_of(readiness)
        if len(steps) != 3:
            fails.append(
                f"`{READINESS_JOB}` must have exactly checkout, daemon guard, "
                f"and readiness action steps in {workflow}."
            )
        else:
            checkout, guard, action = steps
            if (
                not isinstance(checkout, dict)
                or checkout.get("uses") != PINNED_CHECKOUT_ACTION
                or set(checkout) - {"name", "uses"}
            ):
                fails.append(
                    f"`{READINESS_JOB}` must start with the pinned, unredirected "
                    f"checkout action in {workflow}."
                )
            if (
                not isinstance(guard, dict)
                or run_command_of(guard) != READINESS_GUARD_COMMAND
                or set(guard) - {"name", "run"}
            ):
                fails.append(
                    f"`{READINESS_JOB}` must run `{READINESS_GUARD_COMMAND}` "
                    f"unconditionally after checkout in {workflow}."
                )
            if (
                not isinstance(action, dict)
                or run_command_of(action) != READINESS_ACTION_COMMAND
                or set(action) - {"name", "env", "run"}
            ):
                fails.append(
                    f"`{READINESS_JOB}` must end with the canonical "
                    f"`{READINESS_ACTION_COMMAND}` action in {workflow}."
                )
            elif action.get("env") != READINESS_ACTION_ENV:
                fails.append(
                    f"`{READINESS_JOB}` readiness action env must be the exact "
                    f"staging CP + Infisical SSOT mapping in {workflow}."
                )

        for field in ("shell", "working-directory"):
            if field in run_defaults_of(workflow_doc):
                fails.append(
                    f"workflow-level `defaults.run.{field}` is forbidden in "
                    f"{workflow}; it can redirect or replace execution of the "
                    f"local-deploy daemon guard."
                )
        forbidden_env = sorted(
            set(env_of(workflow_doc)).intersection(FORBIDDEN_WORKFLOW_ENV_KEYS)
        )
        if forbidden_env:
            fails.append(
                f"workflow-level env has forbidden execution/endpoint key(s) "
                f"{forbidden_env} in {workflow}; trusted Docker endpoint values "
                f"must come only from local-deploy runner configuration."
            )

    # 5. e2e-smoke must bind the expected candidate SHA before the staging e2e.
    # The e2e has a local-run skip mode when the variable is absent; in this
    # workflow absence would let the hard gate validate a stale/pre-roll fleet.
    e2e = jobs.get("e2e-smoke")
    if isinstance(e2e, dict):
        env_hits = values_contain(e2e.get("env", {}), ["E2E_EXPECT_TENANT_BUILD_SHA"])
        step_hits = values_contain(steps_of(e2e), ["E2E_EXPECT_TENANT_BUILD_SHA"])
        if not env_hits and not step_hits:
            fails.append(
                f"`e2e-smoke` never binds E2E_EXPECT_TENANT_BUILD_SHA in {workflow} "
                f"— staginge2e would skip its tenant /buildinfo candidate-SHA "
                f"guard and could validate a stale fleet."
            )

    # 6. No continue-on-error on a gating job.
    for jk in GATING_JOBS:
        job = jobs.get(jk)
        if has_continue_on_error(job):
            fails.append(
                f"`{jk}` has a continue-on-error key in {workflow} — a literal "
                f"or expression can mask a failure as SUCCESS (Gitea Quirk #10 "
                f"/ mc#1982). Remove the key."
            )

    # 7. No continue-on-error on any step inside a gating job.
    for jk in GATING_JOBS:
        job = jobs.get(jk)
        if not isinstance(job, dict):
            continue
        for idx, step in enumerate(steps_of(job)):
            if has_continue_on_error(step):
                fails.append(
                    f"`{jk}` step {idx} has a continue-on-error key in {workflow} "
                    f"— a literal or expression can mask a failure as SUCCESS "
                    f"(Gitea Quirk #10 / mc#1982). Remove the key."
                )

    # 8. Tenant-image CI must not deploy/reload the CP host. The explicit
    # local-Docker cache preparation above is a provider boundary, not a CP
    # deployment adapter.
    for jk in FORBIDDEN_JOB_KEYS:
        if jk in jobs:
            fails.append(
                f"`{jk}` is present in {workflow} — staging tenant CI must not "
                f"perform CP deploy/reload work. Keep CP-host adapters out of "
                f"this workflow."
            )
    for jk, job in jobs.items():
        if not isinstance(job, dict):
            continue
        for idx, step in enumerate(steps_of(job)):
            if not isinstance(step, dict):
                continue
            hits = sorted(set(values_contain(step, FORBIDDEN_CI_MARKERS)))
            if hits:
                fails.append(
                    f"`{jk}` step {idx} contains CP deploy/reload marker(s) "
                    f"{hits} in {workflow}. Railway or other CP-host deploy "
                    f"adapters must not be required by staging tenant CI."
                )

    if fails:
        print("FAIL: staging tenant CD gate chain is not enforced:")
        for f in fails:
            print(f"  - {f}")
        print()
        print("The gate is enforced by the needs: graph in")
        print(
            f"  {workflow}. Keep await-image→advance-pin→"
            "(redeploy-fleet + runtime-image-readiness)→e2e-smoke"
        )
        print("  wired via needs:, and never continue-on-error a gating job.")
        return 1

    print(
        "OK: staging tenant CD gate chain enforced — "
        "await-image -> advance-pin -> (redeploy-fleet + runtime-image-readiness) -> e2e-smoke, "
        "with rollback-pin coverage and no CP deploy/reload path"
        " (needs: edges intact, no continue-on-error at job or step level)."
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
