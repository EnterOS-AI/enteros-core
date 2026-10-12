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
        # UNDER -coverprofile go emits no "[no test files]" marker at all --
        # just a coverage line and a package-level `pass`. The gate therefore
        # must not depend on the marker, and these fixtures must not hand it
        # one. This exact shape is what reddened run 632196 / job 932832.
        out.append(_ev(Action="output", Package=pkg,
                       Output="\t%s\t\tcoverage: 0.0%% of statements\n" % pkg))
        out.append(_ev(Action="pass", Package=pkg, Elapsed=0.4))
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


def manifest_line(pkg: str, ntest: int = 3, ignored: list[str] | None = None) -> str:
    r"""One row of the `go list -f '{{.ImportPath}} {{len .TestGoFiles}}...'` manifest.

    ntest is what the gate uses to tell "this dir owns no test files" from
    "this dir's tests stopped compiling in" -- a distinction go's console
    output does not reliably carry.
    """
    return "%s %d 0 %s" % (pkg, ntest, ",".join(ignored or []))


def write_case(tmp_path: Path, pkgs: list[str], lines: list[str],
               allowlist: str | None = None,
               manifest: list[str] | None = None) -> list[str]:
    log = tmp_path / "test.json"
    log.write_text("\n".join(lines) + "\n", encoding="utf-8")
    plist = tmp_path / "pkgs.txt"
    if manifest is None:
        manifest = [manifest_line(p) for p in pkgs]
    plist.write_text("\n".join(manifest) + "\n", encoding="utf-8")
    argv = ["--json-log", str(log), "--package-manifest", str(plist)]
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
    """cmd/ packages with only a main.go are not a defect.

    THE REGRESSION TEST for run 632196 / job 932832. The first cut of this
    gate decided "has no test files" by looking for go's "[no test files]"
    marker in the output stream. Under `-coverprofile` go does not print it
    -- it prints a coverage line and a package-level `pass` -- so all three
    test-free cmd/ packages were classified as "tests did not compile in"
    and the gate reddened a healthy run. `go list` metadata replaced the
    marker; pkg_events(no_test_files=True) now emits the coverage shape, so
    this test fails again if anything goes back to reading the prose.
    """
    pkgs, lines = healthy_stream()
    victim = "%s/cmd/justamain" % MOD
    pkgs.append(victim)
    lines += pkg_events(victim, no_test_files=True)
    manifest = [manifest_line(p) for p in pkgs[:-1]] + [manifest_line(victim, ntest=0)]
    res = run_gate(write_case(tmp_path, pkgs, lines, manifest=manifest))
    assert res.returncode == 0, res.stdout
    assert "no-test-files 1" in res.stdout


def test_package_whose_test_files_are_all_tagged_out_fails(tmp_path):
    """Owns _test.go files; a build constraint excluded every one.

    Nothing ran, `go test` exits 0, and the package reads as covered. This
    is the shape the original inline check was written for, and it is
    invisible to any count of what DID run.
    """
    pkgs, lines = healthy_stream()
    victim = "%s/internal/taggedout" % MOD
    pkgs.append(victim)
    lines += pkg_events(victim, no_test_files=True)
    manifest = [manifest_line(p) for p in pkgs[:-1]] + [
        manifest_line(victim, ntest=0, ignored=["only_integration_test.go", "helpers_test.go"])
    ]
    res = run_gate(write_case(tmp_path, pkgs, lines, manifest=manifest))
    assert res.returncode == 1, res.stdout
    assert "build constraint excluded EVERY one of them" in res.stdout


def test_package_in_the_stream_but_not_the_manifest_fails(tmp_path):
    """The two inputs disagree about what the module contains."""
    pkgs, lines = healthy_stream()
    ghost = "%s/internal/ghost" % MOD
    lines += pkg_events(ghost, no_test_files=True)
    res = run_gate(write_case(tmp_path, pkgs, lines))
    assert res.returncode == 1, res.stdout
    assert "NOT in the `go list` manifest" in res.stdout


# ---------------------------------------------------------------------------
# the gate must not pass vacuously itself
# ---------------------------------------------------------------------------

def test_missing_log_fails(tmp_path):
    """It greps a log that isn't produced -> it must not report success."""
    plist = tmp_path / "pkgs.txt"
    plist.write_text("\n".join(manifest_line("%s/p%d" % (MOD, i)) for i in range(45)),
                     encoding="utf-8")
    res = run_gate(["--json-log", str(tmp_path / "nope.json"),
                    "--package-manifest", str(plist)])
    assert res.returncode == 1
    assert "does not exist" in res.stdout
    assert "inspected ZERO packages" in res.stdout


def test_empty_log_fails(tmp_path):
    pkgs, _ = healthy_stream()
    log = tmp_path / "test.json"
    log.write_text("", encoding="utf-8")
    plist = tmp_path / "pkgs.txt"
    plist.write_text("\n".join(manifest_line(p) for p in pkgs), encoding="utf-8")
    res = run_gate(["--json-log", str(log), "--package-manifest", str(plist)])
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
    res = run_gate(["--json-log", str(log), "--package-manifest", str(plist)])
    assert res.returncode == 1, res.stdout
    assert "below the floor" in res.stdout
    assert "ZERO packages executed a single test" in res.stdout


def test_manifest_where_no_package_owns_a_test_file_fails(tmp_path):
    """A wrong -f template yields 61 rows of zeroes.

    Every package then classifies as NO_TEST_FILES, every "must not" rule is
    satisfied, and the gate would wave through a run that asserted nothing.
    """
    pkgs, lines = healthy_stream()
    manifest = [manifest_line(p, ntest=0) for p in pkgs]
    res = run_gate(write_case(tmp_path, pkgs, lines, manifest=manifest))
    assert res.returncode == 1, res.stdout
    assert "NO package in the module owns a compiled-in test file" in res.stdout


def test_manifest_with_too_few_fields_fails(tmp_path):
    """A wrong -f template that emits only the import path.

    Every package would then have no counts at all. Defaulting a missing
    count to zero would silently reclassify the whole module as "owns no
    tests" -- which is the vacuous pass, arrived at through a typo.
    """
    pkgs, lines = healthy_stream()
    res = run_gate(write_case(tmp_path, pkgs, lines, manifest=list(pkgs)))
    assert res.returncode == 1, res.stdout
    assert "unparseable manifest line" in res.stdout


def test_manifest_with_non_numeric_counts_fails(tmp_path):
    pkgs, lines = healthy_stream()
    manifest = ["%s this is not a count" % p for p in pkgs]
    res = run_gate(write_case(tmp_path, pkgs, lines, manifest=manifest))
    assert res.returncode == 1, res.stdout
    assert "non-numeric test-file count" in res.stdout


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
    assert "--package-manifest" in body, (
        "without the `go list` manifest fed in, a package that vanishes from the run is "
        "invisible, and a test-free cmd/ dir cannot be told from one whose tests stopped "
        "compiling in")
    assert "go list -f" in body, "ci.yml must actually PRODUCE the manifest it passes"
    assert ".IgnoredGoFiles" in body, (
        "without IgnoredGoFiles the manifest cannot see a package whose every _test.go is "
        "excluded by a build constraint")
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


# ---------------------------------------------------------------------------
# the race+verbose shell guard, EXECUTED
# ---------------------------------------------------------------------------
#
# Everything above tests the Python gate. The other half of the fix is four
# lines of bash in ci.yml, and those were originally verified with a SINGLE
# negative control: feed it an all-SKIP log, watch it go red. That control
# proves the mechanism FIRES. It cannot find the input on which the mechanism
# is never REACHED -- and there was one:
#
#     h_ran=$(grep -cE '...' /tmp/test-handlers.log || true)   # log missing -> ""
#     if [ "$h_ran" -eq 0 ]; then                              # [: : integer expected -> rc 2
#
# `set -e` is EXEMPT inside an `if` condition, so rc 2 quietly takes the else
# branch and the guard is skipped entirely: exit 0, in the step whose whole
# job is failing closed. The previous `grep -q` form failed closed on a
# missing file BY ACCIDENT (grep's own non-zero exit), so the rewrite
# regressed it. `${h_ran:-0}` restores it on purpose.
#
# These run the REAL step body out of ci.yml, so the falsification set now
# includes the not-reached path and lives in CI instead of a transcript.

import os
import shutil
import subprocess as _sp

import yaml


def _race_step_body() -> str:
    doc = yaml.safe_load(CI.read_text(encoding="utf-8"))
    steps = doc["jobs"]["platform-build"]["steps"]
    bodies = [s["run"] for s in steps if "Race + verbose" in (s.get("name") or "")]
    assert len(bodies) == 1, (
        "expected exactly one 'Race + verbose' step in ci.yml, found %d -- the "
        "extraction below would otherwise test nothing" % len(bodies))
    return bodies[0]


_BASH_CACHE: list = []


def _bash() -> str:
    """A bash that is PROVEN to run, not merely present on PATH.

    `shutil.which("bash")` on Windows finds WSL's stub, which answers the
    which() question and then fails to exec. Every test below asserts a
    NON-ZERO exit; a bash that cannot start returns non-zero too, so an
    unverified interpreter turns those into false passes -- the precise shape
    this PR exists to refuse, reproduced inside its own test suite. Probe it
    instead of trusting it, and fail hard if none works.
    """
    if _BASH_CACHE:
        return _BASH_CACHE[0]
    candidates = [
        shutil.which("bash"),
        os.path.join("C:" + os.sep, "Program Files", "Git", "bin", "bash.exe"),
        os.path.join("C:" + os.sep, "Program Files", "Git", "usr", "bin", "bash.exe"),
        "/bin/bash",
        "/usr/bin/bash",
    ]
    for cand in candidates:
        if not cand:
            continue
        try:
            probe = _sp.run([cand, "-c", "echo __bash_ok__"], capture_output=True,
                            text=True, encoding="utf-8", errors="replace", timeout=30)
        except (OSError, _sp.SubprocessError):
            # OSError alone is not enough. A cold WSL start makes the same
            # stub hang rather than fail fast, so the probe raises
            # TimeoutExpired -- a SubprocessError, NOT an OSError -- which
            # propagated out and aborted the whole group instead of falling
            # through to the next candidate. Loud rather than silent, so it
            # could not resurrect the false pass, but an intermittent abort
            # is an unfixed bug by this repo's rule, not a flake.
            continue
        if probe.returncode == 0 and "__bash_ok__" in probe.stdout:
            _BASH_CACHE.append(cand)
            return cand
    raise AssertionError(
        "no working bash found, so the shell guard below cannot be exercised. This "
        "is a hard failure rather than a skip on purpose: a silently skipped test is "
        "exactly what this PR exists to refuse.")


def _run_race_guard(tmp_path, handlers: str | None, pu: str | None,
                    h_exit: int = 0, pu_exit: int = 0):
    """Execute the real guard with go test stubbed and the logs substituted."""
    bash = _bash()
    body = _race_step_body()
    hlog, pulog = tmp_path / "h.log", tmp_path / "pu.log"
    if handlers is not None:
        hlog.write_text(handlers, encoding="utf-8")
    if pu is not None:
        pulog.write_text(pu, encoding="utf-8")

    lines = [ln for ln in body.splitlines() if not ln.strip().startswith("go test ")]
    script = "\n".join(lines)
    assert "grep -cE" in script, "the guard vanished from the extracted body"
    script = (script
              .replace("handlers_exit=${PIPESTATUS[0]}", "handlers_exit=%d" % h_exit)
              .replace("pu_exit=${PIPESTATUS[0]}", "pu_exit=%d" % pu_exit)
              .replace("/tmp/test-handlers.log", str(hlog).replace("\\", "/"))
              .replace("/tmp/test-pu.log", str(pulog).replace("\\", "/")))
    sh = tmp_path / "guard.sh"
    sh.write_text(script, encoding="utf-8")
    return _sp.run([bash, str(sh)], capture_output=True, text=True,
                   encoding="utf-8", errors="replace")


PASSLOG = "--- PASS: TestA (0.01s)\n--- PASS: TestB (0.01s)\nok\n"
SKIPLOG = "--- SKIP: TestA (0.00s)\n--- SKIP: TestB (0.00s)\nok\n"


def test_race_guard_stays_green_on_a_healthy_log(tmp_path):
    r = _run_race_guard(tmp_path, PASSLOG, PASSLOG)
    assert r.returncode == 0, r.stdout + r.stderr
    assert "no-tests-executed" not in r.stdout


def test_race_guard_reds_when_every_handlers_test_skipped(tmp_path):
    """Hole 1. The removed grep accepted this log as proof of execution."""
    r = _run_race_guard(tmp_path, SKIPLOG, PASSLOG)
    assert r.returncode == 1, r.stdout + r.stderr
    assert "internal/handlers(no-tests-executed)" in r.stdout
    assert "A skip is not an execution" in r.stdout


def test_race_guard_reds_when_every_pendinguploads_test_skipped(tmp_path):
    r = _run_race_guard(tmp_path, PASSLOG, SKIPLOG)
    assert r.returncode == 1, r.stdout + r.stderr
    assert "internal/pendinguploads(no-tests-executed)" in r.stdout


def test_race_guard_fails_closed_when_the_log_does_not_EXIST(tmp_path):
    """Finding B: the not-reached path, not the not-fired path.

    A guard that greps a log nothing produced must go RED. With a bare
    `[ "$h_ran" -eq 0 ]` this exits 0 -- the test comparison errors out and
    `set -e` does not apply inside an `if` condition, so the check is skipped
    rather than failed.
    """
    r = _run_race_guard(tmp_path, None, PASSLOG)
    assert r.returncode != 0, (
        "the race guard PASSED with no handlers log at all:\n" + r.stdout + r.stderr)


def test_race_guard_fails_closed_when_both_logs_are_missing(tmp_path):
    r = _run_race_guard(tmp_path, None, None)
    assert r.returncode != 0, r.stdout + r.stderr


def test_race_guard_uses_the_defaulted_comparison(tmp_path):
    """Pin the fix itself, since the behaviour above can be masked.

    An unrelated `tail` under `set -e` currently exits the step before the
    comparison is reached on a missing log, which is what let the fail-open
    ship in the first place: the assembled step looked fine. Reorder or drop
    that tail and the behavioural test above stops discriminating, so the
    form is pinned directly too.
    """
    body = _race_step_body()
    live = [ln for ln in body.splitlines()
            if ln.strip().startswith("if [") and "_ran" in ln]
    assert live, "the two _ran comparisons vanished from the race step"
    for ln in live:
        assert ":-0}" in ln, (
            "race guard compares an unguarded variable, so an empty value makes the "
            "test error out (rc 2) and `set -e` skips the check inside the `if` "
            "condition -- fail-OPEN in the step that exists to fail closed: " + ln.strip())


def test_race_guard_bare_comparison_really_does_fail_open(tmp_path):
    """The positive control for the test above.

    Without this, `test_race_guard_fails_closed_when_the_log_does_not_EXIST`
    might be passing for some unrelated reason and would keep passing if the
    fix were reverted. Rebuild the OLD form and confirm it exits 0.
    """
    bash = _bash()
    sh = tmp_path / "bare.sh"
    sh.write_text(
        'set -e\n'
        'n=$(grep -cE "^--- (PASS|FAIL): " %s || true)\n'
        'if [ "$n" -eq 0 ]; then echo FIRED; exit 1; fi\n'
        'echo "NOT FIRED"; exit 0\n' % str(tmp_path / "absent.log").replace("\\", "/"),
        encoding="utf-8")
    r = _sp.run([bash, str(sh)], capture_output=True, text=True,
                encoding="utf-8", errors="replace")
    assert r.returncode == 0 and "NOT FIRED" in r.stdout, (
        "the bare form no longer fails open, so the guarded form is not "
        "demonstrably fixing anything: " + r.stdout + r.stderr)


# ---------------------------------------------------------------------------
# an exemption is only as good as the lane it points at
# ---------------------------------------------------------------------------

DRIFT_LANE = REPO / ".gitea" / "workflows" / "sdk-route-milestone-contract-drift.yml"


def test_the_lane_that_justifies_the_allowlist_asserts_execution():
    """The allowlist's escape hatch must not re-create the hole.

    `internal/e2emilestones` is exempt from the module-wide gate ONLY because
    sdk-route-milestone-contract-drift.yml runs it for real. That lane was a
    bare `go test ... -run ... -v` whose only verdict was go's exit code, and
    every way the test can stop running exits 0 (env var dropped -> t.Skip;
    -run regex stale -> "no tests to run"). Dropping the env var would then
    have left the package executing NOWHERE with both gates green.

    So the exemption is coupled to the assertion, here, mechanically: if the
    lane loses its execution check, this fails and the exemption has to go
    with it.

    KEYED ON THE PACKAGE PATH, NOT THE LANE NAME. This was keyed on the
    string "sdk-route-milestone-contract-drift" appearing in the allowlist
    reason, and bailed when it did not. The reason is free-form prose, so
    rewording it to name a different lane -- while keeping the exemption
    exactly as permissive -- silently switched this whole check off. A guard
    that stops applying when someone edits a comment is the same shape as
    everything else this PR closes. The exemption is a claim about
    internal/e2emilestones, so the package path is what selects the check.
    """
    allow = ALLOWLIST_IN_REPO.read_text(encoding="utf-8")
    entries = [ln for ln in allow.splitlines()
               if ln.strip() and not ln.strip().startswith("#")]
    assert entries, (
        "the allowlist parsed to zero entries -- if that file was renamed or its "
        "format changed, this check would return without asserting anything rather "
        "than telling you it had stopped working")
    exempted = [ln.split("#", 1)[0].strip() for ln in entries]
    if not any(p.endswith("/internal/e2emilestones") for p in exempted):
        # The package is no longer exempt at all, so there is nothing for the
        # drift lane to be standing in for. This is the one honest early
        # return: it is keyed on the exemption itself, not on prose about it.
        return

    lane = DRIFT_LANE.read_text(encoding="utf-8")
    live = _executable_lines(lane)
    body = "\n".join(live)

    assert "--- PASS:" in body, (
        "the drift lane no longer counts '--- PASS:' lines, so it cannot tell a run "
        "from a skip -- and the allowlist entry for internal/e2emilestones is "
        "justified BY that lane. Either restore the assertion or drop the exemption.")
    assert "--- SKIP:" in body, (
        "the drift lane no longer fails on a skip. A skip there is the exact event "
        "the allowlist entry assumes cannot happen silently.")
    assert body.count("MOLECULE_RUN_CONTRACT_DRIFT_GATES") >= 2, (
        "a derive-gate step lost its env var; the tests it names would skip.")
    for needle in ("./internal/e2emilestones/", "./internal/router/"):
        assert needle in body, "the drift lane stopped running %s" % needle


def test_drift_lane_execution_checks_are_not_merely_mentioned_in_prose():
    """`_executable_lines` strips comments, so prove it kept real code.

    Without this, the assertions above would pass just as happily on a lane
    that had been reduced to a comment block.
    """
    live = _executable_lines(DRIFT_LANE.read_text(encoding="utf-8"))
    body = "\n".join(live)
    assert body.count("npass=$(grep -c") == 2, (
        "expected both derive-gate steps to compute a PASS count as LIVE shell, "
        "found %d" % body.count("npass=$(grep -c"))
    assert body.count("exit 1") >= 4, (
        "the derive-gate steps stopped having failure arms")
    assert ":-0}" in body, (
        "the drift lane compares an unguarded count -- an empty value makes `[` "
        "error out (rc 2), and `set -e` does not apply inside an `if` condition, "
        "so the check would be silently skipped. Same fail-open as the race guard.")
