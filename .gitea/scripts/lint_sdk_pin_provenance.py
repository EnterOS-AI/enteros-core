#!/usr/bin/env python3
"""lint_sdk_pin_provenance — the SDK pin may only move through the pipeline.

THE HAND PATH THIS CLOSES
-------------------------
The SDK consumer pin is four checked-in files, so "bump it by hand" is just a
commit. The repository even documents how, in two places:

  .gitea/scripts/check_sdk_pin_drift.py, the STUCK remediation text:
      Manual equivalent, from workspace-server/:
        go get go.moleculesai.app/sdk/gen/go@<head_sha>
        go generate ./...
        go test ./internal/providers -run TestSyncedYAMLMatchesCanonicalSHA
        # paste the observed sha into canonicalRegistrySHA256

  workspace-server/internal/providers/sync_canonical_test.go, in the failure
  message a developer reads at the moment they are already editing the file.

That path has been taken for real: on 2026-08-05 sdk-pin-bump run 620174 went
green in 19s having done nothing (the credential was unprovisioned and the lane
warn-skipped), and the pin was bumped by hand four minutes later in 45a61bb.
sdk-pin-bump.yml now fails closed instead of green-skipping, but nothing stopped
the hand commit itself — a convention is not a mechanism.

WHAT IS ENFORCED
----------------
If a diff moves the SDK pin, the commit that moved it must be attributable to
the `sdk-pin-bump` lane. Two forms are accepted:

  FORM A (current). The commit carries a `Pin-Provenance:` trailer holding a
  `[ci-pin-provenance v1 … run=<id> …]` stamp, and that run resolves — through
  the Gitea Actions API, not through the note — to a real `sdk-pin-bump.yml`
  run in this repository. Minting the stamp requires GITHUB_RUN_ID, so it can
  only come from inside a run.

  FORM B (legacy, closing). Author AND committer are the bump bot, the subject
  is the lane's exact `chore(sdk): bump sdk/gen/go pin to <sha12>` and the
  branch is `bump/sdk-<sha12>`. Accepted ONLY for commits authored before
  LEGACY_CUTOFF, because the lane did not emit the trailer before then. PR
  #5094 (head e601732bea7f, authored 2026-08-07T18:42:19Z by
  molecule-sdk-pin-bot) is a real, in-flight, CI-produced bump that predates
  the trailer; failing it would be a gate red-flagging the very lane it exists
  to protect.

A NOTE ON WHAT FORM B IS WORTH. `git commit --author=` forges an author line in
one flag, so Form B is a name check, not a proof. That is precisely why it is
time-boxed rather than kept as a permanent alternative: every commit after the
cutoff must carry a run id that exists.

WHY NOT JUST "the branch is bump/sdk-*"
---------------------------------------
Because that is the check the author of a hand bump would sail through without
noticing. The failure this guards is not malice; it is a developer mid-task
running the four commands the failure message just printed at them. The guard
must fire on THAT commit, on THEIR branch, in THEIR PR — which means keying on
the pin edit, not on the branch.

`checked` is the number of pin-carrying files examined, and it is a constant
(len(PIN_FILES)) on every run — including runs where nothing changed. A guard
whose reported count drops to 0 when it has nothing to do is indistinguishable
from a guard that stopped looking.

Exit codes
----------
  0 — no unattributed pin edit
  1 — a pin edit with no acceptable provenance
  2 — usage / git error
"""

from __future__ import annotations

import argparse
import datetime as _dt
import os
import re
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import pin_provenance  # noqa: E402

# Every checked-in file that carries part of the SDK pin. Sourced from the
# `git add` line in .gitea/workflows/sdk-pin-bump.yml — the lane's own
# definition of what a bump touches.
PIN_FILES = (
    "workspace-server/go.mod",
    "workspace-server/go.sum",
    "workspace-server/internal/providers/gen/registry_gen.go",
    "workspace-server/internal/providers/sync_canonical_test.go",
)

# go.mod and go.sum move for reasons unrelated to the SDK. Only a change to a
# line naming the SDK module counts as a pin edit; a `go mod tidy` that reorders
# an unrelated require block must not be treated as one.
SDK_MODULE = "go.moleculesai.app/sdk/gen/go"
CANONICAL_CONST = "canonicalRegistrySHA256"

BUMP_BOT_NAMES = frozenset({"molecule-sdk-pin-bot"})
BUMP_BOT_EMAILS = frozenset({"sdk-pin-bot@moleculesai.app"})
LEGACY_SUBJECT_RE = re.compile(r"^chore\(sdk\): bump sdk/gen/go pin to [0-9a-f]{12}$")
LEGACY_BRANCH_RE = re.compile(r"^bump/sdk-[0-9a-f]{12}(-onto-[A-Za-z0-9._-]+)?$")
TRAILER_RE = re.compile(r"^Pin-Provenance:\s*(?P<stamp>\[ci-pin-provenance.*\])\s*$", re.M)

# Commits authored strictly before this instant may use FORM B. Chosen as the
# moment the trailer landed in sdk-pin-bump.yml; nothing the lane produces after
# it lacks a trailer, so nothing after it needs the weaker form.
LEGACY_CUTOFF = _dt.datetime(2026, 8, 9, 0, 0, 0, tzinfo=_dt.timezone.utc)

BUMP_WORKFLOWS = frozenset({"sdk-pin-bump.yml"})


def git(*args: str, cwd: str | None = None) -> str:
    proc = subprocess.run(
        ["git", *args],
        cwd=cwd,
        capture_output=True,
        text=True,
        check=False,
    )
    if proc.returncode != 0:
        raise RuntimeError(f"git {' '.join(args)} failed: {proc.stderr.strip()}")
    return proc.stdout


def sdk_pin_edit_in(diff: str) -> bool:
    """True when a unified diff contains an added/removed SDK-pin line."""
    for line in diff.splitlines():
        if not line or line[0] not in "+-":
            continue
        if line.startswith(("+++", "---")):
            continue
        if SDK_MODULE in line or CANONICAL_CONST in line:
            return True
    return False


def commits_touching_pin(base: str, head: str, cwd: str | None) -> list[str]:
    out = git(
        "log",
        "--format=%H",
        f"{base}..{head}",
        "--",
        *PIN_FILES,
        cwd=cwd,
    )
    return [l.strip() for l in out.splitlines() if l.strip()]


def commit_meta(sha: str, cwd: str | None) -> dict:
    # %P (parents) is read as its own field rather than parsed out of %B, and it
    # is load-bearing: FORM A binds the commit's first parent to the cited run's
    # head_sha.
    fmt = "%an%x1f%ae%x1f%cn%x1f%ce%x1f%aI%x1f%P%x1f%B"
    raw = git("show", "-s", f"--format={fmt}", sha, cwd=cwd)
    parts = raw.split("\x1f")
    while len(parts) < 7:
        parts.append("")
    return {
        "author_name": parts[0].strip(),
        "author_email": parts[1].strip(),
        "committer_name": parts[2].strip(),
        "committer_email": parts[3].strip(),
        "authored_at": parts[4].strip(),
        "parents": parts[5].split(),
        "body": parts[6],
    }


def classify(
    sha: str,
    meta: dict,
    branch: str,
    gitea_url: str,
    token: str | None,
    online: bool,
) -> tuple[bool, str]:
    """Return (ok, message) for one pin-touching commit."""
    subject = (meta["body"].splitlines() or [""])[0].strip()

    m = TRAILER_RE.search(meta["body"])
    if m:
        try:
            fields = pin_provenance.parse(m.group("stamp"))
        except pin_provenance.StampError as exc:
            return False, f"{sha[:12]} carries a Pin-Provenance trailer that does not parse: {exc}"
        if not online:
            return True, f"{sha[:12]} FORM A (trailer, run {fields['run']}, citation not checked offline)"
        try:
            repo = pin_provenance.resolve_repo(fields)
        except pin_provenance.StampError as exc:
            return False, f"{sha[:12]} {exc}"
        try:
            run = pin_provenance.fetch_run(fields["run"], repo, gitea_url, token)
        except pin_provenance.Undetermined as exc:
            # An unread API is not a pass and not a fail. The STRUCTURE is
            # valid and only a real run id has this shape, so this is reported
            # as a soft accept and said out loud — never silently upgraded.
            return True, (
                f"{sha[:12]} FORM A (trailer, run {fields['run']}) — citation "
                f"UNVERIFIED: {exc}"
            )
        if run is None:
            return False, (
                f"{sha[:12]} cites run {fields['run']} in {repo}, which does not exist"
            )
        wf = pin_provenance.workflow_file_of(run)
        if wf not in BUMP_WORKFLOWS:
            return False, (
                f"{sha[:12]} cites run {fields['run']}, but that run is {wf!r}, "
                f"not {'/'.join(sorted(BUMP_WORKFLOWS))}"
            )
        # Bind the STAMP to the run (sha= must be that run's head_sha)...
        try:
            pin_provenance.bind_stamp_to_run(fields, run)
        except pin_provenance.StampError as exc:
            return False, f"{sha[:12]} {exc}"
        except pin_provenance.Undetermined as exc:
            return True, (
                f"{sha[:12]} FORM A (trailer, run {fields['run']}) — binding "
                f"UNVERIFIED: {exc}"
            )
        # ...and bind the COMMIT to the run. This is the check whose ABSENCE
        # made an earlier draft's docstring false: a reviewer authored a commit
        # citing run 637995 — a real bump run, for a pin that was not theirs —
        # and it passed, because nothing compared the commit to the run.
        #
        # The bump lane runs `git checkout -b <branch>` from the ref it was
        # dispatched on and commits there, so a lane-produced bump's FIRST
        # PARENT is exactly that run's head_sha. Verified on real lane output:
        # commit 6688db931c11's sole parent is bc9fa9bff2bf, which is run
        # 637995's head_sha.
        #
        # Residual, stated rather than implied: someone who bases their pin
        # edit on exactly that commit still passes. Closing that needs the run
        # to sign something only it could produce, which this fleet has no key
        # for. See pin_provenance.py's module docstring.
        run_head = (run.get("head_sha") or "").lower()
        parents = meta.get("parents") or []
        if not parents:
            return False, (
                f"{sha[:12]} has no parent commit, so it cannot be tied to run "
                f"{fields['run']}"
            )
        if parents[0].lower() != run_head:
            return False, (
                f"{sha[:12]} cites run {fields['run']}, whose head sha is "
                f"{run_head[:12]}, but this commit's parent is {parents[0][:12]}. "
                f"The bump lane commits ON TOP OF the ref it was dispatched on, "
                f"so a genuine bump's parent IS that sha. This commit was "
                f"authored somewhere else and cited a run it did not come from."
            )
        return True, (
            f"{sha[:12]} FORM A — run {fields['run']} = {wf}, actor "
            f"{(run.get('actor') or {}).get('login')}, parent == run head "
            f"{run_head[:12]}"
        )

    # No trailer. FORM B is available only to commits authored before the cutoff.
    try:
        authored = _dt.datetime.fromisoformat(meta["authored_at"])
    except ValueError:
        authored = None
    legacy_window = authored is not None and authored < LEGACY_CUTOFF
    bot = (
        meta["author_name"] in BUMP_BOT_NAMES
        and meta["author_email"] in BUMP_BOT_EMAILS
        and meta["committer_name"] in BUMP_BOT_NAMES
        and meta["committer_email"] in BUMP_BOT_EMAILS
    )
    if legacy_window and bot and LEGACY_SUBJECT_RE.match(subject) and LEGACY_BRANCH_RE.match(branch):
        return True, (
            f"{sha[:12]} FORM B (legacy, authored {meta['authored_at']} before "
            f"{LEGACY_CUTOFF.isoformat()}) — bot author + lane subject + branch {branch}"
        )

    why = []
    if not legacy_window:
        why.append(
            f"authored {meta['authored_at'] or '?'} which is at or after the "
            f"legacy cutoff {LEGACY_CUTOFF.isoformat()}, so a trailer is required"
        )
    if not bot:
        why.append(
            f"author/committer is {meta['author_name']} <{meta['author_email']}> / "
            f"{meta['committer_name']} <{meta['committer_email']}>, not the bump bot"
        )
    if not LEGACY_SUBJECT_RE.match(subject):
        why.append(f"subject {subject!r} is not the lane's bump subject")
    if not LEGACY_BRANCH_RE.match(branch):
        why.append(f"branch {branch!r} is not bump/sdk-<sha12>")
    return False, f"{sha[:12]} has NO Pin-Provenance trailer; " + "; ".join(why)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--base", required=True, help="base ref/sha of the diff")
    ap.add_argument("--head", required=True, help="head ref/sha of the diff")
    ap.add_argument("--branch", default=os.environ.get("PIN_HEAD_BRANCH", ""), help="head branch name")
    ap.add_argument("--repo-root", default=".")
    ap.add_argument("--gitea-url", default=os.environ.get("GITEA_HOST", pin_provenance.DEFAULT_GITEA))
    ap.add_argument("--token-env", default="PIN_PROVENANCE_TOKEN")
    ap.add_argument("--offline", action="store_true")
    args = ap.parse_args(argv)

    cwd = args.repo_root
    token = os.environ.get(args.token_env) or None

    print(f"checked: {len(PIN_FILES)} SDK-pin file(s): {', '.join(PIN_FILES)}")
    try:
        diff = git("diff", "--unified=0", f"{args.base}..{args.head}", "--", *PIN_FILES, cwd=cwd)
    except RuntimeError as exc:
        print(f"::error::{exc}", file=sys.stderr)
        return 2

    if not sdk_pin_edit_in(diff):
        print(
            "sdk pin edits found: 0 — no line naming "
            f"{SDK_MODULE} or {CANONICAL_CONST} changed between "
            f"{args.base[:12]} and {args.head[:12]}. Nothing to attribute."
        )
        return 0

    try:
        shas = commits_touching_pin(args.base, args.head, cwd)
    except RuntimeError as exc:
        print(f"::error::{exc}", file=sys.stderr)
        return 2

    # Only the commits that actually move a pin LINE are judged. A merge commit
    # or a `go mod tidy` that touches go.sum without naming the module is not a
    # pin edit and must not be dragged into the verdict.
    judged: list[tuple[str, dict]] = []
    for sha in shas:
        try:
            cdiff = git(
                "show", "--unified=0", "--format=", sha, "--", *PIN_FILES, cwd=cwd
            )
        except RuntimeError as exc:
            print(f"::error::{exc}", file=sys.stderr)
            return 2
        if sdk_pin_edit_in(cdiff):
            judged.append((sha, commit_meta(sha, cwd)))

    print(f"sdk pin edits found: {len(judged)} commit(s)")
    if not judged:
        # The aggregate diff moved a pin line but no single commit did: that is
        # only reachable through a merge resolution, and it is exactly the shape
        # a hand edit could hide in. Fail rather than shrug.
        print(
            "::error::the diff moves an SDK pin line but no individual commit "
            "does — refusing to attribute a pin move to nothing.",
            file=sys.stderr,
        )
        return 1

    failed = 0
    for sha, meta in judged:
        ok, msg = classify(
            sha, meta, args.branch, args.gitea_url, token, online=not args.offline
        )
        if ok:
            print(f"  OK   {msg}")
        else:
            failed += 1
            print(f"  FAIL {msg}")
            print(f"::error::sdk pin provenance: {msg}", file=sys.stderr)

    if failed:
        print(
            "::error::The SDK pin moved without pipeline provenance. Bump it by "
            "dispatching the `sdk-pin-bump` workflow in molecule-ai/molecule-core "
            "(Actions -> sdk-pin-bump -> Run workflow); it runs go get, "
            "go generate, moves canonicalRegistrySHA256 and opens the PR with a "
            "Pin-Provenance trailer. Do NOT hand-run the go get / go generate / "
            "paste-the-sha sequence — that is the path this guard exists to stop.",
            file=sys.stderr,
        )
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
