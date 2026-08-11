#!/usr/bin/env python3
"""pin_provenance — the SSOT for "this pin was written by the pipeline".

WHY THIS EXISTS
---------------
`runtime_image_pins` is what a tenant actually runs, and until now NOTHING
recorded how a row got there. Measured live on 2026-08-09 against both
production control planes (`GET /cp/admin/runtime-image`):

    cp_prod     promoted_by = api-token-<8 hex>
    cp_staging  promoted_by = api-token-<8 hex>

and the two `notes` values, quoted verbatim from that read (one line each):

    cp_prod     prod pinned-promote from staging-proven (core#5092 concierge identity fix); staging e2e HARD GATE green incl. STEP 3d behavioural identity
    cp_staging  staging tenant image registry.moleculesai.app/molecule-ai/molecule-tenant:staging-b5e48f0

`promoted_by` is USELESS as a provenance signal: it is the CP admin token's
identity, and CI and an operator's laptop present the SAME token, so both render
as `api-token-<8 hex>`. The only field that distinguishes them is `notes`, and
it was free prose. The prod row above is a hand-written sentence — that is what a
hand-promote looks like, and nothing anywhere could tell it from a pipeline write.

WHAT THIS ADDS
--------------
A single-line, machine-parseable stamp that ONLY a CI run can mint, prepended to
`notes` by scripts/deploy/advance-staging-tenant-pin.sh:

    [ci-pin-provenance v1 repo=molecule-ai/molecule-core wf=<workflow> run=<id> job=<key> sha=<40hex>] <free text>

The anchor is `run` — a Gitea Actions run id. `wf` and `repo` are carried for a
human reading the row but are NEVER trusted: the workflow comes from
`GET /actions/runs/<run>` and the repository comes from the verifier's own
ambient GITHUB_REPOSITORY. The API is the SSOT for what a run id was; the note
is not.

WHAT THIS ACTUALLY GUARANTEES, AND WHAT IT DOES NOT
---------------------------------------------------
State this precisely, because an overstated provenance guarantee is worse than
an absent one — a reader who believes the stronger claim stops looking.

An earlier draft of this file claimed a stamp "can only cite someone else's
[run], which the digest/sha cross-check then has to survive". NO SUCH CHECK
EXISTED. `head_sha` was read only into a display string. A reviewer disproved
the claim by authoring a commit that cited run 637995 — a real bump run, for a
pin that was not theirs — and it passed. Replay of any historical pin-writer run
id laundered an arbitrary pin.

ENFORCED NOW (each verified against live runs, not asserted):
  * the cited run EXISTS (404 → reject);
  * it belongs to a pin-WRITING workflow (PIN_WRITER_WORKFLOWS), so an
    unrelated green `ci.yml` run is not cover;
  * it is resolved against the verifier's OWN repository, never the stamp's;
  * `bind_stamp_to_run`: the stamp's `sha=` equals that run's `head_sha`, so a
    hand-written stamp must reproduce a value it cannot guess from the run id;
  * SDK arm only — `lint_sdk_pin_provenance` additionally requires the pin
    commit's FIRST PARENT to be that run's head_sha. The bump lane branches
    from the ref it was dispatched on, so this holds for real lane output
    (commit 6688db931c11's sole parent is bc9fa9bff2bf, which is run 637995's
    head_sha) and fails for a commit authored anywhere else. This is what kills
    the reviewer's replay.

NOT ENFORCED — the residual, stated plainly:
  * An author who bases their pin edit on EXACTLY the cited run's head commit
    can still replay that run id. Closing this needs the run to sign something
    only it could produce; there is no signing key in this fleet, and inventing
    one is a larger change than this lane.
  * The tenant-pin AUDIT has no commit to bind, so it gets the first four
    checks and not the parent check. A hand `curl` that copies a stamp verbatim
    from a real CI-written row, to a DIFFERENT digest, is not detected. What is
    detected is every unstamped write, which is what a hand promote has looked
    like every time it has actually happened here.
  * MOST IMPORTANTLY: the tenant pin is DETECTED, NOT PREVENTED. Nothing here
    stops the write. `POST /cp/admin/runtime-image/promote` accepts an unstamped
    body, because the promote dispatcher's `GuardPromote` hook — which exists
    for precisely this purpose, see molecule-controlplane
    internal/handlers/admin_pin_handler.go — is deliberately left unset on
    `runtimeImagePinResource` (internal/handlers/pin_runtime_image.go: "No
    GuardPromote: the molecule-tenant pin promotes through the SAME ungated
    authenticated-admin path…"). The SDK half IS prevented, because its edit is
    a commit and the guard runs on the diff. Anyone reasoning about this
    mechanism must hold those two halves apart.

WHY A RUN ID AND NOT AN IDENTITY
--------------------------------
Identity was tried first and does not work. A dedicated promote identity would
still be a bearer token handed to a job — and the SAME token an operator uses,
which is why both control planes render every writer as one `api-token-<8 hex>`.
A run id is different in kind: it is minted by infrastructure the writer does
not control, and the checks above make it costly rather than free to reuse.

FAIL-CLOSED MINTING. `mint` requires GITHUB_RUN_ID. Outside Actions there is no
run id, so there is no stamp, so advance-staging-tenant-pin.sh refuses to
promote. That is the mechanism that makes the hand path impossible through the
blessed script rather than merely discouraged. There is deliberately NO bypass
env var: a bypass flag is prose with an `if` around it.

GRANDFATHERING: DERIVED FROM THE TREE, NOT A SNAPSHOT AND NOT A CONSTANT
------------------------------------------------------------------------
Rows written before the stamping code existed cannot carry a stamp, and failing
on them would fail every run forever with no way to fix it — the "gate that
punishes the fix" shape. A row is therefore grandfathered iff

    promoted_at < the commit date of THIS FILE in the checkout under test

i.e. the instant the mechanism landed, computed at run time by
`mechanism_landed_at()`. Nothing is hardcoded and nothing needs maintaining: the
window closes by itself the moment the writers start stamping, and every write
after that instant must carry a stamp.

WHY NOT THE DIGEST SNAPSHOT THIS FILE SHIPPED FIRST. The first version
grandfathered two digests read live on 2026-08-09. That was wrong, and CI proved
it before merge: dispatch 643926 went RED on

    [cp_staging] UNSTAMPED pin sha256:8027955a4ad7… promoted_at
    '2026-08-11T03:50:52.425711Z' notes='staging tenant image
    registry.moleculesai.app/molecule-ai/molecule-tenant:staging-492e031'

That row is staging-tenant-cd's OWN write, in the old script's exact machine
format — a legitimate pipeline write made after the snapshot and before this
change landed. A snapshot cannot know about writes the pipeline makes while the
change is in review, so merging would have reddened main for CI's own work. That
is "arming a signal before it is fed": it converts a missed detection into an
invented one.

WHY `promoted_at` IS TRUSTWORTHY HERE. It is not caller-supplied. The upsert sets
`promoted_at = now()` server-side (molecule-controlplane
internal/handlers/pin_runtime_image.go), so a hand promote cannot backdate itself
into the grandfathered window through the API.

WHY NOT A HARDCODED CUTOVER DATE. Because it would have to be guessed before the
merge date is known, and a guess that lands early red-flags the pipeline while a
guess that lands late blesses a real hand write. The file's own commit date is
the one value that is exactly right by construction.

Exit codes
----------
  0 — clean (or, for `audit`, only grandfathered/undetermined findings)
  1 — a violation the operator must act on
  2 — usage / unreadable input
  3 — `mint` invoked outside a CI run (no GITHUB_RUN_ID)
"""

from __future__ import annotations

import argparse
import datetime as _dt
import json
import os
import re
import subprocess
import sys
import urllib.error
import urllib.request

STAMP_VERSION = "v1"
STAMP_OPEN = "[ci-pin-provenance"
DEFAULT_REPO = "molecule-ai/molecule-core"
DEFAULT_GITEA = "https://git.moleculesai.app"

# The workflow files allowed to write a pin. Anything else citing a run id of a
# workflow outside this set is a violation even if the run genuinely exists —
# otherwise any green run in the repo could be cited as cover.
PIN_WRITER_WORKFLOWS = frozenset(
    {
        "staging-tenant-cd.yml",
        "promote-prod-tenant-pin.yml",
    }
)

def mechanism_landed_at(repo_root: str = ".") -> _dt.datetime | None:
    """When the stamping mechanism landed, per the checkout under test.

    Read from git rather than hardcoded, so the grandfathering window closes on
    its own and cannot drift from reality. Returns None when it cannot be
    determined; the caller must NOT treat that as "grandfather everything".
    """
    # A SHALLOW REPOSITORY CANNOT ANSWER THIS QUESTION, and must not be allowed
    # to answer it wrongly.
    #
    # `--diff-filter=A` finds the commit that ADDED the file. On a truncated
    # history there is no such commit, so git reports the oldest commit it can
    # see — the shallow boundary — as the add. That silently moves the
    # grandfathering boundary FORWARD to the checkout's tip date and excuses
    # every hand write in between. Reproduced end to end on identical fixtures,
    # one full clone and one `--depth 1`, against a row with NO stamp promoted
    # 15 days after the mechanism landed:
    #
    #   full     landed_at 2026-08-10T21:16:34  ->  UNSTAMPED,     violations: 1, exit 1
    #   shallow  landed_at 2026-09-05T12:00:00  ->  GRANDFATHERED, violations: 0, exit 0
    #
    # `checked: 1` prints in BOTH, so the non-vacuity counter does not reveal
    # it. This is checked FIRST, before the log query, because the log query
    # succeeds — it returns a plausible, wrong answer, which is worse than an
    # error.
    #
    # WHY FAIL CLOSED HERE RATHER THAN PIN `fetch-depth: 0` SOMEWHERE. The
    # guard's own header spends eleven lines on how painful a full fetch is on
    # the tailnet runners and already carries `filter: blob:none` for that
    # reason; the next person optimising that step is exactly who would shallow
    # it. A lint asserting the workflow's config would be one edit away from
    # being wrong. Returning None routes into the existing "NOTHING is
    # grandfathered" branch, which is loud, already tested, and survives someone
    # changing the workflow.
    try:
        shallow = subprocess.run(
            ["git", "rev-parse", "--is-shallow-repository"],
            cwd=repo_root,
            capture_output=True,
            text=True,
            check=False,
            timeout=30,
        )
    except Exception:  # noqa: BLE001
        return None
    if shallow.returncode != 0 or (shallow.stdout or "").strip().lower() != "false":
        # "true", or a git too old to answer, or no repo at all. None of those
        # is a history this function may draw a boundary from.
        return None

    # `--diff-filter=A` + last line = the commit that INTRODUCED this file, not
    # the one that last touched it. That distinction is load-bearing: keyed on
    # the LAST touch, any future edit to this file would slide the boundary
    # forward and silently re-grandfather every hand write made in between. The
    # introduction date is fixed for the life of the branch history.
    try:
        out = subprocess.run(
            ["git", "log", "--diff-filter=A", "--format=%cI", "--", _SELF_REL],
            cwd=repo_root,
            capture_output=True,
            text=True,
            check=False,
            timeout=30,
        )
    except Exception:  # noqa: BLE001 — no git, no worktree, no answer
        return None
    if out.returncode != 0:
        return None
    lines = [l.strip() for l in (out.stdout or "").splitlines() if l.strip()]
    if not lines:
        return None
    try:
        return _dt.datetime.fromisoformat(lines[-1])
    except ValueError:
        return None


_SELF_REL = ".gitea/scripts/pin_provenance.py"


def parse_promoted_at(value: str | None) -> _dt.datetime | None:
    """Parse the CP's `promoted_at` (RFC3339, variable fractional digits)."""
    raw = (value or "").strip()
    if not raw:
        return None
    raw = raw.replace("Z", "+00:00")
    # Postgres emits 1-6 fractional digits; fromisoformat wants at most 6.
    m = re.match(r"^(.*?\.\d{1,6})\d*(\+\d{2}:\d{2}|-\d{2}:\d{2})?$", raw)
    if m:
        raw = m.group(1) + (m.group(2) or "+00:00")
    try:
        d = _dt.datetime.fromisoformat(raw)
    except ValueError:
        return None
    return d if d.tzinfo else d.replace(tzinfo=_dt.timezone.utc)


_UNSET = object()

_STAMP_RE = re.compile(
    r"\[ci-pin-provenance\s+(?P<version>v\d+)\s+(?P<fields>[^\]]*)\]"
)
_FIELD_RE = re.compile(r"(?P<key>[a-z_]+)=(?P<value>\S+)")
_RUN_RE = re.compile(r"^\d{1,12}$")
_SHA_RE = re.compile(r"^[0-9a-f]{7,40}$")
_REPO_RE = re.compile(r"^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$")


class StampError(ValueError):
    """A notes string carries no usable provenance stamp."""


# --------------------------------------------------------------------------
# minting
# --------------------------------------------------------------------------


def mint(env: dict[str, str] | None = None) -> str:
    """Build the stamp from the ambient Actions environment.

    GITHUB_RUN_ID is the ONLY mandatory field and the only one that cannot be
    reconstructed later. Everything else degrades to `-`: `wf` is re-derived
    from the run id at audit time anyway, and a stamp that refused to mint
    because some cosmetic variable was unset would take staging CD down for a
    field nothing verifies.
    """
    env = os.environ if env is None else env
    run_id = (env.get("GITHUB_RUN_ID") or "").strip()
    if not run_id:
        raise StampError(
            "GITHUB_RUN_ID is unset — there is no CI run to attribute this pin "
            "write to. Pin writes must go through the pipeline "
            "(.gitea/workflows/staging-tenant-cd.yml for staging, "
            ".gitea/workflows/promote-prod-tenant-pin.yml for production); a "
            "hand-run promote cannot mint provenance and is refused."
        )
    if not _RUN_RE.match(run_id):
        raise StampError(f"GITHUB_RUN_ID {run_id!r} is not a run id")

    def clean(name: str, pattern: re.Pattern[str] | None = None) -> str:
        v = (env.get(name) or "").strip()
        # Whitespace and `]` would break the single-line grammar.
        v = re.sub(r"[\s\]]+", "", v)
        if not v:
            return "-"
        if pattern is not None and not pattern.match(v):
            return "-"
        return v

    repo = clean("GITHUB_REPOSITORY", _REPO_RE)
    workflow = clean("GITHUB_WORKFLOW")
    job = clean("GITHUB_JOB")
    sha = clean("GITHUB_SHA", _SHA_RE)
    return (
        f"{STAMP_OPEN} {STAMP_VERSION} repo={repo} wf={workflow} "
        f"run={run_id} job={job} sha={sha}]"
    )


def stamp_notes(free_text: str, env: dict[str, str] | None = None, limit: int = 500) -> str:
    """Prepend a freshly minted stamp to `free_text`, bounded to `limit`.

    The CP validator rejects notes longer than 500 chars
    (molecule-controlplane internal/handlers/pin_runtime_image.go:
    "notes must be <= 500 chars"), and a rejected promote is a failed deploy.
    The STAMP is what must survive truncation, so the free text is trimmed, not
    the stamp.
    """
    s = mint(env)
    if len(s) >= limit:
        # Cannot happen with real values; refuse rather than emit a half stamp.
        raise StampError(f"stamp alone exceeds the {limit}-char notes budget")
    room = limit - len(s) - 1
    text = (free_text or "").strip()
    if len(text) > room:
        text = text[: max(room - 1, 0)].rstrip() + "…"
    return f"{s} {text}".strip()


# --------------------------------------------------------------------------
# parsing
# --------------------------------------------------------------------------


def parse(notes: str) -> dict[str, str]:
    """Extract the stamp fields from a notes string. Raises StampError."""
    if not notes:
        raise StampError("notes is empty — no provenance stamp")
    m = _STAMP_RE.search(notes)
    if not m:
        raise StampError(
            "no `[ci-pin-provenance …]` stamp in notes — this row was not "
            "written by the pipeline"
        )
    if m.group("version") != STAMP_VERSION:
        raise StampError(
            f"stamp version {m.group('version')} is not {STAMP_VERSION}"
        )
    fields = {fm.group("key"): fm.group("value") for fm in _FIELD_RE.finditer(m.group("fields"))}
    run = fields.get("run", "")
    if not _RUN_RE.match(run):
        raise StampError(f"stamp carries no usable run id (run={run!r})")
    repo = fields.get("repo", "")
    if repo == "-":
        # mint degrades an unset GITHUB_REPOSITORY to `-` rather than refusing:
        # the run id is the anchor, and a pin write must not fail over a field
        # the verifier can supply itself. resolve_repo() fills it in.
        pass
    elif not _REPO_RE.match(repo):
        raise StampError(f"stamp carries no usable repo (repo={repo!r})")
    return fields


def expected_repo(default: str = DEFAULT_REPO) -> str:
    """The repository this verifier is allowed to resolve run ids against.

    Taken from the AMBIENT environment, never from the stamp. See resolve_repo.
    """
    env_repo = (os.environ.get("GITHUB_REPOSITORY") or "").strip()
    return env_repo if _REPO_RE.match(env_repo) else default


def resolve_repo(fields: dict[str, str], expected: str | None = None) -> str:
    """The repo to query for the cited run — and it must be the EXPECTED one.

    This used to return the stamp's own `repo=` verbatim, which made the
    docstring's "a run that EXISTS, in THIS repo" false: a stamp reading
    `repo=attacker/evil-repo` sent the verifier to look up the run id THERE.
    It fails closed for a repo that does not exist, so it degraded detection
    rather than granting anything — but a provenance check that lets the thing
    being checked choose its own authority is not a check.

    The expected repo comes from the ambient environment (GITHUB_REPOSITORY),
    which the audited note cannot influence. A stamp naming a different repo is
    now an error, not a redirect. `-` is accepted because mint deliberately
    degrades an unset GITHUB_REPOSITORY to `-`: a pin write must not fail over
    a cosmetic field, and the verifier supplies the value itself.
    """
    exp = expected or expected_repo()
    repo = fields.get("repo", "")
    if repo == "-" or not repo:
        return exp
    if repo != exp:
        raise StampError(
            f"stamp names repo {repo!r}, but this verifier only resolves run "
            f"ids against {exp!r}. A stamp does not get to choose which "
            f"repository's Actions API vouches for it."
        )
    return exp


# --------------------------------------------------------------------------
# online verification
# --------------------------------------------------------------------------


class Undetermined(RuntimeError):
    """The Actions API could not answer. NOT a pass and NOT a fail."""


def normalize_base(url: str) -> str:
    """Accept `git.example.com` as well as `https://git.example.com`.

    GITEA_HOST is spelled BOTH ways in this fleet — sdk-pin-drift.yml exports
    `https://git.moleculesai.app`, the operator credential file exports a bare
    hostname. urllib raises `unknown url type` on the bare form, which would
    surface as an Undetermined (i.e. a soft accept) rather than as the config
    error it is.
    """
    u = (url or "").strip().rstrip("/")
    if not u:
        return DEFAULT_GITEA
    if not u.startswith(("http://", "https://")):
        u = "https://" + u
    return u


def fetch_run(run_id: str, repo: str, gitea_url: str, token: str | None):
    url = f"{normalize_base(gitea_url)}/api/v1/repos/{repo}/actions/runs/{run_id}"
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    if token:
        req.add_header("Authorization", f"token {token}")
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            return None  # a DEFINITE answer: that run does not exist
        raise Undetermined(f"HTTP {exc.code} reading run {run_id}") from exc
    except Exception as exc:  # noqa: BLE001 — network/DNS/JSON all mean "unread"
        raise Undetermined(f"{type(exc).__name__} reading run {run_id}") from exc


def workflow_file_of(run: dict) -> str:
    """`path` is `<file>.yml@refs/heads/<branch>`; return just the file."""
    return (run.get("path") or "").split("@", 1)[0].strip()


def bind_stamp_to_run(fields: dict[str, str], run: dict) -> None:
    """The stamp's `sha=` must be the run's OWN head sha.

    WHAT THIS CLOSES. Without it the only thing tying a stamp to a run is the
    run id, which is a small integer anyone can type. `sha=` is minted from
    GITHUB_SHA, which for both writer lanes is exactly the run's head_sha
    (staging-tenant-cd is a push run; promote-prod-tenant-pin is a dispatch on
    a branch). Verified against real data: run 637995 reports
    head_sha=bc9fa9bff2bf9c4e952ee309845a1a1ee7c66c4f and the stamp it minted
    carries sha=bc9fa9bff2bf9c4e952ee309845a1a1ee7c66c4f.

    So a hand-written stamp must now name a run AND reproduce that run's head
    sha. It does not stop replay of a genuine (run id, sha) PAIR — see the
    module docstring, which states that residual instead of implying it is
    covered.
    """
    stamp_sha = (fields.get("sha") or "").lower()
    run_sha = (run.get("head_sha") or "").lower()
    if stamp_sha in ("", "-"):
        raise StampError(
            f"stamp cites run {fields['run']} but carries no sha=, so nothing "
            f"ties the two together. Only a stamp minted inside the run has one."
        )
    if not run_sha:
        raise Undetermined(
            f"run {fields['run']} reports no head_sha; cannot bind the stamp to it"
        )
    # The stamp carries the full 40-hex sha; compare on the shorter of the two
    # so a future abbreviated form does not silently start failing everything.
    n = min(len(stamp_sha), len(run_sha))
    if n < 7 or stamp_sha[:n] != run_sha[:n]:
        raise StampError(
            f"stamp cites run {fields['run']}, whose head sha is {run_sha[:12]}, "
            f"but the stamp says sha={stamp_sha[:12]}. The stamp was not minted "
            f"by that run."
        )


def verify_run(
    fields: dict[str, str],
    gitea_url: str,
    token: str | None,
    expected: str | None = None,
) -> str:
    """Confirm the cited run exists, may write pins, and minted THIS stamp.

    Returns a human-readable confirmation. Raises StampError on a DEFINITE
    negative (run absent, a run from a workflow that may not write pins, a repo
    the verifier does not answer for, or a stamp whose sha is not that run's)
    and Undetermined when the API could not be read — an unread signal must
    never be laundered into either verdict.
    """
    repo = resolve_repo(fields, expected)
    run = fetch_run(fields["run"], repo, gitea_url, token)
    if run is None:
        raise StampError(
            f"stamp cites run {fields['run']} in {repo}, which does "
            f"NOT exist. A fabricated run id is a hand-written pin wearing a "
            f"pipeline's name."
        )
    wf = workflow_file_of(run)
    if wf not in PIN_WRITER_WORKFLOWS:
        raise StampError(
            f"stamp cites run {fields['run']}, but that run is {wf!r}, which is "
            f"not a pin-writing workflow ({', '.join(sorted(PIN_WRITER_WORKFLOWS))}). "
            f"Citing an unrelated green run does not make a hand promote a CI promote."
        )
    bind_stamp_to_run(fields, run)
    return (
        f"run {fields['run']} = {wf} @ {(run.get('head_sha') or '')[:12]} "
        f"(event={run.get('event')}, actor={(run.get('actor') or {}).get('login')})"
    )


# --------------------------------------------------------------------------
# audit
# --------------------------------------------------------------------------

TEMPLATE_NAME = "molecule-tenant"
REGION = "global"


# How long after the mechanism is introduced the PRE-PROVENANCE WRITER FORMAT is
# still accepted. This exists for one specific race and expires on its own.
#
# THE RACE. `promoted_at < introduction` covers every row written before this
# file existed. It does NOT cover a row staging-tenant-cd writes AFTER that
# commit and BEFORE this change merges — and staging-tenant-cd runs on every
# merge to main, so during review that is likely, not hypothetical. Without this
# allowance the first push-to-main audit after merge would go red on the
# pipeline's own unstamped write. That is the same "invented false positive"
# this file already learned once, at dispatch 643926.
LEGACY_FORMAT_GRACE = _dt.timedelta(days=14)

# The EXACT note the pre-provenance writer generated, as it appears live:
#   staging tenant image registry.moleculesai.app/molecule-ai/molecule-tenant:staging-492e031
# Machine-shaped, and checked for INTERNAL CONSISTENCY rather than merely
# matched: the tag's sha7 must be the row's own git_sha. A hand promote wanting
# this exemption would have to reproduce the format AND a matching git_sha AND
# land inside the grace window.
_LEGACY_NOTE_RE = re.compile(
    r"^(?:staging )?tenant image\s+(?P<ref>\S+):staging-(?P<sha7>[0-9a-f]{7})$"
)


def is_legacy_writer_note(notes: str, git_sha: str | None) -> bool:
    m = _LEGACY_NOTE_RE.match((notes or "").strip())
    if not m:
        return False
    return (git_sha or "").lower().startswith(m.group("sha7").lower())


def rows_of(doc: object) -> list[dict]:
    if isinstance(doc, list):
        return [r for r in doc if isinstance(r, dict)]
    d = doc if isinstance(doc, dict) else {}
    for key in ("pins", "data", "items"):
        v = d.get(key)
        if isinstance(v, list):
            return [r for r in v if isinstance(r, dict)]
    return []


def audit(
    endpoints: list[tuple[str, str]],
    gitea_url: str,
    token: str | None,
    online: bool,
    out=sys.stdout,
    landed_at: "_dt.datetime | None | object" = _UNSET,
) -> int:
    """Audit the molecule-tenant pin on every endpoint. Returns an exit code."""
    checked = 0
    stamped = 0
    grandfathered = 0
    undetermined = 0
    violations: list[str] = []
    lines: list[str] = ["## molecule-tenant pin provenance audit", ""]

    if landed_at is _UNSET:
        landed_at = mechanism_landed_at()
    if landed_at is None:
        # NOT "grandfather everything". An unknown landing instant means the
        # audit cannot tell a pre-mechanism row from a hand write, and the safe
        # reading of that is the strict one: nothing is excused. Said out loud
        # so a red here is diagnosable rather than mysterious.
        lines.append(
            "> The mechanism's landing instant could not be read from git, so "
            "NOTHING is grandfathered on this run. A row that predates the "
            "stamping code will be reported as unstamped."
        )
        lines.append("")
    else:
        lines.append(
            f"> Stamping landed {landed_at.isoformat()}; rows promoted before "
            f"that instant are grandfathered (they could not have been stamped)."
        )
        lines.append("")

    for label, path in endpoints:
        try:
            with open(path, encoding="utf-8") as fh:
                doc = json.load(fh)
        except Exception as exc:  # noqa: BLE001
            # An unreadable endpoint document is a violation, not a skip: the
            # whole point is that "we could not look" must never render the
            # same as "we looked and it was fine".
            violations.append(f"[{label}] pin document {path} is unreadable ({exc})")
            lines.append(f"* **{label}** — UNREADABLE ({exc})")
            continue
        row = next(
            (
                r
                for r in rows_of(doc)
                if r.get("template_name") == TEMPLATE_NAME
                and (r.get("region") or REGION) == REGION
            ),
            None,
        )
        if row is None:
            violations.append(
                f"[{label}] no ({TEMPLATE_NAME}, {REGION}) row — an absent pin "
                f"must not render as a clean audit"
            )
            lines.append(f"* **{label}** — NO PIN ROW")
            continue

        checked += 1
        digest = (row.get("image_digest") or "").strip()
        notes = row.get("notes") or ""
        promoted_at = parse_promoted_at(row.get("promoted_at"))
        try:
            fields = parse(notes)
        except StampError as exc:
            if (
                landed_at is not None
                and promoted_at is not None
                and promoted_at < landed_at
            ):
                grandfathered += 1
                lines.append(
                    f"* **{label}** — GRANDFATHERED: promoted "
                    f"{promoted_at.isoformat()}, before the stamping mechanism "
                    f"landed {landed_at.isoformat()}, so it could not have been "
                    f"stamped. The next write to this control plane must be."
                )
                continue
            if (
                landed_at is not None
                and promoted_at is not None
                and promoted_at < landed_at + LEGACY_FORMAT_GRACE
                and is_legacy_writer_note(notes, row.get("git_sha"))
            ):
                grandfathered += 1
                lines.append(
                    f"* **{label}** — GRANDFATHERED: the PRE-PROVENANCE WRITER "
                    f"format, internally consistent (tag sha7 matches git_sha "
                    f"{(row.get('git_sha') or '')[:7]}), promoted "
                    f"{promoted_at.isoformat()} inside the "
                    f"{LEGACY_FORMAT_GRACE.days}-day grace after the mechanism "
                    f"landed. This is the pipeline's own write from before the "
                    f"stamping code shipped; the allowance expires "
                    f"{(landed_at + LEGACY_FORMAT_GRACE).isoformat()}."
                )
                continue
            violations.append(
                f"[{label}] UNSTAMPED pin {digest} — {exc}. "
                f"promoted_by={row.get('promoted_by')!r} "
                f"promoted_at={row.get('promoted_at')!r} notes={notes[:120]!r}"
            )
            lines.append(f"* **{label}** — **UNSTAMPED** `{digest[:19]}…` — {exc}")
            continue

        detail = f"run {fields['run']}"
        if online:
            try:
                detail = verify_run(fields, gitea_url, token)
            except StampError as exc:
                violations.append(f"[{label}] {exc}")
                lines.append(f"* **{label}** — **FORGED STAMP** — {exc}")
                continue
            except Undetermined as exc:
                undetermined += 1
                lines.append(
                    f"* **{label}** — stamped (run {fields['run']}), run "
                    f"UNVERIFIED: {exc}. Structure is valid; the citation could "
                    f"not be checked against the Actions API."
                )
                stamped += 1
                continue
        stamped += 1
        lines.append(f"* **{label}** — stamped, {detail}")

    lines.append("")
    lines.append(
        f"checked: {checked}  stamped: {stamped}  grandfathered: {grandfathered}  "
        f"undetermined: {undetermined}  violations: {len(violations)}"
    )
    if checked == 0:
        lines.append("")
        lines.append(
            "**checked: 0** — this audit inspected NO pin row. That is a broken "
            "audit, not a clean one; failing."
        )
    print("\n".join(lines), file=out)
    for v in violations:
        print(f"::error::pin provenance: {v}", file=sys.stderr)
    if violations or checked == 0:
        return 1
    return 0


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description="pin provenance stamp: mint / parse / audit")
    sub = ap.add_subparsers(dest="cmd", required=True)

    m = sub.add_parser("mint", help="print a stamp built from the Actions env")
    m.add_argument("--notes", default="", help="free text to append after the stamp")
    m.add_argument("--limit", type=int, default=500)

    p = sub.add_parser("parse", help="parse a stamp out of a notes string")
    p.add_argument("notes")

    a = sub.add_parser("audit", help="audit live pin documents for provenance")
    a.add_argument("endpoints", nargs="+", metavar="label=pins.json")
    a.add_argument("--gitea-url", default=os.environ.get("GITEA_HOST", DEFAULT_GITEA))
    a.add_argument("--token-env", default="PIN_PROVENANCE_TOKEN")
    a.add_argument(
        "--offline",
        action="store_true",
        help="skip the Actions-API citation check (structure only)",
    )

    args = ap.parse_args(argv)

    if args.cmd == "mint":
        try:
            sys.stdout.write(
                stamp_notes(args.notes, limit=args.limit) if args.notes else mint()
            )
        except StampError as exc:
            print(f"::error::{exc}", file=sys.stderr)
            return 3
        return 0

    if args.cmd == "parse":
        try:
            fields = parse(args.notes)
        except StampError as exc:
            print(f"::error::{exc}", file=sys.stderr)
            return 1
        print(json.dumps(fields, sort_keys=True))
        return 0

    endpoints: list[tuple[str, str]] = []
    for spec in args.endpoints:
        if "=" not in spec:
            print(f"::error::endpoint {spec!r} must be <label>=<pins.json>", file=sys.stderr)
            return 2
        label, path = spec.split("=", 1)
        endpoints.append((label.strip(), path.strip()))
    token = os.environ.get(args.token_env) or None
    return audit(
        endpoints,
        gitea_url=args.gitea_url,
        token=token,
        online=not args.offline,
    )


if __name__ == "__main__":
    sys.exit(main())
