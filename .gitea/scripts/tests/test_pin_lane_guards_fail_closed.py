#!/usr/bin/env python3
"""The pin-bump lanes must go RED when their credential is missing.

WHY THIS FILE EXISTS. `sdk-pin-bump` shipped with a guard that emitted
`::warning::SDK_PIN_BUMP_TOKEN is not provisioned … Skipping`, wrote `skip=1`,
and let every later step's `if:` evaporate. Run 620174 (2026-08-05 12:19Z) was
dispatched during a live release chain, reported SUCCESS in 19 seconds having
done nothing, and the pin bump was then performed by hand (45a61bb, 12:23Z). The
green tick is the whole defect: it is indistinguishable from a bump that
happened. `promote-prod-tenant-pin` carried the identical shape on its plan step.

WHAT IS ASSERTED, AND WHY IT IS NOT A REGEX LINT. The tests below EXECUTE the
guard's own shell text, lifted straight out of the workflow YAML, with the
credential absent, and require a non-zero exit. A grep for `exit 1` would pass on
a guard whose `exit 1` sits in an unreachable branch; running it cannot. That
distinction is the point — this repository keeps re-learning that a gate nobody
has watched fail is not a gate.

WHAT IS *NOT* ASSERTED. This does not claim every credential guard in the repo
must be fatal. A lane with an arm that legitimately cannot hold a credential (a
fork PR, an anonymous-clone fallback that still does the work) may degrade. The
two lanes here have no such arm: both are `workflow_dispatch:`-only, so every run
is an explicit human request to act, and "cannot act" therefore has no honest
green rendering.
"""

from __future__ import annotations

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
WORKFLOWS = REPO / ".gitea" / "workflows"

SDK_PIN_BUMP = WORKFLOWS / "sdk-pin-bump.yml"
PROMOTE_PROD = WORKFLOWS / "promote-prod-tenant-pin.yml"


def _load(path: Path) -> dict:
    return yaml.safe_load(path.read_text(encoding="utf-8"))


def _steps(doc: dict) -> list[dict]:
    out: list[dict] = []
    for job in (doc.get("jobs") or {}).values():
        out.extend(job.get("steps") or [])
    return out


def _step_named(doc: dict, needle: str) -> dict:
    for step in _steps(doc):
        if needle in (step.get("name") or ""):
            return step
    raise AssertionError(f"no step whose name contains {needle!r}")


def _run_guard(script: str, env_overrides: dict[str, str]) -> subprocess.CompletedProcess:
    """Execute a workflow `run:` block under bash with a fake runner env."""
    with tempfile.TemporaryDirectory() as tmp:
        env = {
            "PATH": os.environ.get("PATH", ""),
            "HOME": tmp,
            "GITHUB_OUTPUT": os.path.join(tmp, "gh_output"),
            "GITHUB_STEP_SUMMARY": os.path.join(tmp, "gh_summary"),
            "RUNNER_TEMP": tmp,
        }
        for key in ("GITHUB_OUTPUT", "GITHUB_STEP_SUMMARY"):
            open(env[key], "w").close()
        env.update(env_overrides)
        return subprocess.run(
            [BASH, "-c", script],
            env=env,
            capture_output=True,
            text=True,
            timeout=60,
        )


# --------------------------------------------------------------------------
# sdk-pin-bump
# --------------------------------------------------------------------------

def test_sdk_pin_bump_guard_fails_when_token_absent():
    guard = _step_named(_load(SDK_PIN_BUMP), "bump credential present")
    proc = _run_guard(guard["run"], {"SDK_PIN_BUMP_TOKEN": ""})
    combined = proc.stdout + proc.stderr
    assert proc.returncode != 0, (
        "the guard exited 0 with no credential — that is the green-skip this "
        f"test exists to prevent. output:\n{combined}"
    )
    assert "SDK_PIN_BUMP_TOKEN" in combined, (
        "the failure must NAME the missing secret so the operator knows what to "
        f"provision. output:\n{combined}"
    )
    assert "::error::" in combined, "a fatal guard must annotate as ::error::, not ::warning::"


def test_sdk_pin_bump_guard_passes_when_token_present():
    """The negative proof is only meaningful next to the positive one.

    A guard that failed unconditionally would satisfy the test above while
    breaking the lane outright.
    """
    guard = _step_named(_load(SDK_PIN_BUMP), "bump credential present")
    proc = _run_guard(guard["run"], {"SDK_PIN_BUMP_TOKEN": "not-a-real-token"})
    assert proc.returncode == 0, proc.stdout + proc.stderr


def test_sdk_pin_bump_has_no_credential_derived_skip_gate():
    """No step may be conditioned on a "we have no credential" output.

    This is the structural half: even if the guard itself is fatal,
    reintroducing `if: steps.guard.outputs.skip == '0'` on the working steps
    would recreate a vacuous lane the moment someone softened the guard again.
    """
    doc = _load(SDK_PIN_BUMP)
    for step in _steps(doc):
        cond = str(step.get("if") or "")
        assert "skip" not in cond, (
            f"step {step.get('name')!r} is gated on a skip flag ({cond!r}); the "
            "credential guard must fail the job instead of disarming the steps"
        )


def test_sdk_pin_bump_push_identity_is_the_documented_narrow_bot():
    """The pushing identity is the ONLY thing that narrows this credential.

    Gitea PAT scopes are category-wide: `write:repository` grants write to every
    repo the owning user can write. So swapping the push identity back to a
    broader bot silently widens the token's blast radius without changing a
    single scope string. Pin it to the identity the runbook documents.
    """
    step = _step_named(_load(SDK_PIN_BUMP), "Open the bump PR")
    assert "molecule-sdk-pin-bot" in step["run"]
    assert "molecule-runtime-release-bot" not in step["run"], (
        "molecule-runtime-release-bot has write on 13 other repos; this lane's "
        "credential must not be minted on it"
    )
    runbook = REPO / "docs" / "runbooks" / "sdk-pin-bump-credential.md"
    assert runbook.is_file(), "the credential must stay documented, not become folklore"
    assert "molecule-sdk-pin-bot" in runbook.read_text(encoding="utf-8")


def test_sdk_pin_bump_targets_the_dispatched_ref():
    """A dispatch off main must not propose that ref's content into main."""
    step = _step_named(_load(SDK_PIN_BUMP), "Open the bump PR")
    assert step.get("env", {}).get("BASE_REF") == "${{ github.ref_name }}"
    assert '--base "${BASE_REF}"' in step["run"]


# --------------------------------------------------------------------------
# promote-prod-tenant-pin
# --------------------------------------------------------------------------

def test_promote_prod_plan_fails_when_cp_token_absent():
    plan = _step_named(_load(PROMOTE_PROD), "Plan — current pin vs target")
    proc = _run_guard(plan["run"], {"CP_ADMIN_API_TOKEN": "", "TAG": "staging-deadbee", "TENANT_IMAGE": "example/img", "CP_BASE_URL": "http://127.0.0.1:1"})
    combined = proc.stdout + proc.stderr
    assert proc.returncode != 0, (
        "the prod plan step exited 0 without reading the live pin — a plan that "
        f"planned nothing must not be green. output:\n{combined}"
    )
    assert "CP_ADMIN_API_TOKEN" in combined
    assert "::error::" in combined


def test_promote_prod_apply_stays_double_gated():
    """Guard against this file's own change scope creeping into an arming change.

    Making the plan loud must never become "and while we were here, we let it
    promote". The apply step must remain conditioned on the mode decision, and
    that decision must still require both the freeze being off and the explicit
    arming variable.
    """
    doc = _load(PROMOTE_PROD)
    apply_step = _step_named(doc, "Apply — promote BOTH SSOTs")
    assert apply_step.get("if") == "${{ steps.mode.outputs.apply == '1' }}"

    mode = _step_named(doc, "Decide plan vs apply")
    assert "PROD_AUTO_DEPLOY_DISABLED" in mode["run"]
    assert "PROMOTE_ARMED" in mode["run"]

    on = doc.get("on", doc.get(True))
    assert set(on or {}) == {"workflow_dispatch"}, (
        "promote-prod-tenant-pin must stay dispatch-only; a push/schedule trigger "
        "here would mean prod tenants roll unattended"
    )


if __name__ == "__main__":
    raise SystemExit(pytest.main([__file__, "-v"]))
