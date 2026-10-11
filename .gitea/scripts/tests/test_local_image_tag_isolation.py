"""No two concurrently-schedulable jobs may substitute DIFFERENT content under
one local workspace-template image tag.

THE BUG THIS LOCKS OUT (core#5031)
----------------------------------
`tests/e2e/test_local_provision_lifecycle_e2e.sh` and
`tests/e2e/test_selfhost_concierge_schedules_e2e.sh` each `docker tag` a tiny
STUB runtime over

    molecule-local/workspace-template-claude-code:<template-HEAD-sha12>-<arch>   (+ :latest)

and each restores the previous image id on cleanup. Every component of that name
is derived from the world — runtime, template repo HEAD sha, host arch — so both
jobs compute the SAME string, and the `molecule-runner-*` act_runner instances
share one /var/run/docker.sock. Observed on molecule-core#5030 (head 8fd5f97):

    03:04:15  concierge-schedules  tags its stub over the tag + :latest
    03:04:16  lifecycle            tags its stub over the same tags
    03:04:20  concierge-schedules  cleanup RESTORES the tag to the real image
    03:04:27  lifecycle            re-provision resolves the tag -> the REAL image
    03:04:38  lifecycle            real runtime replies "Invalid API key" -> FAIL

A REQUIRED gate failed with an assertion about LLM credentials. Nothing in that
message points at CI scheduling, which is what makes the defect expensive: it is
silent, misattributing, and load-dependent (lifecycle normally finishes in ~44s
and wins the race; it took 79s that time and lost). The two workflows still start
within ~100s of each other on every commit — 2026-08-03, PR #5039 head f8837f82:
lifecycle-stub 04:53:04-04:55:24 and selfhost-schedules 04:54:44-04:56:02, a 40s
overlap, both on robot-2 — so the window is not hypothetical, it is continuous.

THE PROPERTY
------------
Not "these two jobs must not overlap". Overlap is fine and cheap; a shared
mutable NAME is not. A job that substitutes a stub must own a private tag
namespace, so that concurrency stops being a variable at all:

  A. a script that writes a `molecule-local/workspace-template-*` tag must derive
     that tag from MOLECULE_LOCAL_IMAGE_ISOLATION (the same variable
     workspace-server/internal/provisioner/local_image_isolation.go reads, so the
     writer and the resolver cannot drift);
  B. every job that runs such a script must SET that variable — unless it
     demonstrably does not substitute a stub;
  C. the token must be run-scoped (`github.run_id` + `github.run_attempt`) and
     pairwise DISTINCT across jobs, because two jobs sharing a token is exactly
     the state this removes.

Rule B's exemption is deliberately not a name list: an entry must ALSO be
corroborated by the job putting the script into provisioner-builds mode, where it
tags nothing and the provisioner writes the genuine template for that sha —
content-identical bytes, so a second writer is not a substitution. An exemption
that could be claimed by a stub-substituting job would be a hole, not a rule.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest
import yaml

REPO = Path(__file__).resolve().parents[3]
WORKFLOWS = REPO / ".gitea" / "workflows"
E2E = REPO / "tests" / "e2e"

ISOLATION_ENV = "MOLECULE_LOCAL_IMAGE_ISOLATION"

# The name whose sharing is the defect. Matches both the sha-pinned tag and the
# floating :latest alias, however the script spells the repository part.
LOCAL_TEMPLATE_REPO = re.compile(r"molecule-local/workspace-template-")

# A script "substitutes content" when it hands `docker tag` a destination it
# built itself. We do not try to prove the destination resolves to the shared
# name — that is undecidable from text — so the trigger is the conjunction of
# naming the local template repo anywhere and running `docker tag` at all.
DOCKER_TAG = re.compile(r"^\s*docker\s+tag\b", re.MULTILINE)

# Modes in which the script tags nothing and the provisioner builds the genuine
# template instead. The presence of one of these in a job's env is the evidence
# an exemption must carry.
PROVISIONER_BUILDS_ENV = ("LIFECYCLE_LLM", "LIFECYCLE_PROVISIONER_BUILDS")

# (workflow file, job id) -> reason. See the module docstring: an entry is only
# honoured when the job also carries a PROVISIONER_BUILDS_ENV key.
ISOLATION_EXEMPT: dict[tuple[str, str], str] = {
    ("local-provision-e2e.yml", "lifecycle-real"): (
        "LIFECYCLE_LLM=minimax forces provisioner-builds mode: the script tags "
        "nothing and the provisioner writes the genuine template for that sha, so "
        "a concurrent writer produces identical bytes. Isolating it would force a "
        "cold ~2.5GB rebuild every run to fix a race this job does not have."
    ),
}


def _tag_mutating_scripts() -> list[str]:
    """Repo-relative paths of e2e scripts that write a local template tag."""
    found = []
    for sh in sorted(E2E.glob("*.sh")):
        body = sh.read_text(encoding="utf-8")
        if LOCAL_TEMPLATE_REPO.search(body) and DOCKER_TAG.search(body):
            found.append(f"tests/e2e/{sh.name}")
    return found


def _jobs_running(script_paths: list[str]) -> list[tuple[str, str, dict]]:
    """(workflow file, job id, job dict) for every job that runs one of them."""
    out: list[tuple[str, str, dict]] = []
    for wf in sorted(WORKFLOWS.glob("*.yml")):
        doc = yaml.safe_load(wf.read_text(encoding="utf-8"))
        if not isinstance(doc, dict):
            continue
        for job_id, job in (doc.get("jobs") or {}).items():
            if not isinstance(job, dict):
                continue
            bodies = " ".join(
                s["run"] for s in (job.get("steps") or [])
                if isinstance(s, dict) and isinstance(s.get("run"), str)
            )
            if any(p in bodies for p in script_paths):
                out.append((wf.name, job_id, job))
    return out


def _job_env(job: dict) -> dict:
    env = job.get("env")
    return env if isinstance(env, dict) else {}


def _script_step_env(job: dict, script_paths: list[str]) -> dict:
    """Job env merged with the env of the step(s) that actually run the script.

    The isolation token itself must be at JOB level — the platform-server the
    workflow starts resolves the tag, and it is started by a DIFFERENT step from
    the one running the script — but the evidence for an exemption belongs
    wherever the mode is actually selected, which is the invoking step.
    """
    merged = dict(_job_env(job))
    for s in job.get("steps") or []:
        if not isinstance(s, dict) or not isinstance(s.get("run"), str):
            continue
        if any(p in s["run"] for p in script_paths) and isinstance(s.get("env"), dict):
            merged.update(s["env"])
    return merged


# --------------------------------------------------------------------------
# Fail-closed: an empty discovery would make every rule below vacuously true.
# --------------------------------------------------------------------------

def test_discovery_is_not_empty() -> None:
    scripts = _tag_mutating_scripts()
    assert len(scripts) >= 2, (
        f"found {scripts} tag-mutating e2e scripts; expected at least the two from "
        f"core#5031. A glob/regex that stops matching turns this whole file into a "
        f"pass:0 gate."
    )
    jobs = _jobs_running(scripts)
    assert len(jobs) >= 2, f"only {len(jobs)} workflow job(s) run {scripts} — parse broke"


# --------------------------------------------------------------------------
# Rule A — the writer reads the same variable the resolver reads.
# --------------------------------------------------------------------------

@pytest.mark.parametrize("script", _tag_mutating_scripts())
def test_script_derives_its_tags_from_the_isolation_variable(script: str) -> None:
    body = (REPO / script).read_text(encoding="utf-8")
    assert ISOLATION_ENV in body, (
        f"{script} writes a molecule-local/workspace-template-* tag but never reads "
        f"{ISOLATION_ENV}. That tag name is derived from the template HEAD sha and "
        f"the arch alone, so every concurrent job on this daemon computes the SAME "
        f"string and the last writer wins (core#5031). Derive the tag from "
        f"{ISOLATION_ENV} — the same variable "
        f"workspace-server/internal/provisioner/local_image_isolation.go resolves — "
        f"so the writer and the resolver cannot disagree."
    )


# --------------------------------------------------------------------------
# Rule B — every job that runs such a script owns a namespace, or proves it
# does not substitute.
# --------------------------------------------------------------------------

@pytest.mark.parametrize(
    "wf,job_id,job",
    _jobs_running(_tag_mutating_scripts()),
    ids=lambda v: v if isinstance(v, str) else "job",
)
def test_job_running_a_tag_mutating_script_owns_a_private_namespace(wf: str, job_id: str, job: dict) -> None:
    env = _job_env(job)
    if ISOLATION_ENV in env:
        return

    reason = ISOLATION_EXEMPT.get((wf, job_id))
    assert reason, (
        f"{wf} / job {job_id!r} runs a tag-mutating e2e script without setting "
        f"{ISOLATION_ENV}. It will write the shared "
        f"molecule-local/workspace-template-* tag that every concurrent job on the "
        f"same docker.sock resolves, and its cleanup will restore that tag out from "
        f"under whoever is still using it (core#5031). Set "
        f"{ISOLATION_ENV}: e2e-${{{{ github.run_id }}}}-${{{{ github.run_attempt }}}}-<job> "
        f"at JOB level so both the platform-server and the script read it."
    )
    invoking_env = _script_step_env(job, _tag_mutating_scripts())
    corroborating = [k for k in PROVISIONER_BUILDS_ENV if k in invoking_env]
    assert corroborating, (
        f"{wf} / job {job_id!r} claims the core#5031 isolation exemption "
        f"({reason}) but its env carries none of {PROVISIONER_BUILDS_ENV}, so "
        f"nothing shows it actually skips the stub substitution. An exemption a "
        f"stub-substituting job could claim is a hole, not a rule."
    )


# --------------------------------------------------------------------------
# Rule C — the token is run-scoped and unique.
# --------------------------------------------------------------------------

def _isolation_tokens() -> list[tuple[str, str, str]]:
    out: list[tuple[str, str, str]] = []
    for wf in sorted(WORKFLOWS.glob("*.yml")):
        doc = yaml.safe_load(wf.read_text(encoding="utf-8"))
        if not isinstance(doc, dict):
            continue
        for job_id, job in (doc.get("jobs") or {}).items():
            if isinstance(job, dict) and ISOLATION_ENV in _job_env(job):
                out.append((wf.name, job_id, str(_job_env(job)[ISOLATION_ENV])))
    return out


@pytest.mark.parametrize("wf,job_id,token", _isolation_tokens(), ids=lambda v: str(v)[:40])
def test_isolation_token_is_run_scoped(wf: str, job_id: str, token: str) -> None:
    for expr in ("github.run_id", "github.run_attempt"):
        assert expr in token, (
            f"{wf} / job {job_id!r} sets {ISOLATION_ENV}={token!r}, which omits "
            f"{expr}. A token that is constant across runs is shared by every run "
            f"of this job — including a re-run of the same run, which is why "
            f"run_attempt is required as well. It is the same run-scoped idiom the "
            f"PG/Redis container names and the owned-workspace manifest already use."
        )


def test_isolation_tokens_are_pairwise_distinct() -> None:
    tokens = _isolation_tokens()
    by_token: dict[str, list[str]] = {}
    for wf, job_id, token in tokens:
        by_token.setdefault(token, []).append(f"{wf}:{job_id}")
    dupes = {t: owners for t, owners in by_token.items() if len(owners) > 1}
    assert not dupes, (
        f"jobs share an {ISOLATION_ENV} token: {dupes}. Jobs in the SAME workflow "
        f"run share a run_id and a run_attempt, so the discriminator is the only "
        f"thing separating them; two jobs on one token are back to the shared "
        f"mutable tag core#5031 removed."
    )
