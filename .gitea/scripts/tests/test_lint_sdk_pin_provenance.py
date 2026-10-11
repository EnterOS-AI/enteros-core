#!/usr/bin/env python3
"""Both directions for the SDK-pin provenance guard, on REAL git history.

Every case below builds an actual repository and runs the actual guard over an
actual `base..head`. A regex over the script would pass on a rule sitting in an
unreachable branch and on a rule comparing two empty strings; running it cannot.

The matrix, and why each row exists:

  hand edit, ordinary author        RED    the incident (45a61bb, 2026-08-05)
  hand edit wearing the bot's name  RED    after the legacy cutoff, a name is
                                           not provenance
  legacy bot bump before cutoff     GREEN  PR #5094 is exactly this and must
                                           not be failed by the gate meant to
                                           protect it
  trailer citing a real bump run    GREEN  the form the lane emits now
  trailer citing a run that is not
    a bump run                      RED    citing SOME green run must not work
  trailer citing a nonexistent run  RED
  unrelated go.mod churn            GREEN  the guard must not fire on a PR that
                                           merely touches go.mod
"""

from __future__ import annotations

import datetime as _dt
import os
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]
SCRIPTS = ROOT / ".gitea" / "scripts"
GUARD = SCRIPTS / "lint_sdk_pin_provenance.py"
sys.path.insert(0, str(SCRIPTS))

import lint_sdk_pin_provenance as guard  # noqa: E402
import pin_provenance as pp  # noqa: E402

GO_MOD_BEFORE = """module go.moleculesai.app/core/workspace-server

go 1.24

require (
\tgo.moleculesai.app/sdk/gen/go v0.0.0-20260805220531-3a2aa325b13e
\tgithub.com/gin-gonic/gin v1.10.0
)
"""
GO_MOD_AFTER = GO_MOD_BEFORE.replace(
    "v0.0.0-20260805220531-3a2aa325b13e", "v0.0.0-20260807184038-4412badf9314"
)

BEFORE_CUTOFF = "2026-08-07T18:42:19+00:00"
AFTER_CUTOFF = "2026-08-09T10:00:00+00:00"

BOT = ("molecule-sdk-pin-bot", "sdk-pin-bot@moleculesai.app")
HUMAN = ("Some Developer", "dev@example.com")


def _git(repo: Path, *args: str, env: dict | None = None) -> str:
    e = os.environ.copy()
    e.update(
        {
            "GIT_CONFIG_NOSYSTEM": "1",
            "HOME": str(repo),
            "GIT_TERMINAL_PROMPT": "0",
        }
    )
    if env:
        e.update(env)
    proc = subprocess.run(
        ["git", *args], cwd=repo, env=e, capture_output=True, text=True, check=False
    )
    assert proc.returncode == 0, f"git {' '.join(args)}: {proc.stderr}"
    return proc.stdout


@pytest.fixture()
def repo(tmp_path: Path) -> Path:
    r = tmp_path / "repo"
    (r / "workspace-server").mkdir(parents=True)
    _git(r.parent, "init", "-q", "-b", "main", str(r))
    (r / "workspace-server" / "go.mod").write_text(GO_MOD_BEFORE, encoding="utf-8")
    _git(r, "add", "-A")
    _commit(r, "chore: base", HUMAN, BEFORE_CUTOFF)
    return r


def _commit(repo: Path, message: str, who: tuple[str, str], when: str) -> str:
    name, email = who
    _git(
        repo,
        "-c",
        f"user.name={name}",
        "-c",
        f"user.email={email}",
        "commit",
        "-q",
        "--allow-empty",
        "-m",
        message,
        env={
            "GIT_AUTHOR_DATE": when,
            "GIT_COMMITTER_DATE": when,
            "GIT_AUTHOR_NAME": name,
            "GIT_AUTHOR_EMAIL": email,
            "GIT_COMMITTER_NAME": name,
            "GIT_COMMITTER_EMAIL": email,
        },
    )
    return _git(repo, "rev-parse", "HEAD").strip()


def _bump(repo: Path, message: str, who: tuple[str, str], when: str) -> tuple[str, str]:
    base = _git(repo, "rev-parse", "HEAD").strip()
    (repo / "workspace-server" / "go.mod").write_text(GO_MOD_AFTER, encoding="utf-8")
    _git(repo, "add", "-A")
    head = _commit(repo, message, who, when)
    return base, head


def _run(repo: Path, base: str, head: str, branch: str, extra: list[str] | None = None):
    return subprocess.run(
        [
            sys.executable,
            str(GUARD),
            "--base",
            base,
            "--head",
            head,
            "--branch",
            branch,
            "--repo-root",
            str(repo),
            *(extra or ["--offline"]),
        ],
        capture_output=True,
        text=True,
        check=False,
    )


RUN_ID = "624320"


def trailer(sha: str, run: str = RUN_ID, repo: str = "molecule-ai/molecule-core") -> str:
    """A trailer whose `sha=` is the run's head sha.

    Built per-test rather than fixed, because FORM A now binds BOTH ways: the
    stamp's sha must be the cited run's head_sha, and the commit's first parent
    must be that same sha. A constant trailer could only ever exercise the
    mismatch case.
    """
    return (
        f"Pin-Provenance: [ci-pin-provenance v1 repo={repo} "
        f"wf=sdk-pin-bump run={run} job=bump sha={sha}]"
    )


def bump_run(head_sha: str, path: str = "sdk-pin-bump.yml@refs/heads/main") -> dict:
    return {"path": path, "head_sha": head_sha, "actor": {"login": "molecule-sdk-pin-bot"}}


# --------------------------------------------------------------------------
# the count
# --------------------------------------------------------------------------


def test_checked_count_is_reported_and_never_zero(repo: Path):
    """`checked: 0` is the signature of a guard covering nothing.

    This guard's count is the number of pin-carrying files it examines, so it
    is constant even on a diff that touches none of them.
    """
    base = _git(repo, "rev-parse", "HEAD").strip()
    head = _commit(repo, "docs: unrelated", HUMAN, AFTER_CUTOFF)
    proc = _run(repo, base, head, "feat/unrelated")
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert f"checked: {len(guard.PIN_FILES)}" in proc.stdout
    assert "sdk pin edits found: 0" in proc.stdout


def test_unrelated_go_mod_churn_does_not_fire(repo: Path):
    """A require-block edit that does not name the SDK module is not a pin edit."""
    base = _git(repo, "rev-parse", "HEAD").strip()
    (repo / "workspace-server" / "go.mod").write_text(
        GO_MOD_BEFORE.replace("gin v1.10.0", "gin v1.10.1"), encoding="utf-8"
    )
    _git(repo, "add", "-A")
    head = _commit(repo, "chore: bump gin", HUMAN, AFTER_CUTOFF)
    proc = _run(repo, base, head, "feat/unrelated")
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert "sdk pin edits found: 0" in proc.stdout


# --------------------------------------------------------------------------
# NEGATIVE direction
# --------------------------------------------------------------------------


def test_hand_edited_pin_is_red(repo: Path):
    """The incident shape: a developer bumps the pin mid-task on their own branch."""
    base, head = _bump(
        repo, "chore: while I was in here, bump the sdk pin", HUMAN, AFTER_CUTOFF
    )
    proc = _run(repo, base, head, "feat/some-unrelated-work")
    assert proc.returncode == 1, proc.stdout + proc.stderr
    assert "NO Pin-Provenance trailer" in proc.stdout
    assert "sdk pin edits found: 1" in proc.stdout
    assert "::error::" in proc.stderr


def test_bot_name_alone_is_not_enough_after_the_cutoff(repo: Path):
    """`git commit --author=` is one flag; after the cutoff a trailer is required.

    Varies exactly one thing from test_legacy_bot_bump_is_green below: the date.
    """
    base, head = _bump(
        repo, "chore(sdk): bump sdk/gen/go pin to 4412badf9314", BOT, AFTER_CUTOFF
    )
    proc = _run(repo, base, head, "bump/sdk-4412badf9314")
    assert proc.returncode == 1, proc.stdout + proc.stderr
    assert "legacy cutoff" in proc.stdout


def test_trailer_citing_a_non_bump_workflow_is_red(repo: Path, monkeypatch):
    parent = _git(repo, "rev-parse", "HEAD").strip()
    base, head = _bump(
        repo,
        "chore(sdk): bump sdk/gen/go pin to 4412badf9314\n\n" + trailer(parent),
        HUMAN,
        AFTER_CUTOFF,
    )
    monkeypatch.setattr(
        guard.pin_provenance,
        "fetch_run",
        lambda *a, **k: bump_run(parent, path="ci.yml@refs/heads/main"),
    )
    ok, msg = guard.classify(
        head, guard.commit_meta(head, str(repo)), "bump/sdk-4412badf9314",
        pp.DEFAULT_GITEA, None, online=True,
    )
    assert ok is False, msg
    assert "not sdk-pin-bump.yml" in msg


def test_trailer_citing_a_nonexistent_run_is_red(repo: Path, monkeypatch):
    parent = _git(repo, "rev-parse", "HEAD").strip()
    base, head = _bump(
        repo,
        "chore(sdk): bump sdk/gen/go pin to 4412badf9314\n\n" + trailer(parent),
        HUMAN,
        AFTER_CUTOFF,
    )
    monkeypatch.setattr(guard.pin_provenance, "fetch_run", lambda *a, **k: None)
    ok, msg = guard.classify(
        head, guard.commit_meta(head, str(repo)), "bump/sdk-4412badf9314",
        pp.DEFAULT_GITEA, None, online=True,
    )
    assert ok is False, msg
    assert "does not exist" in msg


def test_replaying_a_real_bump_run_from_another_base_is_red(repo: Path, monkeypatch):
    """THE REVIEWER'S EXPLOIT, as a test.

    An earlier draft passed a commit that cited run 637995 — a real bump run,
    for a pin that was not the author's — because nothing compared the commit to
    the run. Here the trailer is entirely well-formed and the run is a genuine
    sdk-pin-bump run; the ONLY thing wrong is that the commit was authored
    somewhere other than that run's head. That must be enough to fail it.
    """
    parent = _git(repo, "rev-parse", "HEAD").strip()
    elsewhere = "9" * 40
    base, head = _bump(
        repo,
        "chore(sdk): bump sdk/gen/go pin to 4412badf9314\n\n" + trailer(elsewhere),
        HUMAN,
        AFTER_CUTOFF,
    )
    monkeypatch.setattr(guard.pin_provenance, "fetch_run", lambda *a, **k: bump_run(elsewhere))
    ok, msg = guard.classify(
        head, guard.commit_meta(head, str(repo)), "bump/sdk-4412badf9314",
        pp.DEFAULT_GITEA, None, online=True,
    )
    assert ok is False, msg
    assert "authored somewhere else and cited a run it did not come from" in msg
    assert parent[:12] in msg


def test_trailer_naming_another_repo_is_red(repo: Path, monkeypatch):
    """A stamp must not redirect the verifier to an authority of its choosing."""
    parent = _git(repo, "rev-parse", "HEAD").strip()
    base, head = _bump(
        repo,
        "chore(sdk): bump sdk/gen/go pin to 4412badf9314\n\n"
        + trailer(parent, repo="attacker/evil-repo"),
        HUMAN,
        AFTER_CUTOFF,
    )
    monkeypatch.setattr(guard.pin_provenance, "fetch_run", lambda *a, **k: bump_run(parent))
    monkeypatch.setenv("GITHUB_REPOSITORY", "molecule-ai/molecule-core")
    ok, msg = guard.classify(
        head, guard.commit_meta(head, str(repo)), "bump/sdk-4412badf9314",
        pp.DEFAULT_GITEA, None, online=True,
    )
    assert ok is False, msg
    assert "does not get to choose" in msg


def test_a_malformed_trailer_is_red(repo: Path):
    base, head = _bump(
        repo,
        "chore(sdk): bump\n\nPin-Provenance: [ci-pin-provenance v1 repo=molecule-ai/molecule-core run=nope]",
        BOT,
        AFTER_CUTOFF,
    )
    proc = _run(repo, base, head, "bump/sdk-4412badf9314")
    assert proc.returncode == 1, proc.stdout + proc.stderr
    assert "does not parse" in proc.stdout


# --------------------------------------------------------------------------
# POSITIVE direction
# --------------------------------------------------------------------------


def test_legacy_bot_bump_is_green(repo: Path):
    """PR #5094's exact shape: bot author, lane subject, bump branch, pre-cutoff.

    Failing this would red-flag an in-flight, genuinely CI-produced bump — the
    "gate that punishes the fix" failure this repository has shipped before.
    """
    base, head = _bump(
        repo, "chore(sdk): bump sdk/gen/go pin to 4412badf9314", BOT, BEFORE_CUTOFF
    )
    proc = _run(repo, base, head, "bump/sdk-4412badf9314")
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert "FORM B" in proc.stdout


def test_trailer_citing_a_real_bump_run_is_green(repo: Path, monkeypatch):
    """POSITIVE control for the three negatives above: one variable different.

    The trailer's sha, the run's head_sha and the commit's parent all agree —
    which is exactly the shape real lane output has. Verified against live data
    outside the suite: commit 6688db931c11's sole parent is bc9fa9bff2bf, and
    run 637995 reports head_sha=bc9fa9bff2bf9c4e952ee309845a1a1ee7c66c4f.
    """
    parent = _git(repo, "rev-parse", "HEAD").strip()
    base, head = _bump(
        repo,
        "chore(sdk): bump sdk/gen/go pin to 4412badf9314\n\n" + trailer(parent),
        BOT,
        AFTER_CUTOFF,
    )
    monkeypatch.setattr(guard.pin_provenance, "fetch_run", lambda *a, **k: bump_run(parent))
    ok, msg = guard.classify(
        head, guard.commit_meta(head, str(repo)), "bump/sdk-4412badf9314",
        pp.DEFAULT_GITEA, None, online=True,
    )
    assert ok is True, msg
    assert "FORM A" in msg
    assert "parent == run head" in msg


def test_trailer_on_a_non_bump_branch_is_still_green(repo: Path):
    """FORM A keys on the run id and the parent, not the branch.

    A verification dispatch produces `bump/sdk-<sha12>-onto-<slug>`, so
    asserting the branch would fail real lane output.
    """
    parent = _git(repo, "rev-parse", "HEAD").strip()
    base, head = _bump(
        repo,
        "chore(sdk): bump sdk/gen/go pin to 4412badf9314\n\n" + trailer(parent),
        BOT,
        AFTER_CUTOFF,
    )
    proc = _run(repo, base, head, "bump/sdk-4412badf9314-onto-proof-branch")
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert "FORM A" in proc.stdout


# --------------------------------------------------------------------------
# the guard's own failure modes
# --------------------------------------------------------------------------


def test_unread_actions_api_is_reported_not_laundered(repo: Path, monkeypatch):
    """An unread API must not silently become either verdict."""
    parent = _git(repo, "rev-parse", "HEAD").strip()
    base, head = _bump(
        repo,
        "chore(sdk): bump sdk/gen/go pin to 4412badf9314\n\n" + trailer(parent),
        BOT,
        AFTER_CUTOFF,
    )

    def boom(*a, **k):
        raise pp.Undetermined("HTTP 502")

    monkeypatch.setattr(guard.pin_provenance, "fetch_run", boom)
    ok, msg = guard.classify(
        head,
        guard.commit_meta(head, str(repo)),
        "bump/sdk-4412badf9314",
        pp.DEFAULT_GITEA,
        None,
        online=True,
    )
    assert ok is True, msg
    assert "UNVERIFIED" in msg


def test_legacy_cutoff_is_in_the_past_so_form_b_is_actually_closing():
    """A cutoff in the future would leave FORM B permanently open."""
    assert guard.LEGACY_CUTOFF < _dt.datetime.now(_dt.timezone.utc)


if __name__ == "__main__":
    raise SystemExit(pytest.main([__file__, "-v"]))
