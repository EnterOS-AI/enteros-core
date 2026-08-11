#!/usr/bin/env python3
"""Disposition matrix for the pin provenance stamp — BOTH directions.

A guard proven only on the failing case is half a guard: the other half is that
it passes a legitimate write, and skipping that half is how a ratchet once
failed the very commit paying it down. Every rule below is exercised twice — the
row that must go red, and the nearest row that must stay green, varying ONE
input.

The audit rows use real values read off the live control planes, including the
staging row whose treatment CI corrected before merge (dispatch 643926).
"""

from __future__ import annotations

import datetime as _dt
import io
import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / ".gitea" / "scripts"))

import pin_provenance as pp  # noqa: E402

CI_ENV = {
    "GITHUB_RUN_ID": "700001",
    "GITHUB_REPOSITORY": "molecule-ai/molecule-core",
    "GITHUB_WORKFLOW": "staging-tenant-cd",
    "GITHUB_JOB": "advance-pin",
    "GITHUB_SHA": "b7508a4abce6169e619081b0e531e5df6f92bf0c",
}

FRESH_DIGEST = "sha256:" + "1f" * 32

# The grandfathering boundary is an INSTANT derived from the checkout, so the
# tests pin it explicitly instead of depending on git state.
LANDED = _dt.datetime(2026, 8, 10, 12, 0, 0, tzinfo=_dt.timezone.utc)
BEFORE = "2026-08-09T12:38:57.90016Z"
AFTER = "2026-08-11T03:50:52.425711Z"


# --------------------------------------------------------------------------
# minting
# --------------------------------------------------------------------------


def test_mint_requires_a_run_id():
    """No GITHUB_RUN_ID -> no stamp. This is the whole fail-closed mechanism."""
    with pytest.raises(pp.StampError) as exc:
        pp.mint({"GITHUB_REPOSITORY": "molecule-ai/molecule-core"})
    assert "GITHUB_RUN_ID" in str(exc.value)


def test_mint_succeeds_with_a_run_id():
    """The positive control for the test above: one variable different."""
    stamp = pp.mint(CI_ENV)
    assert pp.parse(stamp)["run"] == "700001"


def test_mint_tolerates_missing_cosmetic_fields():
    """Only the run id is load-bearing.

    A stamp that refused to mint because GITHUB_JOB happened to be unset would
    take the staging deploy path down for a field the auditor re-derives from
    the Actions API anyway.
    """
    stamp = pp.mint({"GITHUB_RUN_ID": "700002"})
    fields = pp.parse(stamp)
    assert fields["run"] == "700002"
    assert fields["wf"] == "-"


def test_stamp_notes_stays_within_the_cp_notes_budget():
    """The CP rejects notes > 500 chars, and a rejected promote is a failed deploy."""
    out = pp.stamp_notes("x" * 900, env=CI_ENV)
    assert len(out) <= 500
    # The STAMP is what must survive truncation, not the prose.
    assert pp.parse(out)["run"] == "700001"


# --------------------------------------------------------------------------
# parsing
# --------------------------------------------------------------------------


@pytest.mark.parametrize(
    "notes",
    [
        "",
        "prod pinned-promote from staging-proven (core#5092 concierge identity fix)",
        "staging tenant image registry.moleculesai.app/molecule-ai/molecule-tenant:staging-b5e48f0",
        "[ci-pin-provenance v2 repo=molecule-ai/molecule-core run=1]",
        "[ci-pin-provenance v1 repo=molecule-ai/molecule-core run=notanumber]",
        "[ci-pin-provenance v1 repo=notarepo run=12]",
    ],
)
def test_parse_rejects(notes):
    """The first three are REAL notes strings read off live control planes."""
    with pytest.raises(pp.StampError):
        pp.parse(notes)


def test_parse_accepts_a_minted_stamp_with_trailing_prose():
    fields = pp.parse(pp.stamp_notes("tenant image registry/x:staging-abc1234", env=CI_ENV))
    assert fields["run"] == "700001"
    assert fields["repo"] == "molecule-ai/molecule-core"


# --------------------------------------------------------------------------
# audit
# --------------------------------------------------------------------------


def _pins(tmp_path: Path, name: str, digest: str, notes: str, promoted_at: str = AFTER) -> str:
    p = tmp_path / name
    p.write_text(
        json.dumps(
            {
                "pins": [
                    {
                        "template_name": "molecule-tenant",
                        "region": "global",
                        "image_digest": digest,
                        "git_sha": "b5e48f0708f0e95b85005c3a39d915eae1684b5e",
                        "promoted_at": promoted_at,
                        "promoted_by": "api-token-<8 hex>",
                        "notes": notes,
                    }
                ]
            }
        ),
        encoding="utf-8",
    )
    return str(p)


def _audit(paths: dict[str, str], landed_at=LANDED) -> tuple[int, str]:
    buf = io.StringIO()
    rc = pp.audit(list(paths.items()), gitea_url=pp.DEFAULT_GITEA, token=None,
                  online=False, out=buf, landed_at=landed_at)
    return rc, buf.getvalue()


def test_audit_fires_on_a_hand_written_pin(tmp_path):
    """THE NEGATIVE DIRECTION.

    The note is the exact string read off the production control plane on
    2026-08-09, on a digest that is NOT in the grandfather set — i.e. exactly
    what the next hand promote would look like.
    """
    hand = (
        "prod pinned-promote from staging-proven (core#5092 concierge identity "
        "fix); staging e2e HARD GATE green incl. STEP 3d behavioural identity"
    )
    rc, out = _audit({"cp_prod": _pins(tmp_path, "a.json", FRESH_DIGEST, hand)})
    assert rc == 1, out
    assert "UNSTAMPED" in out
    assert "checked: 1" in out


def test_audit_passes_a_pipeline_written_pin(tmp_path):
    """THE POSITIVE DIRECTION — same digest, only the note differs."""
    stamped = pp.stamp_notes("tenant image registry/x:staging-b5e48f0", env=CI_ENV)
    rc, out = _audit({"cp_prod": _pins(tmp_path, "b.json", FRESH_DIGEST, stamped)})
    assert rc == 0, out
    assert "checked: 1  stamped: 1" in out


def test_audit_grandfathers_only_rows_that_predate_the_mechanism(tmp_path):
    """The boundary is the instant stamping landed, and it varies ONE input.

    Both rows below are byte-identical apart from `promoted_at`. The earlier one
    could not have been stamped; the later one could, so it must not be excused.
    """
    hand = "prod pinned-promote from staging-proven"
    rc, out = _audit({"cp_prod": _pins(tmp_path, "c.json", FRESH_DIGEST, hand, promoted_at=BEFORE)})
    assert rc == 0, out
    assert "GRANDFATHERED" in out and "grandfathered: 1" in out

    rc2, out2 = _audit({"cp_prod": _pins(tmp_path, "d.json", FRESH_DIGEST, hand, promoted_at=AFTER)})
    assert rc2 == 1, out2
    assert "UNSTAMPED" in out2


def test_a_legitimate_pre_merge_CI_write_is_not_red_flagged(tmp_path):
    """REGRESSION for the defect CI caught in dispatch 643926.

    This is staging-tenant-cd's real row, verbatim: the old script's machine
    format, written at 03:50:52Z on 2026-08-11 while this change was in review.
    Under the digest-snapshot grandfathering it was a VIOLATION, which would have
    reddened main for the pipeline's own write. Under the derived instant it is
    correctly grandfathered when the mechanism landed after it.
    """
    notes = ("staging tenant image "
             "registry.moleculesai.app/molecule-ai/molecule-tenant:staging-492e031")
    digest = "sha256:8027955a4ad7317acd11082b679ab7e5e793768fc80e070767ec5e46b441506b"
    landed_after = _dt.datetime(2026, 8, 11, 4, 0, 0, tzinfo=_dt.timezone.utc)
    rc, out = _audit(
        {"cp_staging": _pins(tmp_path, "cd.json", digest, notes,
                             promoted_at="2026-08-11T03:50:52.425711Z")},
        landed_at=landed_after,
    )
    assert rc == 0, out
    assert "GRANDFATHERED" in out


def test_an_unknown_landing_instant_grandfathers_NOTHING(tmp_path):
    """"Could not determine" must not silently become "excuse everything"."""
    hand = "prod pinned-promote from staging-proven"
    rc, out = _audit(
        {"cp_prod": _pins(tmp_path, "u.json", FRESH_DIGEST, hand, promoted_at=BEFORE)},
        landed_at=None,
    )
    assert rc == 1, out
    assert "NOTHING is grandfathered" in out


def test_a_row_with_an_unparseable_promoted_at_is_not_grandfathered(tmp_path):
    hand = "prod pinned-promote from staging-proven"
    rc, out = _audit({"cp_prod": _pins(tmp_path, "bad.json", FRESH_DIGEST, hand, promoted_at="not-a-date")})
    assert rc == 1, out


def test_mechanism_landed_at_agrees_with_THIS_checkouts_shape():
    """Not a mock: the real function against the real checkout it is running in.

    The assertion is the COUPLING, not a fixed answer, because this suite runs
    in two lanes with different checkouts and both are legitimate:

      * pin-provenance-guard.yml checks out `fetch-depth: 0` -> full -> a
        boundary must be derivable;
      * test-ops-scripts.yml uses a bare `actions/checkout` -> SHALLOW -> the
        function must refuse.

    An earlier version of this test asserted "not None" unconditionally and went
    red in the ops lane. That was the test being wrong, not the code — and it is
    also the cleanest evidence that a shallow checkout is not a hypothetical
    here: one of this repo's own lanes already is one.
    """
    shallow = subprocess.run(
        ["git", "rev-parse", "--is-shallow-repository"],
        cwd=str(ROOT), capture_output=True, text=True,
    ).stdout.strip().lower()
    got = pp.mechanism_landed_at(str(ROOT))
    if shallow == "true":
        assert got is None, (
            "a shallow checkout yielded a boundary; the amnesty would be widened "
            f"to {got}"
        )
    else:
        assert got is not None, "a full checkout must yield a boundary"
        assert got.tzinfo is not None


def test_the_boundary_is_the_INTRODUCING_commit_not_the_last_touch():
    """Keyed on the last touch, any later edit would slide the amnesty forward.

    Asserts the git query itself, because the behavioural proof (commit a touch,
    watch the boundary move) cannot run inside a suite that must not mutate the
    repository it is running in. It was run by hand: with `-1` the boundary moved
    21:04:39 -> 21:13:25; with `--diff-filter=A` it did not move.
    """
    src = (ROOT / ".gitea" / "scripts" / "pin_provenance.py").read_text(encoding="utf-8")
    assert '"--diff-filter=A"' in src
    assert "lines[-1]" in src


def test_parse_promoted_at_handles_the_CPs_real_shapes():
    for raw in ("2026-08-11T03:50:52.425711Z", "2026-08-09T12:38:57.90016Z",
                "2026-08-07T23:11:01.140408Z", "2026-08-05T13:10:36.542594Z"):
        d = pp.parse_promoted_at(raw)
        assert d is not None and d.tzinfo is not None, raw
    assert pp.parse_promoted_at("") is None
    assert pp.parse_promoted_at("nope") is None


def test_audit_fails_on_an_absent_pin_row(tmp_path):
    """An absent row must not render as a clean audit."""
    p = tmp_path / "empty.json"
    p.write_text(json.dumps({"pins": []}), encoding="utf-8")
    rc, out = _audit({"cp_prod": str(p)})
    assert rc == 1, out
    assert "checked: 0" in out


def test_audit_fails_on_an_unreadable_document(tmp_path):
    """"We could not look" must never render the same as "we looked and it was fine"."""
    rc, out = _audit({"cp_prod": str(tmp_path / "does-not-exist.json")})
    assert rc == 1, out


def test_audit_reports_a_count_that_is_never_silently_zero(tmp_path):
    stamped = pp.stamp_notes("t", env=CI_ENV)
    rc, out = _audit(
        {
            "cp_prod": _pins(tmp_path, "f.json", FRESH_DIGEST, stamped),
            "cp_staging": _pins(tmp_path, "g.json", FRESH_DIGEST, stamped),
        }
    )
    assert rc == 0, out
    assert "checked: 2" in out


# --------------------------------------------------------------------------
# online citation checking (no network — the fetch is stubbed)
# --------------------------------------------------------------------------


HEAD_SHA = "b7508a4abce6169e619081b0e531e5df6f92bf0c"
STAMP_FIELDS = {
    "run": "637861",
    "repo": "molecule-ai/molecule-core",
    "sha": HEAD_SHA,
}


def _writer_run(**over):
    run = {
        "path": "promote-prod-tenant-pin.yml@refs/heads/main",
        "head_sha": HEAD_SHA,
        "event": "workflow_dispatch",
        "actor": {"login": "hongming"},
    }
    run.update(over)
    return run


def test_verify_run_rejects_a_nonexistent_run(monkeypatch):
    monkeypatch.setattr(pp, "fetch_run", lambda *a, **k: None)
    with pytest.raises(pp.StampError) as exc:
        pp.verify_run(dict(STAMP_FIELDS), pp.DEFAULT_GITEA, None)
    assert "does NOT exist" in str(exc.value)


def test_verify_run_rejects_a_run_from_a_non_pin_workflow(monkeypatch):
    """Citing SOME green run must not launder a hand promote."""
    monkeypatch.setattr(
        pp, "fetch_run", lambda *a, **k: _writer_run(path="ci.yml@refs/heads/main")
    )
    with pytest.raises(pp.StampError) as exc:
        pp.verify_run(dict(STAMP_FIELDS), pp.DEFAULT_GITEA, None)
    assert "not a pin-writing workflow" in str(exc.value)


def test_verify_run_accepts_a_real_pin_writer(monkeypatch):
    monkeypatch.setattr(pp, "fetch_run", lambda *a, **k: _writer_run())
    msg = pp.verify_run(dict(STAMP_FIELDS), pp.DEFAULT_GITEA, None)
    assert "promote-prod-tenant-pin.yml" in msg


# --- the binding that an earlier docstring CLAIMED and did not have ---------


def test_stamp_sha_must_be_the_cited_runs_head_sha(monkeypatch):
    """A stamp must reproduce a value it cannot guess from the run id alone.

    The earlier draft compared nothing: head_sha was read only into a display
    string, so any run id could be typed next to any sha.
    """
    monkeypatch.setattr(pp, "fetch_run", lambda *a, **k: _writer_run())
    bad = dict(STAMP_FIELDS, sha="0" * 40)
    with pytest.raises(pp.StampError) as exc:
        pp.verify_run(bad, pp.DEFAULT_GITEA, None)
    assert "was not minted by that run" in str(exc.value)


def test_a_stamp_with_no_sha_cannot_bind(monkeypatch):
    monkeypatch.setattr(pp, "fetch_run", lambda *a, **k: _writer_run())
    with pytest.raises(pp.StampError):
        pp.verify_run({"run": "637861", "repo": "molecule-ai/molecule-core", "sha": "-"},
                      pp.DEFAULT_GITEA, None)


def test_a_run_without_a_head_sha_is_undetermined_not_a_pass(monkeypatch):
    """Missing data is an unread signal, never a silent accept."""
    monkeypatch.setattr(pp, "fetch_run", lambda *a, **k: _writer_run(head_sha=""))
    with pytest.raises(pp.Undetermined):
        pp.verify_run(dict(STAMP_FIELDS), pp.DEFAULT_GITEA, None)


# --- the stamp does not get to choose its own authority --------------------


def test_resolve_repo_refuses_a_repo_the_verifier_does_not_answer_for():
    """`repo=attacker/evil-repo` used to REDIRECT the lookup; now it is an error."""
    with pytest.raises(pp.StampError) as exc:
        pp.resolve_repo({"repo": "attacker/evil-repo"}, expected="molecule-ai/molecule-core")
    assert "does not get to choose" in str(exc.value)


def test_resolve_repo_accepts_the_expected_repo_and_the_degraded_dash():
    assert pp.resolve_repo({"repo": "molecule-ai/molecule-core"},
                           expected="molecule-ai/molecule-core") == "molecule-ai/molecule-core"
    assert pp.resolve_repo({"repo": "-"},
                           expected="molecule-ai/molecule-core") == "molecule-ai/molecule-core"


def test_expected_repo_comes_from_the_environment_not_the_stamp(monkeypatch):
    monkeypatch.setenv("GITHUB_REPOSITORY", "molecule-ai/other-repo")
    assert pp.expected_repo() == "molecule-ai/other-repo"
    monkeypatch.delenv("GITHUB_REPOSITORY", raising=False)
    assert pp.expected_repo() == pp.DEFAULT_REPO


def test_undetermined_is_neither_a_pass_nor_a_fail(monkeypatch, tmp_path):
    """An unread API must be reported as unread, not converted into a verdict."""

    def boom(*a, **k):
        raise pp.Undetermined("HTTP 502 reading run 1")

    monkeypatch.setattr(pp, "fetch_run", boom)
    stamped = pp.stamp_notes("t", env=CI_ENV)
    buf = io.StringIO()
    rc = pp.audit(
        [("cp_prod", _pins(tmp_path, "h.json", FRESH_DIGEST, stamped))],
        gitea_url=pp.DEFAULT_GITEA,
        token=None,
        online=True,
        out=buf,
    )
    out = buf.getvalue()
    assert rc == 0, out
    assert "UNVERIFIED" in out
    assert "undetermined: 1" in out


def test_normalize_base_accepts_a_bare_hostname():
    assert pp.normalize_base("git.moleculesai.app") == "https://git.moleculesai.app"
    assert pp.normalize_base("https://git.moleculesai.app/") == "https://git.moleculesai.app"


if __name__ == "__main__":
    raise SystemExit(pytest.main([__file__, "-v"]))


# --------------------------------------------------------------------------
# `checked: 0` — the clause, not just the message
# --------------------------------------------------------------------------


def test_audit_of_zero_endpoints_fails_on_the_checked_zero_CLAUSE():
    """MUTATION-TARGETED. Kills the mutant that drops `checked == 0`.

    test_audit_fails_on_an_absent_pin_row looks like it covers this platform's
    signature defect, and does not: it reaches rc=1 through the
    `no (molecule-tenant, global) row` violation, so deleting `or checked == 0`
    from the failing condition leaves it green. Verified by running that mutant.

    This case has NO endpoints, therefore no violations, therefore the ONLY
    thing that can make it fail is the clause itself. `pass: 0` is the signature
    of a guard covering nothing, and an audit that inspected nothing must not
    report clean.
    """
    buf = io.StringIO()
    rc = pp.audit([], gitea_url=pp.DEFAULT_GITEA, token=None, online=False, out=buf)
    out = buf.getvalue()
    assert rc == 1, out
    assert "checked: 0" in out
    assert "this audit inspected NO pin row" in out


# --------------------------------------------------------------------------
# the bounded legacy-writer-format allowance
# --------------------------------------------------------------------------


def test_legacy_writer_note_requires_internal_consistency():
    """Matching the shape is not enough; the tag's sha7 must be the row's git_sha."""
    real = ("staging tenant image "
            "registry.moleculesai.app/molecule-ai/molecule-tenant:staging-492e031")
    assert pp.is_legacy_writer_note(real, "492e031abcdef1234567890abcdef1234567890a")
    assert not pp.is_legacy_writer_note(real, "deadbee0000000000000000000000000000000a")
    assert not pp.is_legacy_writer_note(real, None)
    # the hand-written prod note read live on 2026-08-09
    assert not pp.is_legacy_writer_note(
        "prod pinned-promote from staging-proven (core#5092 concierge identity fix)",
        "492e031",
    )


def test_a_CD_write_made_during_review_is_grandfathered_but_a_hand_write_is_not(tmp_path):
    """The race this allowance exists for, and its negative control.

    Both rows are promoted AFTER the mechanism landed, so the primary
    grandfather clause does not apply to either. They differ in ONE thing: the
    note. The pipeline's machine format with a consistent git_sha is excused;
    the same timestamp with a hand-typed sentence is not.
    """
    landed = _dt.datetime(2026, 8, 11, 0, 0, 0, tzinfo=_dt.timezone.utc)
    after = "2026-08-11T03:50:52.425711Z"
    cd_note = ("staging tenant image "
               "registry.moleculesai.app/molecule-ai/molecule-tenant:staging-492e031")

    p = tmp_path / "cd.json"
    p.write_text(json.dumps({"pins": [{
        "template_name": "molecule-tenant", "region": "global",
        "image_digest": FRESH_DIGEST, "git_sha": "492e031abcdef1234567890abcdef1234567890a",
        "promoted_at": after, "promoted_by": "api-token-x", "notes": cd_note}]}), encoding="utf-8")
    rc, out = _audit({"cp_staging": str(p)}, landed_at=landed)
    assert rc == 0, out
    assert "PRE-PROVENANCE WRITER format" in out

    q = tmp_path / "hand.json"
    q.write_text(json.dumps({"pins": [{
        "template_name": "molecule-tenant", "region": "global",
        "image_digest": FRESH_DIGEST, "git_sha": "492e031abcdef1234567890abcdef1234567890a",
        "promoted_at": after, "promoted_by": "api-token-x",
        "notes": "prod pinned-promote from staging-proven"}]}), encoding="utf-8")
    rc2, out2 = _audit({"cp_prod": str(q)}, landed_at=landed)
    assert rc2 == 1, out2
    assert "UNSTAMPED" in out2


def test_the_legacy_allowance_expires(tmp_path):
    """It is bounded. Past the grace, the same row is a violation."""
    landed = _dt.datetime(2026, 8, 11, 0, 0, 0, tzinfo=_dt.timezone.utc)
    cd_note = ("staging tenant image "
               "registry.moleculesai.app/molecule-ai/molecule-tenant:staging-492e031")
    late = (landed + pp.LEGACY_FORMAT_GRACE + _dt.timedelta(days=1)).isoformat()
    p = tmp_path / "late.json"
    p.write_text(json.dumps({"pins": [{
        "template_name": "molecule-tenant", "region": "global",
        "image_digest": FRESH_DIGEST, "git_sha": "492e031abcdef1234567890abcdef1234567890a",
        "promoted_at": late, "promoted_by": "api-token-x", "notes": cd_note}]}), encoding="utf-8")
    rc, out = _audit({"cp_staging": str(p)}, landed_at=landed)
    assert rc == 1, out
    assert "UNSTAMPED" in out


# --------------------------------------------------------------------------
# the boundary must not be movable by the SHAPE OF THE CHECKOUT
# --------------------------------------------------------------------------


def _fixture_repo(tmp_path: Path, later_commits: int = 3) -> Path:
    """A repo shaped like main after this lands: the mechanism in an older
    commit, later work on top."""
    src = tmp_path / "src"
    (src / ".gitea" / "scripts").mkdir(parents=True)
    (src / ".gitea" / "scripts" / "pin_provenance.py").write_text("# fixture\n", encoding="utf-8")

    def git(*args, when=None):
        env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", HOME=str(tmp_path))
        if when:
            env["GIT_AUTHOR_DATE"] = env["GIT_COMMITTER_DATE"] = when
        r = subprocess.run(["git", *args], cwd=src, env=env, capture_output=True, text=True)
        assert r.returncode == 0, f"git {' '.join(args)}: {r.stderr}"

    subprocess.run(["git", "init", "-q", "-b", "main", str(src)], check=True)
    git("config", "user.email", "t@t")
    git("config", "user.name", "t")
    git("add", "-A")
    git("commit", "-q", "-m", "add the mechanism", when="2026-08-10T21:16:34-07:00")
    for i in range(later_commits):
        (src / f"later{i}.txt").write_text("x", encoding="utf-8")
        git("add", "-A")
        git("commit", "-q", "-m", f"later work {i}", when="2026-09-05T12:00:00+00:00")
    return src


def test_mechanism_landed_at_fails_closed_on_a_shallow_repository(tmp_path):
    """MD4 KILLER, and it drives the REAL function, not an injected parameter.

    `test_an_unknown_landing_instant_grandfathers_NOTHING` passes landed_at=None
    straight in, so it covers the CONSUMER of the failure path and not the
    function that produces it. A mutant returning `now()` instead of None on
    failure survives that test. This one calls mechanism_landed_at() against an
    actual `--depth 1` clone.

    Why it matters: on a truncated history `--diff-filter=A` has no add commit to
    find, so git names the shallow boundary instead and the amnesty silently
    slides forward to the checkout's tip date.
    """
    src = _fixture_repo(tmp_path)
    shallow = tmp_path / "shallow"
    r = subprocess.run(
        ["git", "clone", "-q", "--depth", "1", src.as_uri(), str(shallow)],
        capture_output=True, text=True,
    )
    assert r.returncode == 0, r.stderr
    assert subprocess.run(
        ["git", "rev-parse", "--is-shallow-repository"], cwd=shallow,
        capture_output=True, text=True,
    ).stdout.strip() == "true", "fixture is not actually shallow"

    # THE ASSERTION. Not None here means the boundary was taken from a history
    # that does not contain the add commit.
    assert pp.mechanism_landed_at(str(shallow)) is None

    # POSITIVE CONTROL — the same repo, full: one variable different.
    full = pp.mechanism_landed_at(str(src))
    assert full is not None
    assert full.year == 2026 and full.month == 8 and full.day == 10, full


def test_a_shallow_checkout_cannot_widen_the_amnesty_end_to_end(tmp_path, monkeypatch):
    """The consequence, driven through audit()'s OWN default resolution.

    Fixture: a hand promote with no stamp, 15 days AFTER the true landing
    instant. Before the fix this returned exit 0 / grandfathered: 1 on a shallow
    checkout while returning exit 1 on a full one — same row, same fixture.
    """
    src = _fixture_repo(tmp_path)
    shallow = tmp_path / "shallow"
    subprocess.run(["git", "clone", "-q", "--depth", "1", src.as_uri(), str(shallow)], check=True)

    pins = tmp_path / "pins.json"
    pins.write_text(json.dumps({"pins": [{
        "template_name": "molecule-tenant", "region": "global",
        "image_digest": FRESH_DIGEST, "git_sha": "deadbee",
        "promoted_at": "2026-08-25T12:00:00Z", "promoted_by": "api-token-x",
        "notes": "prod pinned-promote from staging-proven"}]}), encoding="utf-8")

    for root in (src, shallow):
        monkeypatch.chdir(root)
        buf = io.StringIO()
        rc = pp.audit([("cp_prod", str(pins))], gitea_url=pp.DEFAULT_GITEA,
                      token=None, online=False, out=buf)
        out = buf.getvalue()
        assert rc == 1, f"{root.name}: {out}"
        assert "UNSTAMPED" in out, f"{root.name}: {out}"
        assert "GRANDFATHERED" not in out, f"{root.name}: {out}"


def test_a_later_touch_does_not_move_the_boundary(tmp_path):
    """MD3, driven behaviourally instead of asserted textually.

    The source-string assertion elsewhere would not catch a semantically
    equivalent rewrite. This one edits the file in a real repo, commits, and
    checks the boundary did not move — which is the property, not the spelling.
    """
    src = _fixture_repo(tmp_path, later_commits=1)
    before = pp.mechanism_landed_at(str(src))
    assert before is not None

    env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", HOME=str(tmp_path),
               GIT_AUTHOR_DATE="2026-09-05T12:00:00+00:00",
               GIT_COMMITTER_DATE="2026-09-05T12:00:00+00:00")
    f = src / ".gitea" / "scripts" / "pin_provenance.py"
    f.write_text(f.read_text(encoding="utf-8") + "\n# a later, unrelated edit\n", encoding="utf-8")
    subprocess.run(["git", "add", "-A"], cwd=src, env=env, check=True)
    subprocess.run(["git", "commit", "-q", "-m", "touch the mechanism"], cwd=src, env=env, check=True)

    after = pp.mechanism_landed_at(str(src))
    assert after == before, f"the boundary moved {before} -> {after}"
