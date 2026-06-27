#!/usr/bin/env python3
"""Unit tests for tools/merge_commit_guard.py (core#2641)."""

import unittest

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


if __name__ == "__main__":
    unittest.main()
