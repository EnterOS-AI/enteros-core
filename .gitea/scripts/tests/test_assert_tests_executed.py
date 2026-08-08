#!/usr/bin/env python3
"""The no-tests-executed gate, mutated.

Every test here BREAKS something and asserts the gate goes red. A test suite
for a gate that only ever feeds it healthy input proves the gate runs, not
that it checks — and "it runs" is exactly the property the gate under test
exists to refuse in others.

The gate is .gitea/scripts/assert-tests-executed.py. It replaced this grep,
which lived inline in ci.yml's "Race + verbose per-package gate" step:

    grep -qE '^--- (PASS|FAIL|SKIP): ' /tmp/test-handlers.log

with SKIP accepted as proof of execution, over two of the module's 61
packages. `test_all_skip_package_fails` is the direct regression test for the
first hole; the gate's per-package sweep is the second.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[3]
GATE = REPO / ".gitea" / "scripts" / "assert-tests-executed.py"
ALLOWLIST_IN_REPO = REPO / ".gitea" / "tests-may-skip-wholesale.txt"

MOD = "example.com/m/workspace-server"


# ---------------------------------------------------------------------------
# fixture construction: synthesise a `go test -json` stream
# ---------------------------------------------------------------------------

def _ev(**kw) -> str:
    return json.dumps(kw)


def pkg_events(pkg: str, *, passes=0, fails=0, skips=0,
               sub_skips=0, no_test_files=False) -> list[str]:
    """Emit the event shape `go test -json` really produces for one package.

    Shapes verified against go1.26.4 output on this module, including the
    detail that a CACHED package still replays its full per-test event
    stream — so a cache hit does not read as "executed nothing".
    """
    out = [_ev(Action="start", Package=pkg)]
    if no_test_files:
        out.append(_ev(Action="output", Package=pkg, Output="?   \t%s\t[no test files]\n" % pkg))
        out.append(_ev(Action="skip", Package=pkg, Elapsed=0))
        return out
    n = 0
    for action, count in (("pass", passes), ("fail", fails), ("skip", skips)):
        for _ in range(count):
            n += 1
            name = "Test%s%d" % (action.capitalize(), n)
            out.append(_ev(Action="run", Package=pkg, Test=name))
            out.append(_ev(Action=action, Package=pkg, Test=name, Elapsed=0.01))
    for i in range(sub_skips):
        parent = "TestParentWithSkippedSubtests"
        out.append(_ev(Action="run", Package=pkg, Test=parent))
        out.append(_ev(Action="skip", Package=pkg, Test="%s/case%d" % (parent, i), Elapsed=0))
    out.append(_ev(Action="fail" if fails else "pass", Package=pkg, Elapsed=0.1))
    return out


def healthy_stream(n_pkgs: int = 45) -> tuple[list[str], list[str]]:
    """A module-shaped run: n_pkgs packages, all executing real tests."""
    pkgs = ["%s/internal/p%02d" % (MOD, i) for i in range(n_pkgs)]
    lines: list[str] = []
    for p in pkgs:
        lines += pkg_events(p, passes=3)
    return pkgs, lines


def write_case(tmp_path: Path, pkgs: list[str], lines: list[str],
               allowlist: str | None = None) -> list[str]:
    log = tmp_path / "test.json"
    log.write_text("\n".join(lines) + "\n", encoding="utf-8")
    plist = tmp_path / "pkgs.txt"
    plist.write_text("\n".join(pkgs) + "\n", encoding="utf-8")
    argv = ["--json-log", str(log), "--expected-packages", str(plist)]
    if allowlist is not None:
        al = tmp_path / "allow.txt"
        al.write_text(allowlist, encoding="utf-8")
        argv += ["--allowlist", str(al)]
    return argv


def run_gate(argv: list[str]) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(GATE), *argv],
        capture_output=True, text=True, encoding="utf-8", errors="replace",
    )


# ---------------------------------------------------------------------------
# the control: the gate must NOT fire on a healthy run
# ---------------------------------------------------------------------------

def test_healthy_run_passes(tmp_path):
    """A gate that always fails gets disabled within a week."""
    pkgs, lines = healthy_stream()
    res = run_gate(write_case(tmp_path, pkgs, lines))
    assert res.returncode == 0, res.stdout + res.stderr
    assert "inspected 45 package(s) | executed 45" in res.stdout


def test_healthy_run_reports_a_nonzero_inspected_count(tmp_path):
    """Non-vacuity, stated out loud in the log.

    The count is the evidence a reader can check without rerunning anything.
    If it ever reads 0, the summary line itself says so.
    """
    pkgs, lines = healthy_stream()
    res = run_gate(write_case(tmp_path, pkgs, lines))
    assert res.returncode == 0
    assert "inspected 0 package(s)" not in res.stdout
    n = int(res.stdout.split("inspected ")[1].split(" package")[0])
    assert n >= 40


# ---------------------------------------------------------------------------
# hole 1: SKIP is not execution
# ---------------------------------------------------------------------------

def test_all_skip_package_fails(tmp_path):
    """THE regression test. The old grep accepted `--- SKIP:` as execution."""
    pkgs, lines = healthy_stream()
    victim = "%s/internal/allskip" % MOD
    pkgs.append(victim)
    lines += pkg_events(victim, skips=7)
    res = run_gate(write_case(tmp_path, pkgs, lines))
    assert res.returncode == 1, res.stdout
    assert "internal/allskip executed ZERO tests" in res.stdout
    assert "ALL of them skipped" in res.stdout


def test_one_real_test_among_skips_is_enough(tmp_path):
    """The gate asserts execution, not the absence of skips.

    A package may skip 99 of 100 tests; that is a coverage question, not a
    vacuity one. Getting this wrong would make the gate fire constantly on
    healthy packages and guarantee it is switched off.
    """
    pkgs, lines = healthy_stream()
    victim = "%s/internal/mostlyskip" % MOD
    pkgs.append(victim)
    lines += pkg_events(victim, passes=1, skips=40)
    res = run_gate(write_case(tmp_path, pkgs, lines))
    assert res.returncode == 0, res.stdout


def test_a_failing_test_still_counts_as_executed(tmp_path):
    """A real FAIL is proof a test body ran. It reds the build via go's own
    exit code, not via this gate; the gate must not double-report it."""
    pkgs, lines = healthy_stream()
    victim = "%s/internal/reds" % MOD
    pkgs.append(victim)
    lines += pkg_events(victim, fails=2)
    res = run_gate(write_case(tmp_path, pkgs, lines))
    assert res.returncode == 0, res.stdout


# ---------------------------------------------------------------------------
# hole 2: coverage beyond two packages
# ---------------------------------------------------------------------------

def test_package_that_produced_no_events_at_all_fails(tmp_path):
    """`go list` knows the package; the run never touched it.

    This is the original no-tests-executed shape: tests stopped compiling in
    (build tag, rename, moved path) and `go test` exited 0.
    """
    pkgs, lines = healthy_stream()
    pkgs.append("%s/internal/vanished" % MOD)
    res = run_gate(write_case(tmp_path, pkgs, lines))
    assert res.returncode == 1, res.stdout
    assert "produced NO event in the test stream" in res.stdout
    assert "internal/vanished" in res.stdout


def test_tests_expected_but_no_verdict_and_no_no_test_files_marker(tmp_path):
    pkgs, lines = healthy_stream()
    victim = "%s/internal/silent" % MOD
    pkgs.append(victim)
    lines += [_ev(Action="start", Package=victim), _ev(Action="pass", Package=victim, Elapsed=0)]
    res = run_gate(write_case(tmp_path, pkgs, lines))
    assert res.returncode == 1, res.stdout
    assert "produced NO test verdict at all" in res.stdout


def test_no_test_files_package_is_not_a_failure(tmp_path):
    """cmd/ packages with only a main.go are not a defect."""
    pkgs, lines = healthy_stream()
    victim = "%s/cmd/justamain" % MOD
    pkgs.append(victim)
    lines += pkg_events(victim, no_test_files=True)
    res = run_gate(write_case(tmp_path, pkgs, lines))
    assert res.returncode == 0, res.stdout
    assert "no-test-files 1" in res.stdout


# ---------------------------------------------------------------------------
# the gate must not pass vacuously itself
# ---------------------------------------------------------------------------

def test_missing_log_fails(tmp_path):
    """It greps a log that isn't produced -> it must not report success."""
    plist = tmp_path / "pkgs.txt"
    plist.write_text("\n".join("%s/p%d" % (MOD, i) for i in range(45)), encoding="utf-8")
    res = run_gate(["--json-log", str(tmp_path / "nope.json"),
                    "--expected-packages", str(plist)])
    assert res.returncode == 1
    assert "does not exist" in res.stdout
    assert "inspected ZERO packages" in res.stdout


def test_empty_log_fails(tmp_path):
    pkgs, _ = healthy_stream()
    log = tmp_path / "test.json"
    log.write_text("", encoding="utf-8")
    plist = tmp_path / "pkgs.txt"
    plist.write_text("\n".join(pkgs), encoding="utf-8")
    res = run_gate(["--json-log", str(log), "--expected-packages", str(plist)])
    assert res.returncode == 1
    assert "EMPTY" in res.stdout


def test_two_empty_sets_do_not_compare_equal_into_a_pass(tmp_path):
    """The subtlest vacuity: `go list` matched nothing AND the run produced
    nothing, so set(expected) - set(seen) is empty and every "must not" rule
    is satisfied. Only the floor and the must-execute rule catch this."""
    log = tmp_path / "test.json"
    log.write_text(_ev(Action="output", Package="x", Output="hi\n") + "\n", encoding="utf-8")
    plist = tmp_path / "pkgs.txt"
    plist.write_text("", encoding="utf-8")
    res = run_gate(["--json-log", str(log), "--expected-packages", str(plist)])
    assert res.returncode == 1, res.stdout
    assert "below the floor" in res.stdout
    assert "ZERO packages executed a single test" in res.stdout


def test_a_handful_of_packages_trips_the_floor(tmp_path):
    """A run that covered 3 real packages is a wrong -run/path, not a module."""
    pkgs, lines = healthy_stream(n_pkgs=3)
    res = run_gate(write_case(tmp_path, pkgs, lines))
    assert res.returncode == 1, res.stdout
    assert "below the floor of 40" in res.stdout


def test_floor_is_configurable_but_still_enforced(tmp_path):
    pkgs, lines = healthy_stream(n_pkgs=3)
    argv = write_case(tmp_path, pkgs, lines) + ["--min-packages", "3"]
    assert run_gate(argv).returncode == 0
    argv = write_case(tmp_path, pkgs, lines) + ["--min-packages", "4"]
    assert run_gate(argv).returncode == 1


def test_every_package_empty_of_tests_fails(tmp_path):
    """45 packages, all `[no test files]`. Nothing ran. Nothing is asserted by
    any "must not" rule -- the must-execute rule is the only thing standing."""
    pkgs = ["%s/cmd/c%02d" % (MOD, i) for i in range(45)]
    lines: list[str] = []
    for p in pkgs:
        lines += pkg_events(p, no_test_files=True)
    res = run_gate(write_case(tmp_path, pkgs, lines))
    assert res.returncode == 1, res.stdout
    assert "ZERO packages executed a single test" in res.stdout


# ---------------------------------------------------------------------------
# the allowlist is explicit, justified, and cannot rot
# ---------------------------------------------------------------------------

def test_allowlisted_all_skip_passes_and_is_named_in_the_log(tmp_path):
    pkgs, lines = healthy_stream()
    victim = "%s/internal/allskip" % MOD
    pkgs.append(victim)
    lines += pkg_events(victim, skips=2)
    res = run_gate(write_case(
        tmp_path, pkgs, lines,
        allowlist="%s  # runs in other-lane.yml which sets THE_ENV\n" % victim))
    assert res.returncode == 0, res.stdout
    assert "allowlisted" in res.stdout
    assert "other-lane.yml" in res.stdout, "the reason must reach the log, not just the file"


def test_allowlist_entry_without_a_reason_is_rejected(tmp_path):
    pkgs, lines = healthy_stream()
    victim = "%s/internal/allskip" % MOD
    pkgs.append(victim)
    lines += pkg_events(victim, skips=2)
    res = run_gate(write_case(tmp_path, pkgs, lines, allowlist="%s\n" % victim))
    assert res.returncode == 1, res.stdout
    assert "has no reason" in res.stdout


def test_stale_allowlist_entry_fails(tmp_path):
    """The package started executing again. The exemption is now a lie."""
    pkgs, lines = healthy_stream()
    healthy = pkgs[0]
    res = run_gate(write_case(
        tmp_path, pkgs, lines,
        allowlist="%s  # used to skip\n" % healthy))
    assert res.returncode == 1, res.stdout
    assert "is STALE" in res.stdout
    assert "delete the line" in res.stdout


def test_allowlist_entry_for_a_deleted_package_fails(tmp_path):
    pkgs, lines = healthy_stream()
    res = run_gate(write_case(
        tmp_path, pkgs, lines,
        allowlist="%s/internal/gone  # it was renamed and nobody told the list\n" % MOD))
    assert res.returncode == 1, res.stdout
    assert "Remove the line" in res.stdout


def test_all_skip_without_an_allowlist_configured_still_fails(tmp_path):
    """No --allowlist must mean "nothing is exempt", never "everything is"."""
    pkgs, lines = healthy_stream()
    victim = "%s/internal/allskip" % MOD
    pkgs.append(victim)
    lines += pkg_events(victim, skips=3)
    res = run_gate(write_case(tmp_path, pkgs, lines))
    assert res.returncode == 1, res.stdout


def test_configured_allowlist_file_missing_fails(tmp_path):
    pkgs, lines = healthy_stream()
    argv = write_case(tmp_path, pkgs, lines) + ["--allowlist", str(tmp_path / "nope.txt")]
    res = run_gate(argv)
    assert res.returncode == 1
    assert "allowlist file not found" in res.stdout


# ---------------------------------------------------------------------------
# the allowlist checked into this repo
# ---------------------------------------------------------------------------

def test_repo_allowlist_is_parseable_and_every_entry_is_justified():
    """Whatever is on the real list must state where the package DOES run."""
    text = ALLOWLIST_IN_REPO.read_text(encoding="utf-8")
    entries = [ln for ln in text.splitlines() if ln.strip() and not ln.strip().startswith("#")]
    assert entries, "the allowlist is empty; if that is now true, delete the file and the flag"
    for ln in entries:
        assert "#" in ln, "no reason on: %s" % ln
        pkg, reason = ln.split("#", 1)
        assert pkg.strip().startswith("git.moleculesai.app/"), ln
        assert len(reason.strip()) > 40, "reason too thin to follow: %s" % ln
        assert ".yml" in reason or "workflow" in reason.lower(), (
            "the reason must name the lane where the package actually executes: %s" % ln)


# ---------------------------------------------------------------------------
# ci.yml wiring: the gate has to be CALLED, with its inputs
# ---------------------------------------------------------------------------

CI = REPO / ".gitea" / "workflows" / "ci.yml"


def test_ci_invokes_the_gate_over_the_whole_module():
    body = CI.read_text(encoding="utf-8")
    assert "assert-tests-executed.py" in body, "the gate is not wired into ci.yml at all"
    assert "go test -json" in body, "the gate needs a -json stream to parse"
    assert "--expected-packages" in body, (
        "without `go list ./...` fed in, a package that vanishes from the run is invisible")
    assert "--allowlist" in body


def _executable_lines(text: str) -> list[str]:
    """Lines that the shell actually runs.

    Comment lines are excluded deliberately: ci.yml documents the removed
    grep in prose right next to its replacement, and a lint that cannot tell
    a description of a defect from the defect is a lint nobody can write
    comments around.
    """
    out = []
    for ln in text.splitlines():
        stripped = ln.strip()
        if not stripped or stripped.startswith("#"):
            continue
        out.append(ln)
    return out


@pytest.mark.parametrize("hole", ["(PASS|FAIL|SKIP)", "(PASS|SKIP|FAIL)", "(SKIP|PASS|FAIL)"])
def test_ci_no_longer_accepts_skip_as_proof_of_execution(hole):
    """The exact grep this work removed must not come back.

    `grep -qE '^--- (PASS|FAIL|SKIP): '` counted a skipped test as proof the
    package executed something. Any live line that puts SKIP in the same
    alternation as PASS is that hole reopening.
    """
    offenders = [ln for ln in _executable_lines(CI.read_text(encoding="utf-8")) if hole in ln]
    assert not offenders, (
        "ci.yml again treats `--- SKIP:` as proof that tests executed -- that is the "
        "vacuous pass the no-tests-executed gate exists to detect:\n  " + "\n  ".join(offenders))


def test_the_regression_lint_can_actually_see_the_defect():
    """A lint that skips comments could just as easily skip everything.

    Feed it the removed grep as a LIVE line and confirm it fires; otherwise
    the clean result above proves only that _executable_lines returned [].
    """
    live = "          if ! grep -qE '^--- (PASS|FAIL|SKIP): ' /tmp/test-handlers.log; then"
    assert [ln for ln in _executable_lines(live) if "(PASS|FAIL|SKIP)" in ln]
    commented = "          # if ! grep -qE '^--- (PASS|FAIL|SKIP): ' /tmp/x.log; then"
    assert not [ln for ln in _executable_lines(commented) if "(PASS|FAIL|SKIP)" in ln]
    assert len(_executable_lines(CI.read_text(encoding="utf-8"))) > 500, (
        "the CI body parsed down to almost nothing -- the lint above passed vacuously")
