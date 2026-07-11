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

    def write_approvers(self, *logins):
        directory = tempfile.TemporaryDirectory()
        path = pathlib.Path(directory.name) / "approvers.txt"
        path.write_text("\n".join(logins) + "\n", encoding="utf-8")
        self.addCleanup(directory.cleanup)
        return path

    def test_pm_approved_label_from_authorized_actor_enables_override(self):
        path = self.write_event(
            {
                "repository": {"full_name": "molecule-ai/molecule-core"},
                "pull_request": {
                    "number": 123,
                    "labels": [{"name": "diff-guard:pm-approved"}],
                }
            }
        )
        timeline = [
            {
                "id": 10,
                "type": "label",
                "label": {"name": "diff-guard:pm-approved"},
                "user": {"login": "product-owner"},
            }
        ]

        self.assertTrue(
            pr_diff_guard.protected_deletion_override_active(
                path,
                timeline_fetcher=lambda _event: timeline,
                approvers_path=self.write_approvers(
                    "product-owner", "product-manager"
                ),
            )
        )

    def test_pm_approved_label_from_unauthorized_actor_fails_closed(self):
        path = self.write_event(
            {
                "repository": {"full_name": "molecule-ai/molecule-core"},
                "pull_request": {
                    "number": 123,
                    "labels": [{"name": "diff-guard:pm-approved"}],
                },
            }
        )
        timeline = [
            {
                "id": 10,
                "type": "label",
                "label": {"name": "diff-guard:pm-approved"},
                "user": {"login": "release-bot"},
            }
        ]

        self.assertFalse(
            pr_diff_guard.protected_deletion_override_active(
                path,
                timeline_fetcher=lambda _event: timeline,
                approvers_path=self.write_approvers("product-owner"),
            )
        )

    def test_label_removal_after_authorized_application_fails_closed(self):
        path = self.write_event(
            {
                "repository": {"full_name": "molecule-ai/molecule-core"},
                "pull_request": {
                    "number": 123,
                    "labels": [{"name": "diff-guard:pm-approved"}],
                },
            }
        )
        timeline = [
            {
                "id": 10,
                "type": "label",
                "label": {"name": "diff-guard:pm-approved"},
                "user": {"login": "product-owner"},
            },
            {
                "id": 11,
                "type": "unlabel",
                "label": {"name": "diff-guard:pm-approved"},
                "user": {"login": "product-owner"},
            },
        ]

        self.assertFalse(
            pr_diff_guard.protected_deletion_override_active(
                path,
                timeline_fetcher=lambda _event: timeline,
                approvers_path=self.write_approvers("product-owner"),
            )
        )

    def test_timeline_api_failure_fails_closed(self):
        path = self.write_event(
            {
                "repository": {"full_name": "molecule-ai/molecule-core"},
                "pull_request": {
                    "number": 123,
                    "labels": [{"name": "diff-guard:pm-approved"}],
                },
            }
        )

        def fail_fetch(_event):
            raise OSError("timeline unavailable")

        self.assertFalse(
            pr_diff_guard.protected_deletion_override_active(
                path,
                timeline_fetcher=fail_fetch,
                approvers_path=self.write_approvers("product-owner"),
            )
        )

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
