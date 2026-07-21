import os
import subprocess
import textwrap
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
LINT = ROOT / ".gitea" / "scripts" / "lint_staging_tenant_cd_gate_chain.py"


def run_lint(workflow: Path) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    env["STAGING_TENANT_CD_PATH"] = str(workflow)
    return subprocess.run(
        ["python3", str(LINT)],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )


def write_workflow(tmp_path: Path, body: str) -> Path:
    path = tmp_path / "staging-tenant-cd.yml"
    path.write_text(textwrap.dedent(body), encoding="utf-8")
    return path


def test_accepts_guarded_runtime_readiness_fleet_gate(tmp_path: Path):
    workflow = write_workflow(
        tmp_path,
        """
        jobs:
          await-image:
            steps:
              - run: echo image ready
          advance-pin:
            needs: [await-image]
            steps:
              - run: bash scripts/deploy/advance-staging-tenant-pin.sh --tag staging-deadbee
          redeploy-fleet:
            needs: [advance-pin, await-image]
            steps:
              - run: bash scripts/deploy/redeploy-staging-fleet.sh --tag staging-deadbee
          runtime-image-readiness:
            needs: [advance-pin]
            runs-on: local-deploy
            timeout-minutes: 30
            steps:
              - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd
              - run: bash scripts/deploy/require-local-deploy-daemon.sh
              - env:
                  CP_BASE_URL: https://staging-api.moleculesai.app
                  INFISICAL_CLIENT_ID: ${{ secrets.INFISICAL_CI_CLIENT_ID }}
                  INFISICAL_CLIENT_SECRET: ${{ secrets.INFISICAL_CI_CLIENT_SECRET }}
                  INFISICAL_PROJECT_ID: ${{ secrets.INFISICAL_CI_PROJECT_ID }}
                run: bash scripts/deploy/prepare-staging-runtime-images.sh
          e2e-smoke:
            needs: [redeploy-fleet, runtime-image-readiness]
            steps:
              - run: echo "E2E_EXPECT_TENANT_BUILD_SHA=$GITHUB_SHA" >> "$GITHUB_ENV"
              - run: go test -tags staging_e2e ./internal/staginge2e/
          rollback-pin:
            needs: [advance-pin, redeploy-fleet, runtime-image-readiness, e2e-smoke]
            if: always()
            steps:
              - run: |
                  bash scripts/deploy/advance-staging-tenant-pin.sh --image "$OLD_IMAGE"
                  bash scripts/deploy/redeploy-staging-fleet.sh --image "$OLD_IMAGE"
        """,
    )

    result = run_lint(workflow)

    assert result.returncode == 0, result.stdout
    assert "no CP deploy/reload path" in result.stdout


def test_rejects_railway_reload_in_staging_tenant_ci(tmp_path: Path):
    workflow = write_workflow(
        tmp_path,
        """
        jobs:
          await-image:
            steps:
              - run: echo image ready
          advance-pin:
            needs: [await-image]
            steps:
              - run: bash scripts/deploy/advance-staging-tenant-pin.sh --tag staging-deadbee
          reload-cp-candidate:
            needs: [advance-pin, await-image]
            steps:
              - run: npm install -g @railway/cli@4.59.0
              - run: bash scripts/deploy/reload-staging-controlplane.sh --tag staging-deadbee
          redeploy-fleet:
            needs: [reload-cp-candidate]
            steps:
              - run: bash scripts/deploy/redeploy-staging-fleet.sh --tag staging-deadbee
          e2e-smoke:
            needs: [redeploy-fleet]
            steps:
              - run: echo "E2E_EXPECT_TENANT_BUILD_SHA=$GITHUB_SHA" >> "$GITHUB_ENV"
              - run: go test -tags staging_e2e ./internal/staginge2e/
          rollback-pin:
            needs: [advance-pin, reload-cp-candidate, redeploy-fleet, e2e-smoke]
            if: always()
            steps:
              - run: bash scripts/deploy/reload-staging-controlplane.sh --image "$OLD_IMAGE"
        """,
    )

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "reload-cp-candidate" in result.stdout
    assert "Railway" in result.stdout


def test_requires_e2e_after_fleet_roll(tmp_path: Path):
    workflow = write_workflow(
        tmp_path,
        """
        jobs:
          await-image:
            steps:
              - run: echo image ready
          advance-pin:
            needs: [await-image]
            steps:
              - run: echo pin
          redeploy-fleet:
            needs: [advance-pin]
            steps:
              - run: echo roll
          e2e-smoke:
            needs: [advance-pin]
            steps:
              - run: echo e2e
          rollback-pin:
            needs: [advance-pin, redeploy-fleet, e2e-smoke]
            if: always()
            steps:
              - run: echo rollback
        """,
    )

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "`e2e-smoke` does not directly `needs:` `redeploy-fleet`" in result.stdout


def test_requires_e2e_candidate_build_sha_guard(tmp_path: Path):
    workflow = write_workflow(
        tmp_path,
        """
        jobs:
          await-image:
            steps:
              - run: echo image ready
          advance-pin:
            needs: [await-image]
            steps:
              - run: bash scripts/deploy/advance-staging-tenant-pin.sh --tag staging-deadbee
          redeploy-fleet:
            needs: [advance-pin]
            steps:
              - run: bash scripts/deploy/redeploy-staging-fleet.sh --tag staging-deadbee
          e2e-smoke:
            needs: [redeploy-fleet]
            steps:
              - run: go test -tags staging_e2e ./internal/staginge2e/
          rollback-pin:
            needs: [advance-pin, redeploy-fleet, e2e-smoke]
            if: always()
            steps:
              - run: echo rollback
        """,
    )

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "E2E_EXPECT_TENANT_BUILD_SHA" in result.stdout
    assert "candidate-SHA guard" in result.stdout


def readiness_workflow(*, runner="local-deploy", action=None, rollback_readiness=True):
    action = action or "bash scripts/deploy/prepare-staging-runtime-images.sh"
    rollback_needs = "advance-pin, redeploy-fleet, runtime-image-readiness, e2e-smoke"
    if not rollback_readiness:
        rollback_needs = "advance-pin, redeploy-fleet, e2e-smoke"
    return f"""
        jobs:
          await-image:
            steps:
              - run: echo image ready
          advance-pin:
            needs: [await-image]
            steps:
              - run: echo pin
          redeploy-fleet:
            needs: [advance-pin]
            steps:
              - run: echo roll
          runtime-image-readiness:
            needs: [advance-pin]
            runs-on: {runner}
            timeout-minutes: 30
            steps:
              - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd
              - run: bash scripts/deploy/require-local-deploy-daemon.sh
              - env:
                  CP_BASE_URL: https://staging-api.moleculesai.app
                  INFISICAL_CLIENT_ID: ${{{{ secrets.INFISICAL_CI_CLIENT_ID }}}}
                  INFISICAL_CLIENT_SECRET: ${{{{ secrets.INFISICAL_CI_CLIENT_SECRET }}}}
                  INFISICAL_PROJECT_ID: ${{{{ secrets.INFISICAL_CI_PROJECT_ID }}}}
                run: {action}
          e2e-smoke:
            needs: [redeploy-fleet, runtime-image-readiness]
            steps:
              - run: echo "E2E_EXPECT_TENANT_BUILD_SHA=$GITHUB_SHA" >> "$GITHUB_ENV"
              - run: go test -tags staging_e2e ./internal/staginge2e/
          rollback-pin:
            needs: [{rollback_needs}]
            if: always()
            steps:
              - run: echo rollback
    """


def test_requires_runtime_image_readiness_before_e2e(tmp_path: Path):
    workflow = write_workflow(
        tmp_path,
        """
        jobs:
          await-image:
            steps: [{run: echo image ready}]
          advance-pin:
            needs: [await-image]
            steps: [{run: echo pin}]
          redeploy-fleet:
            needs: [advance-pin]
            steps: [{run: echo roll}]
          e2e-smoke:
            needs: [redeploy-fleet]
            steps:
              - run: echo "E2E_EXPECT_TENANT_BUILD_SHA=$GITHUB_SHA" >> "$GITHUB_ENV"
          rollback-pin:
            needs: [advance-pin, redeploy-fleet, e2e-smoke]
            if: always()
            steps: [{run: echo rollback}]
        """,
    )

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "runtime-image-readiness" in result.stdout


def test_runtime_readiness_requires_exact_local_deploy_runner(tmp_path: Path):
    workflow = write_workflow(tmp_path, readiness_workflow(runner="docker-host"))

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "must run on exactly `local-deploy`" in result.stdout


def test_runtime_readiness_action_is_canonical(tmp_path: Path):
    workflow = write_workflow(tmp_path, readiness_workflow(action="true"))

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "prepare-staging-runtime-images.sh" in result.stdout


def test_runtime_readiness_cannot_redirect_to_production_cp(tmp_path: Path):
    body = readiness_workflow().replace(
        "CP_BASE_URL: https://staging-api.moleculesai.app",
        "CP_BASE_URL: https://api.moleculesai.app",
    )
    workflow = write_workflow(tmp_path, body)

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "exact staging CP + Infisical SSOT mapping" in result.stdout


def test_runtime_readiness_cannot_skip_daemon_guard(tmp_path: Path):
    body = readiness_workflow().replace(
        "bash scripts/deploy/require-local-deploy-daemon.sh", "true"
    )
    workflow = write_workflow(tmp_path, body)

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "require-local-deploy-daemon.sh" in result.stdout


def test_rollback_directly_covers_runtime_readiness(tmp_path: Path):
    workflow = write_workflow(tmp_path, readiness_workflow(rollback_readiness=False))

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "runtime-image-readiness" in result.stdout


def test_runtime_readiness_cannot_mask_failure_with_expression(tmp_path: Path):
    body = readiness_workflow().replace(
        "          runtime-image-readiness:\n",
        "          runtime-image-readiness:\n"
        "            continue-on-error: ${{ vars.MASK_RUNTIME_PULL_FAILURE }}\n",
    )
    workflow = write_workflow(tmp_path, body)

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "continue-on-error key" in result.stdout


def test_always_bridge_cannot_replace_direct_readiness_gate(tmp_path: Path):
    body = readiness_workflow().replace(
        "          e2e-smoke:\n"
        "            needs: [redeploy-fleet, runtime-image-readiness]\n",
        "          readiness-bridge:\n"
        "            needs: [runtime-image-readiness]\n"
        "            if: always()\n"
        "            steps: [{run: echo bridge}]\n"
        "          e2e-smoke:\n"
        "            needs: [redeploy-fleet, readiness-bridge]\n",
    )
    workflow = write_workflow(tmp_path, body)

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "does not directly `needs:` `runtime-image-readiness`" in result.stdout


def test_runtime_readiness_cannot_override_daemon_endpoint_at_job_scope(tmp_path: Path):
    body = readiness_workflow().replace(
        "          runtime-image-readiness:\n            needs: [advance-pin]\n",
        "          runtime-image-readiness:\n"
        "            needs: [advance-pin]\n"
        "            env:\n"
        "              DOCKER_HOST: tcp://redirect.example.test:2376\n"
        "              MOLECULE_PROD_DOCKER_HOST: tcp://redirect.example.test:2376\n",
        1,
    )
    workflow = write_workflow(tmp_path, body)

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "execution schema" in result.stdout


def test_workflow_defaults_cannot_syntax_check_instead_of_execute_guard(tmp_path: Path):
    body = readiness_workflow().replace(
        "        jobs:\n",
        "        defaults:\n"
        "          run:\n"
        "            shell: bash -n {0}\n"
        "        jobs:\n",
    )
    workflow = write_workflow(tmp_path, body)

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "defaults.run.shell" in result.stdout


def test_workflow_env_cannot_replace_runner_owned_daemon_endpoint(tmp_path: Path):
    body = readiness_workflow().replace(
        "        jobs:\n",
        "        env:\n"
        "          DOCKER_HOST: tcp://redirect.example.test:2376\n"
        "          MOLECULE_PROD_DOCKER_HOST: tcp://redirect.example.test:2376\n"
        "        jobs:\n",
    )
    workflow = write_workflow(tmp_path, body)

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "trusted Docker endpoint values" in result.stdout


def test_e2e_cannot_override_failed_needs_with_always_condition(tmp_path: Path):
    body = readiness_workflow().replace(
        "          e2e-smoke:\n"
        "            needs: [redeploy-fleet, runtime-image-readiness]\n",
        "          e2e-smoke:\n"
        "            needs: [redeploy-fleet, runtime-image-readiness]\n"
        "            if: always()\n",
    )
    workflow = write_workflow(tmp_path, body)

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "must use the default success-only condition" in result.stdout


def test_await_image_cannot_mask_failure(tmp_path: Path):
    body = readiness_workflow().replace(
        "          await-image:\n",
        "          await-image:\n            continue-on-error: false\n",
    )
    workflow = write_workflow(tmp_path, body)

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "`await-image` has a continue-on-error key" in result.stdout
