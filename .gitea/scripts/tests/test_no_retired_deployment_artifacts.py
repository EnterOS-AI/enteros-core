"""Keep retired operator-host and cloud-specific deployment paths out."""

import subprocess
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
        "scripts/cleanup-rogue-workspaces.sh",
        "scripts/build-images.sh",
        "scripts/test-a2a-cross-runtime.sh",
        "scripts/test-all-adapters.sh",
        "scripts/test-cross-agent-chat.sh",
        "scripts/test-hermes-plugin-e2e.sh",
        "scripts/test-team-e2e.sh",
        "scripts/wheel_smoke.py",
        "tests/e2e/test_claude_code_e2e.sh",
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


def test_current_local_smoke_uses_current_registry_example() -> None:
    script = (ROOT / "scripts/local-tenant-smoke.sh").read_text()
    assert "IMAGE=ghcr.io/" not in script
    assert "IMAGE=registry.moleculesai.app/molecule-ai/" in script


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

    env_example = (ROOT / ".env.example").read_text()
    assert "NEVER pass a production tenant ADMIN_TOKEN" in env_example
    assert "Store in fly secrets" not in env_example
    assert "only this value is accepted on /admin/*" not in env_example

    current_auth_surfaces = (
        "canvas/src/lib/api.ts",
        "canvas/next.config.ts",
        "workspace-server/internal/middleware/wsauth_middleware.go",
        "docs/design/rfc-selfhost-onboarding-scene.md",
    )
    stale_claims = (
        "the canvas now always sends a bearer",
        "canvas always sends a bearer",
        "AdminAuth deliberately fails open",
        "Set both to the same value, or unset both",
    )
    stale = [
        f"{path}: {claim}"
        for path in current_auth_surfaces
        for claim in stale_claims
        if claim in (ROOT / path).read_text()
    ]
    assert not stale, (
        "current auth guidance must keep the public local-dev bearer separate "
        "from production session auth; found: " + ", ".join(stale)
    )


def test_current_build_and_ci_comments_match_gitea_public_template_reality() -> None:
    build_surfaces = (
        "workspace-server/Dockerfile",
        "workspace-server/Dockerfile.tenant",
    )
    absolute_private_claims = [
        path
        for path in build_surfaces
        if "every workspace-template" in (ROOT / path).read_text()
    ]
    assert not absolute_private_claims, (
        "public template repositories must not be documented as universally "
        "private; stale files: " + ", ".join(absolute_private_claims)
    )

    stale_ci_path = ".github/workflows/handlers-postgres-integration.yml"
    stale_go_comments = [
        str(path.relative_to(ROOT))
        for path in (ROOT / "workspace-server").rglob("*.go")
        if stale_ci_path in path.read_text()
    ]
    assert not stale_go_comments, (
        "current Go test comments must point to the active Gitea workflow; "
        "stale files: " + ", ".join(stale_go_comments)
    )


def test_current_deployment_comments_do_not_overclaim_rollout() -> None:
    canvas_publish = (
        ROOT / ".gitea/workflows/publish-canvas-image.yml"
    ).read_text()
    tenant_publish = (
        ROOT / ".gitea/workflows/publish-workspace-server-image.yml"
    ).read_text()
    local_redeploy = (
        ROOT / ".gitea/workflows/redeploy-tenants-on-main.yml"
    ).read_text()

    assert "github.event.inputs.platform_url" not in canvas_publish
    assert "github.event.inputs.ws_url" not in canvas_publish
    assert "No post-promotion tenant redeploy is guaranteed" in tenant_publish
    assert "guarantees at least one redeploy" not in tenant_publish
    assert "fresh production provisions now resolve" not in tenant_publish
    assert "Production tenants now run as local Docker containers" not in local_redeploy
    assert "not an inventory or rollout of the entire production tenant set" in local_redeploy


def test_active_skill_and_runtime_guidance_matches_current_contracts() -> None:
    skill_guide = (ROOT / "docs/guides/skill-catalog.md").read_text()
    contributing = (ROOT / "CONTRIBUTING.md").read_text()
    selfhost_rfc = (
        ROOT / "docs/design/rfc-selfhost-onboarding-scene.md"
    ).read_text()

    fictional_commands = (
        "molecule skills install",
        "molecule skills upgrade",
        "molecule skills list",
        "molecule skills init",
        "molecule skills bundle",
        "molecule skills uninstall",
    )
    assert not any(command in skill_guide for command in fictional_commands)
    assert "scripts/*.py" in skill_guide
    assert "does **not** import a `tools/` directory" in skill_guide
    assert "MOLECULE_WORKSPACES`" in contributing
    assert "MOLECULE_WORKSPACES_JSON`" in contributing
    assert "there is no `MOLECULE_WORKSPACES`" not in contributing
    assert "the compiled **hermes** fallback" in selfhost_rfc
    assert "MOLECULE_DEFAULT_RUNTIME` else **openclaw**" not in selfhost_rfc


def test_external_push_guidance_requires_fail_closed_inbound_auth() -> None:
    registration = (
        ROOT / "docs/guides/external-agent-registration.md"
    ).read_text()
    quickstart = (
        ROOT / "docs/guides/external-workspace-quickstart.md"
    ).read_text()

    for text in (registration, quickstart):
        assert "platform_inbound_secret" in text
    assert "constant time" in registration
    assert "before reading or dispatching the request body" in registration
    assert "Never start a" in registration
    assert "public listener when the secret is empty" in registration


def test_current_comments_do_not_point_at_retired_runner_or_runtime_tree() -> None:
    workflow_needles = (
        "/opt/molecule/runners/config.yaml",
        "devops-engineer persona",
        "operator-host runners",
        "operator host's docker daemon",
    )
    stale_workflows = [
        f"{path.relative_to(ROOT)}: {needle}"
        for path in (ROOT / ".gitea/workflows").glob("*.yml")
        for needle in workflow_needles
        if needle in path.read_text()
    ]

    runtime_tree_needles = (
        "workspace/adapter_base.py",
        "workspace/a2a_mcp_server.py",
        "workspace/config.py",
        "workspace/entrypoint.sh",
        "workspace/executor_helpers.py",
        "workspace/heartbeat.py",
        "workspace/inbox.py",
        "workspace/internal_chat_uploads",
        "workspace/main.py",
        "workspace/preflight.py",
        "workspace/scripts/molecule-git-token-helper.sh",
    )
    production_go = [
        path
        for path in (ROOT / "workspace-server").rglob("*.go")
        if not path.name.endswith("_test.go")
    ]
    stale_runtime_refs = [
        f"{path.relative_to(ROOT)}: {needle}"
        for path in production_go
        for needle in runtime_tree_needles
        if needle in path.read_text()
    ]

    stale = stale_workflows + stale_runtime_refs
    assert not stale, (
        "current workflow/code comments must describe the self-hosted runner "
        "and standalone molecule-ai-workspace-runtime layout; found: "
        + ", ".join(stale)
    )


def test_canonical_cross_runtime_smoke_uses_current_human_auth() -> None:
    script = (ROOT / "scripts/test-all-runtimes-a2a-e2e.sh").read_text()
    assert 'LOCAL_ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:-${ADMIN_TOKEN:-}}"' in script
    assert (
        'EXTRA_HEADERS+=("-H" "Authorization: Bearer $LOCAL_ADMIN_TOKEN")'
        in script
    )
    assert 'EXTRA_HEADERS+=("-H" "Authorization: Bearer $TENANT_ADMIN_TOKEN")' in script
    assert 'TENANT_ADMIN_TOKEN is required for a hosted tenant' in script
    assert 'ADMIN_TOKEN or MOLECULE_ADMIN_TOKEN is required for localhost' in script
    assert (
        '[ -z "${SKIP_HERMES:-}" ] || [ -z "${SKIP_CODEX:-}" ] '
        '|| [ -z "${SKIP_OPENCLAW:-}" ]'
    ) in script
    assert "Provision the five runtimes" not in script

    helper = (ROOT / "tests/e2e/_lib.sh").read_text()
    assert 'E2E_ALLOW_SAME_ORIGIN_FALLBACK:-' in helper
    assert "auth fails closed" in helper
    assert '. "$env_file"' not in helper
    assert '. "$ROOT/.env"' not in script


def test_dotenv_admin_reader_does_not_execute_file(tmp_path: Path) -> None:
    marker = tmp_path / "executed"
    env_file = tmp_path / ".env"
    env_file.write_text(
        f"OTHER=$(touch {marker})\n"
        "export ADMIN_TOKEN=\"literal-admin-value\"\n"
    )
    result = subprocess.run(
        [
            "bash",
            "-c",
            'source tests/e2e/_lib.sh; e2e_read_dotenv_value ADMIN_TOKEN "$1"',
            "dotenv-reader-test",
            str(env_file),
        ],
        cwd=ROOT,
        check=True,
        capture_output=True,
        text=True,
    )
    assert result.stdout == "literal-admin-value"
    assert not marker.exists()


def test_current_e2e_comments_do_not_advertise_removed_auth_bypass() -> None:
    surfaces = tuple((ROOT / "tests/e2e").glob("*.sh")) + (
        ROOT / "docker-compose.yml",
        ROOT / "workspace-server/internal/wsauth/tokens.go",
        ROOT / "workspace-server/internal/handlers/platform_agent.go",
    )
    stale_claims = (
        "devmode fail-open",
        "fail-open dev platform",
        "fresh installs with no tokens fail open",
        "middleware fail-open path activates",
        "AdminAuth's Tier-1 fail-open",
    )
    stale = [
        f"{path.relative_to(ROOT)}: {claim}"
        for path in surfaces
        for claim in stale_claims
        if claim.lower() in path.read_text().lower()
    ]
    assert not stale, (
        "current E2E/bootstrap guidance must reflect fail-closed auth; found: "
        + ", ".join(stale)
    )


def test_seeded_agent_cards_do_not_advertise_retired_operator_host() -> None:
    corrective_name = "20260715090000_refresh_seeded_agent_card_ops_description.up.sql"
    stale = [
        str(path.relative_to(ROOT))
        for path in (ROOT / "workspace-server/migrations").glob("*.up.sql")
        if path.name != corrective_name
        if "Direct hands-on ops on operator host and Neon" in path.read_text()
    ]
    assert not stale, (
        "forward migrations must not seed the retired operator-host workflow: "
        + ", ".join(stale)
    )

    corrective_up = (
        ROOT
        / "workspace-server/migrations/20260715090000_refresh_seeded_agent_card_ops_description.up.sql"
    ).read_text()
    corrective_down = (
        ROOT
        / "workspace-server/migrations/20260715090000_refresh_seeded_agent_card_ops_description.down.sql"
    ).read_text()
    assert corrective_up.count("Direct hands-on ops on operator host and Neon") >= 2
    assert "Intentionally irreversible/no-op" in corrective_down
    assert "UPDATE workspaces" not in corrective_down
    assert "Direct hands-on operations" not in corrective_down


def test_opencode_guide_uses_current_domain_route_and_config_shape() -> None:
    guide = (ROOT / "docs/integrations/opencode.md").read_text()
    env_example = (ROOT / ".env.example").read_text()
    stale = (
        "api.molecule.ai",
        '"mcpServers"',
        "https://$MOLECULE_MCP_URL",
        '"scopes": ["mcp:read", "mcp:delegate"]',
    )
    assert not any(needle in guide for needle in stale)
    assert not any(needle in env_example for needle in stale)
    assert "/admin/workspaces/$WORKSPACE_ID/tokens" in guide
    assert '"mcp": {' in guide
    assert "{env:MOLECULE_MCP_URL}" in guide
