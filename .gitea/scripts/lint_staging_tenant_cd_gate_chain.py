#!/usr/bin/env python3
"""lint_staging_tenant_cd_gate_chain — the staging tenant fleet roll cannot be
un-gated.

The gated staging tenant CD lives at .gitea/workflows/staging-tenant-cd.yml:

    await-image -> advance-pin -> redeploy-fleet -> e2e-smoke

It is enforced INTRA-workflow by Gitea `needs:` (a red upstream job skips every
downstream job). The pin advances before the local-docker staging fleet rolls to
the candidate image; e2e-smoke then validates the deployed staging surface.
rollback-pin must always run and restore the old pin + fleet image whenever any
candidate-path job fails or is skipped. CP-host redeploys are provider-owned and
must not be wired into this tenant-image CI path. That gate runs on
`push` to main (post-merge), so it cannot itself be a pre-merge
branch-protection context — but the INVARIANTS that make it a real gate CAN be
checked statically, pre-merge, and made merge-blocking via `ci / all-required`.

This lint is that mechanical guard. It fails the build if anyone:

  1. breaks the gate chain — advance-pin must `needs:` await-image,
     redeploy-fleet must `needs:` advance-pin, and e2e-smoke must `needs:`
     redeploy-fleet (transitively, so e2e never validates a pre-roll fleet); OR
  2. breaks rollback coverage — rollback-pin must `needs:` every candidate-path
     job and run under `if: always()`; OR
  3. adds `continue-on-error: true` to any gating job (advance-pin,
     e2e-smoke, redeploy-fleet, rollback-pin) OR to any step inside one —
     continue-on-error rolls a failed step up to a SUCCESS job status (Gitea
     Quirk #10 / mc#1982), which would let a downstream `needs:`-dependent job
     run despite a real failure, silently re-opening the ungated fleet roll this
     gate closes; OR
  4. reintroduces provider-specific CP deploy wiring (Railway CLI/tokens or the
     legacy reload-cp-candidate job) into this tenant-image CI path.

Behavior-based (parses the YAML `needs:` graph), not grep-by-name: a job rename
that keeps the edges is fine; dropping an edge is the failure.

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


def workflow_path():
    return os.environ.get("STAGING_TENANT_CD_PATH", DEFAULT_WORKFLOW)


# job_key -> the job_key it must (transitively) depend on. These are the
# load-bearing gate edges: each downstream stage must not be reachable while
# its upstream is red.
REQUIRED_EDGES = [
    ("advance-pin", "await-image"),
    ("redeploy-fleet", "advance-pin"),
    ("e2e-smoke", "redeploy-fleet"),
]

# Jobs whose failure MUST skip everything downstream — so continue-on-error
# (which masks a failure as success) is forbidden on them.
GATING_JOBS = [
    "advance-pin",
    "redeploy-fleet",
    "e2e-smoke",
    "rollback-pin",
]
ROLLBACK_NEEDS = [
    "advance-pin",
    "redeploy-fleet",
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


def load_jobs(path):
    with open(path) as f:
        doc = yaml.safe_load(f)
    if not isinstance(doc, dict):
        raise ValueError(f"{path}: not a mapping")
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        raise ValueError(f"{path}: no jobs: block")
    return jobs


def needs_of(job):
    n = job.get("needs")
    if n is None:
        return []
    if isinstance(n, str):
        return [n]
    if isinstance(n, list):
        return [x for x in n if isinstance(x, str)]
    return []


def depends_transitively(jobs, start, target, _seen=None):
    if _seen is None:
        _seen = set()
    if start in _seen:
        return False
    _seen.add(start)
    job = jobs.get(start)
    if not isinstance(job, dict):
        return False
    direct = needs_of(job)
    if target in direct:
        return True
    return any(depends_transitively(jobs, dep, target, _seen) for dep in direct)


def coe_true(job):
    coe = job.get("continue-on-error", False)
    return coe is True or (isinstance(coe, str) and coe.strip().lower() == "true")


def always_if(job):
    value = job.get("if", "")
    return isinstance(value, str) and value.strip() == "always()"


def steps_of(job):
    steps = job.get("steps")
    return steps if isinstance(steps, list) else []


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
        print(f"FAIL: {workflow} is missing — the gated staging tenant CD "
              f"workflow must exist.")
        return 1
    try:
        jobs = load_jobs(workflow)
    except Exception as e:
        print(f"FAIL: cannot parse {workflow}: {e}")
        return 1

    fails = []

    # 1. Every gating job must exist.
    for jk in GATING_JOBS + ["await-image"]:
        if jk not in jobs:
            fails.append(f"gate job `{jk}` is missing from {workflow} — the "
                         f"await-image→advance-pin→redeploy-fleet→e2e-smoke "
                         f"chain is broken.")

    # 2. The chain edges must hold (transitively).
    for downstream, upstream in REQUIRED_EDGES:
        if downstream not in jobs or upstream not in jobs:
            continue  # already reported missing above
        if not depends_transitively(jobs, downstream, upstream):
            fails.append(
                f"`{downstream}` does not (transitively) `needs:` `{upstream}` "
                f"in {workflow} — a red `{upstream}` would NOT skip "
                f"`{downstream}`, so the gate can be bypassed.")

    # 3. Rollback must cover every post-pin path and must always run.
    rollback = jobs.get("rollback-pin")
    if isinstance(rollback, dict):
        direct_needs = set(needs_of(rollback))
        missing = [need for need in ROLLBACK_NEEDS if need not in direct_needs]
        if missing:
            fails.append(
                f"`rollback-pin` does not directly `needs:` {missing} in "
                f"{workflow} — it would not have every candidate-path result "
                f"available and may fail to restore the old staging pin/fleet image.")
        if not always_if(rollback):
            fails.append(
                f"`rollback-pin` must use `if: always()` in {workflow} so it "
                f"runs after failed or skipped candidate-path jobs.")

    # 4. No continue-on-error on a gating job.
    for jk in GATING_JOBS:
        job = jobs.get(jk)
        if isinstance(job, dict) and coe_true(job):
            fails.append(
                f"`{jk}` has continue-on-error: true in {workflow} — a failed "
                f"step would roll up to SUCCESS (Gitea Quirk #10 / mc#1982) and "
                f"let the downstream roll run despite a red gate. Remove it.")

    # 5. No continue-on-error on any step inside a gating job.
    for jk in GATING_JOBS:
        job = jobs.get(jk)
        if not isinstance(job, dict):
            continue
        for idx, step in enumerate(steps_of(job)):
            if isinstance(step, dict) and coe_true(step):
                fails.append(
                    f"`{jk}` step {idx} has continue-on-error: true in {workflow} "
                    f"— a failed step inside a gating job would roll up to SUCCESS "
                    f"(Gitea Quirk #10 / mc#1982) and let the downstream roll run "
                    f"despite a red gate. Remove it.")

    # 6. Tenant-image CI must stay provider-agnostic. CP-host deploy/reload code
    # belongs behind provider adapters outside this workflow.
    for jk in FORBIDDEN_JOB_KEYS:
        if jk in jobs:
            fails.append(
                f"`{jk}` is present in {workflow} — staging tenant CI must not "
                f"perform provider-specific CP deploy/reload work. Keep provider "
                f"adapters out of this workflow.")
    for jk, job in jobs.items():
        if not isinstance(job, dict):
            continue
        for idx, step in enumerate(steps_of(job)):
            if not isinstance(step, dict):
                continue
            hits = sorted(set(values_contain(step, FORBIDDEN_CI_MARKERS)))
            if hits:
                fails.append(
                    f"`{jk}` step {idx} contains provider-specific deploy marker(s) "
                    f"{hits} in {workflow}. Railway or other CP-host deploy "
                    f"adapters must not be required by staging tenant CI.")

    if fails:
        print("FAIL: staging tenant CD gate chain is not enforced:")
        for f in fails:
            print(f"  - {f}")
        print()
        print("The gate is enforced by the needs: graph in")
        print(f"  {workflow}. Keep await-image→advance-pin→"
              "redeploy-fleet→e2e-smoke")
        print("  wired via needs:, and never continue-on-error a gating job.")
        return 1

    print("OK: staging tenant CD gate chain enforced — "
          "await-image -> advance-pin -> redeploy-fleet -> e2e-smoke, "
          "with rollback-pin coverage and provider-agnostic CI"
          " (needs: edges intact, no continue-on-error at job or step level).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
