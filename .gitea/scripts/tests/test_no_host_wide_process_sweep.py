"""No Gitea workflow may signal a process it did not itself start.

THE BUG THIS LOCKS OUT
----------------------
`e2e-api`, `local-provision-e2e`, `e2e-peer-visibility` and `e2e-chat` all boot a
real `platform-server` on the SHARED `docker-host` runner, and they demonstrably
run concurrently across different PRs. THREE of them (e2e-api, e2e-peer-visibility
and local-provision-e2e, the last one twice — four steps in all) carried a
pre-start step that scanned /proc and killed ANY process whose cmdline contained
"platform-server":

    for pid in $(grep -l "platform-serve" /proc/[0-9]*/comm 2>/dev/null); do
      cmdline=$(cat "/proc/${kpid}/cmdline" | tr '\\0' ' ')
      if echo "$cmdline" | grep -q "platform-server"; then kill "$kpid"; fi
    done

That is host-wide, not run-scoped. A job starting inside another PR's live e2e
window killed THAT PR's platform-server mid-test, reddening a required context on
a PR that changed nothing. Under this repo's branch protection
(`status_check_contexts=['*']`, every posted context must be success) one such
red freezes the entire merge queue. It is the repo's non-hermetic shared-runner
failure mode, and the sweep was its mechanism.

The sweeps bought nothing, either: every one of those lanes now allocates an
EPHEMERAL port (binds :0), so no leftover process can hold "our" port — the
original #1046 rationale ("kill it so the port is definitively free") died when
the lanes stopped hard-coding :8080. `e2e-chat.yml` boots a platform-server on
the same runner with no sweep at all and is green.

THE PROPERTY
------------
A sweep is the conjunction of two things: ENUMERATING processes you do not own
(by name, via /proc, pgrep, pkill, killall, ps|grep) and SIGNALLING them. Either
alone is fine — `kill -0 "$PID"` is a liveness probe, and `ps` alone is
diagnostic. Together they are cross-run interference. This lint fails on the
conjunction.

The sanctioned shapes, all run-scoped, are unaffected:
  * `kill "$(cat workspace-server/platform.pid)"` — the PID this run recorded;
  * `kill -0 "$PLATFORM_PID"`                     — liveness probe, no signal;
  * `timeout --signal=TERM ... ./platform-server` — self-reaping own child;
  * `docker rm -f "$PG_CONTAINER"`                — run-scoped container name.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest
import yaml

WORKFLOWS = Path(__file__).resolve().parents[2] / "workflows"

# Enumerating processes you do not own.
ENUMERATION = (
    re.compile(r"/proc/(?:\[0-9\]\*|\*)/"),        # /proc/[0-9]*/comm, /proc/*/cmdline
    re.compile(r"\bpgrep\b"),
    re.compile(r"\bps\s+(?:-e|-A|aux|ax)\b"),
)

# Signalling. `kill -0` is excluded: it sends no signal, it only asks "alive?".
SIGNAL = re.compile(r"\bkill\b(?!\s+-0\b)")

# Kill-by-name: enumeration and signalling fused into one command. Always host-wide.
KILL_BY_NAME = re.compile(r"\b(?:pkill|killall)\b")


def _run_blocks() -> list[tuple[str, str, str]]:
    """(workflow file, step name, shell body) for every `run:` in every workflow."""
    blocks: list[tuple[str, str, str]] = []
    for wf in sorted(WORKFLOWS.glob("*.yml")):
        doc = yaml.safe_load(wf.read_text())
        if not isinstance(doc, dict):
            continue
        for job in (doc.get("jobs") or {}).values():
            if not isinstance(job, dict):
                continue
            for step in job.get("steps") or []:
                if isinstance(step, dict) and isinstance(step.get("run"), str):
                    blocks.append((wf.name, step.get("name") or "<unnamed>", step["run"]))
    return blocks


def test_workflows_exist() -> None:
    """Fail-closed: an empty glob would make every test below vacuously pass."""
    blocks = _run_blocks()
    assert len(blocks) > 50, f"only parsed {len(blocks)} run-blocks — glob/parse broke"


@pytest.mark.parametrize("wf,name,body", _run_blocks(), ids=lambda v: str(v)[:40])
def test_no_step_enumerates_and_signals_foreign_processes(wf: str, name: str, body: str) -> None:
    if KILL_BY_NAME.search(body):
        pytest.fail(
            f"{wf} / step {name!r} kills processes BY NAME (pkill/killall). That is "
            f"host-wide on the shared docker-host runner: it reaches into a "
            f"concurrent PR's run and kills its server, reddening a required "
            f"context on an unrelated PR and wedging the ['*'] merge queue. Kill "
            f"only the PID this run started (e.g. from platform.pid)."
        )

    if not SIGNAL.search(body):
        return

    for pattern in ENUMERATION:
        if pattern.search(body):
            pytest.fail(
                f"{wf} / step {name!r} ENUMERATES processes ({pattern.pattern}) and "
                f"SIGNALS them. On the shared docker-host runner that sweep kills "
                f"concurrent PRs' platform-servers mid-test — a required context "
                f"goes red on a PR that changed nothing, and under branch protection "
                f"status_check_contexts=['*'] the merge queue freezes. Signal only "
                f"the PID this run recorded; let an orphan self-reap via `timeout`."
            )


def test_the_platform_lanes_still_reap_their_own_server() -> None:
    """The fix must not have become a leak: deleting the sweep is only safe
    because each lane still stops the server it started, and a hard-cancelled
    run's orphan self-reaps under `timeout`.
    """
    lanes = {
        "e2e-api.yml",
        "local-provision-e2e.yml",
    }
    for wf in lanes:
        text = (WORKFLOWS / wf).read_text()
        assert "platform.pid" in text, f"{wf} no longer records the PID it started"
        assert re.search(r"timeout\s+--signal=TERM[^\n]*\./platform-server", text), (
            f"{wf} starts platform-server without a `timeout` self-reaper. Without it, "
            f"a hard-cancelled run leaks a server forever — and the host-wide sweep "
            f"that used to mop those up is (correctly) gone. Re-adding the sweep is "
            f"NOT the fix; it wedges the merge queue."
        )
