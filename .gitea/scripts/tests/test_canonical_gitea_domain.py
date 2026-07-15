"""Regression guards for domain-only Gitea API access in active CI paths."""

from pathlib import Path

import pytest
import yaml


ROOT = Path(__file__).resolve().parents[3]
CANONICAL_BASE = "https://git.moleculesai.app"
CANONICAL_API = f"{CANONICAL_BASE}/api/v1"
EXACT_USER_AGENT = "curl/8.4.0"


def _workflow() -> dict:
    with (ROOT / ".gitea/workflows/main-canary.yml").open(encoding="utf-8") as f:
        return yaml.safe_load(f)


def _step(name: str) -> dict:
    for step in _workflow()["jobs"]["canary"]["steps"]:
        if step.get("name") == name:
            return step
    raise AssertionError(f"main-canary step not found: {name}")


def _assert_python_edge_request_contract(run: str) -> None:
    request_count = run.count("urllib.request.Request(")
    exact_ua_count = run.count(f'"User-Agent":"{EXACT_USER_AGENT}"')

    assert request_count == 2, "expected Gitea and Sentry Request sites"
    assert exact_ua_count == request_count, "every Request must carry the exact UA"
    assert 'API=(os.environ.get("GITEA_API_URL","").rstrip("/")' in run
    assert f'or "{CANONICAL_API}"' in run


def _assert_ci_status_source_contract(source: str) -> None:
    authfile_ua = 'printf \'user-agent = "%s"\\n\' "$CI_STATUS_UA"'

    assert f"CI_STATUS_UA='{EXACT_USER_AGENT}'" in source
    assert "GITHUB_SERVER_URL" not in source
    assert "molecule-gitea-local" not in source
    assert source.count('"$curl_bin"') == 5
    assert source.count('-A "$CI_STATUS_UA"') == 2
    assert source.count('-K "$authfile"') == 3
    assert source.count(authfile_ua) == 2


def test_main_canary_issue_filer_uses_canonical_api_and_browse_urls() -> None:
    step = _step("File CRITICAL gitea issue on canary failure")
    env = step["env"]
    run = step["run"]

    assert env["GITEA_API_URL"] == CANONICAL_API
    assert env["GITEA_HOST"] == "git.moleculesai.app"
    assert env["RUN_URL"].startswith(f"{CANONICAL_BASE}/")
    assert f'or "{CANONICAL_API}"' in run
    assert "molecule-gitea-local" not in run
    assert "github.api_url" not in str(step)
    assert "github.server_url" not in str(step)


def test_main_canary_infisical_curls_use_exact_user_agent() -> None:
    run = _step("Fetch AUTO_SYNC_TOKEN from Infisical SSOT")["run"]
    curl_lines = [line.strip() for line in run.splitlines() if "curl " in line]

    assert len(curl_lines) == 2, "expected login and secret-read curl calls"
    for line in curl_lines:
        assert f"-A {EXACT_USER_AGENT}" in line, line
    assert 'if [ -z "$AUTO_SYNC_TOKEN" ]' in run
    assert "exit 1" in run


def test_main_canary_python_edge_requests_use_exact_user_agent() -> None:
    run = _step("File CRITICAL gitea issue on canary failure")["run"]

    _assert_python_edge_request_contract(run)


@pytest.mark.parametrize("occurrence", [0, 1])
def test_python_edge_ua_guard_rejects_each_request_mutation(occurrence: int) -> None:
    run = _step("File CRITICAL gitea issue on canary failure")["run"]
    needle = f'"User-Agent":"{EXACT_USER_AGENT}"'
    pieces = run.split(needle)
    assert len(pieces) == 3
    mutated = needle.join(pieces[: occurrence + 1]) + "".join(
        pieces[occurrence + 1 : occurrence + 2]
    )
    if occurrence == 0:
        mutated += needle + pieces[2]

    with pytest.raises(AssertionError):
        _assert_python_edge_request_contract(mutated)


def test_python_edge_override_guard_rejects_wrong_env_lookup() -> None:
    run = _step("File CRITICAL gitea issue on canary failure")["run"]
    mutated = run.replace(
        'os.environ.get("GITEA_API_URL","")',
        'os.environ.get("NONEXISTENT_GITEA_URL","")',
        1,
    )
    assert mutated != run

    with pytest.raises(AssertionError):
        _assert_python_edge_request_contract(mutated)


def test_ci_status_library_uses_exact_user_agent_and_no_ambient_runner_url() -> None:
    source = (ROOT / ".gitea/scripts/lib/ci-status.sh").read_text(encoding="utf-8")

    _assert_ci_status_source_contract(source)


def test_ci_status_authfile_ua_guard_rejects_directive_mutation() -> None:
    source = (ROOT / ".gitea/scripts/lib/ci-status.sh").read_text(encoding="utf-8")
    mutated = source.replace("user-agent =", "referer =", 1)
    assert mutated != source

    with pytest.raises(AssertionError):
        _assert_ci_status_source_contract(mutated)


def test_active_ci_status_comments_do_not_claim_an_internal_operator_path() -> None:
    paths = (
        ".gitea/scripts/lib/ci-status.sh",
        ".gitea/workflows/main-canary.yml",
        ".gitea/workflows/reserved-path-review.yml",
        ".gitea/workflows/secret-scan.yml",
    )
    stale_claims = (
        "internal-host-preferred",
        "internal Gitea host preferred",
        "runner-reachable internal API",
        "runner-pool hardening (operator)",
        "molecule-gitea-local",
        "SSOT lib + contract as qa-review / security-review",
        "the qa/security contract",
        "secret-scan + reserved-path-review are the remaining emit_review_status consumers",
    )

    for relative_path in paths:
        text = (ROOT / relative_path).read_text(encoding="utf-8")
        for claim in stale_claims:
            assert claim not in text, f"{relative_path} still claims {claim!r}"
