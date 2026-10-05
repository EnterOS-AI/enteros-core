"""ws-orphan-reap.sh — the instance-scoped e2e teardown and the standing ws-* sweep.

WHY THIS GUARD EXISTS
---------------------
ws-<id> workspace containers provisioned by e2e platforms on the CI hosts' shared
docker daemon survived for months, crash-looping ~300k times each, with their
volumes. Two things let them: the e2e jobs removed only the containers their
manifest knew about (never the ones the platform made on its own, e.g. a bundle
import racing a delete — RC09), and the standing janitor aged containers by
State.StartedAt, which a crash-looper resets every few seconds, so it always read
them as "young" and removed nothing — and never their volumes (RC10).

These tests drive the script against a fake daemon and pin:
  * `instance` removes exactly the containers stamped with THIS run's platform
    instance (sha256(DATABASE_URL)[:16], as the platform derives it) — never a
    concurrent run's — with `rm -fv` and their ws-<id>-* volumes;
  * `sweep` ages by .Created (a crash-looper with a fresh StartedAt IS stale),
    keeps anything younger than the floor, removes volumes, honours DRY_RUN and
    the safety cap;
  * both refuse the production box (100.64.0.3), where managed ws-* containers
    are real tenant workspaces;
  * the workflows actually call it, in the right place.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / ".gitea" / "scripts" / "ws-orphan-reap.sh"
WORKFLOWS = ROOT / ".gitea" / "workflows"
BASH = shutil.which("bash") or "bash"
MANAGED, INSTANCE = "molecule.platform.managed", "molecule.platform.instance"

FAKE_DOCKER_PY = r'''
import json, os, sys
sys.stdout.reconfigure(newline="\n")  # a real docker CLI never emits CRLF
state_path = os.path.join(os.environ["FAKE_STATE"], "daemon.json")
st = json.load(open(state_path))
args = sys.argv[1:]
with open(os.path.join(os.environ["FAKE_STATE"], "calls.jsonl"), "a") as f:
    f.write(json.dumps(args) + "\n")

def save():
    json.dump(st, open(state_path, "w"))

def match(labels, filters):
    for flt in filters:
        k, _, v = flt[len("label="):].partition("=")
        if labels.get(k) != v:
            return False
    return True

def filters_of(a):
    return [a[i + 1] for i, x in enumerate(a[:-1]) if x == "--filter"]

cmd = args[0]
if cmd == "info":
    print(st["os"]); sys.exit(0)
if cmd == "ps":
    for cid, c in st["containers"].items():
        if match(c["labels"], filters_of(args)):
            print(cid)
    sys.exit(0)
if cmd == "inspect":
    tmpl, cid = args[2], args[3]
    c = st["containers"].get(cid)
    if c is None:
        print("Error: No such object: " + cid, file=sys.stderr); sys.exit(1)
    if ".Mounts" in tmpl:
        print("".join(m + " " for m in c["mounts"]))
        sys.exit(0)
    out = tmpl
    for tok, val in (("{{.Name}}", "/" + c["name"]), ("{{.Created}}", c["created"]),
                     ("{{.State.StartedAt}}", c["started"]), ("{{.RestartCount}}", str(c["restarts"])),
                     ("{{.State.Status}}", c["state"]),
                     ('{{index .Config.Labels "molecule.platform.instance"}}', c["labels"].get("molecule.platform.instance", ""))):
        out = out.replace(tok, val)
    print(out)
    sys.exit(0)
if cmd == "rm":
    cid = args[-1]
    c = st["containers"].pop(cid, None)
    if c is None:
        sys.exit(1)
    if "-fv" in args or "-v" in args:
        for v in c.get("anon", []):
            st["volumes"].pop(v, None)
    save(); sys.exit(0)
if cmd == "volume" and args[1] == "ls":
    for name, v in st["volumes"].items():
        if match(v["labels"], filters_of(args)):
            print(name)
    sys.exit(0)
if cmd == "volume" and args[1] == "rm":
    rc = 0
    for name in [a for a in args[2:] if not a.startswith("-")]:
        if any(name in c["mounts"] for c in st["containers"].values()):
            print("volume is in use", file=sys.stderr); rc = 1; continue
        if st["volumes"].pop(name, None) is None:
            rc = 1
    save(); sys.exit(rc)
sys.exit(1)
'''


def iso(seconds_ago: float) -> str:
    t = datetime.fromtimestamp(time.time() - seconds_ago, tz=timezone.utc)
    return t.strftime("%Y-%m-%dT%H:%M:%S.123456789Z")


def ws(name: str, *, instance: str | None, created_ago: float = 60, started_ago: float | None = None,
       restarts: int = 0, state: str = "running", managed: bool = True, mounts=(), anon=()) -> dict:
    labels = {}
    if managed:
        labels[MANAGED] = "true"
    if instance:
        labels[INSTANCE] = instance
    return {"name": name, "labels": labels, "created": iso(created_ago),
            "started": iso(created_ago if started_ago is None else started_ago), "restarts": restarts,
            "state": state, "mounts": list(mounts), "anon": list(anon)}


class Daemon:
    def __init__(self, tmp: Path, containers: dict, volumes: dict, os_name: str = "Ubuntu 24.04.4 LTS",
                 ip_addrs: str = "    inet 100.64.0.2/32 scope global tailscale0"):
        self.tmp = tmp
        bindir = tmp / "bin"
        bindir.mkdir()
        (tmp / "fake_docker.py").write_text(FAKE_DOCKER_PY, encoding="utf-8")
        py = Path(sys.executable).as_posix()
        for name, body in {
            "docker": f'#!/usr/bin/env bash\nexec "{py}" "$FAKE_STATE/fake_docker.py" "$@"\n',
            "ip": f"#!/usr/bin/env bash\nprintf '%s\\n' '2: eth0    inet 10.0.0.5/24 brd x' '{ip_addrs}'\n",
        }.items():
            f = bindir / name
            f.write_text(body, encoding="utf-8", newline="\n")
            f.chmod(0o755)
        self.state = {"os": os_name, "containers": containers, "volumes": volumes}
        (tmp / "daemon.json").write_text(json.dumps(self.state), encoding="utf-8")

    def run(self, *args: str, **env_extra: str) -> subprocess.CompletedProcess[str]:
        env = os.environ.copy()
        env["PATH"] = str(self.tmp / "bin") + os.pathsep + env["PATH"]
        env["FAKE_STATE"] = self.tmp.as_posix()
        for k in ("DOCKER_HOST", "WS_MAX_AGE_SECONDS", "WS_MAX_AGE_HOURS", "SAFETY_CAP", "DRY_RUN"):
            env.pop(k, None)
        env.update(env_extra)
        return subprocess.run([BASH, str(SCRIPT), *args], env=env, text=True,
                              capture_output=True, timeout=120, check=False)

    @property
    def now(self) -> dict:
        return json.loads((self.tmp / "daemon.json").read_text(encoding="utf-8"))

    def calls(self) -> list[list[str]]:
        p = self.tmp / "calls.jsonl"
        return [json.loads(l) for l in p.read_text().splitlines()] if p.exists() else []


DSN = "postgres://dev:dev@127.0.0.1:50950/molecule?sslmode=disable"
OURS = hashlib.sha256(DSN.encode()).hexdigest()[:16]  # provisioner.PlatformInstanceID
THEIRS = hashlib.sha256(DSN.replace("50950", "52592").encode()).hexdigest()[:16]


def _vol(instance: str | None = None) -> dict:
    return {"labels": ({MANAGED: "true", INSTANCE: instance} if instance else {})}


def test_instance_removes_exactly_this_runs_workspaces_and_their_volumes(tmp_path: Path) -> None:
    d = Daemon(tmp_path, containers={
        "c-ours": ws("ws-aaa", instance=OURS, mounts=["ws-aaa-configs", "ws-aaa-workspace"], anon=["anon-1"]),
        "c-theirs": ws("ws-bbb", instance=THEIRS, mounts=["ws-bbb-configs"]),   # a concurrent run
        "c-oddname": ws("not-a-workspace", instance=OURS),
    }, volumes={
        "ws-aaa-configs": _vol(),                 # unlabelled: recreated by a bind
        "ws-aaa-workspace": _vol(),               # unlabelled: always bind-created
        "ws-aaa-claude-sessions": _vol(OURS),     # given but never mounted (tier 1)
        "anon-1": _vol(),
        "ws-ccc-configs": _vol(OURS),             # a Start that never got a container
        "ws-bbb-configs": _vol(THEIRS),
    })
    r = d.run("instance", DSN)
    assert r.returncode == 0, r.stderr
    after = d.now
    assert set(after["containers"]) == {"c-theirs", "c-oddname"}, after["containers"].keys()
    assert set(after["volumes"]) == {"ws-bbb-configs"}, after["volumes"].keys()
    # The listing was by THIS instance's label — that is what keeps a concurrent
    # run's live workspace out of reach.
    ps = [c for c in d.calls() if c[0] == "ps"]
    assert ps == [["ps", "-aq", "--no-trunc", "--filter", f"label={MANAGED}=true",
                   "--filter", f"label={INSTANCE}={OURS}"]], ps
    assert ["rm", "-fv", "c-ours"] in d.calls()


def test_instance_id_is_the_platforms_sha256_prefix(tmp_path: Path) -> None:
    d = Daemon(tmp_path, containers={}, volumes={})
    assert d.run("instance", DSN).returncode == 0
    assert f"label={INSTANCE}={OURS}" in [a for c in d.calls() for a in c]
    assert re.fullmatch(r"[0-9a-f]{16}", OURS)


@pytest.mark.parametrize("why,env,os_name,ip", [
    ("DOCKER_HOST is the prod box", {"DOCKER_HOST": "tcp://100.64.0.3:2375"}, "Ubuntu", "inet 100.64.0.2/32"),
    ("this host is the prod box", {}, "Ubuntu", "    inet 100.64.0.3/32 scope global tailscale0"),
    ("Docker Desktop daemon", {}, "Docker Desktop", "inet 10.1.1.1/24"),
])
@pytest.mark.parametrize("mode", [("instance", DSN), ("sweep",)])
def test_refuses_the_production_box(tmp_path: Path, why: str, env: dict, os_name: str, ip: str, mode: tuple) -> None:
    d = Daemon(tmp_path, containers={"c": ws("ws-x", instance=OURS, created_ago=99999)},
               volumes={}, os_name=os_name, ip_addrs=ip)
    r = d.run(*mode, **env)
    assert r.returncode == 3, (why, r.stdout, r.stderr)
    assert not [c for c in d.calls() if c[0] in ("ps", "rm", "volume")], f"{why}: listed/removed before refusing"
    assert "c" in d.now["containers"]


def test_instance_needs_a_dsn(tmp_path: Path) -> None:
    d = Daemon(tmp_path, containers={}, volumes={})
    assert d.run("instance", "").returncode == 2
    assert d.calls() == []


def test_sweep_ages_by_created_so_crash_loopers_are_reaped_with_their_volumes(tmp_path: Path) -> None:
    d = Daemon(tmp_path, containers={
        # The RC10 shape: created hours ago, restarting every few seconds (so its
        # StartedAt is always fresh). The old StartedAt logic kept it forever.
        "c-loop": ws("ws-3fb2904d", instance="2ac9d4b4c2f0a1e7", created_ago=3 * 3600, started_ago=5,
                     restarts=309112, state="restarting", mounts=["ws-3fb2904d-configs"]),
        "c-young": ws("ws-fresh", instance=OURS, created_ago=600),
        "c-unmanaged": ws("ws-unmanaged", instance=None, managed=False, created_ago=9 * 3600),
    }, volumes={"ws-3fb2904d-configs": _vol(), "ws-3fb2904d-workspace": _vol()})
    r = d.run("sweep")
    assert r.returncode == 0, r.stderr
    assert "STALE  ws-3fb2904d" in r.stdout and "restarts=309112" in r.stdout, r.stdout
    assert set(d.now["containers"]) == {"c-young", "c-unmanaged"}
    assert d.now["volumes"] == {}, "the stale workspace's volumes must go with it"
    assert ["ps", "-aq", "--no-trunc", "--filter", f"label={MANAGED}=true"] in d.calls()


def test_sweep_dry_run_and_safety_cap_remove_nothing(tmp_path: Path) -> None:
    containers = {"c-old": ws("ws-old", instance=OURS, created_ago=5 * 3600)}
    d = Daemon(tmp_path, containers=dict(containers), volumes={"ws-old-configs": _vol()})
    r = d.run("sweep", DRY_RUN="true")
    assert r.returncode == 0 and "DRY RUN" in r.stdout, r.stdout
    assert "c-old" in d.now["containers"] and "ws-old-configs" in d.now["volumes"]
    r = d.run("sweep", SAFETY_CAP="0")
    assert r.returncode == 1 and "cap=0" in r.stderr, r.stderr
    assert "c-old" in d.now["containers"]


def test_sweep_floor_is_respected(tmp_path: Path) -> None:
    d = Daemon(tmp_path, containers={"c": ws("ws-run", instance=OURS, created_ago=301)}, volumes={})
    assert d.run("sweep", WS_MAX_AGE_SECONDS="600").returncode == 0
    assert "c" in d.now["containers"], "younger than the floor: may belong to a live run"
    assert d.run("sweep", WS_MAX_AGE_SECONDS="300").returncode == 0
    assert "c" not in d.now["containers"]


# ---------------------------------------------------------------------------
# Static guards: the instance listing is positive-identity, and the workflows
# call the script where it matters.
# ---------------------------------------------------------------------------

def test_instance_mode_lists_only_by_its_own_instance_label() -> None:
    src = SCRIPT.read_text(encoding="utf-8")
    body = src.split("cmd_instance() {", 1)[1].split("\n}\n", 1)[0]
    listings = [ln for ln in body.splitlines()
                if re.search(r"docker\s+(?:ps|container\s+ls|volume\s+ls)\b", ln)]
    assert listings, "instance mode no longer lists anything — the test would be vacuous"
    for ln in listings:
        assert '--filter "label=${LABEL_INSTANCE}=${inst}"' in ln, (
            f"instance-mode listing not filtered by this run's instance label:\n    {ln}\n"
            "Any other listing reaches a concurrent run's workspaces on the shared daemon (#4346)."
        )


def _steps(wf: str):
    doc = yaml.safe_load((WORKFLOWS / wf).read_text(encoding="utf-8"))
    for job_name, job in doc["jobs"].items():
        yield job_name, job, job.get("steps") or []


@pytest.mark.parametrize("wf", ["e2e-api.yml", "local-provision-e2e.yml"])
def test_platform_lanes_reap_their_instance_after_stopping_the_platform(wf: str) -> None:
    jobs = 0
    for job_name, job, steps in _steps(wf):
        names = [s.get("name", "") for s in steps]
        if "Stop platform" not in names:
            continue
        jobs += 1
        i = names.index("Stop platform")
        reap = [k for k, s in enumerate(steps) if "ws-orphan-reap.sh instance" in (s.get("run") or "")]
        assert reap, f"{wf}::{job_name} never runs the instance-scoped teardown"
        k = reap[0]
        assert k > i, f"{wf}::{job_name}: the reap runs BEFORE the platform is stopped — it can still create workspaces"
        assert str(steps[k].get("if", "")).startswith("always()"), f"{wf}::{job_name}: the reap must be if: always()"
        manifest = [n for n, s in enumerate(steps) if re.search(r'docker rm -f "ws-\$\{wsid\}"', s.get("run") or "")]
        assert all(k < n for n in manifest), (
            f"{wf}::{job_name}: the reap must run BEFORE the manifest teardown — it derives volume "
            "names from the containers, which that step removes"
        )
        assert job.get("env", {}).get("MOLECULE_WORKSPACE_RESTART_POLICY") == "no", (
            f"{wf}::{job_name}: CI workspaces must run with restart policy 'no' (quoted: a bare no is YAML false)"
        )
    assert jobs >= 1


def test_the_standing_janitor_runs_the_tested_sweep() -> None:
    runs = [s.get("run") or "" for _, _, steps in _steps("sweep-stale-ws-orphans.yml") for s in steps]
    assert any(r.strip() == "bash .gitea/scripts/ws-orphan-reap.sh sweep" for r in runs), runs
    assert not any("StartedAt" in r for r in runs), "the janitor must not age by StartedAt (RC10)"
