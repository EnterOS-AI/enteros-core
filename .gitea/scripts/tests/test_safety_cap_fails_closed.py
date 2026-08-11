#!/usr/bin/env python3
"""A cap that ERRORS on its input must not read as a cap that PASSED.

WHY THIS FILE EXISTS
--------------------
`sweep-stale-e2e-orgs.yml`'s Safety gate is the last thing between a degraded
control-plane read and a step that DELETES staging tenants. It was written as

    count="${{ steps.identify.outputs.count }}"
    if [ "$count" -gt "$SAFETY_CAP" ]; then ... exit 1; fi
    echo "Within safety cap ✓"

`[ "$count" -gt N ]` with an empty or non-numeric count is not false — it is an
error, rc 2, "integer expected". `set -e` is EXEMPT inside an `if` condition, so
the error does not stop the step; it falls through to the else branch and prints
the pass line. The single input the cap exists to distrust is the one it waved
through.

This is defence in depth rather than a fix for a demonstrated live bug: `count`
comes from `wc -l` and is numeric today. The tests below pin the SHAPE so that a
future edit to the identify step cannot quietly re-open it.

WHAT IS ASSERTED
----------------
The real `run:` body is lifted out of the YAML and EXECUTED under the runner's
own `bash --noprofile --norc -e -o pipefail`, with the `${{ }}` interpolation
replaced by the value under test. A regex assertion could not tell "refuses" from
"prints an error and proceeds"; running it can.

Both directions, and the mutation control matters as much as the fix: the
numeric cases must keep behaving EXACTLY as before, or this hardening has
changed a verdict, which it must not.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
from pathlib import Path

import pytest
import yaml

BASH = os.environ.get("PYTEST_BASH") or shutil.which("bash") or "/bin/bash"

REPO = Path(__file__).resolve().parents[3]
WORKFLOW = REPO / ".gitea" / "workflows" / "sweep-stale-e2e-orgs.yml"

SAFETY_CAP = "50"
PASS_MARKER = "Within safety cap"


def _gate_body() -> str:
    doc = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    bodies = [
        step["run"]
        for job in (doc.get("jobs") or {}).values()
        for step in (job.get("steps") or [])
        if step.get("name") == "Safety gate" and step.get("run")
    ]
    assert len(bodies) == 1, (
        f"expected exactly one 'Safety gate' step, found {len(bodies)} — the "
        "extractor is out of step with the workflow"
    )
    return bodies[0]


GATE_BODY = _gate_body()


def test_the_extractor_found_the_real_gate() -> None:
    """Anti-vacuity: a body that lost its comparison would pass every case."""
    assert "-gt" in GATE_BODY and "SAFETY_CAP" in GATE_BODY, GATE_BODY
    assert PASS_MARKER in GATE_BODY, "the pass line moved; assertions below are blind"


def _run(count: str, body: str | None = None) -> subprocess.CompletedProcess:
    text = body if body is not None else GATE_BODY
    # The step reads the count through a `${{ }}` expression, which the runner
    # substitutes literally before bash ever sees it. Substituting here the same
    # way is what makes an EMPTY count representable at all.
    script = re.sub(r"\$\{\{[^}]*\}\}", count, text)
    return subprocess.run(
        [BASH, "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script],
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        env={
            "SAFETY_CAP": SAFETY_CAP,
            "PATH": os.environ.get("PATH", ""),
            "SYSTEMROOT": os.environ.get("SYSTEMROOT", ""),
        },
        timeout=60,
    )


@pytest.mark.parametrize("count", ["", "abc", "12abc", " ", "-1", "1 2"])
def test_a_count_that_is_not_a_number_REFUSES_the_sweep(count: str) -> None:
    """The state the cap exists to distrust must never reach the delete step."""
    result = _run(count)

    assert result.returncode != 0, (
        f"count={count!r} was waved through (rc=0). An unknown count is not a "
        f"small one.\nstdout={result.stdout!r}"
    )
    assert PASS_MARKER not in result.stdout, (
        f"count={count!r} printed the PASS line: {result.stdout!r}"
    )
    assert "::error::" in result.stdout, (
        f"count={count!r} failed without saying why: {result.stdout!r}"
    )


@pytest.mark.parametrize("count", ["1", "7", "49", "50"])
def test_a_count_within_the_cap_still_passes_UNCHANGED(count: str) -> None:
    """The hardening must not change a single verdict it was not aimed at."""
    result = _run(count)

    assert result.returncode == 0, (
        f"count={count} should pass the cap: {result.stdout!r} {result.stderr!r}"
    )
    assert PASS_MARKER in result.stdout, result.stdout


@pytest.mark.parametrize("count", ["51", "999"])
def test_a_count_over_the_cap_still_REFUSES(count: str) -> None:
    result = _run(count)

    assert result.returncode == 1, result.stdout
    assert "Refusing to delete" in result.stdout, result.stdout


def test_removing_the_validation_REOPENS_the_hole() -> None:
    """Mutation control.

    Strip the `case` guard and the empty count must go back to being waved
    through. Without this, the tests above could be passing for some reason
    other than the guard — and the whole point is that the ERROR silently
    became a PASS.
    """
    mutated = re.sub(
        r"\n\s*case \"\$count\" in.*?\n\s*esac\n", "\n", GATE_BODY, flags=re.DOTALL
    )
    assert mutated != GATE_BODY, "the case guard was not found to strip"

    fixed = _run("")
    broken = _run("", body=mutated)

    assert fixed.returncode != 0 and PASS_MARKER not in fixed.stdout
    assert broken.returncode == 0 and PASS_MARKER in broken.stdout, (
        "stripping the validation did NOT reopen the wave-through, so the "
        f"validation is not what closes it. broken={broken.stdout!r}"
    )
