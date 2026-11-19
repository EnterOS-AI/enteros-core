"""The per-job dind must reap ITSELF when the job that started it is gone.

WHY THIS GUARD EXISTS
---------------------
tests/harness/dind.sh runs the e2e-ephemeral / harness-replays topology inside a
privileged docker:dind on the runner's SHARED host daemon. Its teardown (`dind.sh
down` = `docker rm -fv`) lives in an `if: always()` step — and the deployed
act_runner SKIPS always() steps on a cancelled job. So every superseded run left
its dind running until a host reaper removed it ~2h later with `docker rm -f`
(no -v), stranding the dind's anonymous /var/lib/docker volume: ~8 GB per run,
~1 TB on one CI host (189 of 190 reaped dinds were cancelled runs).

The fix makes the dind self-reaping: it is started with `--rm` and a watchdog as
PID 1 that stops it once its OWNER (the job container) is no longer running, or at
a lifetime cap; the auto-remove then takes the volume with it. These tests pin:

  1. `up` actually wires it: --rm, the watchdog entrypoint, the owner id + the
     host-socket bind the watchdog polls through, and the owner/run labels.
  2. the watchdog stops dockerd when the daemon twice says the owner is gone —
     and NOT on a single miss, and NOT when the daemon query merely FAILS (a busy
     daemon is not evidence; killing a live job's dind would red the gate).
  3. the lifetime cap applies when no owner could be identified.
  4. `down` still removes the dind WITH its volume (`rm -fv`).
"""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import time
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]
DIND = ROOT / "tests" / "harness" / "dind.sh"
OWNER = "a" * 64
# Resolved through PATH (on Windows a bare "bash" hits the WSL stub in System32).
BASH = shutil.which("bash") or "bash"
SH = shutil.which("sh") or "sh"

FAKE_DOCKER = r"""#!/usr/bin/env bash
# Fake host docker CLI for dind.sh's `up`: records every call, answers just enough.
printf '%s\n' "$*" >> "$FAKE_STATE/calls"
if [ -n "${DOCKER_HOST:-}" ]; then exit 0; fi   # nested_docker info → reachable
case "$1" in
  info|rm|logs) exit 0 ;;
  inspect)
    if [ "$2" = "-f" ]; then
      case "$3" in
        *State.Running*)
          [ "$4" = "$FAKE_OWNER" ] && [ -n "$FAKE_OWNER" ] && { printf '%s\n' "$FAKE_OWNER"; exit 0; }
          exit 1 ;;
        *Mounts*) printf '%s\n' /var/run/docker.sock; exit 0 ;;
      esac
    fi
    exit 0 ;;
  run)  printf '%s\0' "$@" > "$FAKE_STATE/run-args"; echo deadbeef; exit 0 ;;
  port) echo "127.0.0.1:4$(( RANDOM % 9000 + 1000 ))"; exit 0 ;;
  cp)   d="${3%/}"; mkdir -p "$d"; touch "$d/ca.pem" "$d/cert.pem" "$d/key.pem"; exit 0 ;;
esac
exit 0
"""


def _env(tmp: Path, **extra: str) -> dict[str, str]:
    env = os.environ.copy()
    env["PATH"] = str(tmp / "bin") + os.pathsep + env["PATH"]
    env["FAKE_STATE"] = tmp.as_posix()
    env["GITHUB_WORKSPACE"] = tmp.as_posix()
    env["GITHUB_ENV"] = (tmp / "github_env").as_posix()
    env["GITHUB_RUN_ID"] = "4242"
    env["DIND_NS"] = "unit"
    for k in ("DIND_OWNER", "DIND_MAX_LIFETIME", "DOCKER_HOST"):
        env.pop(k, None)
    env.update(extra)
    return env


def _fake_bin(tmp: Path, name: str, body: str) -> None:
    bindir = tmp / "bin"
    bindir.mkdir(exist_ok=True)
    f = bindir / name
    f.write_text(body, encoding="utf-8", newline="\n")
    f.chmod(0o755)


def _up(tmp: Path, **extra: str) -> tuple[subprocess.CompletedProcess[str], list[str]]:
    _fake_bin(tmp, "docker", FAKE_DOCKER)
    proc = subprocess.run(
        [BASH, str(DIND), "up"], cwd=tmp, env=_env(tmp, **extra),
        text=True, capture_output=True, timeout=120, check=False,
    )
    run_args = tmp / "run-args"
    args = run_args.read_bytes().decode().split("\0")[:-1] if run_args.exists() else []
    return proc, args


def _flag_values(args: list[str], flag: str) -> list[str]:
    return [args[i + 1] for i, a in enumerate(args[:-1]) if a == flag]


def test_up_starts_a_self_reaping_dind_bound_to_its_owner(tmp_path: Path) -> None:
    proc, args = _up(tmp_path, FAKE_OWNER=OWNER, DIND_OWNER=OWNER)
    assert proc.returncode == 0, proc.stderr
    assert args and args[0] == "run", f"no `docker run` recorded: {proc.stderr}"

    # Removed WITH its anonymous volume when the watchdog exits.
    assert "--rm" in args
    # The watchdog is PID 1 and is exactly the program `dind.sh watchdog` prints.
    assert _flag_values(args, "--entrypoint") == ["sh"]
    watchdog = subprocess.run([BASH, str(DIND), "watchdog"], text=True,
                              capture_output=True, check=True).stdout.rstrip("\n")
    assert args[-2:] == ["-c", watchdog]

    envs = _flag_values(args, "-e")
    assert f"DIND_OWNER={OWNER}" in envs
    assert "DIND_MAX_LIFETIME=10800" in envs
    assert _flag_values(args, "--mount") == [
        "type=bind,source=/var/run/docker.sock,target=/run/dind-host-docker.sock"
    ], "the watchdog needs the SAME host socket the job container got"

    labels = _flag_values(args, "--label")
    assert f"molecule.ci.dind-owner={OWNER}" in labels
    assert "molecule.ci.run=4242" in labels


def test_up_without_an_identifiable_owner_warns_and_relies_on_the_cap(tmp_path: Path) -> None:
    # The daemon does not confirm the owner (FAKE_OWNER empty): the watchdog must
    # NOT be armed with an id it would immediately read as "gone".
    proc, args = _up(tmp_path, FAKE_OWNER="", DIND_OWNER="b" * 64, DIND_MAX_LIFETIME="7200")
    assert proc.returncode == 0, proc.stderr
    assert "could not identify the job container" in proc.stderr
    # Not "only the cap will reap it": a host reaper's `rm -f` usually gets there
    # first, and that strands the volume.
    assert "(no -v) strands its /var/lib/docker volume" in proc.stderr, proc.stderr
    assert not any(e.startswith("DIND_OWNER=") for e in _flag_values(args, "-e"))
    assert _flag_values(args, "--mount") == []
    assert "molecule.ci.dind-owner=unknown" in _flag_values(args, "--label")
    assert "DIND_MAX_LIFETIME=7200" in _flag_values(args, "-e")


def test_up_rejects_a_lifetime_cap_that_would_kill_the_dind_at_once(tmp_path: Path) -> None:
    for bad in ("0", "3h", "-5"):
        proc, args = _up(tmp_path, FAKE_OWNER=OWNER, DIND_OWNER=OWNER, DIND_MAX_LIFETIME=bad)
        assert proc.returncode == 2, (bad, proc.stderr)
        assert args == [], f"started a dind with DIND_MAX_LIFETIME={bad!r}"


def _strip_comments(text: str) -> str:
    return "\n".join(line for line in text.splitlines() if not line.lstrip().startswith("#"))


def test_down_removes_the_dind_with_its_volume() -> None:
    code = _strip_comments(DIND.read_text(encoding="utf-8"))
    down = code.split("\ndown() {", 1)[1].split("\n}\n", 1)[0]
    assert re.search(r'host_docker rm -fv "\$DIND"', down), "down must use rm -fv"
    assert not re.search(r"rm -f\s", down), "a plain `rm -f` strands the ~8 GB /var/lib/docker volume"


# ---------------------------------------------------------------------------
# The watchdog itself, run under `sh` with a fake dockerd and a fake host daemon.
# ---------------------------------------------------------------------------

# Detached from the test's pipes and bounded (~30s), so a watchdog that FAILS to
# stop it makes the test fail on its timeout instead of hanging on an orphan.
FAKE_DOCKERD = """#!/bin/sh
exec >/dev/null 2>&1
echo $$ > "$FAKE_STATE/dockerd.pid"
trap 'echo TERM >> "$FAKE_STATE/dockerd"; exit 0' TERM
echo started >> "$FAKE_STATE/dockerd"
i=0
while [ "$i" -lt 600 ]; do sleep 0.05; i=$((i + 1)); done
"""

# Answers the watchdog's `ps --filter id=<owner>` from a script of responses:
# up = owner listed, down = daemon says not running, err = the query failed.
FAKE_HOST_DOCKER = """#!/bin/sh
printf '%s\\n' "$*" >> "$FAKE_STATE/polls"
n=$(wc -l < "$FAKE_STATE/polls")
r=$(sed -n "${n}p" "$FAKE_STATE/script")
[ -n "$r" ] || r=$(tail -n 1 "$FAKE_STATE/script")
case "$r" in
  up)   printf '%s\\n' "$DIND_OWNER" ;;
  down) : ;;
  err)  echo "Cannot connect to the Docker daemon" >&2; exit 1 ;;
esac
"""


def _run_watchdog(tmp: Path, script: list[str], *, owner: str = OWNER, cap: int = 30,
                  timeout: float = 20) -> tuple[subprocess.CompletedProcess[str], float]:
    _fake_bin(tmp, "dockerd-entrypoint.sh", FAKE_DOCKERD)
    _fake_bin(tmp, "docker", FAKE_HOST_DOCKER)
    (tmp / "script").write_text("\n".join(script) + "\n", encoding="utf-8", newline="\n")
    program = subprocess.run([BASH, str(DIND), "watchdog"], text=True,
                             capture_output=True, check=True).stdout
    env = _env(tmp, DIND_MAX_LIFETIME=str(cap), DIND_WATCHDOG_INTERVAL="0.1")
    if owner:
        env["DIND_OWNER"] = owner
    t0 = time.monotonic()
    try:
        proc = subprocess.run([SH, "-c", program], cwd=tmp, env=env, text=True,
                              capture_output=True, timeout=timeout, check=False)
    except subprocess.TimeoutExpired:
        pytest.fail(f"watchdog still running after {timeout}s — it never stopped dockerd/exited")
    finally:
        # Never leave the fake dockerd behind (bash `kill`: on Windows the pid is MSYS's).
        pid = tmp / "dockerd.pid"
        if pid.exists():
            subprocess.run([BASH, "-c", f"kill {pid.read_text().strip()} 2>/dev/null"], check=False)
    return proc, time.monotonic() - t0


def _polls(tmp: Path) -> list[str]:
    p = tmp / "polls"
    return p.read_text(encoding="utf-8").splitlines() if p.exists() else []


def test_watchdog_stops_dockerd_once_the_owner_is_gone(tmp_path: Path) -> None:
    proc, elapsed = _run_watchdog(tmp_path, ["up", "up", "up", "down", "down"])
    assert "owner container" in proc.stderr and "is gone" in proc.stderr, proc.stderr
    assert (tmp_path / "dockerd").read_text().split() == ["started", "TERM"], (
        "the watchdog exited without stopping dockerd — the container would linger"
    )
    polls = _polls(tmp_path)
    assert len(polls) == 5, polls
    # It asks the HOST daemon (the socket bind) about exactly its owner.
    assert all(
        p.startswith("-H unix:///run/dind-host-docker.sock ps -q --no-trunc --filter id=" + OWNER)
        for p in polls
    ), polls
    assert elapsed < 15


def test_watchdog_needs_two_consecutive_misses(tmp_path: Path) -> None:
    # A single "not running" between "running"s never stops it; the cap does.
    proc, _ = _run_watchdog(tmp_path, ["up", "down"] * 100, cap=3)
    assert "lifetime cap 3s reached" in proc.stderr, proc.stderr
    assert "is gone" not in proc.stderr
    assert len(_polls(tmp_path)) >= 3, "never saw a miss followed by a recovery"


def test_watchdog_does_not_treat_a_failed_query_as_owner_gone(tmp_path: Path) -> None:
    # Daemon unreachable / busy: no verdict. Only the cap may stop it.
    proc, elapsed = _run_watchdog(tmp_path, ["err"], cap=3)
    assert "lifetime cap 3s reached" in proc.stderr, proc.stderr
    assert "is gone" not in proc.stderr
    assert elapsed >= 2, "stopped before the cap on a failed query"
    assert len(_polls(tmp_path)) >= 2, "it must keep asking, not give up"
    assert (tmp_path / "dockerd").read_text().split() == ["started", "TERM"]


def test_watchdog_without_an_owner_is_bounded_by_the_cap(tmp_path: Path) -> None:
    proc, _ = _run_watchdog(tmp_path, ["up"], owner="", cap=1)
    assert "lifetime cap 1s reached" in proc.stderr, proc.stderr
    assert _polls(tmp_path) == [], "no owner: it must not poll the host daemon at all"
