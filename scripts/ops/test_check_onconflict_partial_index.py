"""Unit tests for check_onconflict_partial_index.py.

A lint is itself code that can rot into a no-op, so these pin BOTH directions:
it must fire on the real defect (core#4977's queueDriftEntry) and must stay
silent on the correct forms already in the tree — a noisy lint gets ignored,
which is the same outcome as no lint.

Run locally:
  ``python3 -m unittest scripts/ops/test_check_onconflict_partial_index.py -v``
"""

import importlib.util
import tempfile
import unittest
from pathlib import Path

SCRIPT_PATH = Path(__file__).parent / "check_onconflict_partial_index.py"
spec = importlib.util.spec_from_file_location("occ", SCRIPT_PATH)
occ = importlib.util.module_from_spec(spec)
spec.loader.exec_module(occ)

PARTIAL_SQL = """
CREATE UNIQUE INDEX IF NOT EXISTS q_pending_uniq
  ON plugin_update_queue(workspace_id, plugin_name)
  WHERE status = 'pending';
"""

TOTAL_SQL = """
CREATE UNIQUE INDEX IF NOT EXISTS wp_ws_name
  ON workspace_plugins(workspace_id, plugin_name);
"""


class _Fixture(unittest.TestCase):
    def build(self, sql, go_src):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        mig = root / "migrations"
        mig.mkdir()
        (mig / "001_init.up.sql").write_text(sql, encoding="utf-8")
        go = root / "gosrc"
        go.mkdir()
        (go / "x.go").write_text(go_src, encoding="utf-8")
        return mig, go

    def scan(self, sql, go_src):
        mig, go = self.build(sql, go_src)
        return occ.scan_go(go, occ.collect_unique_indexes(mig))


class TestFiresOnTheDefect(_Fixture):
    def test_bare_inference_against_partial_index(self):
        """core#4977 itself: every INSERT was rejected at runtime, silently."""
        v = self.scan(PARTIAL_SQL, 'package p\nconst q = `\n'
                      'INSERT INTO plugin_update_queue (workspace_id, plugin_name)\n'
                      'VALUES ($1,$2)\n'
                      'ON CONFLICT (workspace_id, plugin_name) DO NOTHING\n`\n')
        self.assertEqual(len(v), 1, v)
        self.assertIn("plugin_update_queue", v[0])
        self.assertIn("WHERE status = 'pending'", v[0],
                      "the message must name the predicate so the fix is obvious")

    def test_column_order_does_not_change_the_verdict(self):
        """(a,b) and (b,a) infer the same index."""
        v = self.scan(PARTIAL_SQL, 'package p\nconst q = `\n'
                      'INSERT INTO plugin_update_queue (plugin_name, workspace_id)\n'
                      'VALUES ($1,$2)\n'
                      'ON CONFLICT (plugin_name, workspace_id) DO NOTHING\n`\n')
        self.assertEqual(len(v), 1, v)


class TestStaysSilentWhenCorrect(_Fixture):
    def test_predicate_repeated(self):
        """The fix must clear the lint, or it is unfixable noise."""
        self.assertEqual(self.scan(PARTIAL_SQL, 'package p\nconst q = `\n'
                         'INSERT INTO plugin_update_queue (workspace_id, plugin_name)\n'
                         'VALUES ($1,$2)\n'
                         "ON CONFLICT (workspace_id, plugin_name) WHERE status = 'pending' "
                         'DO NOTHING\n`\n'), [])

    def test_total_unique_index_is_inferable_bare(self):
        """Flagging a non-partial index would be a false positive."""
        self.assertEqual(self.scan(TOTAL_SQL, 'package p\nconst q = `\n'
                         'INSERT INTO workspace_plugins (workspace_id, plugin_name)\n'
                         'VALUES ($1,$2)\n'
                         'ON CONFLICT (workspace_id, plugin_name) DO UPDATE SET '
                         'plugin_name = EXCLUDED.plugin_name\n`\n'), [])

    def test_prose_in_comments_is_not_code(self):
        """Regression: the first version flagged the sweeper's own doc comment,
        reporting the same defect twice at a line nobody could act on."""
        self.assertEqual(self.scan(PARTIAL_SQL, 'package p\n'
                         '// On drift we INSERT INTO plugin_update_queue (ON CONFLICT DO\n'
                         '// NOTHING so a re-drift while pending is a no-op).\n'
                         'const unrelated = 1\n'), [])

    def test_unknown_table_gets_no_opinion(self):
        """No index knowledge => no verdict. The lint must not guess."""
        self.assertEqual(self.scan(PARTIAL_SQL, 'package p\nconst q = `\n'
                         'INSERT INTO some_other_table (a, b) VALUES ($1,$2)\n'
                         'ON CONFLICT (a, b) DO NOTHING\n`\n'), [])


class TestAntiVacuity(unittest.TestCase):
    """If the migration parser breaks it returns nothing and every check passes
    — the exact failure mode this lint exists to catch, turned on itself."""

    def test_real_migrations_yield_partial_indexes(self):
        root = Path(__file__).resolve().parents[2]
        mig = root / "workspace-server" / "migrations"
        if not mig.is_dir():
            self.skipTest("migrations dir not present in this checkout")
        idx = occ.collect_unique_indexes(mig)
        partial = [
            (t, n, p)
            for t, cols in idx.items()
            for _c, lst in cols.items()
            for n, p in lst
            if p is not None
        ]
        self.assertTrue(partial, "parsed ZERO partial unique indexes from the real tree")
        self.assertTrue(
            any(t == "plugin_update_queue" for t, _n, _p in partial),
            "plugin_update_queue_pending_unique missing from the parse — the very "
            "index this lint was written for",
        )

    def test_real_tree_is_currently_clean(self):
        """Guards the fix itself: if someone drops the predicate from
        queueDriftEntry, this fails at PR time instead of in production."""
        root = Path(__file__).resolve().parents[2]
        mig = root / "workspace-server" / "migrations"
        go = root / "workspace-server"
        if not mig.is_dir() or not go.is_dir():
            self.skipTest("workspace-server not present in this checkout")
        self.assertEqual(occ.scan_go(go, occ.collect_unique_indexes(mig)), [])


if __name__ == "__main__":
    unittest.main()
