"""The staging CD lane's k8s arm, asserted against the SHIPPED `run:` bodies.

WHY THE STEP BODIES AND NOT A HELPER
------------------------------------
The defect these tests lock down is a WIRING defect, not a logic defect. For
eight days `redeploy-fleet` passed no census source into
redeploy-staging-fleet.sh, so the k8s arm never armed, the script's anti-vacuity
guard refused (correctly), and rollback-pin reverted the pin on every run — while
every staging tenant stayed on a 2026-08-05 build.

A test that exercised a helper function would have been green throughout, because
the helper was fine and nothing called it with what it needed. So each behavioural
case below EXTRACTS the real `run:` body out of
.gitea/workflows/staging-tenant-cd.yml, substitutes the `${{ }}` expressions the
way Gitea would, and executes it under the same interpreter Gitea uses —
`bash --noprofile --norc -e -o pipefail`. Delete the assertion from the workflow
and the corresponding case here goes red.

That `-e` matters and is why the bodies are run with it rather than with plain
`bash`: a failing command aborts the step BEFORE any diagnostic it was supposed
to print, and this repository has shipped that exact defect (a capture whose
failure killed the step above its own `-z` guard). Several cases below assert
that a specific diagnostic REACHED the output, which is only true if the body
still captures exit status instead of letting `-e` win.

The substrate-roller behaviour itself (empty census refused, failed roll fails,
an exclusion must be earned) is covered offline in
scripts/deploy/tests/test-staging-fleet-k8s-wiring.sh, which drives the real
scripts against a fake docker + fake kubectl.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import textwrap
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[3]
WORKFLOW = ROOT / ".gitea" / "workflows" / "staging-tenant-cd.yml"

BASH = shutil.which("bash")


def _doc():
    return yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))


def _job(key: str):
    jobs = _doc()["jobs"]
    assert key in jobs, f"{key} is missing from {WORKFLOW.name}"
    return jobs[key]


def _step(job_key: str, name_fragment: str):
    for step in _job(job_key).get("steps") or []:
        if name_fragment.lower() in str(step.get("name", "")).lower():
            return step
    raise AssertionError(
        f"no step in `{job_key}` whose name contains {name_fragment!r} — the step "
        f"this test asserts on was renamed or deleted, which is itself the "
        f"regression (a guard that no longer runs)."
    )


_EXPR = re.compile(r"\$\{\{[^}]*\}\}")


def _render(run: str, substitutions: dict[str, str] | None = None) -> str:
    """Substitute `${{ … }}` the way the templater does.

    Any expression NOT explicitly given a value is replaced with the empty
    string — the same thing Gitea does for an unset var — rather than left in
    place, where bash would see literal `${{` and die with a syntax error that
    would mask whatever the case was actually testing.
    """
    subs = substitutions or {}

    def repl(m: re.Match) -> str:
        key = m.group(0).strip("${} ").strip()
        return subs.get(key, "")

    return _EXPR.sub(repl, run)


def _run_body(
    body: str,
    tmp_path: Path,
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess:
    """Execute a rendered step body exactly as Gitea does."""
    script = tmp_path / "step.sh"
    script.write_text(body, encoding="utf-8")
    full = {
        "PATH": os.environ.get("PATH", ""),
        "RUNNER_TEMP": str(tmp_path / "runnertmp"),
        "GITHUB_ENV": str(tmp_path / "github.env"),
        "GITHUB_OUTPUT": str(tmp_path / "github.output"),
        "GITHUB_STEP_SUMMARY": str(tmp_path / "summary.md"),
        "GITHUB_RUN_ID": "424242",
        "RUNNER_NAME": "test-runner",
    }
    (tmp_path / "runnertmp").mkdir(exist_ok=True)
    for f in ("github.env", "github.output", "summary.md"):
        (tmp_path / f).touch()
    full.update(env or {})
    return subprocess.run(
        [BASH, "--noprofile", "--norc", "-e", "-o", "pipefail", str(script)],
        cwd=tmp_path,
        env=full,
        capture_output=True,
        text=True,
    )


def _fake_scripts(tmp_path: Path, **bodies: str) -> None:
    """Create scripts/deploy/<name> stubs the rendered body will invoke by path."""
    d = tmp_path / "scripts" / "deploy"
    d.mkdir(parents=True, exist_ok=True)
    for name, body in bodies.items():
        # `redeploy_staging_fleet_sh` -> `redeploy-staging-fleet.sh`
        assert name.endswith("_sh"), f"stub name {name!r} must end in _sh"
        p = d / (name[: -len("_sh")].replace("_", "-") + ".sh")
        p.write_text("#!/usr/bin/env bash\n" + textwrap.dedent(body), encoding="utf-8")
        p.chmod(0o755)


pytestmark = pytest.mark.skipif(BASH is None, reason="bash is required to execute step bodies")


# ---------------------------------------------------------------------------
# Structural: the wiring exists at all.
# ---------------------------------------------------------------------------

def test_advance_pin_publishes_the_digests_the_k8s_arm_rolls_to():
    """A tag is not a usable target for the k8s arm.

    Tenant Deployments pull from a different registry HOST than the tag this lane
    dispatches, so re-pinning them by tag points them at a registry the cluster
    is not configured for. The roller's --digest mode re-pins each workload to
    <its own repo>@<digest>; without these outputs there is no digest to pass.
    """
    outputs = _job("advance-pin").get("outputs") or {}
    for key in ("new_digest", "old_digest"):
        assert key in outputs, (
            f"advance-pin does not publish `{key}`. Without it the forward roll "
            f"(or the revert) has no digest and would fall back to a tag, which "
            f"names a registry host the tenant workloads do not pull from."
        )


def test_the_fleet_jobs_run_where_kubectl_and_the_staging_daemon_are():
    """`docker-host` is outside k3s and is not the staging tenant daemon.

    Moving either job back there silently restores the original defect: no
    kubectl means no census and no k8s roll, and the docker arm enumerates a CI
    build daemon instead of the daemon the staging provisioner mutates.
    """
    for jk in ("redeploy-fleet", "rollback-pin"):
        assert _job(jk).get("runs-on") == "local-deploy", (
            f"`{jk}` must run on `local-deploy`: it is the only runner with a "
            f"route to the cluster AND the operator-configured mTLS endpoint for "
            f"the staging tenant daemon."
        )


def test_the_roll_step_passes_a_digest_and_an_exclusion_list():
    env = _step("redeploy-fleet", "Roll the staging tenant fleet").get("env") or {}
    assert env.get("K8S_TENANT_DIGEST") == "${{ needs.advance-pin.outputs.new_digest }}", (
        "the roll step must hand the k8s arm the digest advance-pin just promoted"
    )
    assert "EXCLUDE_SLUGS" in env, (
        "the roll step must pass EXCLUDE_SLUGS through; redeploy-staging-fleet.sh "
        "reads it as EXCLUDE and forwards it to the k8s roller"
    )


def test_no_step_in_this_lane_asserts_an_empty_fleet():
    """`--allow-empty` makes every anti-vacuity guard in the lane optional."""
    offenders = []
    for jk, job in _doc()["jobs"].items():
        for step in job.get("steps") or []:
            run = step.get("run") or ""
            for line in run.splitlines():
                stripped = line.strip()
                if stripped.startswith("#"):
                    continue  # naming the flag in a comment is fine
                if "--allow-empty" in stripped:
                    offenders.append(f"{jk} :: {step.get('name')!r} -> {stripped}")
    assert not offenders, (
        "a step in the staging CD lane passes --allow-empty. That flag is the one "
        "way a roll that touches nothing can exit 0, and this is the lane that "
        "reported `tenants=0 … success` for twelve days.\n    "
        + "\n    ".join(offenders)
    )


def test_the_revert_puts_the_pin_back_before_it_needs_a_cluster_credential():
    """Ordering is the whole safety property of the rollback path.

    Everything after the census resolution needs a credential the operator may
    not have seeded. If that resolution came FIRST, an absent secret would fail
    rollback-pin before the pin was restored, leaving staging pinned to a
    candidate that had already failed — strictly worse than the state this job
    exists to repair.
    """
    run = _step("rollback-pin", "Revert the staging pin").get("run") or ""
    pin = run.find("advance-staging-tenant-pin.sh --image")
    census = run.find("resolve-k8s-fleet-census.sh")
    assert pin != -1, "the revert step no longer calls advance-staging-tenant-pin.sh"
    assert census != -1, (
        "the revert step no longer resolves a census, so its fleet re-roll would "
        "roll the docker substrate only — i.e. nothing — while reporting success"
    )
    assert pin < census, (
        "the revert resolves a cluster credential BEFORE restoring the pin. An "
        "unseeded credential would then strand staging on the failed candidate."
    )


# ---------------------------------------------------------------------------
# Behavioural: execute the shipped bodies.
# ---------------------------------------------------------------------------

def test_the_roll_body_refuses_when_the_census_never_reached_it(tmp_path: Path):
    """The exact regression: the roll step running with no census wiring.

    Left to the script, this surfaces as "0 staging tenant containers found …
    and no k8s arm was dispatched" — true, but it points at the fleet rather
    than at the wiring that failed to arm it, which is how this lane stayed
    misdiagnosed for eight days.
    """
    body = _render(_step("redeploy-fleet", "Roll the staging tenant fleet")["run"])
    _fake_scripts(tmp_path, redeploy_staging_fleet_sh="exit 0\n")
    r = _run_body(body, tmp_path, {"STAGING_TENANT_FLAGS": ""})
    assert r.returncode == 1, f"expected refusal, got {r.returncode}\n{r.stdout}{r.stderr}"
    assert "SUBSTRATE_CENSUS_CMD is unset" in (r.stdout + r.stderr)


def test_the_roll_body_runs_the_fleet_script_once_the_census_is_wired(tmp_path: Path):
    """The positive control.

    Without it the case above would also pass if the step had been replaced with
    `exit 1` — a guard that refuses everything is not a guard.
    """
    body = _render(
        _step("redeploy-fleet", "Roll the staging tenant fleet")["run"],
        {"needs.await-image.outputs.tag": "staging-0badc0de"},
    )
    marker = tmp_path / "rolled"
    _fake_scripts(
        tmp_path,
        redeploy_staging_fleet_sh=f'printf "%s\\n" "$*" > "{marker.as_posix()}"\nexit 0\n',
    )
    r = _run_body(
        body,
        tmp_path,
        {"STAGING_TENANT_FLAGS": "", "SUBSTRATE_CENSUS_CMD": "/bin/true"},
    )
    assert r.returncode == 0, f"{r.stdout}{r.stderr}"
    assert marker.exists(), "the fleet script was never invoked"
    assert "--tag staging-0badc0de" in marker.read_text(encoding="utf-8")


def test_the_census_body_refuses_a_resolver_that_succeeds_and_writes_nothing(tmp_path: Path):
    """An empty resolve is its own failure, named separately from a failed one.

    Conflating them is how a reporting fault hides: the roller would then be
    handed no census source and its own guard would report "the fleet is empty",
    a fact about this wiring dressed up as a fact about staging.
    """
    body = _render(_step("redeploy-fleet", "Resolve the cluster credential")["run"])
    _fake_scripts(tmp_path, resolve_k8s_fleet_census_sh="exit 0\n")
    r = _run_body(body, tmp_path)
    assert r.returncode == 1, f"{r.stdout}{r.stderr}"
    assert "wrote NOTHING" in (r.stdout + r.stderr)


def test_the_census_body_surfaces_a_resolver_failure_instead_of_dying_on_it(tmp_path: Path):
    """`-e` discipline, asserted through the shipped body.

    A bare `bash resolve-…` here would abort the step at that line and this
    diagnostic would never print. Asserting the diagnostic REACHED the output is
    what proves the rc-captured form is still there.
    """
    body = _render(_step("redeploy-fleet", "Resolve the cluster credential")["run"])
    _fake_scripts(tmp_path, resolve_k8s_fleet_census_sh="exit 7\n")
    r = _run_body(body, tmp_path)
    assert r.returncode == 7, f"expected the resolver's own exit code, got {r.returncode}"
    assert "could not resolve a cluster credential" in (r.stdout + r.stderr)


def test_the_census_body_publishes_the_wiring_on_the_happy_path(tmp_path: Path):
    body = _render(_step("redeploy-fleet", "Resolve the cluster credential")["run"])
    _fake_scripts(
        tmp_path,
        resolve_k8s_fleet_census_sh=(
            'out=""\n'
            'while [ "$#" -gt 0 ]; do [ "$1" = "--env-out" ] && out="$2"; shift; done\n'
            'printf "KUBECONFIG=/tmp/k\\nSUBSTRATE_CENSUS_CMD=/tmp/c\\nFLEET_CENSUS_TSV=/tmp/t\\n" > "$out"\n'
        ),
    )
    r = _run_body(body, tmp_path)
    assert r.returncode == 0, f"{r.stdout}{r.stderr}"
    published = (tmp_path / "github.env").read_text(encoding="utf-8")
    for key in ("KUBECONFIG=", "SUBSTRATE_CENSUS_CMD=", "FLEET_CENSUS_TSV="):
        assert key in published, f"{key} never reached $GITHUB_ENV:\n{published}"


def test_the_drift_body_refuses_to_plan_against_a_moving_tag(tmp_path: Path):
    """No digest means no verifiable target, and a tag means the wrong registry."""
    body = _render(_step("redeploy-fleet", "Fleet drift audit")["run"])
    _fake_scripts(tmp_path, fleet_drift_audit_sh="exit 0\n")
    r = _run_body(body, tmp_path)
    assert r.returncode == 1, f"{r.stdout}{r.stderr}"
    assert "Refusing to fall back to a moving tag" in (r.stdout + r.stderr)


def test_the_toolchain_body_refuses_an_empty_resolution(tmp_path: Path):
    """A preflight that exits 0 having resolved nothing puts every later step
    back on ambient PATH lookup while reporting success."""
    body = _render(_step("redeploy-fleet", "resolve the toolchain")["run"])
    _fake_scripts(tmp_path, require_deploy_toolchain_sh="exit 0\n")
    r = _run_body(body, tmp_path)
    assert r.returncode == 1, f"{r.stdout}{r.stderr}"
    assert "resolved NOTHING" in (r.stdout + r.stderr)


def test_the_revert_body_restores_the_pin_even_when_the_cluster_is_unreachable(tmp_path: Path):
    """THE rollback safety property, executed rather than asserted structurally.

    With no cluster credential the fleet cannot be rolled back — but the PIN
    must already be back on the previous image, and the step must say plainly
    that the two are now out of step and need an operator.
    """
    body = _render(
        _step("rollback-pin", "Revert the staging pin")["run"],
        {
            "needs.advance-pin.result": "success",
            "needs.redeploy-fleet.result": "failure",
            "needs.runtime-image-readiness.result": "success",
            "needs.e2e-smoke.result": "skipped",
        },
    )
    # The real ran-sentinel library, sourced by the body, plus stubs for
    # everything that would touch a control plane or a cluster.
    (tmp_path / "scripts" / "deploy").mkdir(parents=True, exist_ok=True)
    shutil.copy(
        ROOT / "scripts" / "deploy" / "ran-sentinel.sh",
        tmp_path / "scripts" / "deploy" / "ran-sentinel.sh",
    )
    pin_marker = tmp_path / "pin-reverted"
    _fake_scripts(
        tmp_path,
        advance_staging_tenant_pin_sh=f'printf "%s\\n" "$*" > "{pin_marker.as_posix()}"\nexit 0\n',
        require_local_deploy_daemon_sh="exit 0\n",
        require_deploy_toolchain_sh='printf "KUBECTL=/bin/true\\nDEPLOY_PYTHON3=/bin/true\\n"\n',
        # The credential the operator has not seeded yet.
        resolve_k8s_fleet_census_sh="exit 1\n",
        redeploy_staging_fleet_sh='echo "THE FLEET WAS ROLLED" >&2\nexit 0\n',
    )
    rid = "424242"
    env = {
        "ADVANCE_RESULT": "success",
        "REDEPLOY_RESULT": "failure",
        "READINESS_RESULT": "success",
        "E2E_RESULT": "skipped",
        "REDEPLOY_BEGIN": f"ran-sentinel/v1 redeploy-fleet begin run={rid}",
        "REDEPLOY_END": f"ran-sentinel/v1 redeploy-fleet end run={rid}",
        "READINESS_BEGIN": f"ran-sentinel/v1 runtime-image-readiness begin run={rid}",
        "READINESS_END": f"ran-sentinel/v1 runtime-image-readiness end run={rid}",
        "E2E_BEGIN": "",
        "E2E_END": "",
        "OLD_IMAGE": "registry.example.invalid/molecule-ai/molecule-tenant:staging-0ldsha0",
        "OLD_GIT_SHA": "0ldsha0",
        "OLD_DIGEST": "sha256:" + "a" * 64,
        "STAGING_TENANT_FLAGS": "",
    }
    r = _run_body(body, tmp_path, env)
    combined = r.stdout + r.stderr
    assert pin_marker.exists(), (
        "the pin was NOT reverted before the cluster credential was needed — an "
        "unseeded secret would strand staging on the failed candidate:\n" + combined
    )
    assert "--image registry.example.invalid" in pin_marker.read_text(encoding="utf-8")
    assert r.returncode != 0, "a revert that could not roll the fleet must not report success"
    assert "PIN has been reverted" in combined and "FLEET could not be" in combined
    assert "THE FLEET WAS ROLLED" not in combined, (
        "the fleet re-roll ran despite an unresolvable census — it would have "
        "rolled the docker substrate only, i.e. nothing, and exited 0"
    )


def test_the_revert_body_rolls_the_fleet_back_when_the_cluster_is_reachable(tmp_path: Path):
    """Positive control for the case above: the revert must still actually roll."""
    body = _render(
        _step("rollback-pin", "Revert the staging pin")["run"],
        {"needs.advance-pin.result": "success"},
    )
    (tmp_path / "scripts" / "deploy").mkdir(parents=True, exist_ok=True)
    shutil.copy(
        ROOT / "scripts" / "deploy" / "ran-sentinel.sh",
        tmp_path / "scripts" / "deploy" / "ran-sentinel.sh",
    )
    roll_marker = tmp_path / "rolled"
    _fake_scripts(
        tmp_path,
        advance_staging_tenant_pin_sh="exit 0\n",
        require_local_deploy_daemon_sh="exit 0\n",
        require_deploy_toolchain_sh='printf "KUBECTL=/bin/true\\nDEPLOY_PYTHON3=/bin/true\\n"\n',
        resolve_k8s_fleet_census_sh=(
            'out=""\n'
            'while [ "$#" -gt 0 ]; do [ "$1" = "--env-out" ] && out="$2"; shift; done\n'
            'printf "SUBSTRATE_CENSUS_CMD=/bin/true\\n" > "$out"\n'
        ),
        redeploy_staging_fleet_sh=(
            f'printf "digest=%s exclude=%s args=%s\\n" '
            f'"${{K8S_TENANT_DIGEST:-}}" "${{EXCLUDE_SLUGS:-<unset>}}" "$*" '
            f'> "{roll_marker.as_posix()}"\nexit 0\n'
        ),
    )
    rid = "424242"
    env = {
        "ADVANCE_RESULT": "success",
        "REDEPLOY_RESULT": "failure",
        "READINESS_RESULT": "success",
        "E2E_RESULT": "skipped",
        "REDEPLOY_BEGIN": f"ran-sentinel/v1 redeploy-fleet begin run={rid}",
        "REDEPLOY_END": f"ran-sentinel/v1 redeploy-fleet end run={rid}",
        "READINESS_BEGIN": f"ran-sentinel/v1 runtime-image-readiness begin run={rid}",
        "READINESS_END": f"ran-sentinel/v1 runtime-image-readiness end run={rid}",
        "E2E_BEGIN": "",
        "E2E_END": "",
        "OLD_IMAGE": "registry.example.invalid/molecule-ai/molecule-tenant:staging-0ldsha0",
        "OLD_GIT_SHA": "0ldsha0",
        "OLD_DIGEST": "sha256:" + "a" * 64,
        "STAGING_TENANT_FLAGS": "",
    }
    r = _run_body(body, tmp_path, env)
    assert r.returncode == 0, f"{r.stdout}{r.stderr}"
    assert roll_marker.exists(), "the fleet was never re-rolled"
    rolled = roll_marker.read_text(encoding="utf-8")
    assert "digest=sha256:" + "a" * 64 in rolled, (
        "the revert did not hand the k8s arm the PREVIOUS digest, so the k8s "
        "tenants would have been left on the failed candidate:\n" + rolled
    )


def test_the_revert_refuses_when_there_is_no_previous_digest(tmp_path: Path):
    """No prior pin means no k8s target to revert to. Rolling only the docker
    arm and exiting 0 would report a rollback that did not happen."""
    body = _render(
        _step("rollback-pin", "Revert the staging pin")["run"],
        {"needs.advance-pin.result": "success"},
    )
    (tmp_path / "scripts" / "deploy").mkdir(parents=True, exist_ok=True)
    shutil.copy(
        ROOT / "scripts" / "deploy" / "ran-sentinel.sh",
        tmp_path / "scripts" / "deploy" / "ran-sentinel.sh",
    )
    _fake_scripts(
        tmp_path,
        advance_staging_tenant_pin_sh="exit 0\n",
        require_local_deploy_daemon_sh="exit 0\n",
        require_deploy_toolchain_sh='printf "KUBECTL=/bin/true\\nDEPLOY_PYTHON3=/bin/true\\n"\n',
        resolve_k8s_fleet_census_sh=(
            'out=""\n'
            'while [ "$#" -gt 0 ]; do [ "$1" = "--env-out" ] && out="$2"; shift; done\n'
            'printf "SUBSTRATE_CENSUS_CMD=/bin/true\\n" > "$out"\n'
        ),
        redeploy_staging_fleet_sh='echo "THE FLEET WAS ROLLED" >&2\nexit 0\n',
    )
    rid = "424242"
    env = {
        "ADVANCE_RESULT": "success",
        "REDEPLOY_RESULT": "failure",
        "READINESS_RESULT": "success",
        "E2E_RESULT": "skipped",
        "REDEPLOY_BEGIN": f"ran-sentinel/v1 redeploy-fleet begin run={rid}",
        "REDEPLOY_END": f"ran-sentinel/v1 redeploy-fleet end run={rid}",
        "READINESS_BEGIN": f"ran-sentinel/v1 runtime-image-readiness begin run={rid}",
        "READINESS_END": f"ran-sentinel/v1 runtime-image-readiness end run={rid}",
        "E2E_BEGIN": "",
        "E2E_END": "",
        "OLD_IMAGE": "registry.example.invalid/molecule-ai/molecule-tenant:staging-0ldsha0",
        "OLD_GIT_SHA": "0ldsha0",
        "OLD_DIGEST": "",
        "STAGING_TENANT_FLAGS": "",
    }
    r = _run_body(body, tmp_path, env)
    combined = r.stdout + r.stderr
    assert r.returncode != 0, combined
    assert "no old_digest" in combined
    assert "THE FLEET WAS ROLLED" not in combined
