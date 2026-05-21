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
    mod = load_module()

    assert mod.classify("ci", ["workspace-server/internal/handlers/a2a_proxy.go"]) == {
        "platform": True,
        "canvas": False,
        "python": False,
        "scripts": False,
    }
    assert mod.classify("ci", ["canvas/src/app/page.tsx"]) == {
        "platform": False,
        "canvas": True,
        "python": False,
        "scripts": False,
    }
    assert mod.classify("ci", ["tests/e2e/test_model_slug.sh"]) == {
        "platform": False,
        "canvas": False,
        "python": False,
        "scripts": True,
    }
    assert mod.classify("ci", [".gitea/workflows/ci.yml", "README.md"]) == {
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
