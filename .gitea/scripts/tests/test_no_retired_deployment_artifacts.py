"""Keep retired operator-host and cloud-specific deployment paths out."""

from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]


def test_retired_deployment_artifacts_are_absent() -> None:
    retired = (
        "scripts/demo-freeze.sh",
        "scripts/demo-thaw.sh",
        "scripts/demo-day-runbook.md",
        "scripts/ops/ensure-ecr-lifecycle.sh",
        "railway.toml",
        "scripts/promote-tenant-image.sh",
        "scripts/rollback-latest.sh",
        "scripts/staging-smoke.sh",
        "scripts/ops/check-prod-versions.sh",
        "scripts/ops/sweep-aws-secrets.sh",
        "scripts/ops/sweep-cf-orphans.sh",
        ".gitea/workflows/staging-verify.yml",
        ".gitea/workflows/sweep-aws-secrets.yml",
        ".gitea/workflows/sweep-cf-orphans.yml",
        "workspace-server/docs/openapi/management.yaml",
        "workspace-server/internal/imagewatch/watch.go",
        "workspace-server/internal/imagewatch/watch_test.go",
    )

    present = [path for path in retired if (ROOT / path).exists()]
    assert not present, (
        "retired deployment artifacts must stay deleted; "
        f"found: {', '.join(present)}"
    )


def test_retired_image_auto_refresh_wiring_is_absent() -> None:
    surfaces = (
        ".env.example",
        "docker-compose.yml",
        "scripts/test-nuke-and-rebuild.sh",
        "workspace-server/cmd/server/main.go",
    )
    forbidden = ("IMAGE_AUTO_REFRESH", "image-auto-refresh", "internal/imagewatch")

    stale = [
        f"{path}: {needle}"
        for path in surfaces
        for needle in forbidden
        if needle in (ROOT / path).read_text()
    ]
    assert not stale, (
        "the retired background image watcher must not be configured or "
        "advertised; found: " + ", ".join(stale)
    )


def test_runtime_harnesses_use_current_distribution_name() -> None:
    dependency_surfaces = (
        ".gitea/workflows/e2e-api.yml",
        ".gitea/workflows/harness-replays.yml",
        "tests/harness/requirements.txt",
    )

    stale = [
        path
        for path in dependency_surfaces
        if "molecule-ai-workspace-runtime" in (ROOT / path).read_text()
    ]
    assert not stale, (
        "runtime dependencies must install the current "
        "molecules-workspace-runtime distribution; stale files: "
        + ", ".join(stale)
    )

    replay_workflow = (ROOT / ".gitea/workflows/harness-replays.yml").read_text()
    requirements = (ROOT / "tests/harness/requirements.txt").read_text()
    executable_replay_lines = [
        line for line in replay_workflow.splitlines() if not line.lstrip().startswith("#")
    ]
    assert not any("--extra-index-url" in line for line in executable_replay_lines)
    assert "--extra-index-url" not in requirements
    assert "--index-url https://git.moleculesai.app/api/packages/molecule-ai/pypi/simple/" in replay_workflow
    assert "pip download --no-deps" in replay_workflow
    assert '"$RUNTIME_DOWNLOAD"/molecules_workspace_runtime-*.whl' in replay_workflow
    assert "molecules-workspace-runtime" not in requirements


def test_local_e2e_scripts_have_no_retired_checkout_path() -> None:
    scripts = (
        "scripts/test-a2a-cross-runtime.sh",
        "scripts/test-all-adapters.sh",
        "scripts/test-team-e2e.sh",
    )
    stale = [
        path
        for path in scripts
        if "/Users/hongming/Documents/GitHub" in (ROOT / path).read_text()
    ]
    assert not stale, "local E2E scripts must resolve the repo dynamically: " + ", ".join(stale)
