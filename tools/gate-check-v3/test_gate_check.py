import importlib.util
import pathlib


SCRIPT = pathlib.Path(__file__).with_name("gate_check.py")


def load_gate_check():
    spec = importlib.util.spec_from_file_location("gate_check", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    assert spec.loader is not None
    spec.loader.exec_module(mod)
    return mod


def test_run_skips_pr_not_targeting_default_branch(monkeypatch):
    mod = load_gate_check()

    def fake_api_get(path):
        assert path == "/repos/molecule-ai/molecule-core/pulls/843"
        return {
            "number": 843,
            "base": {"ref": "staging"},
            "head": {"sha": "84b9ca3a129075b8d5159eda5e678f68be1af20f"},
        }

    monkeypatch.setenv("DEFAULT_BRANCH", "main")
    monkeypatch.setattr(mod, "api_get", fake_api_get)

    result = mod.run("molecule-ai/molecule-core", 843, post_comment=False)

    assert result["verdict"] == "CLEAR"
    assert result["skipped"] is True
    assert "staging" in result["reason"]


def test_signal_1_infra_sre_login_alias_resolved_to_core_devops(monkeypatch):
    """infra-sre posts [devops-agent] APPROVED → engineers gate satisfied via LOGIN_ALIASES."""
    mod = load_gate_check()

    def fake_api_get(path):
        # PR 900 has tier:low label
        if path == "/repos/molecule-ai/molecule-core/pulls/900":
            return {
                "number": 900,
                "labels": [{"name": "tier:low"}],
            }
        raise AssertionError(f"unexpected api_get: {path}")

    def fake_api_list(path):
        if path == "/repos/molecule-ai/molecule-core/issues/900/comments":
            return []
        if path == "/repos/molecule-ai/molecule-core/pulls/900/comments":
            return []
        if path == "/repos/molecule-ai/molecule-core/pulls/900/reviews":
            return [
                {
                    "id": 1,
                    "user": {"login": "infra-sre"},
                    "state": "APPROVED",
                    "submitted_at": "2026-05-13T10:00:00Z",
                }
            ]
        raise AssertionError(f"unexpected api_list: {path}")

    monkeypatch.setattr(mod, "api_get", fake_api_get)
    monkeypatch.setattr(mod, "api_list", fake_api_list)

    result = mod.signal_1_comment_scan(900, "molecule-ai/molecule-core")

    assert result["verdict"] == "CLEAR"
    assert result["signal"] == "agent_tag_comments"
    # infra-sre (aliased to core-devops) should satisfy engineers gate
    engineers = result["results"]["core-devops"]
    assert engineers["verdict"] == "APPROVED"
    assert engineers["group"] == "engineers"


def test_signal_1_null_user_in_review_does_not_crash(monkeypatch):
    """Regression: Gitea may return reviews with user=null (deleted/bot edge case).
    signal_1_comment_scan must survive this without AttributeError."""
    mod = load_gate_check()

    def fake_api_get(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/901":
            return {
                "number": 901,
                "labels": [{"name": "tier:low"}],
            }
        raise AssertionError(f"unexpected api_get: {path}")

    def fake_api_list(path):
        if path == "/repos/molecule-ai/molecule-core/issues/901/comments":
            return []
        if path == "/repos/molecule-ai/molecule-core/pulls/901/comments":
            return []
        if path == "/repos/molecule-ai/molecule-core/pulls/901/reviews":
            return [
                {
                    "id": 1,
                    "user": None,  # <-- the regression trigger
                    "state": "APPROVED",
                    "submitted_at": "2026-05-13T10:00:00Z",
                },
                {
                    "id": 2,
                    "user": {"login": "core-devops"},
                    "state": "APPROVED",
                    "submitted_at": "2026-05-13T10:01:00Z",
                },
            ]
        raise AssertionError(f"unexpected api_list: {path}")

    monkeypatch.setattr(mod, "api_get", fake_api_get)
    monkeypatch.setattr(mod, "api_list", fake_api_list)

    result = mod.signal_1_comment_scan(901, "molecule-ai/molecule-core")

    # Should not crash; the valid review from core-devops still satisfies engineers gate
    assert result["verdict"] == "CLEAR"
    assert result["results"]["core-devops"]["verdict"] == "APPROVED"


def test_signal_2_draft_request_changes_does_not_block(monkeypatch):
    """official=False REQUEST_CHANGES is a draft/pending review and must NOT
    block the gate (matching review-check.sh post-#1818 official-filter)."""
    mod = load_gate_check()

    def fake_api_list(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/902/reviews":
            return [
                {
                    "id": 1,
                    "user": {"login": "agent-reviewer"},
                    "state": "REQUEST_CHANGES",
                    "official": False,
                    "dismissed": False,
                    "submitted_at": "2026-05-13T10:00:00Z",
                }
            ]
        raise AssertionError(f"unexpected api_list: {path}")

    monkeypatch.setattr(mod, "api_list", fake_api_list)

    result = mod.signal_2_reviews(902, "molecule-ai/molecule-core")
    assert result["verdict"] == "CLEAR"
    assert result["blocking_reviews"] == []


def test_signal_2_null_user_in_request_changes_does_not_crash(monkeypatch):
    """Regression: Gitea may return user=null on a REQUEST_CHANGES review.
    signal_2_reviews must survive this without AttributeError."""
    mod = load_gate_check()

    def fake_api_list(path):
        if path == "/repos/molecule-ai/molecule-core/pulls/903/reviews":
            return [
                {
                    "id": 1,
                    "user": None,
                    "state": "REQUEST_CHANGES",
                    "official": True,
                    "dismissed": False,
                    "submitted_at": "2026-05-13T10:00:00Z",
                }
            ]
        raise AssertionError(f"unexpected api_list: {path}")

    monkeypatch.setattr(mod, "api_list", fake_api_list)

    result = mod.signal_2_reviews(903, "molecule-ai/molecule-core")
    assert result["verdict"] == "CLEAR"
    assert result["blocking_reviews"] == []
