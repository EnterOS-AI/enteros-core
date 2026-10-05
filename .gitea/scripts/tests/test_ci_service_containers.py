"""CI service containers (Postgres / Redis) must not leak a volume per run.

WHY THIS GUARD EXISTS
---------------------
e2e-api, e2e-chat, handlers-postgres-integration, local-provision-e2e and
selfhost-concierge-schedules-e2e start postgres:16 / pgvector:pg15 / redis:7 on
the runner's SHARED host docker daemon. Those images declare a VOLUME for their
data dir, so every plain `docker run` created an anonymous volume, and every
teardown removed the container with `docker rm -f` — no -v — leaving the volume
dangling: two per run, ~17k on one CI host in six weeks. A cancelled job does not
run its teardown at all.

The fix: every start goes through .gitea/scripts/ci-service-container.sh, which
mounts a size-capped tmpfs at the data dir (no volume is ever created) and labels
the container with its run; every teardown uses `rm -fv`. These tests pin the
script's behaviour and that no workflow drifts back to the inline form.
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / ".gitea" / "scripts" / "ci-service-container.sh"
WORKFLOWS = ROOT / ".gitea" / "workflows"
BASH = shutil.which("bash") or "bash"

FAKE_DOCKER = """#!/usr/bin/env bash
printf '%s\\0' "$@" >> "$FAKE_STATE/calls"
printf '\\n' >> "$FAKE_STATE/calls"
[ "$1" = run ] && echo 0123456789ab
exit 0
"""


def _run(tmp: Path, *args: str) -> tuple[subprocess.CompletedProcess[str], list[list[str]]]:
    bindir = tmp / "bin"
    bindir.mkdir(exist_ok=True)
    fake = bindir / "docker"
    fake.write_text(FAKE_DOCKER, encoding="utf-8", newline="\n")
    fake.chmod(0o755)
    env = os.environ.copy()
    env["PATH"] = str(bindir) + os.pathsep + env["PATH"]
    env["FAKE_STATE"] = tmp.as_posix()
    env["GITHUB_RUN_ID"] = "4242"
    proc = subprocess.run([BASH, str(SCRIPT), *args], env=env, text=True,
                          capture_output=True, check=False)
    calls_file = tmp / "calls"
    calls = []
    if calls_file.exists():
        for line in calls_file.read_bytes().decode().split("\n"):
            if line:
                calls.append(line.split("\0")[:-1])
    return proc, calls


@pytest.mark.parametrize("kind,image,tmpfs", [
    ("postgres", "postgres:16", "/var/lib/postgresql/data:size=1g"),
    ("postgres", "pgvector/pgvector:pg15", "/var/lib/postgresql/data:size=1g"),
    ("redis", "redis:7", "/data:size=256m"),
])
def test_starts_with_a_tmpfs_data_dir_and_a_run_label(tmp_path: Path, kind: str, image: str, tmpfs: str) -> None:
    proc, calls = _run(tmp_path, kind, "pg-unit-1", image, "-e", "X=1", "-p", "127.0.0.1::5432")
    assert proc.returncode == 0, proc.stderr
    assert proc.stdout.strip() == "0123456789ab", "must print the id like `docker run -d`"
    # Pre-clean first, WITH -v (a leftover from a rerun must not strand its volume).
    assert calls[0] == ["rm", "-fv", "pg-unit-1"]
    run = calls[1]
    assert run[:4] == ["run", "-d", "--name", "pg-unit-1"]
    assert run[-1] == image, "caller args must come BEFORE the image"
    assert "--tmpfs" in run and run[run.index("--tmpfs") + 1] == tmpfs
    assert "--label" in run and run[run.index("--label") + 1] == "molecule.ci.run=4242"
    assert run[-5:-1] == ["-e", "X=1", "-p", "127.0.0.1::5432"]
    assert len(calls) == 2


@pytest.mark.parametrize("args", [("mysql", "x", "mysql:8"), ("postgres", "x"), ()])
def test_rejects_unknown_kinds_and_short_usage_without_touching_docker(tmp_path: Path, args: tuple[str, ...]) -> None:
    proc, calls = _run(tmp_path, *args)
    assert proc.returncode == 2
    assert calls == []


# ---------------------------------------------------------------------------
# Workflow drift guards
# ---------------------------------------------------------------------------

_SERVICE_IMAGE = re.compile(r"(?<![\w/.-])(?:postgres|redis|pgvector/pgvector)(?::[\w.-]+)?(?=\s|$|\")")
_SERVICE_VAR = re.compile(r"\$\{?(?:PG_CONTAINER|REDIS_CONTAINER|PG_NAME)\}?")


def _commands():
    """(workflow, step, logical command) for every run block, comments stripped and
    backslash-continuations joined, so a multi-line `docker run` is one string."""
    for wf in sorted(WORKFLOWS.glob("*.yml")):
        doc = yaml.safe_load(wf.read_text(encoding="utf-8"))
        if not isinstance(doc, dict):
            continue
        for job in (doc.get("jobs") or {}).values():
            for step in (job or {}).get("steps") or []:
                run = step.get("run") if isinstance(step, dict) else None
                if not isinstance(run, str):
                    continue
                code = "\n".join(re.sub(r"(^|\s)#.*$", "", ln) for ln in run.splitlines())
                for cmd in re.sub(r"\\\n\s*", " ", code).splitlines():
                    if cmd.strip():
                        yield wf.name, step.get("name") or "<unnamed>", cmd.strip()


def test_the_scan_sees_the_service_container_starts() -> None:
    """Fail-closed: a broken parse would make the guards below vacuously pass."""
    expected = {  # pg + redis each; handlers runs pgvector only; local-provision has 2 jobs
        "e2e-api.yml": 2, "e2e-chat.yml": 2, "handlers-postgres-integration.yml": 1,
        "local-provision-e2e.yml": 4, "selfhost-concierge-schedules-e2e.yml": 2,
    }
    seen: dict[str, int] = {}
    for wf, _, cmd in _commands():
        if "ci-service-container.sh" in cmd:
            seen[wf] = seen.get(wf, 0) + 1
    for wf, n in expected.items():
        assert seen.get(wf, 0) >= n, f"{wf}: expected {n} service-container start(s), saw {seen.get(wf, 0)}"


def test_no_workflow_runs_a_volume_declaring_service_image_inline() -> None:
    offenders = [
        f"{wf} :: {step!r}: {cmd}" for wf, step, cmd in _commands()
        if re.search(r"\bdocker\s+run\b", cmd) and _SERVICE_IMAGE.search(cmd.split("docker run", 1)[1])
    ]
    assert not offenders, (
        "a workflow starts postgres/redis/pgvector with a bare `docker run`. Those images "
        "declare a VOLUME, so each start creates an anonymous volume that `docker rm -f` "
        "strands. Start it with `bash .gitea/scripts/ci-service-container.sh` (tmpfs data "
        f"dir + run label).\n    Offenders: {offenders}"
    )


def test_service_container_teardowns_remove_volumes() -> None:
    offenders = [
        f"{wf} :: {step!r}: {cmd}" for wf, step, cmd in _commands()
        if re.search(r"\bdocker\s+(?:container\s+)?rm\s+-f\b(?!v)", cmd) and _SERVICE_VAR.search(cmd)
    ]
    assert not offenders, (
        "a service-container teardown uses `docker rm -f` without -v; any anonymous "
        f"volume the container has is left dangling. Use `docker rm -fv`.\n    Offenders: {offenders}"
    )


def test_no_detect_step_passes_an_empty_push_base() -> None:
    """handlers-postgres-integration passed `${GITHUB_EVENT_BEFORE:-}`. act_runner
    never sets GITHUB_EVENT_BEFORE, so the base was always empty, detect-changes
    read every push as "everything changed", and each push started pgvector."""
    offenders = [
        f"{wf} :: {step!r}" for wf, step, cmd in _commands()
        if "detect-changes.py" in cmd and re.search(r'--push-before\s+"\$\{GITHUB_EVENT_BEFORE:-\}"', cmd)
    ]
    assert not offenders, offenders
    handlers = [cmd for wf, _, cmd in _commands()
                if wf == "handlers-postgres-integration.yml" and "detect-changes.py" in cmd]
    assert handlers and all('--push-before "${GITHUB_EVENT_BEFORE:-$PUSH_BEFORE}"' in c for c in handlers), handlers
