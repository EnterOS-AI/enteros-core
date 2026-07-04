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


if __name__ == "__main__":
    unittest.main()
