from __future__ import annotations

import json
from pathlib import Path
import re

import yaml


ROOT = Path(__file__).resolve().parents[3]
WORKFLOW_PATH = ROOT / ".gitea/workflows/e2e-staging-saas.yml"
ACTIVE_LOCAL_JOBS = (
    "e2e-staging-saas",
    "e2e-staging-platform-boot",
    "e2e-staging-concierge-user-tasks",
    "e2e-staging-concierge-creates-workspace",
)
ACTIVE_HARNESSES = (
    ROOT / "tests/e2e/test_staging_full_saas.sh",
    ROOT / "tests/e2e/test_staging_concierge_e2e.sh",
    ROOT / "tests/e2e/test_staging_concierge_creates_workspace_e2e.sh",
)


def _shell_function(source: str, name: str) -> str:
    match = re.search(rf"(?ms)^{re.escape(name)}\(\) \{{\n(.*?)^\}}$", source)
    assert match is not None, f"missing shell function {name}"
    return match.group(1)


def test_active_local_jobs_do_not_receive_retired_aws_credentials_or_toggles() -> None:
    workflow_source = WORKFLOW_PATH.read_text()
    workflow = yaml.safe_load(workflow_source)
    jobs = workflow["jobs"]

    for job_name in ACTIVE_LOCAL_JOBS:
        job = jobs[job_name]
        serialized = json.dumps(job, sort_keys=True)
        source_match = re.search(
            rf"(?ms)^  {re.escape(job_name)}:\n.*?(?=^  [a-zA-Z0-9_-]+:\n|\Z)",
            workflow_source,
        )
        assert source_match is not None
        active_job_source = source_match.group(0)
        assert job["env"]["E2E_INFRA_BACKEND"] == "local-docker"
        assert "source tests/e2e/lib/cp_purge_receipt.sh" in active_job_source
        assert "e2e_cp_require_staging_origin" in active_job_source
        assert "e2e_cp_fetch_org_roster_json" in active_job_source
        assert "e2e_cp_delete_and_verify_purge" in active_job_source
        assert "discovery inconclusive" in active_job_source
        assert "orgs=$(curl" not in active_job_source
        assert "/cp/admin/tenants/" not in active_job_source
        empty_run_guard = 'if [ -z "${GITHUB_RUN_ID:-}" ]; then'
        assert empty_run_guard in active_job_source
        assert active_job_source.index(empty_run_guard) < active_job_source.index(
            "e2e_cp_fetch_org_roster_json"
        )
        assert "GITHUB_RUN_ID is empty" in active_job_source
        creation_guard = 'created_slug="${E2E_CREATED_SLUG:-}"'
        assert creation_guard in active_job_source
        assert active_job_source.index(creation_guard) < active_job_source.index(
            "e2e_cp_fetch_org_roster_json"
        )
        assert 'created_org_id="${E2E_CREATED_ORG_ID:-}"' in active_job_source
        assert "verified creation identity is unavailable" in active_job_source
        assert "run_id = os.environ['GITHUB_RUN_ID']" in active_job_source
        assert "expected_slug = os.environ['E2E_CREATED_SLUG']" in active_job_source
        assert "expected_org_id = os.environ['E2E_CREATED_ORG_ID']" in active_job_source
        assert "os.environ.get('GITHUB_RUN_ID'" not in active_job_source
        assert "if not run_id:" in active_job_source
        assert "if o.get('slug') == expected_slug" in active_job_source
        assert "prefixes =" not in active_job_source
        assert "datetime.date" not in active_job_source
        assert '"$MOLECULE_CP_URL" "$ADMIN_TOKEN" "$slug" "$created_org_id"' in active_job_source
        for date_wide_fallback in (
            "tuple(f'e2e-{d}-' for d in dates)",
            "tuple(f'e2e-smoke-{d}-platform-' for d in dates)",
            "tuple(f'e2e-cncrg-{d}-' for d in dates)",
            "tuple(f'e2e-cncrg-mk-{d}-' for d in dates)",
        ):
            assert date_wide_fallback not in active_job_source
        for retired_text in (
            "AWS_ACCESS_KEY_ID",
            "AWS_SECRET_ACCESS_KEY",
            "E2E_AWS_LEAK_CHECK",
            "E2E_AWS_TERMINATE_LEAKS",
            "E2E_EC2_ENABLED",
            "one-line knob",
        ):
            assert retired_text not in serialized
            assert retired_text not in active_job_source


def test_active_harnesses_use_exact_cp_purge_proof_without_ec2_false_clean() -> None:
    for path in ACTIVE_HARNESSES:
        source = path.read_text()
        assert "lib/cp_purge_receipt.sh" in source
        assert "e2e_cp_require_local_backend" in source
        assert "e2e_cp_require_staging_origin" in source
        assert "e2e_cp_delete_and_verify_purge" in source
        assert "ORG_CREATION_VERIFIED=0" in source
        assert "ORG_CREATION_VERIFIED=1" in source
        assert 'e2e_cp_validate_org_id "$ORG_ID"' in source
        assert source.index('e2e_cp_validate_org_id "$ORG_ID"') < source.index(
            "ORG_CREATION_VERIFIED=1"
        )
        assert 'e2e_cp_publish_creation_identity "$SLUG" "$ORG_ID"' in source
        assert source.index("ORG_CREATION_VERIFIED=1") < source.index(
            'e2e_cp_publish_creation_identity "$SLUG" "$ORG_ID"'
        )
        assert '"$CP_URL" "$ADMIN_TOKEN" "$SLUG" "$ORG_ID"' in source
        assert "local entry_rc=$?" in source
        assert 'case "$entry_rc"' in source
        assert "lib/aws_leak_check.sh" not in source
        assert "e2e_verify_no_ec2_leaks_for_slug" not in source
        assert "E2E_AWS_LEAK_CHECK" not in source
        assert "E2E_AWS_TERMINATE_LEAKS" not in source
        assert "no orphan org or EC2 resources" not in source

        if path.name in {
            "test_staging_concierge_e2e.sh",
            "test_staging_concierge_creates_workspace_e2e.sh",
        }:
            status_check = 'case "$CREATE_HTTP_CODE" in'
            assert 'CREATE_HTTP_CODE=$(admin_call POST /cp/admin/orgs' in source
            assert '-o "$CREATE_BODYFILE" -w \'%{http_code}\'' in source
            assert status_check in source
            assert source.index(status_check) < source.index("ORG_CREATION_VERIFIED=1")


def test_active_exit_traps_skip_destructive_precreate_cleanup_and_preserve_rc() -> None:
    full_source = (ROOT / "tests/e2e/test_staging_full_saas.sh").read_text()
    full_cleanup = _shell_function(full_source, "cleanup_org")
    full_wrapper = _shell_function(full_source, "cleanup_org_and_bodyfile")
    assert full_cleanup.lstrip().startswith("# Capture upstream exit code")
    assert "local entry_rc=$?" in full_cleanup
    assert "e2e_cp_delete_and_verify_purge" in full_cleanup
    identity_guard = 'if [ "$ORG_CREATION_VERIFIED" != "1" ]; then'
    assert identity_guard in full_cleanup
    assert full_cleanup.index(identity_guard) < full_cleanup.index(
        "e2e_cp_delete_and_verify_purge"
    )
    assert "skipping destructive org teardown" in full_cleanup
    assert 'case "$entry_rc"' in full_cleanup
    assert "local entry_rc=$?" in full_wrapper
    assert 'CREATE_BODYFILE=""' in full_source
    assert full_source.index('CREATE_BODYFILE=""') < full_source.index(
        "trap cleanup_org_and_bodyfile EXIT INT TERM"
    )
    assert 'if [ -n "$CREATE_BODYFILE" ]; then' in full_wrapper
    assert "cleanup_org" in full_wrapper
    assert 'exit "$entry_rc"' in full_wrapper
    assert "trap cleanup_org_and_bodyfile EXIT INT TERM" in full_source

    direct_traps = (
        (ROOT / "tests/e2e/test_staging_concierge_e2e.sh", "cleanup_org"),
        (ROOT / "tests/e2e/test_staging_concierge_creates_workspace_e2e.sh", "cleanup"),
    )
    for path, function_name in direct_traps:
        source = path.read_text()
        cleanup = _shell_function(source, function_name)
        assert cleanup.lstrip().startswith("local entry_rc=$?")
        assert "e2e_cp_delete_and_verify_purge" in cleanup
        assert identity_guard in cleanup
        assert cleanup.index(identity_guard) < cleanup.index(
            "e2e_cp_delete_and_verify_purge"
        )
        assert "skipping destructive org teardown" in cleanup
        assert 'case "$entry_rc"' in cleanup
        assert f"trap {function_name} EXIT INT TERM" in source

    creates_cleanup = _shell_function(
        (ROOT / "tests/e2e/test_staging_concierge_creates_workspace_e2e.sh").read_text(),
        "cleanup",
    )
    assert creates_cleanup.index(identity_guard) < creates_cleanup.index(
        '-X DELETE "$TENANT_URL/workspaces/$WORKER_ID?confirm=true"'
    )


def test_structured_observability_never_marks_skipped_provider_scan_as_pass() -> None:
    source = (
        ROOT / "tests/e2e/test_staging_concierge_creates_workspace_e2e.sh"
    ).read_text()
    assert "obs_step_end zero_leftover_verify pass" not in source
    assert "obs_step_end zero_leftover_verify skip" in source
    assert "CP purge completed and exact org absent" in source


def test_cp_purge_unit_contract_is_wired_into_required_ci() -> None:
    ci = (ROOT / ".gitea/workflows/ci.yml").read_text()
    assert "bash tests/e2e/lib/test_cp_purge_receipt_unit.sh" in ci


def test_staging_runbook_states_the_exact_teardown_proof_boundary() -> None:
    runbook = (ROOT / "tests/e2e/STAGING_SAAS_E2E.md").read_text()
    normalized = " ".join(runbook.split())
    assert "E2E_INFRA_BACKEND=local-docker" in normalized
    assert "exact completed purge audit" in normalized
    assert "creation-returned org ID" in normalized
    assert "delete response is lost during local-Docker network detach" in normalized
    assert (
        "completed purge audit for the same creation-returned slug/org ID" in normalized
    )
    assert "recorded no earlier than that DELETE attempt" in normalized
    assert "missing, stale, or malformed audit remains a hard failure" in normalized
    assert "/cp/admin/tenants/<slug>/boot-events?limit=1" in normalized
    assert "HTTP 404" in normalized
    assert "does not directly enumerate provider resources" in normalized
    assert "molecule-ai/internal#639" in normalized


def test_safety_net_comments_match_exact_published_identity_boundary() -> None:
    source = WORKFLOW_PATH.read_text()
    assert source.count("exact slug/ID pair published after a successful org") == 3
    assert "Best-effort: find any e2e-YYYYMMDD" not in source
    assert "Sweep any e2e-cncrg-YYYYMMDD" not in source
    assert "died before exporting its slug" not in source


def test_ephemeral_shared_harness_caller_uses_explicit_loopback_contract() -> None:
    source = (ROOT / "tests/e2e/ephemeral_cp_happy_path.sh").read_text()
    runbook = (ROOT / "docs/e2e-ephemeral-gate.md").read_text()
    for function_name in ("run_scenario", "run_scenario_concierge_user_tasks"):
        caller = _shell_function(source, function_name)
        assert "E2E_INFRA_BACKEND=local-docker" in caller
        assert "E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1" in caller
    for text in (source, runbook):
        assert "E2E_INFRA_BACKEND=local-docker" in text
        assert "E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1" in text
        assert "E2E_AWS_LEAK_CHECK" not in text
