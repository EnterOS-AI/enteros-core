"""Guards for the PRODUCTION molecule-tenant pin planner.

The production fleet is split across TWO control-plane databases on the prod
box — `molecule-cp-prod` (:8092) and `molecule-cp-staging` (:8096) — and tenants
are assigned to one or the other by their `cp-env` label. Verified live on
2026-08-08: the two CPs held DIFFERENT `molecule-tenant` pins
(`sha256:747d3df6…` / git `f3c9eaf24…` on :8092 versus `sha256:f1972d8c…` /
git `1802ebe22…` on :8096).

A planner that reads ONE endpoint therefore renders a plan that is true for at
most half the fleet, and — this is the dangerous part — a promote against one
endpoint returns 200 while the reconciler reports the out-of-scope tenants
converged *against their own pin*. The miss is silent and looks exactly like
success. These tests pin the multi-CP contract so that shape cannot come back.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
PLAN = ROOT / ".gitea" / "scripts" / "prod_pin_plan.py"

TARGET = "registry.moleculesai.app/molecule-ai/molecule-tenant:staging-1802ebe"


def _pins(digest: str, git_sha: str) -> dict:
    return {
        "pins": [
            {
                "template_name": "claude-code",
                "region": "global",
                "image_digest": "sha256:" + "a" * 64,
                "git_sha": "09d62b9",
            },
            {
                "template_name": "molecule-tenant",
                "region": "global",
                "image_digest": digest,
                "git_sha": git_sha,
                "promoted_at": "2026-08-07T23:11:01Z",
                "promoted_by": "api-token-SYNTHETIC",
            },
        ]
    }


def _write(tmp_path: Path, name: str, doc: object) -> Path:
    p = tmp_path / name
    p.write_text(json.dumps(doc), encoding="utf-8")
    return p


def _run(args: list[str]) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(PLAN), *args],
        capture_output=True,
        text=True,
        check=False,
    )


def test_plan_renders_every_control_plane_not_just_the_first(tmp_path: Path):
    """Both CP endpoints must appear in the rendered plan.

    This is the live 2026-08-08 divergence: :8092 behind trunk, :8096 already on
    it. A single-CP renderer prints one section and silently drops the other.
    """
    a = _write(tmp_path, "cp_prod.json", _pins("sha256:" + "7" * 64, "f3c9eaf24"))
    b = _write(tmp_path, "cp_staging.json", _pins("sha256:" + "f" * 64, "1802ebe22"))

    r = _run(["--target", TARGET, f"cp_prod={a}", f"cp_staging={b}"])

    assert r.returncode == 0, f"planner failed: {r.stderr}"
    assert "cp_prod" in r.stdout, "plan omitted the cp_prod endpoint"
    assert "cp_staging" in r.stdout, "plan omitted the cp_staging endpoint"
    assert "7" * 64 in r.stdout, "plan omitted the cp_prod current digest"
    assert "f" * 64 in r.stdout, "plan omitted the cp_staging current digest"


def test_absent_tenant_row_on_a_LATER_endpoint_is_fatal(tmp_path: Path):
    """An absent row must fail even when it is not the FIRST endpoint.

    `prod_pin_plan.py` already refused an absent `(molecule-tenant, global)` row
    — but only for the one document it read. With several endpoints the failure
    has to be checked per endpoint, otherwise a missing reserved pin row on the
    second CP renders as a clean plan and a fresh provision there would fail.
    """
    good = _write(tmp_path, "cp_prod.json", _pins("sha256:" + "7" * 64, "f3c9eaf24"))
    bad = _write(tmp_path, "cp_staging.json", {"pins": [
        {"template_name": "claude-code", "region": "global",
         "image_digest": "sha256:" + "a" * 64, "git_sha": "09d62b9"},
    ]})

    r = _run(["--target", TARGET, f"cp_prod={good}", f"cp_staging={bad}"])

    assert r.returncode == 1, (
        "planner exited %r with a MISSING (molecule-tenant, global) row on the "
        "second endpoint — an absent reserved row must never render as 'nothing "
        "to compare'" % r.returncode
    )
    assert "cp_staging" in (r.stderr + r.stdout), (
        "the error must name WHICH endpoint is missing the row"
    )


def test_plan_reports_per_endpoint_whether_a_promote_is_needed(tmp_path: Path):
    """The plan must say, per CP, whether the target is already live there.

    Without this the operator cannot tell a two-CP promote from a one-CP one,
    which is exactly how the 2026-08-05 fleet roll needed the promote executed
    twice before anyone noticed.
    """
    target_digest = "sha256:" + "f" * 64
    behind = _write(tmp_path, "a.json", _pins("sha256:" + "7" * 64, "f3c9eaf24"))
    already = _write(tmp_path, "b.json", _pins(target_digest, "1802ebe22"))

    r = _run([
        "--target", TARGET,
        "--target-digest", target_digest,
        f"cp_prod={behind}",
        f"cp_staging={already}",
    ])

    assert r.returncode == 0, f"planner failed: {r.stderr}"
    out = r.stdout
    assert "PROMOTE REQUIRED" in out, "plan never flags the endpoint that is behind"
    assert "already at target" in out, "plan never flags the endpoint already on target"


def test_single_endpoint_form_still_works(tmp_path: Path):
    """Backward compatibility: one endpoint is a degenerate multi-CP plan."""
    a = _write(tmp_path, "one.json", _pins("sha256:" + "7" * 64, "f3c9eaf24"))
    r = _run(["--target", TARGET, f"cp_prod={a}"])
    assert r.returncode == 0, f"planner failed: {r.stderr}"
    assert "7" * 64 in r.stdout


def test_unreadable_document_on_any_endpoint_is_fatal(tmp_path: Path):
    """A CP whose response will not parse must not be skipped silently."""
    good = _write(tmp_path, "good.json", _pins("sha256:" + "7" * 64, "f3c9eaf24"))
    bad = tmp_path / "bad.json"
    bad.write_text("not json at all", encoding="utf-8")

    r = _run(["--target", TARGET, f"cp_prod={good}", f"cp_staging={bad}"])

    assert r.returncode == 1, "an unparseable CP response must fail the plan"
    assert "cp_staging" in (r.stderr + r.stdout)


# --------------------------------------------------------------------------
# --needed-out: the machine-readable verdict the apply step gates on
# --------------------------------------------------------------------------

def test_needed_out_lists_only_the_endpoints_behind_target(tmp_path: Path):
    """The apply gate is only as good as this file actually being written.

    Caught live: the workflow passed --needed-out and the plan rendered
    correctly, but the planner never created the file, so the apply step would
    have hit its "no verdict file" branch and refused every time. A
    workflow-text assertion cannot see that — only running the planner can.
    """
    target_digest = "sha256:" + "f" * 64
    behind = _write(tmp_path, "a.json", _pins("sha256:" + "7" * 64, "f3c9eaf24"))
    already = _write(tmp_path, "b.json", _pins(target_digest, "1802ebe22"))
    out = tmp_path / "needed.txt"

    r = _run([
        "--target", TARGET, "--target-digest", target_digest,
        "--needed-out", str(out),
        f"cp_prod={behind}", f"cp_staging={already}",
    ])
    assert r.returncode == 0, r.stderr
    assert out.exists(), "--needed-out was accepted but no verdict file was written"
    assert out.read_text(encoding="utf-8").split() == ["cp_prod"], out.read_text(encoding="utf-8")


def test_needed_out_is_written_EMPTY_when_nothing_needs_promoting(tmp_path: Path):
    """Empty file != absent file.

    The apply step treats an absent file as "the plan never ran" and refuses,
    and an empty one as "nothing to do". Writing nothing at all would turn a
    clean no-op into a hard failure.
    """
    target_digest = "sha256:" + "f" * 64
    a = _write(tmp_path, "a.json", _pins(target_digest, "1802ebe22"))
    b = _write(tmp_path, "b.json", _pins(target_digest, "1802ebe22"))
    out = tmp_path / "needed.txt"

    r = _run([
        "--target", TARGET, "--target-digest", target_digest,
        "--needed-out", str(out),
        f"cp_prod={a}", f"cp_staging={b}",
    ])
    assert r.returncode == 0, r.stderr
    assert out.exists(), "the verdict file must exist even when nothing needs promoting"
    assert out.read_text(encoding="utf-8").strip() == ""


def test_no_verdict_file_is_written_when_an_endpoint_is_invalid(tmp_path: Path):
    """A failed plan must not leave a stale all-clear behind."""
    good = _write(tmp_path, "good.json", _pins("sha256:" + "7" * 64, "f3c9eaf24"))
    bad = _write(tmp_path, "bad.json", {"pins": []})
    out = tmp_path / "needed.txt"

    r = _run([
        "--target", TARGET, "--target-digest", "sha256:" + "f" * 64,
        "--needed-out", str(out),
        f"cp_prod={good}", f"cp_staging={bad}",
    ])
    assert r.returncode == 1
    assert not out.exists(), "a failed plan wrote a verdict file — an apply could read it as all-clear"
