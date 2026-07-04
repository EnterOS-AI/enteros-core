#!/usr/bin/env python3
"""lint_staging_tenant_cd_gate_chain — the staging tenant fleet roll cannot be
un-gated.

The gated staging tenant CD lives at .gitea/workflows/staging-tenant-cd.yml:

    await-image -> e2e-smoke -> advance-pin -> redeploy-fleet

It is enforced INTRA-workflow by Gitea `needs:` (a red upstream job skips every
downstream job, so a failed staging e2e means the fleet is never rolled and the
image pin is never advanced). That gate runs on `push` to staging (post-merge),
so it cannot itself be a pre-merge branch-protection context — but the INVARIANTS
that make it a real gate CAN be checked statically, pre-merge, and made
merge-blocking via `ci / all-required`.

This lint is that mechanical guard. It fails the build if anyone:

  1. breaks the gate chain — e2e-smoke must `needs:` await-image, advance-pin
     must `needs:` e2e-smoke, redeploy-fleet must `needs:` advance-pin
     (transitively, so the fleet can never roll while the e2e gate is red); OR
  2. adds `continue-on-error: true` to any gating job (e2e-smoke, advance-pin,
     redeploy-fleet) OR to any step inside one — continue-on-error rolls a
     failed step up to a SUCCESS job status (Gitea Quirk #10 / mc#1982), which
     would let a downstream `needs:`-dependent job run despite a real failure,
     silently re-opening the ungated fleet roll this gate closes.

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
    ("e2e-smoke", "await-image"),
    ("advance-pin", "e2e-smoke"),
    ("redeploy-fleet", "advance-pin"),
]

# Jobs whose failure MUST skip everything downstream — so continue-on-error
# (which masks a failure as success) is forbidden on them.
GATING_JOBS = ["e2e-smoke", "advance-pin", "redeploy-fleet"]


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


def steps_of(job):
    steps = job.get("steps")
    return steps if isinstance(steps, list) else []


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
                         f"await-image→e2e-smoke→advance-pin→redeploy-fleet "
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

    # 3. No continue-on-error on a gating job.
    for jk in GATING_JOBS:
        job = jobs.get(jk)
        if isinstance(job, dict) and coe_true(job):
            fails.append(
                f"`{jk}` has continue-on-error: true in {workflow} — a failed "
                f"step would roll up to SUCCESS (Gitea Quirk #10 / mc#1982) and "
                f"let the downstream roll run despite a red gate. Remove it.")

    # 4. No continue-on-error on any step inside a gating job.
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

    if fails:
        print("FAIL: staging tenant CD gate chain is not enforced:")
        for f in fails:
            print(f"  - {f}")
        print()
        print("The gate is enforced by the needs: graph in")
        print(f"  {workflow}. Keep await-image→e2e-smoke→advance-pin→"
              "redeploy-fleet")
        print("  wired via needs:, and never continue-on-error a gating job.")
        return 1

    print("OK: staging tenant CD gate chain enforced — "
          "await-image -> " + " -> ".join(GATING_JOBS) +
          " (needs: edges intact, no continue-on-error at job or step level).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
