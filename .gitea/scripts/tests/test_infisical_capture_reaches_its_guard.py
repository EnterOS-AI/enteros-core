#!/usr/bin/env python3
"""A secret-fetch step must survive its own transport failure long enough to SAY so.

WHY THIS FILE EXISTS
--------------------
Gitea/act_runner invokes a `run:` block as

    bash --noprofile --norc -e -o pipefail {0}

so `-e` is on at line 1 and a step cannot infer its flags from its own `set`
line — it can only ADD to them.  Every Infisical fetch in this tree has the
shape

    TOK=$(curl -fsS ... | python3 -c '...json...')
    if [ -z "$TOK" ]; then echo "::error::..."; exit 1; fi

When the transfer dies at DNS/connect/timeout, curl exits non-zero and writes
nothing; the `python3` it feeds then raises `JSONDecodeError` and exits 1 too.
A bare `VAR=$(pipeline)` inherits that status, `-e` kills the step **one line
above the guard**, and the operator gets a Python traceback instead of the
diagnosis the guard exists to print.  On 2026-08-11 the same class of capture —
a step documented "Best-effort. Failure here doesn't fail the workflow" — held
main red for 70 minutes and froze the merge queue (core#5166).

WHAT IS ASSERTED, AND WHY IT IS NOT A REGEX LINT
------------------------------------------------
Every case below EXECUTES the real `run:` body lifted out of the workflow YAML
under the real runner invocation, against a fake `curl` on PATH.  A grep for
`|| echo ""` would pass on a fallback attached to the wrong subshell, and would
pass on a guard that can never fire.  Running the body cannot.

Four directions per step, and the last two are the negative control:

  fixed  + reachable curl  -> exit 0
  fixed  + dead curl       -> non-zero AND the step's own `::error::` printed
  mutated (fallback stripped) + dead curl -> the `::error::` is GONE
  mutated + reachable curl -> stdout byte-identical to `fixed` (sha256)

The last one is the point of the whole exercise: the fallback may only ever
change the FAILURE path.  If it moves a single byte of the happy path, this
test goes red.

A zero-length `::add-mask::` is asserted absent on the failure path too.  With
the guards now reachable, an `add-mask` that used to run only on the success
path can run with an empty value, and a zero-length mask pattern is at best
ignored and at worst applied to every log line.
"""

from __future__ import annotations

import hashlib
import os
import re
import shutil
import subprocess
import textwrap
from pathlib import Path

import pytest
import yaml

# On the CI runner this is simply /usr/bin/bash. The override exists for Windows
# developer boxes, where a bare `bash` resolves to the WSL launcher stub in
# System32 and fails with 0x8007274c instead of running the script.
BASH = os.environ.get("PYTEST_BASH") or shutil.which("bash") or "/bin/bash"

REPO = Path(__file__).resolve().parents[3]
WORKFLOWS = REPO / ".gitea" / "workflows"

LOGIN_MARKER = "auth/universal-auth/login"

# The fallback this suite exists to defend. Stripping it is the mutation.
FALLBACK = ' || echo ""'

# Every environment variable the harness provides, named explicitly. An
# ALLOWLIST and never a blocklist: these steps handle Infisical credentials, and
# a pattern-based "hide anything that looks secret" would silently pass the real
# ones through the moment a name stopped matching the pattern. Nothing from the
# ambient environment reaches the step.
HARNESS_ENV_ALLOWLIST = (
    "INFISICAL_CI_CLIENT_ID",
    "INFISICAL_CI_CLIENT_SECRET",
    "INFISICAL_PROJECT_ID",
    "INFISICAL_CI_PROJECT_ID",
    "INFISICAL_CLIENT_ID",
    "INFISICAL_CLIENT_SECRET",
    "INFISICAL_BASE",
    "CP_BASE_URL",
    "TRANSIENT_LEDGER",
)

# Platform plumbing the interpreter and shell need in order to start at all.
# Also an allowlist, and disjoint from the credential names above: nothing here
# can carry an Infisical token, so passing these through cannot leak one.
SYSTEM_ENV_PASSTHROUGH = (
    "SYSTEMROOT", "SystemRoot", "WINDIR", "COMSPEC", "PATHEXT",
    "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "HOME",
    "APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "TEMP", "TMP", "TMPDIR",
    "LANG", "LC_ALL", "MSYSTEM",
)

FAKE_VALUES = {
    "INFISICAL_CI_CLIENT_ID": "fake-client-id",
    "INFISICAL_CI_CLIENT_SECRET": "fake-client-secret",
    "INFISICAL_PROJECT_ID": "fake-project",
    "INFISICAL_CI_PROJECT_ID": "fake-project",
    "INFISICAL_CLIENT_ID": "fake-client-id",
    "INFISICAL_CLIENT_SECRET": "fake-client-secret",
    "INFISICAL_BASE": "https://key.invalid",
    "CP_BASE_URL": "https://cp.invalid",
    "TRANSIENT_LEDGER": "transient.txt",
}

# A fake `curl`. A failing transfer reproduces the exact shape that took main
# down: the transfer dies before the response exists, so `-o` never opens its
# file and `-w` never prints.
#
# CCS_FAIL_AT is the whole point of the second revision of this file. Failing
# EVERY call only ever exercises the FIRST capture in a step: the first guard
# `exit 1`s and nothing below it runs. A mutant that stripped the fallback from
# the SECOND capture in zone-count-drift.yml survived the entire suite and the
# lint, and `sweep-cf-tunnels.yml:150` — an unguarded capture in the same step
# whose sibling was fixed — sat in a tree this file called green. So the shim
# counts its own invocations in a file and fails from index CCS_FAIL_AT onward;
# sweeping that index walks the failure down the step, capture by capture.
CURL_SHIM = r'''#!/usr/bin/env python3
import os, sys
args = sys.argv[1:]
out = None
wfmt = None
url = None
i = 0
while i < len(args):
    a = args[i]
    if a in ("-o", "--output"):
        out = args[i + 1]; i += 2; continue
    if a in ("-w", "--write-out"):
        wfmt = args[i + 1]; i += 2; continue
    if a.startswith("http"):
        url = a
    i += 1

# Count this invocation. Ordinary file I/O is enough: the step bodies are
# sequential shell, so there is no concurrent writer.
counter = os.environ["CCS_COUNTER"]
try:
    with open(counter) as fh:
        n = int(fh.read().strip() or "0")
except (OSError, ValueError):
    n = 0
with open(counter, "w") as fh:
    fh.write(str(n + 1))

fail_at = os.environ.get("CCS_FAIL_AT", "")
if fail_at != "" and n >= int(fail_at):
    sys.stderr.write("curl: (7) Failed to connect to host: Could not connect to server\n")
    sys.exit(7)

if url and "/auth/universal-auth/login" in url:
    body = '{"accessToken": "fake-access-token"}'
elif url and "/api/v3/secrets/raw/" in url:
    name = url.split("/api/v3/secrets/raw/", 1)[1].split("?", 1)[0]
    body = '{"secret": {"secretValue": "fake-value-for-%s"}}' % name
else:
    body = "{}"

if out:
    with open(out, "w") as fh:
        fh.write(body)
else:
    sys.stdout.write(body)
if wfmt:
    sys.stdout.write(wfmt.replace("%{http_code}", "200"))
sys.exit(0)
'''


# --------------------------------------------------------------------------
# YAML extraction — the real step bodies, never a hand-transcribed copy
# --------------------------------------------------------------------------


def _interp_free(run: str) -> str:
    """Replace `${{ ... }}` with a literal so the body is runnable bash.

    Substitution, not deletion: an empty string could change a `[ -z ]` verdict
    and make the harness prove something the runner never sees.
    """
    return re.sub(r"\$\{\{[^}]*\}\}", "harness-interpolated", run)


def _steps() -> list[tuple[str, str, str]]:
    """(workflow, step name, run body) for every Infisical-login step."""
    found = []
    for path in sorted(WORKFLOWS.glob("*.yml")):
        doc = yaml.safe_load(path.read_text(encoding="utf-8"))
        if not isinstance(doc, dict):
            continue
        for job_name, job in (doc.get("jobs") or {}).items():
            for step in (job.get("steps") or []):
                run = step.get("run") or ""
                if LOGIN_MARKER not in run:
                    continue
                found.append(
                    (path.name, f"{job_name}::{step.get('name', '?')}",
                     _interp_free(run))
                )
    return found


STEPS = _steps()
STEP_IDS = [f"{wf}::{name}" for wf, name, _ in STEPS]


def test_the_suite_covers_something() -> None:
    """Anti-vacuity. A suite that discovered no steps passes every assertion.

    `pass:0` is this platform's signature failure mode; it must be an error,
    never a green.
    """
    assert STEPS, "discovered ZERO Infisical-login steps - the extractor matched nothing"
    # Floor, not an equality: new workflows must not have to edit this number,
    # but a collapse of the extractor must not read as success either.
    assert len(STEPS) >= 25, f"only {len(STEPS)} steps discovered; extractor likely broke"


# --------------------------------------------------------------------------
# Execution harness
# --------------------------------------------------------------------------


def _run(
    tmp_path: Path, body: str, mode: str, fail_at: int | None = None
) -> subprocess.CompletedProcess:
    """Execute a step body. `fail_at` is the 0-based curl invocation from which
    the transfer dies; None means every transfer succeeds."""
    workdir = tmp_path / f"{mode}-{'x' if fail_at is None else fail_at}"
    workdir.mkdir(parents=True, exist_ok=True)
    bindir = workdir / "bin"
    bindir.mkdir(exist_ok=True)
    shim = bindir / "curl"
    shim.write_text(CURL_SHIM, encoding="utf-8")
    shim.chmod(0o755)
    # Windows developer boxes resolve `curl` by extension, not by +x.
    (bindir / "curl.py").write_text(CURL_SHIM, encoding="utf-8")
    (bindir / "curl.bat").write_text(
        "@python \"%~dp0curl.py\" %*\r\n", encoding="utf-8")

    script = workdir / "step.sh"
    script.write_text(body, encoding="utf-8", newline="\n")
    github_env = workdir / "github_env"
    github_env.write_text("", encoding="utf-8")

    counter = workdir / "curl_calls"
    counter.write_text("0", encoding="utf-8")

    env = {
        "PATH": str(bindir) + os.pathsep + os.environ.get("PATH", ""),
        "CCS_COUNTER": str(counter),
        "CCS_FAIL_AT": "" if fail_at is None else str(fail_at),
        "GITHUB_ENV": str(github_env),
        "GITHUB_OUTPUT": str(workdir / "github_output"),
        "RUNNER_TEMP": str(workdir),
        "PYTHONIOENCODING": "utf-8",
    }
    for key in SYSTEM_ENV_PASSTHROUGH:
        if key in os.environ:
            env[key] = os.environ[key]
    for key in HARNESS_ENV_ALLOWLIST:
        env[key] = FAKE_VALUES[key]

    return subprocess.run(
        [BASH, "--noprofile", "--norc", "-e", "-o", "pipefail", str(script)],
        cwd=str(workdir),
        env=env,
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=120,
    )


def _mutate(body: str) -> str:
    """Remove every fallback this suite defends — the negative control."""
    return body.replace(FALLBACK, "")


def _diagnostics(out: str) -> list[str]:
    return [
        ln for ln in out.splitlines()
        if "::error::" in ln or "::warning::" in ln
    ]


_CALL_COUNT_CACHE: dict[str, int] = {}


def _happy_curl_calls(tmp_path: Path, body: str) -> int:
    """How many curl invocations the step makes when nothing fails.

    This is the sweep bound. It is MEASURED by running the step, not parsed out
    of the YAML: a count derived from a regex would inherit exactly the blind
    spot (`read_secret_staging` vs `read_secret`) that let an unguarded capture
    sit in a green tree in the first place.

    Cached on the body text. The sweep parametrises over a fixed position range
    and skips positions past the end, so without a cache the SKIPPED cases each
    pay for a full step execution just to learn they should skip — hundreds of
    subprocesses to decide nothing.
    """
    if body in _CALL_COUNT_CACHE:
        return _CALL_COUNT_CACHE[body]
    result = _run(tmp_path / "count", body, "count", fail_at=None)
    counter = tmp_path / "count" / "count-x" / "curl_calls"
    if not counter.exists():
        _CALL_COUNT_CACHE[body] = 0
        return 0
    assert result.returncode == 0, (
        f"the happy path must complete before its calls can be counted "
        f"(rc={result.returncode}): {result.stdout!r} {result.stderr[-400:]!r}"
    )
    calls = int(counter.read_text(encoding="utf-8").strip() or "0")
    _CALL_COUNT_CACHE[body] = calls
    return calls


# Sweep bound. Every discovered step is asserted below to make FEWER than this
# many curl calls, so the bound can never silently truncate the sweep — raising
# it is a one-line change that the assertion will demand of anyone who needs it.
# The deepest step in the tree today makes 3 calls; 6 keeps headroom without
# generating hundreds of skipped cases that make the report harder to read.
MAX_CURL_CALLS = 6


# --------------------------------------------------------------------------
# The directions
# --------------------------------------------------------------------------


@pytest.mark.parametrize("wf,name,body", STEPS, ids=STEP_IDS)
def test_every_capture_position_is_reachable_by_the_harness(
    wf: str, name: str, body: str, tmp_path: Path
) -> None:
    """The sweep bound must actually cover the step.

    Anti-vacuity for the sweep itself: if a step made more calls than
    MAX_CURL_CALLS, the positions past the bound would go untested and every
    parametrised case would still be green.
    """
    calls = _happy_curl_calls(tmp_path, body)
    assert calls > 0, f"{wf} {name}: the happy path made no curl calls at all"
    assert calls < MAX_CURL_CALLS, (
        f"{wf} {name}: {calls} curl calls exceeds the sweep bound "
        f"{MAX_CURL_CALLS}; raise the bound or the later captures go untested"
    )


@pytest.mark.parametrize("fail_at", range(MAX_CURL_CALLS))
@pytest.mark.parametrize("wf,name,body", STEPS, ids=STEP_IDS)
def test_a_failure_at_ANY_capture_position_still_diagnoses(
    wf: str, name: str, body: str, fail_at: int, tmp_path: Path
) -> None:
    """Walk the transport failure down the step, one capture at a time.

    WHY THIS EXISTS. The first revision of this file failed EVERY curl call, so
    the first guard `exit 1`d and no capture below it was ever executed. "32/32
    green" meant 32 FIRST captures, not 64 sites. Two things hid in that gap:

      * a mutant stripping the fallback from the SECOND capture in
        `zone-count-drift.yml` survived the whole suite AND the lint;
      * `sweep-cf-tunnels.yml:150` — `CP_STAGING_ADMIN_API_TOKEN`, captured
        through a `read_secret_staging` helper — had no fallback and an
        unwrapped `add-mask`, in the SAME step whose sibling capture was fixed.

    Failing from call index `fail_at` onward makes the earlier captures succeed,
    so execution reaches the later ones and their guards are exercised too.
    """
    calls = _happy_curl_calls(tmp_path, body)
    if fail_at >= calls:
        pytest.skip(f"step makes only {calls} curl call(s)")
    result = _run(tmp_path, body, "dead", fail_at=fail_at)
    diags = _diagnostics(result.stdout)
    assert diags, (
        f"{wf} {name}: transport died at curl call #{fail_at} and the step "
        f"printed NO ::error::/::warning:: of its own (rc={result.returncode}). "
        f"A capture below the first one is dying before its guard.\n"
        f"stdout={result.stdout!r}\nstderr={result.stderr[-800:]!r}"
    )
    empty_masks = [
        ln for ln in result.stdout.splitlines() if ln.strip() == "::add-mask::"
    ]
    assert not empty_masks, (
        f"{wf} {name}: ZERO-LENGTH ::add-mask:: when curl call #{fail_at} died. "
        "An empty mask pattern is at best ignored and at worst applied to every "
        'log line; guard it with `if [ -n "$VAR" ]`.'
    )


@pytest.mark.parametrize("wf,name,body", STEPS, ids=STEP_IDS)
def test_dead_transport_still_prints_the_step_s_own_diagnostic(
    wf: str, name: str, body: str, tmp_path: Path
) -> None:
    """The step must reach its own guard and SAY what went wrong.

    The assertion is on the DIAGNOSTIC, not on the exit status, because the two
    correct behaviours differ: most of these steps fail closed (`::error::` +
    exit 1), while `local-provision-e2e` deliberately records a transient and
    exits 0 on an unreachable external dependency (`::warning::`). Asserting
    `returncode != 0` would demand that one step change its verdict — and this
    change alters no verdict anywhere. What both shapes owe the operator is a
    line naming the cause; a step that dies with neither is the defect.
    """
    result = _run(tmp_path, body, "dead", fail_at=0)
    diags = _diagnostics(result.stdout)
    assert diags, (
        f"{wf} {name}: step ended with NO ::error::/::warning:: of its own "
        f"(rc={result.returncode}). The guard below the capture never ran.\n"
        f"stdout={result.stdout!r}\nstderr={result.stderr[-800:]!r}"
    )
    if result.returncode == 0:
        # Exiting 0 on a dead dependency is only defensible when the step SAYS
        # it is recording a transient, never silently.
        assert any("::warning::" in d for d in diags), (
            f"{wf} {name}: exited 0 on a dead transport without a ::warning::"
        )


@pytest.mark.parametrize("wf,name,body", STEPS, ids=STEP_IDS)
def test_stripping_the_fallback_LOSES_the_diagnostic(
    wf: str, name: str, body: str, tmp_path: Path
) -> None:
    """Negative control: mutate exactly one thing and a guard goes unreachable.

    Swept across capture positions for the same reason the test above is: a
    control that only ever kills the FIRST capture proves nothing about the
    second, and a mutant on the second is precisely what survived the first
    revision of this suite. It is enough that SOME position loses its
    diagnostic — the mutation removes every fallback in the step, and a step
    whose diagnostics are unchanged at every position is one where the
    fallbacks are doing nothing.
    """
    mutated = _mutate(body)
    if mutated == body:
        pytest.skip("no fallback in this step to strip")
    calls = _happy_curl_calls(tmp_path, body)
    losses = []
    for fail_at in range(calls):
        fixed = _diagnostics(_run(tmp_path / "f", body, "dead", fail_at).stdout)
        broken = _diagnostics(_run(tmp_path / "b", mutated, "dead", fail_at).stdout)
        assert fixed, (
            f"{wf} {name}: precondition — the fixed body must diagnose at "
            f"position {fail_at}"
        )
        if len(broken) < len(fixed):
            losses.append(fail_at)
    assert losses, (
        f"{wf} {name}: stripping every fallback changed nothing at ANY of the "
        f"{calls} failure positions, so the fallbacks are not what make the "
        "guards reachable."
    )


@pytest.mark.parametrize("wf,name,body", STEPS, ids=STEP_IDS)
def test_happy_path_is_byte_identical_with_and_without_the_fallback(
    wf: str, name: str, body: str, tmp_path: Path
) -> None:
    """The fallback may only ever change the FAILURE path.

    Compared by sha256 of stdout, which is what #5166's reviewer asked for.
    """
    mutated = _mutate(body)
    if mutated == body:
        pytest.skip("no fallback in this step to strip")
    a = _run(tmp_path / "a", body, "happy", fail_at=None)
    b = _run(tmp_path / "b", mutated, "happy", fail_at=None)
    sha_a = hashlib.sha256(a.stdout.encode("utf-8")).hexdigest()
    sha_b = hashlib.sha256(b.stdout.encode("utf-8")).hexdigest()
    assert a.returncode == b.returncode, (
        f"{wf} {name}: happy-path exit status changed "
        f"({b.returncode} -> {a.returncode})"
    )
    assert sha_a == sha_b, textwrap.dedent(
        f"""\
        {wf} {name}: the fallback moved the HAPPY path.
          with fallback    sha256={sha_a}
          without fallback sha256={sha_b}
          with:    {a.stdout!r}
          without: {b.stdout!r}
        """
    )
