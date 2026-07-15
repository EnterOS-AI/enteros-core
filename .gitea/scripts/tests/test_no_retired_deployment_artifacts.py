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
        "scripts/install-workspace-runtime.sh",
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
    installer = (ROOT / "scripts/install-workspace-runtime.sh").read_text()
    requirements = (ROOT / "tests/harness/requirements.txt").read_text()
    requirement_entries = [
        line.strip()
        for line in requirements.splitlines()
        if line.strip() and not line.lstrip().startswith("#")
    ]
    executable_replay_lines = [
        line
        for text in (replay_workflow, installer)
        for line in text.splitlines()
        if not line.lstrip().startswith("#")
    ]
    assert not any("--extra-index-url" in line for line in executable_replay_lines)
    assert "--extra-index-url" not in requirements
    assert "bash scripts/install-workspace-runtime.sh" in replay_workflow
    assert 'PRIVATE_INDEX="https://git.moleculesai.app/api/packages/molecule-ai/pypi/simple/"' in installer
    assert "pip download --no-deps" in installer
    assert '"molecules-workspace-runtime==${RUNTIME_VERSION}"' in installer
    assert '"$PYTHON_BIN" -m pip install --index-url "$PUBLIC_INDEX" "${wheels[0]}"' in installer
    assert not any("molecules-workspace-runtime" in line for line in requirement_entries)


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


def test_tenant_canvas_does_not_bake_admin_token_into_public_js() -> None:
    tenant_build_surfaces = (
        "workspace-server/Dockerfile.tenant",
        ".gitea/workflows/publish-workspace-server-image.yml",
    )
    leaked = []
    for path in tenant_build_surfaces:
        executable_lines = [
            line
            for line in (ROOT / path).read_text().splitlines()
            if line.strip() and not line.lstrip().startswith("#")
        ]
        if any("NEXT_PUBLIC_ADMIN_TOKEN" in line for line in executable_lines):
            leaked.append(path)

    assert not leaked, (
        "tenant Canvas builds must authenticate with verified sessions or "
        "deliberately supplied credentials, not a tenant admin secret baked "
        "into public JavaScript; found: " + ", ".join(leaked)
    )

    dev_start = (ROOT / "scripts/dev-start.sh").read_text()
    assert "canvas image baked with the matching NEXT_PUBLIC_ADMIN_TOKEN" not in dev_start
