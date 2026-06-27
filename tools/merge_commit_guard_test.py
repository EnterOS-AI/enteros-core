#!/usr/bin/env python3
"""Unit tests for tools/merge_commit_guard.py (core#2641)."""

import os
import subprocess
import tempfile
import unittest
import urllib.error
from unittest.mock import patch

import merge_commit_guard as guard


class ParsePrNumberTest(unittest.TestCase):
    def test_merge_queue_message(self):
        self.assertEqual(
            guard.pr_number_from_message("Merge PR #3050 via Gitea merge queue"),
            3050,
        )

    def test_regular_merge_message(self):
        self.assertEqual(
            guard.pr_number_from_message(
                "Merge pull request '#3050' (#3050) from molecule-ai/some-branch"
            ),
            3050,
        )

    def test_quoted_number_without_hash(self):
        self.assertEqual(
            guard.pr_number_from_message(
                "Merge pull request '3050' (#3050) from molecule-ai/some-branch"
            ),
            3050,
        )

    def test_fallback_trailing_pr(self):
        self.assertEqual(
            guard.pr_number_from_message(
                "Some manual merge subject (#2641)\n\nBody here."
            ),
            2641,
        )

    def test_no_pr_number(self):
        self.assertIsNone(
            guard.pr_number_from_message("chore: bump version without PR reference")
        )


class MainIntegrationTest(unittest.TestCase):
    """Exercise main() against a real temporary git repository."""

    def setUp(self):
        self._orig_cwd = os.getcwd()
        self.tmp = tempfile.TemporaryDirectory()
        self.repo = self.tmp.name
        self._git("init", "-b", "main")
        self._git("config", "user.email", "guard-test@example.com")
        self._git("config", "user.name", "Guard Test")

        self._write("root.txt", "root")
        self._git("add", "root.txt")
        self._git("commit", "-m", "initial")
        self.initial = self._rev_parse()

        self._git("checkout", "-b", "feature")
        self._write("feature.txt", "feature")
        self._git("add", "feature.txt")
        self._git("commit", "-m", "feature commit")
        self.feature_head = self._rev_parse()

        self._git("checkout", "main")
        self._git(
            "merge",
            "--no-ff",
            "feature",
            "-m",
            "Merge PR #123 via Gitea merge queue",
        )
        self.merge = self._rev_parse()

        os.chdir(self.repo)
        self.env = {
            "GITEA_TOKEN": "test-token",
            "GITHUB_REPOSITORY": "molecule-ai/molecule-core",
            "GITHUB_SERVER_URL": "https://git.moleculesai.app",
            "GITHUB_EVENT_BEFORE": self.initial,
            "GITHUB_EVENT_AFTER": self.merge,
        }

    def tearDown(self):
        os.chdir(self._orig_cwd)
        self.tmp.cleanup()

    def _git(self, *args: str) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            ["git", *args],
            cwd=self.repo,
            capture_output=True,
            text=True,
            check=True,
        )

    def _rev_parse(self) -> str:
        return self._git("rev-parse", "HEAD").stdout.strip()

    def _write(self, name: str, content: str) -> None:
        with open(os.path.join(self.repo, name), "w", encoding="utf-8") as f:
            f.write(content)

    @patch("merge_commit_guard.pr_head_sha")
    def test_passes_when_pr_head_is_ancestor(self, mock_head_sha):
        mock_head_sha.return_value = self.feature_head
        with patch.dict(os.environ, self.env, clear=False):
            self.assertEqual(guard.main(), 0)

    @patch("merge_commit_guard.pr_head_sha")
    def test_fails_closed_when_pr_head_api_errors(self, mock_head_sha):
        # CR2 #14649 regression: an API error must not silently skip the check.
        mock_head_sha.side_effect = urllib.error.URLError("connection refused")
        with patch.dict(os.environ, self.env, clear=False):
            self.assertEqual(guard.main(), 1)


if __name__ == "__main__":
    unittest.main()
