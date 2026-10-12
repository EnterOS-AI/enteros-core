"""The local-provision E2E must only ever touch workspace containers IT created.

WHY THIS GUARD EXISTS (#4346)
-----------------------------
`local-provision-e2e.yml` runs on `runs-on: docker-host` with a PER-SHA concurrency
group. That serialises a SHA against itself and nothing else — so two different PRs
run this workflow CONCURRENTLY, against the SAME docker daemon, each provisioning
`ws-<uuid>` containers on it.

The teardown used to identify its own containers BY EXCLUSION: snapshot every `ws-*`
container at job start, then delete anything that wasn't in the snapshot. But "not in
my baseline" is not "mine". A run that starts LATER creates containers that are, by
construction, absent from an earlier run's baseline — so the earlier run's teardown
found the later run's LIVE workspace, classified it as its own leak, and `docker rm -f`'d
it mid-test.

The victim's symptom was indistinguishable from a product bug: its container simply
ceased to exist, with nothing wrong anywhere in its provisioning path.

    FAIL: workspace back online after restart (status=provisioning)
    last_sample_error: <none>
    container_running (exact match): <none>      <-- gone
    ws-5d76a8f7-... Restarting (1) 8 seconds ago <-- someone else's, still alive

That reads exactly like a flake, and it is not one.

THE RULE THIS ENFORCES
----------------------
Identify what you own POSITIVELY. The e2e script knows each workspace id the moment
`POST /workspaces` returns it, and the container is named `ws-<id>`, so it records the
id in `$E2E_WS_MANIFEST`. Teardown deletes exactly those names. That is correct no
matter what any concurrent run does, or when it started.

Two things must hold, and neither is checkable by reading one file:
  1. EVERY step that runs the e2e script declares E2E_WS_MANIFEST — a call site that
     does not is a call site whose containers leak forever, because teardown will not
     know about them.
  2. NO step LISTS the daemon's containers at all (any idiom — `docker ps`, `docker
     container ls`, `… | grep ws-`). Enumerating is unnecessary here and is the one way
     a step reaches a concurrent run's container — destructively (`docker rm`) or
     misleadingly (a stranger's logs explaining your own failure). Enforced as a
     PROPERTY (the listing verb), not a blacklist of flag shapes — review walked four
     shapes past the first cut. The driver may read-enumerate for diagnostics but must
     never enumerate-then-destroy.
"""

from __future__ import annotations

import re
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[3]
WORKFLOW = ROOT / ".gitea" / "workflows" / "local-provision-e2e.yml"

E2E_SCRIPT = "tests/e2e/test_local_provision_lifecycle_e2e.sh"
EXPECTED_MANIFEST = "/tmp/ws_owned_${{ github.run_id }}_${{ github.run_attempt }}.txt"

# THE BANNED CLASS IS "ASK THE DAEMON WHICH CONTAINERS EXIST", not one regex shape.
#
# The first cut matched `docker ps … --filter name=ws-` without a trailing `$`. Review
# walked four real enumerations straight past it — all still delete a concurrent run's
# container — while every test stayed green:
#
#   docker ps --format '{{.Names}}' | grep ws-      (no --filter)
#   docker container ls --filter name=ws            (`container ls`, not `ps`)
#   docker ps -a | awk '/ws-/'                       (no --filter)
#   docker ps --filter name=ws                       (no trailing dash even)
#
# That is the property-vs-shape trap (same one the SCRIPT_RE guard in #4322 fell into):
# a guard that blacklists syntaxes is always one idiom behind. So this guard asserts the
# PROPERTY instead — the workflow does not LIST the daemon at all. It never needs to:
# service containers are known by their own var, and ws-* containers by the manifest.
# Any listing verb is the antipattern, whatever flags spell it.
#
# `docker inspect`/`port`/`logs`/`exec` are BY NAME, not listings, so they are fine.
# `stats`/`top`/`compose ps`/`system df` are also snapshot listers — review reached a
# cross-run sweep through `docker stats … | grep ws-`, so they are banned too.
_DAEMON_ENUMERATION = re.compile(
    r"docker\s+(?:ps|container\s+(?:ls|list|ps)|ls|image\s+ls|images|stats|top"
    r"|compose\s+ps|system\s+df)\b",
    re.IGNORECASE,
)
# A raw query of the docker socket API (`curl --unix-socket …/docker.sock …/containers`)
# is a daemon listing with no `docker` verb at all — it cannot be cleanly linted, and is
# the argument for the #4353 direction (a tiny reviewed teardown as a required check)
# over an ever-growing blacklist. Flagged here only if the exact socket+containers pair
# appears, as a tripwire, not a claim of completeness.
_SOCKET_CONTAINER_QUERY = re.compile(
    r"docker\.sock[^\n]*/containers", re.IGNORECASE,
)
# Removing a container / killing / pruning. Co-locating any of these with an
# enumeration is the enumerate-then-destroy shape #4346 is.
_DESTRUCTIVE_DOCKER = re.compile(
    r"docker\s+(?:rm|kill|stop)\b"
    r"|docker\s+container\s+(?:rm|kill|stop|prune)\b"
    r"|docker\s+(?:system|container|volume|image)\s+prune\b",
    re.IGNORECASE,
)


def _strip_comments(run: str) -> str:
    """Drop `#` comments so the guard reads CODE, not prose. The teardown's own comments
    say "there is no docker ps here" — matching those would be absurd."""
    return "\n".join(re.sub(r"(^|\s)#.*$", "", line) for line in run.splitlines())


def _steps():
    doc = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    for job_name, job in (doc.get("jobs") or {}).items():
        for step in job.get("steps") or []:
            yield job_name, step, (step.get("run") or "")


def test_the_workflow_still_runs_the_e2e_script():
    """Otherwise every assertion below is vacuously true."""
    hits = [s for _, s, run in _steps() if E2E_SCRIPT in run]
    assert hits, (
        f"no step in {WORKFLOW.name} runs {E2E_SCRIPT} — this guard would pass no "
        f"matter how broken the teardown got"
    )
    # Both jobs (stub REQUIRED + real ADVISORY) provision workspaces.
    assert len(hits) >= 2, (
        f"expected the e2e script to run in BOTH the stub and real jobs; found "
        f"{len(hits)}. If a job stopped provisioning, delete this deliberately."
    )


def test_every_e2e_invocation_declares_the_owned_workspace_manifest():
    for job_name, step, run in _steps():
        if E2E_SCRIPT not in run:
            continue
        where = f"{job_name} :: {step.get('name')!r}"
        env = step.get("env") or {}
        assert "E2E_WS_MANIFEST" in env, (
            f"{where} provisions workspaces but does not declare E2E_WS_MANIFEST.\n"
            f"    The e2e script records each workspace id it creates there, and the "
            f"teardown deletes exactly those. Without it the script records nothing, "
            f"teardown finds nothing, and every ws-<uuid> container this job creates "
            f"LEAKS — running forever and pegging CPU on the shared docker-host runner "
            f"(#2883: 13 orphans on ded-1, 11+3 on the prod robots).\n"
            f"    Do NOT 'fix' this by going back to deleting every ws-* container that "
            f"isn't in a start-of-job baseline. That is #4346: it deletes a CONCURRENT "
            f"run's live workspace."
        )
        assert str(env["E2E_WS_MANIFEST"]).strip() == EXPECTED_MANIFEST, (
            f"{where} points E2E_WS_MANIFEST at {env['E2E_WS_MANIFEST']!r}, expected "
            f"{EXPECTED_MANIFEST!r}. The path must be RUN-SCOPED (run_id + run_attempt): "
            f"a shared path means two concurrent runs on the same host write each "
            f"other's ids into one file, and each one's teardown then deletes the "
            f"other's live containers — the very bug this replaced, reintroduced "
            f"through the back door."
        )


def test_the_workflow_never_enumerates_the_daemon():
    """PROPERTY, not shape: no step lists the daemon's containers, in any idiom.

    This is the round-1 review's F1. A guard that blacklisted `docker ps --filter
    name=ws-` was walked past by `docker ps --format | grep ws-`, `docker container
    ls`, and `docker ps -a | awk` — every one still deletes a concurrent run's live
    container, with the guard green. Enumerating is unnecessary here (service
    containers are known by var, ws-* by the manifest), so ANY listing verb is banned
    and there is nothing left to spell around.
    """
    offenders = []
    for job_name, step, run in _steps():
        code = _strip_comments(run)
        m = _DAEMON_ENUMERATION.search(code)
        if m:
            offenders.append(f"{job_name} :: {step.get('name')!r} -> {m.group(0).strip()}")
    assert not offenders, (
        "a step LISTS the docker daemon's containers. On this shared host the list "
        "includes OTHER concurrent runs' containers, and acting on it — deleting "
        "(`docker rm`) or diagnosing (`docker logs $(… | head -1)`) — reaches into a "
        "stranger's run. That is #4346.\n"
        "    This workflow never needs to list: service containers are named by their "
        "own var, ws-* containers come from $E2E_WS_MANIFEST. Delete/inspect BY NAME.\n"
        "    Note the ban is on the listing VERB (ps / container ls / …), not a flag "
        "pattern, precisely so a new idiom cannot slip past it.\n"
        f"    Offenders: {offenders}"
    )


def test_ws_teardown_deletes_by_manifest_name_only():
    """F1b: strengthen 'reads the manifest' from a substring check to the real property.

    The old test passed if the string `MANIFEST=` merely APPEARED in the step — so a
    teardown that read the manifest AND then deleted by daemon enumeration passed. Now:
    the ws teardown must (a) loop over the manifest file, (b) target `docker rm ws-<the
    loop var>`, and (c) contain NO enumeration at all (subsumed by the workflow-wide
    property above, re-asserted here so a failure points at the teardown specifically).
    """
    teardowns = [
        (j, s, _strip_comments(run)) for j, s, run in _steps()
        if _DESTRUCTIVE_DOCKER.search(_strip_comments(run)) and "ws-" in _strip_comments(run)
        and ("MANIFEST" in run or "ws_owned_" in run)
    ]
    assert teardowns, (
        "no ws-* teardown step found that reads the manifest. Either the teardown is "
        "gone (every ws container leaks, #2883) or it no longer keys on the manifest "
        "(it is deleting by some other rule — every other rule tried here deleted a "
        "concurrent run's container, #4346)."
    )
    for job_name, step, code in teardowns:
        where = f"{job_name} :: {step.get('name')!r}"
        assert re.search(r"<\s*[\"']?\$\{?MANIFEST", code) or "done < " in code, (
            f"{where} names the manifest but does not LOOP OVER it (`while read … < "
            f'"$MANIFEST"`). Referencing the variable is not the same as deleting from '
            f"it."
        )
        # POSITIVE SHAPE, ANCHORED — the destroy line must be EXACTLY
        # `docker rm -f "ws-${wsid}"` with nothing but redirects / `|| true` after it.
        #
        # Asserting only that `docker rm -f "ws-${wsid}"` APPEARS is not enough: review
        # slipped a SECOND target onto the same line and stayed green —
        #   docker rm -f "ws-${wsid}" "$OTHER"
        #   OTHERS=$(cat /tmp/ws_owned_*.txt | sed 's/^/ws-/'); docker rm -f "ws-${wsid}" $OTHERS
        # The second reads EVERY run's manifest off the shared /tmp (including concurrent
        # runs') and deletes them — #4346 resurrected through the manifest instead of the
        # daemon. Anchoring the whole command to the single manifest name closes both.
        # The per-line filter IS _DESTRUCTIVE_DOCKER — the single source of "a destructive
        # docker line" — never a hand-typed re-spelling of it. A copy is always one idiom
        # behind: review slipped a `docker container rm -f "$OTHER"` line past a narrower
        # `docker rm` filter once already, and a `docker system prune` (targetless, so it
        # sweeps EVERY concurrent run's stopped containers off the shared daemon) would
        # slip past a copy that lists only rm/kill/stop. Selecting with the canonical
        # pattern holds every destructive form — prune included — to the anchored own-name
        # shape below, and prune (which takes no own-name target) is thereby forbidden here.
        destroy_lines = [
            ln.strip() for ln in code.splitlines()
            if _DESTRUCTIVE_DOCKER.search(ln)
        ]
        assert destroy_lines, f"{where}: no destroy line found in the teardown step"
        allowed = re.compile(
            r'^docker\s+(?:container\s+)?rm\s+-f\s+"ws-\$\{?wsid\}?"'  # the one manifest loop var
            r'(?:\s+>\s*\S+|\s+2>&1|\s+\|\|\s+true)*\s*$',  # only redirects / `|| true`
            re.IGNORECASE,
        )
        for ln in destroy_lines:
            assert allowed.match(ln), (
                f"{where}: teardown destroy line is not EXACTLY `docker rm -f "
                f'"ws-${{wsid}}"` (+ redirects/`|| true`):\n        {ln}\n'
                f"    A trailing argument or a second target lets the teardown delete "
                f"something other than the one manifest id — review used exactly that to "
                f"delete concurrent runs' containers via a `cat ws_owned_*` glob. Delete "
                f"strictly the single name the manifest loop yielded, nothing beside it."
            )
        # Defense-in-depth on the (b2) glob itself: the ONLY manifest this step may read
        # is the run-scoped one. A `ws_owned_*` wildcard, or any ws_owned_ path not
        # immediately followed by the run_id/run_attempt template, reaches other runs.
        for m in re.finditer(r"ws_owned_(.{0,25})", code):
            assert m.group(1).startswith("${{ github.run_id"), (
                f"{where} references a manifest path `ws_owned_{m.group(1)}` that is not "
                f"the run-scoped `ws_owned_${{{{ github.run_id }}}}_${{{{ github.run_attempt "
                f"}}}}.txt`. A glob or a bare prefix reads OTHER concurrent runs' manifests "
                f"— deleting their live containers is #4346."
            )
        assert not _DAEMON_ENUMERATION.search(code), (
            f"{where} enumerates the daemon inside the teardown. It must delete strictly "
            f"by manifest name and never list — listing is how the wrong victim gets "
            f"picked (#4346)."
        )
        assert not _SOCKET_CONTAINER_QUERY.search(code), (
            f"{where} queries the docker socket's /containers API — a daemon listing by "
            f"another name. Delete by manifest id only."
        )


# ---------------------------------------------------------------------------
# The other half of the contract. The workflow can be perfect and still leak every
# container, if the SCRIPT never writes an id into the manifest it was handed.
# ---------------------------------------------------------------------------

E2E_DRIVER = ROOT / "tests" / "e2e" / "test_local_provision_lifecycle_e2e.sh"


def test_the_e2e_driver_records_every_workspace_it_provisions():
    """The manifest is the ONLY thing that makes teardown correct, and the script is
    the only thing that fills it. If the script stops appending, the workflow still
    passes its own lint, teardown finds an empty manifest, deletes nothing, and every
    ws-<uuid> container this job creates runs forever on the shared host (#2883).

    That failure is silent — the e2e itself still goes green.
    """
    src = E2E_DRIVER.read_text(encoding="utf-8")

    # Every workspace this script creates comes from exactly one POST.
    posts = re.findall(r'-X\s+POST\s+"\$BASE/workspaces"', src)
    assert len(posts) == 1, (
        f"expected exactly 1 `POST $BASE/workspaces` in {E2E_DRIVER.name}, found "
        f"{len(posts)}. Each one provisions a container that MUST be recorded in the "
        f"manifest — if a second provisioning call was added, append its id too and "
        f"update this count deliberately."
    )

    assert re.search(r'>>\s*"\$E2E_WS_MANIFEST"', src), (
        f"{E2E_DRIVER.name} never appends to $E2E_WS_MANIFEST.\n"
        f"    The workflow hands it a manifest path and its teardown deletes exactly the "
        f"ids it finds there. If the script records nothing, teardown deletes nothing, "
        f"and every ws-<uuid> container this job provisions LEAKS — running forever and "
        f"pegging CPU on the shared docker-host runner. The e2e still passes, so nothing "
        f"tells you (#2883, #4346)."
    )

    # ...and it must record the id it actually provisioned, not something else.
    assert re.search(r'echo\s+"\$WSID"\s*>>\s*"\$E2E_WS_MANIFEST"', src), (
        f"{E2E_DRIVER.name} appends to the manifest, but not $WSID — the id that "
        f"`POST /workspaces` returned and that the container is named after (ws-$WSID). "
        f"Recording anything else means teardown deletes the wrong container, or none."
    )


def test_the_driver_never_enumerate_then_destroys():
    """F3: extend the property to the e2e DRIVER, not just the workflow YAML.

    The driver (`tests/e2e/…lifecycle_e2e.sh`) legitimately LISTS ws-* containers for
    READ-ONLY diagnostics (is my own container up? what else is on this host?), so the
    workflow-wide "never enumerate" rule cannot apply to it wholesale. But its only
    DESTRUCTIVE docker op must be on an EXACT own name (`ws-$WSID`), never a target that
    flowed out of an enumeration. A future `docker ps … | xargs docker rm` in the driver
    would re-open #4346 from the other file, and the guard used to scan only the YAML.
    """
    driver = ROOT / E2E_SCRIPT
    text = driver.read_text(encoding="utf-8")
    code = _strip_comments(text)

    # POSITIVE SHAPE: every destroy in the driver must target a LITERAL OWN NAME —
    # `"$(container_name …)"` or a `ws-$WSID` literal — never a variable or a
    # substitution whose value came from a list.
    #
    # A blacklist of enumerate-then-destroy shapes is one-line-scoped and review hopped
    # the two apart:
    #   VICTIMS=$(docker ps -aq --filter name=ws-)   # line 1
    #   docker rm -f $VICTIMS                          # line 5
    # The data-flow is invisible to a per-line regex. But the DESTROY line itself is
    # local and its target is `$VICTIMS`, which is not a literal own name — so assert the
    # target's shape and the multi-line hop is closed no matter how far apart the halves
    # sit, and no matter whether the list came from the daemon, a file, or a glob.
    own_target = re.compile(
        r'^docker\s+(?:container\s+)?(?:rm|kill|stop)\s+(?:-\S+\s+)*'   # verb (+ container) + flags
        r'"(?:\$\(container_name\b[^\n]*\)|ws-\$\{?WSID\}?)"'  # "$(container_name …)" or "ws-$WSID"
        r'(?:\s+>\s*\S+|\s+2>&1|\s+\|\|\s+true)*\s*$',    # only redirects / `|| true`
        re.IGNORECASE,
    )
    # The per-line filter IS _DESTRUCTIVE_DOCKER (single source), so every destructive
    # form — including a targetless `docker … prune` that would sweep a concurrent run's
    # containers off the shared daemon — is held to the own-name shape above.
    offenders = []
    for ln in code.splitlines():
        s2 = ln.strip()
        if _DESTRUCTIVE_DOCKER.search(s2) and not own_target.match(s2):
            offenders.append(s2)
    assert not offenders, (
        f"{E2E_SCRIPT} has a destroy line whose target is not a LITERAL own name "
        f'(`"$(container_name …)"` or `"ws-$WSID"`).\n'
        "    The driver may LIST ws-* read-only (diagnostics), but a destroyer targeting "
        "a variable/substitution can delete whatever a list put in it — a concurrent "
        "run's live container (#4346), and the enumeration may sit lines away where a "
        "per-line regex can't see it. Target the exact own name, nothing else.\n"
        f"    Offenders: {offenders}"
    )
