"""Unit tests for check_migration_collisions.py — focuses on the regex
classifier + the diff/base-set logic that runs without git.

The end-to-end git diff + gh pr list path is exercised manually (running
the workflow against test PRs). These tests pin the pure-logic surface
so a regression in migration-name parsing fails immediately at PR time.

Run locally: ``python3 -m unittest scripts/ops/test_check_migration_collisions.py -v``
"""

import importlib.util
import unittest
from pathlib import Path

# Load the script as a module without invoking main(). We import the
# regex + helpers directly so we can test them without setting up git.
SCRIPT_PATH = Path(__file__).parent / "check_migration_collisions.py"
spec = importlib.util.spec_from_file_location("ccm", SCRIPT_PATH)
ccm = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ccm)


class TestMigrationFileRe(unittest.TestCase):
    """The regex classifier — the load-bearing piece of the detector."""

    def test_matches_standard_three_digit_prefix(self):
        m = ccm.MIGRATION_FILE_RE.match("044_platform_inbound_secret.up.sql")
        assert m is not None
        assert int(m.group(1)) == 44
        assert m.group(2) == "up"

    def test_matches_down_migration(self):
        m = ccm.MIGRATION_FILE_RE.match("044_platform_inbound_secret.down.sql")
        assert m is not None
        assert int(m.group(1)) == 44
        assert m.group(2) == "down"

    def test_matches_date_shaped_prefix(self):
        # Real example from the repo: 20260417000000_workflow_checkpoints
        m = ccm.MIGRATION_FILE_RE.match("20260417000000_workflow_checkpoints.up.sql")
        assert m is not None
        assert int(m.group(1)) == 20260417000000

    def test_matches_long_compound_name(self):
        m = ccm.MIGRATION_FILE_RE.match("042_a2a_queue.up.sql")
        assert m is not None
        assert int(m.group(1)) == 42

    def test_rejects_no_prefix(self):
        assert ccm.MIGRATION_FILE_RE.match("readme.md") is None

    def test_rejects_alpha_prefix(self):
        assert ccm.MIGRATION_FILE_RE.match("abc_migration.up.sql") is None

    def test_rejects_wrong_extension(self):
        assert ccm.MIGRATION_FILE_RE.match("044_test.sql") is None
        assert ccm.MIGRATION_FILE_RE.match("044_test.up.txt") is None

    def test_rejects_path_separator(self):
        # Filename only — paths come pre-split via Path(line).name
        assert ccm.MIGRATION_FILE_RE.match("044/test.up.sql") is None

    def test_rejects_no_underscore(self):
        # Naming convention requires <digits>_<name>
        assert ccm.MIGRATION_FILE_RE.match("044.up.sql") is None


class TestGiteaGetUserAgent(unittest.TestCase):
    """Regression: _gitea_get must send a browser-legit User-Agent.

    Without it the Cloudflare edge 1010-bans the default python-urllib UA,
    _gitea_get returns None, and the cross-PR migration-collision arm silently
    fails OPEN (two PRs with the same prefix both pass). Found by the
    2026-07-02 CF-1010 sweep alongside gate-check-v3 (core#3397).
    """

    def test_gitea_get_sets_browser_user_agent(self):
        import io
        import urllib.request as _u

        captured = {}

        class _Resp(io.BytesIO):
            def __enter__(self):
                return self

            def __exit__(self, *a):
                return False

        def fake_urlopen(req, timeout=None):
            captured["req"] = req
            return _Resp(b"{}")

        orig = _u.urlopen
        _u.urlopen = fake_urlopen
        try:
            ccm._gitea_get("repos/molecule-ai/molecule-core")
        finally:
            _u.urlopen = orig

        ua = captured["req"].get_header("User-agent")
        assert ua == "molecule-ci-gate/1.0 (+gitea-api)", (
            f"expected browser-legit UA, got {ua!r}"
        )


class TestOpenPrPeerContainedInHead(unittest.TestCase):
    """Regression: a merge-train PR carries the queued PR's commits, and the
    queued PR is still open while the train runs. Its migration is the same
    change, not a race, so it must not count as a collision. Before the fix
    every train of a migration-bearing PR went red here (core#5308,
    2026-11-22: eight train attempts, all `Migration version collision
    check` failures against #5308 itself). A peer whose head is NOT in this
    PR's head (a genuine race), or whose head cannot be resolved, still
    counts.
    """

    MIG = "workspace-server/migrations/20261121120000_x.up.sql"

    def setUp(self):
        import json
        self._json = json
        self._orig_get = ccm._gitea_get
        self._orig_anc = ccm.is_ancestor

    def tearDown(self):
        ccm._gitea_get = self._orig_get
        ccm.is_ancestor = self._orig_anc

    def _fake_api(self, peers):
        def fake_get(path, params=None):
            if path.endswith("/pulls"):
                return self._json.dumps([
                    {"number": n, "head": {"ref": f"b{n}", "sha": sha}}
                    for n, sha in peers
                ]).encode()
            return self._json.dumps([{"filename": self.MIG}]).encode()
        ccm._gitea_get = fake_get

    def test_peer_contained_in_head_is_not_a_collision(self):
        self._fake_api([(5308, "aaa")])
        ccm.is_ancestor = lambda sha, ref: sha == "aaa" and ref == "TRAIN"
        got = ccm.open_prs_with_migration_prefix(
            "o/r", 20261121120000, exclude_pr=5331, head_ref="TRAIN")
        assert got == [], got

    def test_peer_that_contains_head_is_not_a_collision(self):
        # The mirror case: the queued PR's own check re-runs while a train PR
        # that carries it is open.
        self._fake_api([(5331, "train")])
        ccm.is_ancestor = lambda sha, ref: sha == "PRHEAD" and ref == "train"
        got = ccm.open_prs_with_migration_prefix(
            "o/r", 20261121120000, exclude_pr=5308, head_ref="PRHEAD")
        assert got == [], got

    def test_peer_not_contained_is_still_a_collision(self):
        self._fake_api([(5400, "bbb")])
        ccm.is_ancestor = lambda sha, ref: False
        got = ccm.open_prs_with_migration_prefix(
            "o/r", 20261121120000, exclude_pr=5331, head_ref="TRAIN")
        assert [p["number"] for p in got] == [5400], got

    def test_peer_without_head_sha_is_still_a_collision(self):
        self._fake_api([(5400, "")])
        ccm.is_ancestor = lambda sha, ref: True
        got = ccm.open_prs_with_migration_prefix(
            "o/r", 20261121120000, exclude_pr=5331, head_ref="TRAIN")
        assert [p["number"] for p in got] == [5400], got

    def test_is_ancestor_on_a_real_repo(self):
        import subprocess
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            def git(*a):
                return subprocess.run(
                    ["git", "-C", d, "-c", "user.name=t", "-c", "user.email=t@t", *a],
                    check=True, capture_output=True, text=True).stdout.strip()
            git("init", "-q")
            git("commit", "-q", "--allow-empty", "-m", "a")
            a = git("rev-parse", "HEAD")
            git("commit", "-q", "--allow-empty", "-m", "b")
            b = git("rev-parse", "HEAD")
            cwd = __import__("os").getcwd()
            __import__("os").chdir(d)
            try:
                assert ccm.is_ancestor(a, b) is True
                assert ccm.is_ancestor(b, a) is False
                # unknown object: fail closed (not contained)
                assert ccm.is_ancestor("0" * 40, b) is False
            finally:
                __import__("os").chdir(cwd)


if __name__ == "__main__":
    unittest.main()
