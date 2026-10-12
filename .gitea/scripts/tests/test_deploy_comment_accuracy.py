"""Regression guard for active staging/deploy guidance.

These files are operational entry points, not an archive.  Their comments must
describe the live trigger and enforcement graph: shared-staging E2E is an
independent post-merge signal, parked contexts are not presence-required by the
merge queue, and an emitted red status can still block through branch
protection.
"""

from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[3]


def _text(relative_path: str) -> str:
    return (REPO_ROOT / relative_path).read_text(encoding="utf-8")


def _assert_absent(relative_path: str, stale_claims: tuple[str, ...]) -> None:
    text = _text(relative_path)
    for claim in stale_claims:
        assert claim not in text, f"{relative_path} still contains stale claim: {claim!r}"


def test_required_context_guidance_uses_presence_not_deploy_gate_semantics() -> None:
    path = ".gitea/required-contexts.txt"
    _assert_absent(
        path,
        (
            "It runs as a DEPLOY GATE",
            "post-merge DEPLOY gate",
            "runs live on push/dispatch as the deploy gate",
            "DOCUMENTED-required, NOT merge-blocking",
            "RED on main",
        ),
    )
    text = _text(path)
    assert "not presence-required by the merge queue" in text
    assert "an emitted red still blocks" in text


def test_concierge_harness_does_not_claim_live_per_pr_or_prod_promotion_gate() -> None:
    _assert_absent(
        "tests/e2e/test_staging_concierge_creates_workspace_e2e.sh",
        (
            "runs PER-PR",
            'required "E2E Staging Concierge Creates Workspace" context',
            "blocks the prod promote",
            "On push / dispatch / cron",
            "push-to-main / dispatch / cron",
        ),
    )


def test_full_saas_harness_does_not_describe_retired_cron_ec2_or_required_state() -> None:
    _assert_absent(
        "tests/e2e/test_staging_full_saas.sh",
        (
            "push/dispatch/cron",
            "On push / dispatch / cron",
            "push-to-main / dispatch / cron",
            "#48 made it merge-blocking",
            "remaining surface is real-infra (EC2 cold boot, CF DNS) latency",
            "STILL BLOCKS making it REQUIRED",
            "for the cron canary",
            "scheduled synthetic E2E",
        ),
    )


def test_production_redeploy_does_not_claim_staging_e2e_is_an_upstream_gate() -> None:
    path = ".gitea/workflows/redeploy-tenants-on-main.yml"
    _assert_absent(
        path,
        (
            "enforced upstream by the staging gate",
            "blocks the prod promote",
        ),
    )
    assert "independent of the staging E2E workflows" in _text(path)
