"""Regression guard: CI / Platform (Go) must provision a Python that satisfies
the workspace-runtime's `requires-python` BEFORE it pip-installs that runtime.

Why this guard exists
---------------------
`CI / Platform (Go)` checks out `molecule-ai-workspace-runtime` and runs
`python3 -m pip install .` so the MCPServerAdaptor contract tests exercise the
REAL runtime instead of skipping. That step used the runner's bare interpreter.

`ubuntu-latest` on this fleet ships **Python 3.10.12**, and the runtime declares
`requires-python = ">=3.11"` in its pyproject.toml. pip therefore refused the
install outright:

    ERROR: Package 'molecules-workspace-runtime' requires a different Python:
           3.10.12 not in '>=3.11'

which failed the step (`set -euo pipefail`, no `|| true`) and reddened
`CI / Platform (Go)` on main — run 620337 / job 916932, commit dd1841976.

Nothing in the repo pinned the interpreter, so the job's Python floor was
whatever the runner image happened to ship. A runner-image bump could silently
re-break (or silently mask) this at any time. This test pins the contract:
an explicit `actions/setup-python` step, at or above the runtime's floor,
ordered before the install.

It deliberately does NOT assert an exact version — only that the floor is met —
so routine `python-version` bumps stay green while a removal or a downgrade
below the runtime's floor fails closed.
"""

from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[3]

# The floor declared by molecule-ai-workspace-runtime's pyproject.toml
# (`requires-python = ">=3.11"`). Kept as a tuple so the comparison is
# numeric, not lexical — "3.9" > "3.11" as strings.
RUNTIME_REQUIRES_PYTHON = (3, 11)

INSTALL_STEP_NAME = (
    "Install molecule-ai-workspace-runtime and its Python deps for contract tests"
)


def _platform_steps() -> list:
    with (ROOT / ".gitea/workflows/ci.yml").open(encoding="utf-8") as f:
        workflow = yaml.safe_load(f)
    job = workflow["jobs"]["platform-build"]
    assert job["name"] == "Platform (Go)", (
        "the platform-build job was renamed; this guard keys on the job that "
        "pip-installs molecule-ai-workspace-runtime."
    )
    return job["steps"]


def _parse_version(raw) -> tuple:
    parts = str(raw).strip().split(".")
    return tuple(int(p) for p in parts if p.isdigit())


def _index_of_install_step(steps: list) -> int:
    for i, step in enumerate(steps):
        if step.get("name") == INSTALL_STEP_NAME:
            return i
    raise AssertionError(
        f"CI / Platform (Go) no longer has a step named {INSTALL_STEP_NAME!r} — "
        "if the runtime install moved, move this guard with it rather than "
        "deleting it."
    )


def _setup_python_steps(steps: list) -> list:
    found = []
    for i, step in enumerate(steps):
        uses = str(step.get("uses") or "")
        if uses.startswith("actions/setup-python"):
            found.append((i, step))
    return found


def test_platform_go_sets_up_python_at_or_above_the_runtime_floor() -> None:
    steps = _platform_steps()
    setups = _setup_python_steps(steps)

    assert setups, (
        "CI / Platform (Go) has no actions/setup-python step, so "
        "`python3 -m pip install .` runs against whatever interpreter the "
        "runner image ships. ubuntu-latest ships 3.10.12 and "
        "molecule-ai-workspace-runtime requires >=3.11, so the install fails "
        "with 'requires a different Python'."
    )

    floor_str = ".".join(str(p) for p in RUNTIME_REQUIRES_PYTHON)
    for _, step in setups:
        with_block = step.get("with") or {}
        assert "python-version" in with_block, (
            "actions/setup-python must pin an explicit python-version; without "
            "one it resolves to the runner default (3.10.12), which is below "
            f"the runtime's >={floor_str} floor."
        )
        version = _parse_version(with_block["python-version"])
        assert version >= RUNTIME_REQUIRES_PYTHON, (
            f"CI / Platform (Go) sets up Python {with_block['python-version']}, "
            f"below molecule-ai-workspace-runtime's requires-python >={floor_str}. "
            "pip will refuse the install with 'requires a different Python'."
        )


def test_python_setup_precedes_the_runtime_install() -> None:
    steps = _platform_steps()
    setups = _setup_python_steps(steps)
    assert setups, "no actions/setup-python step (see the floor test for why)"

    install_at = _index_of_install_step(steps)
    first_setup_at = min(i for i, _ in setups)

    assert first_setup_at < install_at, (
        f"actions/setup-python is at step index {first_setup_at} but the runtime "
        f"install is at {install_at}. Setting the interpreter up AFTER the install "
        "leaves the install itself on the runner's 3.10.12 — the exact failure "
        "this guard exists to catch."
    )


def test_runtime_install_step_is_not_masked() -> None:
    """A `|| true` / continue-on-error here would re-create the CR2 #12653
    false-green: the contract test SKIPS with 'runtime not available' and the
    green Platform (Go) job is not exercising the real MCPServerAdaptor."""
    steps = _platform_steps()
    step = steps[_index_of_install_step(steps)]

    assert step.get("continue-on-error") is not True, (
        "the runtime install step must not be masked with continue-on-error — "
        "a failed install silently downgrades the contract test to a skip."
    )
    assert "|| true" not in str(step.get("run") or ""), (
        "the runtime install step must not swallow failures with `|| true` "
        "(CR2 #12653 regression)."
    )
