"""Tests for `.gitea/scripts/detect-changes.py`."""

from __future__ import annotations

import importlib.util
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[1]
SCRIPT = REPO_ROOT / ".gitea" / "scripts" / "detect-changes.py"


def load_module():
    spec = importlib.util.spec_from_file_location("detect_changes", SCRIPT)
    assert spec is not None
    module = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(module)
    return module


def test_ci_profile_classifies_surfaces():
    """The `ci` profile is a DENY-LIST: a lane runs unless the change is
    PROVABLY INERT for it. It used to be an allow-list, and this test used to
    assert the allow-list's central bug as correct behaviour (see the
    `.gitea/` case at the bottom).

    Inert set is deliberately tiny — INERT_PROSE = ("docs/", r".*\\.md$") — plus
    per-lane exclusions:
        platform = deny_list()                             # nothing else is inert
        canvas   = deny_list("workspace-server/")
        scripts  = deny_list("workspace-server/", "canvas/")
        python   = ^workspace/                             # still an allow-list
    An allow-list is vacuous-by-omission by construction: forget a path and the
    gate silently does not run. A deny-list can only ever be wrong in the safe
    direction (running a lane that need not have run).
    """
    mod = load_module()

    assert mod.classify("ci", ["workspace-server/internal/handlers/a2a_proxy.go"]) == {
        "platform": True,
        "canvas": False,   # canvas excludes workspace-server/
        "python": False,   # ^workspace/ does NOT match "workspace-server/"
        "scripts": False,  # scripts excludes workspace-server/
    }
    # canvas -> platform is TRUE, and that is not an over-fire: the Go suite reads
    # a canvas golden fixture (workspace-server/internal/handlers/
    # external_connection_test.go), so a canvas edit really can red the Go lane.
    assert mod.classify("ci", ["canvas/src/app/page.tsx"]) == {
        "platform": True,
        "canvas": True,
        "python": False,
        "scripts": False,  # scripts excludes canvas/
    }
    assert mod.classify("ci", ["tests/e2e/test_model_slug.sh"]) == {
        "platform": True,
        "canvas": True,
        "python": False,
        "scripts": True,
    }
    # ── C2 REGRESSION LOCK ────────────────────────────────────────────────
    # This case previously asserted {platform: False, canvas: False, python:
    # False, scripts: False} — i.e. "a PR touching ONLY the CI machinery
    # triggers NOTHING" — written down as the expected result and kept green.
    # That is finding C2: every heavy job then took its no-op arm and
    # `CI / all-required` reported SUCCESS having run ZERO tests. The change
    # most capable of breaking CI was asserted to run no CI, and the test suite
    # was DEFENDING the hole. (`^scripts/` is anchored, so it never matched
    # `.gitea/scripts/`; nothing in the old allow-list matched `.gitea/` at all.)
    #
    # README.md alone is inert — but a change is classified by its NON-inert
    # members, so the ci.yml edit must light the lanes up.
    assert mod.classify("ci", [".gitea/workflows/ci.yml", "README.md"]) == {
        "platform": True,
        "canvas": True,
        "python": False,
        "scripts": True,
    }
    # NEGATIVE CONTROL for the deny-list itself. Without this, `deny_list()`
    # could degenerate to `.*` — "always true" — and every assertion above would
    # still pass while the profile had stopped discriminating entirely. A
    # prose-only change must still be inert on EVERY lane.
    assert mod.classify("ci", ["docs/adr/004-sdk-owns-adapter.md", "README.md"]) == {
        "platform": False,
        "canvas": False,
        "python": False,
        "scripts": False,
    }


def test_handlers_postgres_profile_is_narrower_than_workspace_server():
    mod = load_module()

    assert mod.classify("handlers-postgres", ["workspace-server/internal/handlers/a2a_proxy.go"]) == {
        "handlers": True,
    }
    assert mod.classify("handlers-postgres", ["workspace-server/internal/provisioner/provisioner.go"]) == {
        "handlers": False,
    }


def test_e2e_api_profile_covers_api_inputs():
    mod = load_module()

    assert mod.classify("e2e-api", ["workspace-server/internal/handlers/workspace.go"]) == {
        "api": True,
    }
    assert mod.classify("e2e-api", ["tests/e2e/test_api.sh"]) == {"api": True}
    assert mod.classify("e2e-api", ["canvas/src/app/page.tsx"]) == {"api": False}


def test_fail_open_all_true_for_missing_base():
    mod = load_module()

    assert mod.all_true("ci") == {
        "platform": True,
        "canvas": True,
        "python": True,
        "scripts": True,
    }


def test_fetch_base_prefers_advertised_base_ref(monkeypatch):
    mod = load_module()
    calls: list[list[str]] = []
    exists_checks = 0

    def fake_base_exists(base: str) -> bool:
        nonlocal exists_checks
        exists_checks += 1
        return exists_checks >= 1

    def fake_run_git(args: list[str], *, timeout: int = 30):
        calls.append(args)

        class Result:
            returncode = 0
            stdout = ""
            stderr = ""

        return Result()

    monkeypatch.setattr(mod, "base_exists", fake_base_exists)
    monkeypatch.setattr(mod, "run_git", fake_run_git)

    mod.fetch_base("abc123", "main")

    assert calls == [["fetch", "--depth=1", "origin", "main"]]


def test_fetch_base_falls_back_to_sha_when_ref_fetch_does_not_materialize(monkeypatch):
    mod = load_module()
    calls: list[list[str]] = []

    monkeypatch.setattr(mod, "base_exists", lambda _base: False)

    def fake_run_git(args: list[str], *, timeout: int = 30):
        calls.append(args)

        class Result:
            returncode = 0
            stdout = ""
            stderr = ""

        return Result()

    monkeypatch.setattr(mod, "run_git", fake_run_git)

    mod.fetch_base("abc123", "main")

    assert calls == [
        ["fetch", "--depth=1", "origin", "main"],
        ["fetch", "--depth=1", "origin", "abc123"],
    ]


def test_changed_paths_uses_merge_base_for_pull_request(monkeypatch):
    mod = load_module()
    calls: list[list[str]] = []

    def fake_run_git(args: list[str], *, timeout: int = 30):
        calls.append(args)

        class Result:
            returncode = 0
            stdout = "workspace/agent.py\n"
            stderr = ""

        if args[0] == "merge-base":
            Result.stdout = "merge123\n"
        return Result()

    monkeypatch.setattr(mod, "run_git", fake_run_git)

    assert mod.changed_paths("base123", use_merge_base=True) == ["workspace/agent.py"]
    assert calls == [
        ["merge-base", "base123", "HEAD"],
        ["diff", "--name-only", "merge123", "HEAD"],
    ]


def test_detect_deepens_base_ref_when_pr_merge_base_missing(monkeypatch):
    mod = load_module()
    calls: list[tuple[str, str | None]] = []
    merge_base_calls = 0

    monkeypatch.setattr(mod, "base_exists", lambda _base: True)

    def fake_merge_base(base: str):
        nonlocal merge_base_calls
        merge_base_calls += 1
        if merge_base_calls == 1:
            return None
        return "merge123"

    def fake_deepen_base_ref(base_ref: str):
        calls.append(("deepen", base_ref))

    def fake_changed_paths(base: str, *, use_merge_base: bool):
        calls.append(("changed", str(use_merge_base)))
        return [".gitea/workflows/ci.yml"]

    monkeypatch.setattr(mod, "merge_base", fake_merge_base)
    monkeypatch.setattr(mod, "deepen_base_ref", fake_deepen_base_ref)
    monkeypatch.setattr(mod, "changed_paths", fake_changed_paths)

    # fake_changed_paths returns [".gitea/workflows/ci.yml"] — under the deny-list
    # that lights up every lane for which a CI-machinery edit is not provably
    # inert. This block used to expect all-False, which was C2 again: the very
    # change that can break CI, asserted to run no CI. The point of THIS test is
    # the deepen/merge-base call ORDER below; the classification just has to be
    # the truthful one.
    assert mod.detect("ci", "pull_request", "base123", "", "main") == {
        "platform": True,
        "canvas": True,
        "python": False,   # ^workspace/ — still an allow-list, correctly unmatched
        "scripts": True,
    }
    assert calls == [("deepen", "main"), ("changed", "True")]
