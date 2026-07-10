import importlib.util
import json
import pathlib
import tempfile
import unittest


SCRIPT_PATH = pathlib.Path(__file__).parents[1] / "pr-diff-guard.py"
SPEC = importlib.util.spec_from_file_location("pr_diff_guard", SCRIPT_PATH)
assert SPEC is not None and SPEC.loader is not None
pr_diff_guard = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(pr_diff_guard)


class ProtectedDeletionOverrideTest(unittest.TestCase):
    def write_event(self, payload):
        directory = tempfile.TemporaryDirectory()
        path = pathlib.Path(directory.name) / "event.json"
        path.write_text(json.dumps(payload), encoding="utf-8")
        self.addCleanup(directory.cleanup)
        return path

    def test_pm_approved_label_enables_override(self):
        path = self.write_event(
            {
                "pull_request": {
                    "labels": [{"name": "diff-guard:pm-approved"}],
                }
            }
        )

        self.assertTrue(pr_diff_guard.protected_deletion_override_active(path))

    def test_unrelated_label_does_not_enable_override(self):
        path = self.write_event(
            {
                "pull_request": {
                    "labels": [{"name": "tier:low"}],
                }
            }
        )

        self.assertFalse(pr_diff_guard.protected_deletion_override_active(path))

    def test_malformed_event_fails_closed(self):
        directory = tempfile.TemporaryDirectory()
        path = pathlib.Path(directory.name) / "event.json"
        path.write_text("not json", encoding="utf-8")
        self.addCleanup(directory.cleanup)

        self.assertFalse(pr_diff_guard.protected_deletion_override_active(path))


if __name__ == "__main__":
    unittest.main()
