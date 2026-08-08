#!/usr/bin/env python3
"""A CI task that never executed must never be read as a verdict.

WHY THIS FILE EXISTS
--------------------
Gitea's `PickTask` COMMITS a task row as `running` with `runner_id` set and only
THEN assembles the FetchTask payload. FetchTask is at-most-once — no ack, and
nothing re-queues a claimed-but-undelivered task — so with act_runner's
`fetch_timeout: 5s` a lost response leaves a committed claim that no runner is
working on. `StopZombieTasks` reaps it as **failure** 10-15 minutes later, with
an EMPTY log. Through `needs.<job>.result` that reaped row is byte-identical to
a job that ran and genuinely failed.

`staging-tenant-cd.yml` advances the staging tenant-image pin BEFORE testing it
and then decides in-shell from `E2E_RESULT`. So a phantom red does not merely
mis-report: it ROLLS THE STAGING FLEET. Two pin rollbacks were caused this way
(`candidate path failed after pin advance (redeploy=success readiness=success
e2e=failure)`; CP audit 40708 landed 23s after the reaper stamped task 989633).
And `rollback-pin` task 885373 was itself a phantom, so a genuinely-failed
candidate stayed pinned because its rollback was never delivered.

WHAT IS ASSERTED, AND WHY IT IS NOT A REGEX LINT
------------------------------------------------
Every test below EXECUTES the real `run:` body lifted out of
`.gitea/workflows/staging-tenant-cd.yml` under `bash --noprofile --norc -e -o
pipefail`, against fake `scripts/deploy/*.sh` that record whether the fleet was
actually rolled. A grep for `ran-sentinel` would pass on a guard whose check
sits in an unreachable branch, and would pass on a check that compares two empty
strings. Running the body cannot.

The suite is built around four cases the pipeline must distinguish, a negative
control that varies EXACTLY ONE input, and a machine-checked proof of the
asymmetry property (a missing sentinel may only ever SUPPRESS a rollback).
"""

from __future__ import annotations

import itertools
import os
import shutil
import subprocess
import tempfile
from pathlib import Path

import pytest
import yaml

# On the CI runner this is simply /usr/bin/bash. The override exists for Windows
# developer boxes, where a bare `bash` resolves to the WSL launcher stub in
# System32 and fails with 0x8007274c instead of running the script.
BASH = os.environ.get("PYTEST_BASH") or shutil.which("bash") or "/bin/bash"

REPO = Path(__file__).resolve().parents[3]
WORKFLOW = REPO / ".gitea" / "workflows" / "staging-tenant-cd.yml"
LIBRARY = REPO / "scripts" / "deploy" / "ran-sentinel.sh"

RUN_ID = "990001"

# The jobs whose `needs.<job>.result` is read as a verdict by rollback-pin.
COVERED_JOBS = ["redeploy-fleet", "runtime-image-readiness", "e2e-smoke"]


def _token(job: str, phase: str, run_id: str = RUN_ID) -> str:
    """The token a job must emit — built here INDEPENDENTLY of the library.

    Hand-constructing it is deliberate: if this and `ran_sentinel_expect` ever
    disagree, `test_library_expectation_matches_the_independently_built_token`
    goes red rather than both drifting together.
    """
    return f"ran-sentinel/v1 {job} {phase} run={run_id}"


# --------------------------------------------------------------------------
# YAML extraction — the real step bodies, never a hand-transcribed copy
# --------------------------------------------------------------------------


def _workflow() -> dict:
    return yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))


def _job(key: str) -> dict:
    jobs = _workflow()["jobs"]
    assert key in jobs, f"job {key!r} is missing from {WORKFLOW}"
    return jobs[key]


def _step_named(job_key: str, needle: str) -> dict:
    for step in _job(job_key).get("steps") or []:
        if needle in (step.get("name") or ""):
            return step
    raise AssertionError(
        f"no step in job {job_key!r} whose name contains {needle!r} — "
        f"the ran-sentinel wiring this suite exists to protect is absent."
    )


# --------------------------------------------------------------------------
# Execution harness
# --------------------------------------------------------------------------


def _fake_repo(tmp: Path) -> Path:
    """A working tree with the REAL sentinel library and FAKE deploy scripts.

    The fakes append their argv to $ROLLBACK_LOG, so "did this body roll the
    fleet?" is an observation, not an inference from the body's text.
    """
    (tmp / "scripts" / "deploy").mkdir(parents=True)
    shutil.copyfile(LIBRARY, tmp / "scripts" / "deploy" / "ran-sentinel.sh")
    for name in ("advance-staging-tenant-pin.sh", "redeploy-staging-fleet.sh"):
        target = tmp / "scripts" / "deploy" / name
        target.write_text(
            "#!/usr/bin/env bash\n"
            'printf "%s %s\\n" "' + name + '" "$*" >> "$ROLLBACK_LOG"\n'
            "exit 0\n",
            encoding="utf-8",
        )
        target.chmod(0o755)
    return tmp


def _run_body(body: str, env_overrides: dict[str, str], *, cwd: Path | None = None):
    """Execute a workflow `run:` body under a fake runner environment."""
    with tempfile.TemporaryDirectory() as raw:
        tmp = Path(raw)
        repo = cwd or _fake_repo(tmp / "repo")
        log = tmp / "rollback.log"
        log.touch()
        gh_output = tmp / "gh_output"
        gh_output.touch()
        env = {
            "PATH": os.environ.get("PATH", ""),
            "HOME": str(tmp),
            "TMPDIR": str(tmp),
            "ROLLBACK_LOG": str(log),
            "GITHUB_OUTPUT": str(gh_output),
            "GITHUB_STEP_SUMMARY": str(tmp / "gh_summary"),
            "GITHUB_RUN_ID": RUN_ID,
        }
        env.update(env_overrides)
        # Written to a FILE, not passed with `-c`. That is how a workflow `run:`
        # body is actually executed, and it keeps the shell text away from the
        # host's argv quoting rules entirely.
        script = tmp / "step.sh"
        script.write_text(body, encoding="utf-8", newline="\n")
        proc = subprocess.run(
            [BASH, "--noprofile", "--norc", "-e", "-o", "pipefail", str(script)],
            cwd=str(repo),
            env=env,
            capture_output=True,
            text=True,
            timeout=300,
        )
        proc.rollback_log = log.read_text(encoding="utf-8")  # type: ignore[attr-defined]
        proc.gh_output = gh_output.read_text(encoding="utf-8")  # type: ignore[attr-defined]
        return proc


def _fake_repo_ctx(tmp: Path) -> Path:
    return _fake_repo(tmp)


def _decision_env(
    *,
    e2e_result: str = "success",
    e2e_begin: str | None = None,
    e2e_end: str | None = None,
    redeploy_result: str = "success",
    redeploy_begin: str | None = None,
    redeploy_end: str | None = None,
    readiness_result: str = "success",
    readiness_begin: str | None = None,
    readiness_end: str | None = None,
    advance_result: str = "success",
    old_image: str = "registry.moleculesai.app/molecule-ai/molecule-tenant:staging-0ldbeef",
    old_git_sha: str = "0ldbeef0ldbeef0ldbeef0ldbeef0ldbeef0ldbe",
) -> dict[str, str]:
    """Build the exact `env:` block rollback-pin receives.

    `None` for a sentinel means "the job emitted nothing" — i.e. Gitea resolves
    `needs.<job>.outputs.ran_begin` to the empty string, which is what a phantom
    produces.
    """

    def sent(value: str | None, job: str, phase: str) -> str:
        return _token(job, phase) if value is None else value

    return {
        "ADVANCE_RESULT": advance_result,
        "REDEPLOY_RESULT": redeploy_result,
        "READINESS_RESULT": readiness_result,
        "E2E_RESULT": e2e_result,
        "REDEPLOY_BEGIN": sent(redeploy_begin, "redeploy-fleet", "begin"),
        "REDEPLOY_END": sent(redeploy_end, "redeploy-fleet", "end"),
        "READINESS_BEGIN": sent(readiness_begin, "runtime-image-readiness", "begin"),
        "READINESS_END": sent(readiness_end, "runtime-image-readiness", "end"),
        "E2E_BEGIN": sent(e2e_begin, "e2e-smoke", "begin"),
        "E2E_END": sent(e2e_end, "e2e-smoke", "end"),
        "OLD_IMAGE": old_image,
        "OLD_GIT_SHA": old_git_sha,
        "TENANT_IMAGE": "registry.moleculesai.app/molecule-ai/molecule-tenant",
        "TENANT_IMAGE_NAME": "registry.moleculesai.app/molecule-ai/molecule-tenant",
        "STAGING_TENANT_FLAGS": "",
        "INFISICAL_CLIENT_ID": "fake",
        "INFISICAL_CLIENT_SECRET": "fake",
        "INFISICAL_PROJECT_ID": "fake",
    }


def _run_decision(**kwargs):
    body = _step_named("rollback-pin", "Revert the staging pin")["run"]
    return _run_body(body, _decision_env(**kwargs))


def _rolled(proc) -> bool:
    return "redeploy-staging-fleet.sh" in proc.rollback_log


# ==========================================================================
# THE FOUR CASES
# ==========================================================================


def test_case1_genuine_failure_with_sentinel_present_still_rolls_back():
    """Do not weaken the existing gate.

    A real e2e failure, on a job that demonstrably ran start to finish, must
    revert the pin and the fleet exactly as it does today.
    """
    proc = _run_decision(e2e_result="failure")
    out = proc.stdout + proc.stderr
    assert _rolled(proc), (
        "a GENUINE e2e failure (complete ran-sentinel) did not roll back — the "
        f"sentinel work has weakened the existing gate.\noutput:\n{out}\n"
        f"rollback log:\n{proc.rollback_log}"
    )
    assert "advance-staging-tenant-pin.sh" in proc.rollback_log, proc.rollback_log


def test_case2_failure_with_sentinel_absent_does_not_roll_and_fails_loud():
    """THE DEFECT. A phantom red must not move the fleet.

    `e2e-smoke` reports `failure` but emitted no sentinel: no step of that job
    ever ran, so its `failure` is the reaper's, not the candidate's.
    """
    proc = _run_decision(e2e_result="failure", e2e_begin="", e2e_end="")
    out = proc.stdout + proc.stderr
    assert not _rolled(proc), (
        "A PHANTOM e2e failure ROLLED THE STAGING FLEET. This is the exact "
        "defect: a task that never executed was read as a verdict.\n"
        f"rollback log:\n{proc.rollback_log}\noutput:\n{out}"
    )
    assert proc.returncode != 0, (
        "a missing ran-sentinel must be REPORTED, not silently absorbed — a "
        f"green here is the vacuous pass this file exists to stop.\noutput:\n{out}"
    )
    assert "::error::" in out, (
        f"an infra fault must annotate as ::error::, not ::warning::\noutput:\n{out}"
    )
    assert "ran-sentinel" in out, (
        f"the failure must NAME the mechanism so an operator can act.\noutput:\n{out}"
    )


def test_case3_success_with_sentinel_present_does_not_roll_back():
    proc = _run_decision(e2e_result="success")
    out = proc.stdout + proc.stderr
    assert not _rolled(proc), f"a green candidate was rolled back:\n{proc.rollback_log}"
    assert proc.returncode == 0, out


def test_case4_rollback_pin_own_missing_sentinel_is_surfaced():
    """`rollback-pin` is equally exposed — task 885373 was itself a phantom.

    A rollback that never ran must not be mistaken for one that succeeded: the
    pin is then left on a candidate that genuinely FAILED.
    """
    body = _step_named("rollback-audit", "ran-sentinel from rollback-pin")["run"]
    env = _decision_env(e2e_result="failure")
    env.update(
        {
            "ROLLBACK_RESULT": "failure",
            "ROLLBACK_BEGIN": "",
            "ROLLBACK_END": "",
        }
    )
    proc = _run_body(body, env)
    out = proc.stdout + proc.stderr
    assert proc.returncode != 0, (
        "a phantom rollback-pin was not surfaced — the staging pin is left on a "
        f"genuinely-failed candidate and nothing says so.\noutput:\n{out}"
    )
    assert "::error::" in out, out
    assert "rollback-pin" in out, out


def test_case4_positive_control_rollback_pin_that_really_ran_is_accepted():
    """The negative case above is only meaningful next to this one."""
    body = _step_named("rollback-audit", "ran-sentinel from rollback-pin")["run"]
    env = _decision_env(e2e_result="failure")
    env.update(
        {
            "ROLLBACK_RESULT": "success",
            "ROLLBACK_BEGIN": _token("rollback-pin", "begin"),
            "ROLLBACK_END": _token("rollback-pin", "end"),
        }
    )
    proc = _run_body(body, env)
    assert proc.returncode == 0, proc.stdout + proc.stderr


# ==========================================================================
# NEGATIVE CONTROL — one input varied, same code path
# ==========================================================================


@pytest.mark.parametrize("result", ["failure", "cancelled"])
def test_negative_control_only_the_sentinel_differs(result):
    """Identical inputs; ONLY the sentinel's presence changes.

    Same job, same result, same old image, same code path. Present => roll.
    Absent => do not roll, and say so loudly. If both arms behaved the same the
    sentinel would be decorative.
    """
    present = _run_decision(e2e_result=result)
    absent = _run_decision(e2e_result=result, e2e_begin="", e2e_end="")

    assert _rolled(present), (
        "control arm (sentinel PRESENT) did not roll back; the negative control "
        f"proves nothing.\n{present.stdout}{present.stderr}"
    )
    assert not _rolled(absent), (
        "test arm (sentinel ABSENT) rolled back anyway — the sentinel is not "
        f"actually consulted.\n{absent.stdout}{absent.stderr}"
    )
    assert present.returncode == 0 and absent.returncode != 0


def test_negative_control_covers_every_job_whose_result_is_read():
    """redeploy-fleet and runtime-image-readiness feed the SAME decision.

    A phantom on either of them today produces `failure`, which rolls the fleet
    just as surely as a phantom e2e does.
    """
    for job, kwargs_present, kwargs_absent in (
        (
            "redeploy-fleet",
            {"redeploy_result": "failure"},
            {"redeploy_result": "failure", "redeploy_begin": "", "redeploy_end": ""},
        ),
        (
            "runtime-image-readiness",
            {"readiness_result": "failure"},
            {"readiness_result": "failure", "readiness_begin": "", "readiness_end": ""},
        ),
    ):
        present = _run_decision(**kwargs_present)
        absent = _run_decision(**kwargs_absent)
        assert _rolled(present), f"{job}: control arm did not roll back"
        assert not _rolled(absent), (
            f"{job}: a phantom result rolled the staging fleet — this job's "
            "result feeds the same decision and needs the same sentinel."
        )
        assert absent.returncode != 0, f"{job}: phantom not reported"


def test_a_legitimately_skipped_job_still_rolls_back():
    """`skipped` is NOT a phantom, and must keep its existing meaning.

    When redeploy-fleet genuinely fails, e2e-smoke is skipped and emits no
    sentinel — legitimately. Treating that absence as an infra fault would
    suppress a rollback that is genuinely owed.
    """
    proc = _run_decision(
        redeploy_result="failure",
        e2e_result="skipped",
        e2e_begin="",
        e2e_end="",
    )
    assert _rolled(proc), (
        "a genuinely-failed redeploy with a legitimately SKIPPED e2e did not "
        f"roll back.\n{proc.stdout}{proc.stderr}"
    )


# ==========================================================================
# THE ASYMMETRY PROPERTY — machine-checked, exhaustive
# ==========================================================================


RESULTS = ["success", "failure", "cancelled", "skipped"]


def _decide_many(cases: list[list[tuple[str, str, bool, bool]]]) -> list[str]:
    """Evaluate many decisions through the REAL library in ONE bash process.

    Batched purely for speed — every case still goes through
    `ran_sentinel_decide` itself, not a Python re-implementation of it.
    """
    lines = [". scripts/deploy/ran-sentinel.sh"]
    for case in cases:
        args = []
        for job, result, begin, end in case:
            args += [
                job,
                result,
                _token(job, "begin") if begin else "",
                _token(job, "end") if end else "",
            ]
        quoted = " ".join(f"'{a}'" for a in args)
        lines.append(f"ran_sentinel_decide {quoted}; printf '\\n'")
    proc = _run_body("\n".join(lines) + "\n", {})
    assert proc.returncode == 0, proc.stdout + proc.stderr
    out = proc.stdout.split("\n")[:-1] if proc.stdout.endswith("\n") else proc.stdout.split("\n")
    assert len(out) == len(cases), (len(out), len(cases), proc.stdout)
    return out


def test_a_missing_sentinel_can_never_cause_a_rollback():
    """Exhaustive proof of the safety property, over the real shell code.

    For every combination of the three covered jobs' results, and every way of
    degrading the FIRST job's sentinel, removing sentinel evidence must never
    turn a non-ROLLBACK decision into ROLLBACK. Rolling the fleet is the
    destructive direction; it may only ever follow POSITIVE evidence that the
    condemning job actually ran.
    """
    combos = list(itertools.product(RESULTS, repeat=len(COVERED_JOBS)))
    degradations = [(b, e) for b, e in itertools.product([True, False], repeat=2)]

    intact_cases = [
        [(job, r, True, True) for job, r in zip(COVERED_JOBS, combo)]
        for combo in combos
    ]
    degraded_cases = []
    index = []
    for ci, combo in enumerate(combos):
        for begin, end in degradations:
            case = [(job, r, True, True) for job, r in zip(COVERED_JOBS, combo)]
            case[0] = (COVERED_JOBS[0], combo[0], begin, end)
            degraded_cases.append(case)
            index.append((ci, combo, begin, end))

    intact = _decide_many(intact_cases)
    degraded = _decide_many(degraded_cases)

    inverted = [
        (combo, begin, end, intact[ci], degraded[i])
        for i, (ci, combo, begin, end) in enumerate(index)
        if degraded[i] == "ROLLBACK" and intact[ci] != "ROLLBACK"
    ]
    assert not inverted, (
        "degrading a sentinel CREATED a rollback — the asymmetry is inverted, "
        f"which is the one thing this design may not do: {inverted}"
    )
    # Guard against the property passing because nothing ever rolls back.
    assert "ROLLBACK" in intact, "no intact combination ever decided ROLLBACK"
    assert "NO_ROLLBACK" in intact, "no intact combination ever decided NO_ROLLBACK"
    assert "INFRA_FAULT" in degraded, "no degraded combination was an INFRA_FAULT"


# ==========================================================================
# NON-VACUITY — the check must be capable of failing
# ==========================================================================


def _lib(snippet: str, env: dict[str, str] | None = None):
    body = ". scripts/deploy/ran-sentinel.sh\n" + snippet
    return _run_body(body, env or {})


def test_expected_token_is_refused_when_run_id_is_empty():
    """THE vacuity trap.

    If `ran_sentinel_expect` returned an empty string on an empty GITHUB_RUN_ID,
    then `[ "$begin" = "$expected" ]` would compare "" to "" and EVERY phantom
    would classify as having run. The library must refuse instead.
    """
    for run_id in ("", None):
        env = {} if run_id is None else {"GITHUB_RUN_ID": run_id}
        if run_id is None:
            # Genuinely UNSET, not merely empty.
            proc = _run_body(
                ". scripts/deploy/ran-sentinel.sh\n"
                "unset GITHUB_RUN_ID\n"
                "ran_sentinel_expect e2e-smoke begin\n",
                {},
            )
        else:
            proc = _lib("ran_sentinel_expect e2e-smoke begin\n", env)
        out = proc.stdout + proc.stderr
        assert proc.returncode != 0, f"expectation built from an empty run id: {out!r}"
        assert proc.stdout.strip() == "", (
            f"an unusable expectation was still printed: {proc.stdout!r}"
        )
        assert "GITHUB_RUN_ID" in out, out


def test_decision_step_fails_closed_when_run_id_is_empty():
    """And the failure must propagate all the way out to "do not roll"."""
    body = _step_named("rollback-pin", "Revert the staging pin")["run"]
    env = _decision_env(e2e_result="failure")
    env["GITHUB_RUN_ID"] = ""
    proc = _run_body(body, env)
    assert proc.returncode != 0, proc.stdout + proc.stderr
    assert not _rolled(proc), (
        "an unusable expectation still rolled the fleet: " + proc.rollback_log
    )


@pytest.mark.parametrize(
    "bogus",
    [
        "",
        " ",
        "ran-sentinel/v1",
        "ran-sentinel/v1 e2e-smoke begin",
        "ran-sentinel/v1 e2e-smoke begin run=",
        "ran-sentinel/v1 e2e-smoke begin run=1",
        "ran-sentinel/v1 redeploy-fleet begin run=" + RUN_ID,
        "ran-sentinel/v1 e2e-smoke end run=" + RUN_ID,
        "true",
        "1",
    ],
)
def test_classify_rejects_every_near_miss_token(bogus):
    """Empty, unset, truncated, wrong job, wrong phase, wrong run — all phantom.

    In particular the bare-prefix and `run=` cases prove the check is a full
    equality against a run-bound value, not a substring or truthiness test.
    """
    proc = _lib(
        f"ran_sentinel_classify e2e-smoke failure '{bogus}' '{_token('e2e-smoke', 'end')}'\n"
    )
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert proc.stdout.strip() == "phantom", (
        f"token {bogus!r} was accepted as proof that the job ran: "
        f"{proc.stdout!r}"
    )


def test_classify_rejects_an_unset_variable_under_set_u():
    """An unset variable must not become an accidental no-op.

    Under `set -u` an unset expansion aborts; the library reads its inputs with
    explicit `${x-}` defaults so the abort cannot be what decides the verdict.
    """
    proc = _run_body(
        "set -u\n"
        ". scripts/deploy/ran-sentinel.sh\n"
        f"ran_sentinel_classify e2e-smoke failure \"${{NEVER_SET-}}\" \"${{ALSO_NEVER_SET-}}\"\n",
        {},
    )
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert proc.stdout.strip() == "phantom", proc.stdout


def test_classify_refuses_an_empty_result():
    proc = _lib(
        f"ran_sentinel_classify e2e-smoke '' '{_token('e2e-smoke', 'begin')}' "
        f"'{_token('e2e-smoke', 'end')}' || echo rc=$?\n"
    )
    out = proc.stdout + proc.stderr
    assert "rc=2" in out, out
    assert "::error::" in out, out


def test_classify_full_truth_table():
    """Every state the three-valued sentinel can express, pinned."""
    good_b = _token("e2e-smoke", "begin")
    good_e = _token("e2e-smoke", "end")
    cases = [
        ("skipped", "", "", "not-dispatched"),
        ("failure", "", "", "phantom"),
        ("success", "", "", "phantom"),
        ("success", good_b, "", "phantom-green"),
        ("success", good_b, good_e, "ran-ok"),
        ("failure", good_b, "", "ran-failed-partial"),
        ("failure", good_b, good_e, "ran-failed"),
        ("cancelled", good_b, good_e, "ran-failed"),
    ]
    for result, begin, end, expected in cases:
        proc = _lib(f"ran_sentinel_classify e2e-smoke '{result}' '{begin}' '{end}'\n")
        assert proc.stdout.strip() == expected, (
            f"({result!r}, begin={bool(begin)}, end={bool(end)}) -> "
            f"{proc.stdout.strip()!r}, expected {expected!r}"
        )


def test_a_green_that_never_reached_its_last_step_is_not_a_green():
    """`phantom-green` must not be silently accepted as "nothing to do"."""
    proc = _run_decision(e2e_result="success", e2e_end="")
    out = proc.stdout + proc.stderr
    assert proc.returncode != 0, (
        "a success whose job never reached its final step was accepted as a "
        f"real green.\noutput:\n{out}"
    )
    assert not _rolled(proc), (
        "a missing sentinel CAUSED a rollback — forbidden direction: "
        + proc.rollback_log
    )


# ==========================================================================
# EMITTER / VERIFIER CROSS-CHECK
# ==========================================================================


@pytest.mark.parametrize("job", COVERED_JOBS + ["rollback-pin"])
@pytest.mark.parametrize("phase", ["BEGIN", "END"])
def test_emitted_token_matches_what_the_verifier_expects(job, phase):
    """Run the REAL emitter step and the REAL verifier; require agreement.

    The token text is necessarily duplicated: the emitting steps must run BEFORE
    `actions/checkout` (a marker that needs the repo on disk is not a marker
    that the job started), so they cannot source the library. This test is what
    keeps the two halves honest — edit one without the other and it goes red.
    """
    step = _step_named(job, f"ran-sentinel {phase}")
    proc = _run_body(step["run"], dict(step.get("env") or {}))
    assert proc.returncode == 0, proc.stdout + proc.stderr

    emitted = [
        line[len("token=") :]
        for line in proc.gh_output.splitlines()
        if line.startswith("token=")
    ]
    assert len(emitted) == 1, (
        f"{job}/{phase} wrote {len(emitted)} token line(s) to $GITHUB_OUTPUT: "
        f"{proc.gh_output!r}"
    )

    verifier = _lib(f"ran_sentinel_expect '{job}' '{phase.lower()}'\n")
    assert verifier.returncode == 0, verifier.stdout + verifier.stderr
    assert emitted[0] == verifier.stdout, (
        f"{job}/{phase} emits {emitted[0]!r} but the verifier expects "
        f"{verifier.stdout!r} — the emitter and the check have drifted, so the "
        f"sentinel could never be satisfied (or is satisfied by the wrong thing)."
    )
    assert emitted[0] == _token(job, phase.lower()), (
        f"{job}/{phase} emits {emitted[0]!r}, which is not the documented "
        f"format {_token(job, phase.lower())!r}"
    )


@pytest.mark.parametrize("job", COVERED_JOBS + ["rollback-pin"])
@pytest.mark.parametrize("phase", ["BEGIN", "END"])
def test_emitter_refuses_to_emit_a_run_independent_token(job, phase):
    """The emitter has the same vacuity trap and must fail closed too."""
    step = _step_named(job, f"ran-sentinel {phase}")
    env = dict(step.get("env") or {})
    env["GITHUB_RUN_ID"] = ""
    proc = _run_body(step["run"], env)
    out = proc.stdout + proc.stderr
    assert proc.returncode != 0, f"{job}/{phase} emitted with no run id: {out}"
    assert "token=" not in proc.gh_output, (
        f"{job}/{phase} still wrote a token: {proc.gh_output!r}"
    )


# ==========================================================================
# STRUCTURE — the marker cannot be emitted without the job body running
# ==========================================================================


@pytest.mark.parametrize("job", COVERED_JOBS + ["rollback-pin"])
def test_sentinel_is_first_and_last_step_of_every_covered_job(job):
    """First-step-and-last-step is the shape.

    BEGIN must be step 0 — before `actions/checkout`, before any `if:` — so that
    the ONLY way it can be emitted is for the job body to have been dispatched
    and started. END must be the final step under `if: always()` so that a job
    killed part-way through is distinguishable from one that completed, while a
    job that ran and genuinely FAILED still emits it (otherwise a real failure
    would suppress its own rollback).
    """
    steps = _job(job)["steps"]
    first, last = steps[0], steps[-1]

    assert "ran-sentinel BEGIN" in (first.get("name") or ""), (
        f"{job}: the ran-sentinel BEGIN marker is not the FIRST step "
        f"(first is {first.get('name') or first.get('uses')!r}). Any step ahead "
        f"of it could fail and leave a job that DID run looking like a phantom."
    )
    assert "if" not in first, (
        f"{job}: the BEGIN marker carries an `if:` — it must be unconditional, "
        f"or a false condition would forge a phantom."
    )
    assert "ran-sentinel END" in (last.get("name") or ""), (
        f"{job}: the ran-sentinel END marker is not the LAST step "
        f"(last is {last.get('name') or last.get('uses')!r})."
    )
    assert str(last.get("if", "")).strip() == "always()", (
        f"{job}: the END marker must be `if: always()`. Without it a job that "
        f"ran and genuinely FAILED would emit no END, and a real failure would "
        f"be misread — suppressing a rollback that is genuinely owed."
    )

    for step in (first, last):
        assert "continue-on-error" not in step, (
            f"{job}: a continue-on-error sentinel step could mask its own "
            f"failure to emit."
        )

    outputs = _job(job).get("outputs") or {}
    assert outputs.get("ran_begin") == "${{ steps.ran_begin.outputs.token }}", outputs
    assert outputs.get("ran_end") == "${{ steps.ran_end.outputs.token }}", outputs


def test_the_sentinel_path_contains_no_retry_and_no_masking():
    """A missing sentinel is a fault to REPORT, not a condition to paper over.

    Scoped to the jobs and step bodies this mechanism owns. `await-image`'s
    registry poll predates the sentinel and is a dynamic wait on a real signal,
    not a retry of a failed job — it is deliberately out of scope here.
    """
    jobs = _workflow()["jobs"]
    for job_key in COVERED_JOBS + ["rollback-pin", "rollback-audit"]:
        job = jobs[job_key]
        assert "continue-on-error" not in job, job_key
        for step in job.get("steps") or []:
            assert "continue-on-error" not in step, (job_key, step.get("name"))

    sentinel_bodies = [
        _step_named("rollback-pin", "Revert the staging pin")["run"],
        _step_named("rollback-audit", "ran-sentinel from rollback-pin")["run"],
    ]
    for job in COVERED_JOBS + ["rollback-pin"]:
        for phase in ("BEGIN", "END"):
            sentinel_bodies.append(_step_named(job, f"ran-sentinel {phase}")["run"])

    for body in sentinel_bodies:
        effective = "\n".join(
            line for line in body.splitlines() if not line.strip().startswith("#")
        ).lower()
        for banned in ("sleep ", "until ", "while ", "retry", "curl"):
            assert banned not in effective, (
                f"{banned!r} appears in a ran-sentinel body — this mechanism "
                f"must be a pure local decision with no waiting, retrying or "
                f"network dependency:\n{body}"
            )
