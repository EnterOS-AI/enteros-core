#!/usr/bin/env python3
"""Offline unit tests for tests/e2e/lib/pin_template_model.py (no live infra).

The helper rewrites, in the e2e tenant bundle only, the default-model lines that
workspace-server's Create reads, so a template-defaulted workspace in the
ephemeral gate runs on the lane's e2e model, never the Carbon platform default.
"""

import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE / "lib"))
import pin_template_model as ptm  # noqa: E402

E2E = "google/gemini-3.1-flash-lite"

HERMES_SHAPE = """runtime: hermes
runtime_config:
  # Default model.
  model: google/gemini-4-hydrogen

  models:
    - id: google/gemini-4-hydrogen
      name: Gemini 4 Hydrogen (Molecule platform)
"""

OPENCLAW_SHAPE = """runtime: openclaw
model: google/gemini-4-hydrogen  # platform default
runtime_config:
  model: google/gemini-4-hydrogen
  models:
  - id: google/gemini-4-hydrogen
"""


class PinTest(unittest.TestCase):
    def test_nested_runtime_config_model(self):
        out, n = ptm.pin(HERMES_SHAPE, E2E)
        self.assertEqual(n, 1)
        self.assertIn("  model: " + E2E + "\n", out)
        # the menu is untouched
        self.assertIn("    - id: google/gemini-4-hydrogen\n", out)

    def test_top_level_and_nested_keep_comments(self):
        out, n = ptm.pin(OPENCLAW_SHAPE, E2E)
        self.assertEqual(n, 2)
        self.assertIn("model: " + E2E + "  # platform default\n", out)
        self.assertIn("  model: " + E2E + "\n", out)
        self.assertNotIn("hydrogen\n  models", out.replace("- id: google/gemini-4-hydrogen", ""))

    def test_only_first_nested_model_is_rewritten(self):
        text = "runtime_config:\n  model: a/b\n  sub:\n    model: c/d\n"
        out, n = ptm.pin(text, E2E)
        self.assertEqual(n, 1)
        self.assertIn("    model: c/d", out)

    def test_blank_line_ends_runtime_config_like_create(self):
        # workspace.go Create's scanner leaves the block on any unindented line,
        # blank lines included, so a model after one is not the default it reads.
        text = "runtime_config:\n\n  model: a/b\n"
        out, n = ptm.pin(text, E2E)
        self.assertEqual(n, 0)
        self.assertEqual(out, text)

    def test_cli_fails_on_missing_template_or_default(self):
        with tempfile.TemporaryDirectory() as d:
            root = Path(d)
            (root / "hermes").mkdir()
            (root / "hermes" / "config.yaml").write_text(HERMES_SHAPE, encoding="utf-8")
            (root / "bare").mkdir()
            (root / "bare" / "config.yaml").write_text("runtime: hermes\n", encoding="utf-8")
            script = str(HERE / "lib" / "pin_template_model.py")
            ok = subprocess.run([sys.executable, script, d, E2E, "hermes"], capture_output=True, text=True)
            self.assertEqual(ok.returncode, 0, ok.stderr)
            self.assertIn("  model: " + E2E, (root / "hermes" / "config.yaml").read_text(encoding="utf-8"))
            missing = subprocess.run([sys.executable, script, d, E2E, "nope"], capture_output=True, text=True)
            self.assertEqual(missing.returncode, 1)
            nodefault = subprocess.run([sys.executable, script, d, E2E, "bare"], capture_output=True, text=True)
            self.assertEqual(nodefault.returncode, 1)


if __name__ == "__main__":
    unittest.main()
