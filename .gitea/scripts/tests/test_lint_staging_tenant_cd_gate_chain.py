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


# ---------------------------------------------------------------------------
# ran-sentinel fixtures
# ---------------------------------------------------------------------------
# The lint now also requires that every job whose `result` is consumed as a
# verdict proves it ACTUALLY RAN (see scripts/deploy/ran-sentinel.sh). These
# snippets are the minimum wiring a fixture needs to get PAST that rule, so the
# pre-existing gate-shape assertions below still test what they were written to
# test. Behaviour of the sentinel itself is proven by executing the real step
# bodies in .gitea/scripts/tests/test_ran_sentinel.py.
SENTINEL_OUTPUTS = """            outputs:
              ran_begin: ${{ steps.ran_begin.outputs.token }}
              ran_end: ${{ steps.ran_end.outputs.token }}
"""
SENTINEL_BEGIN = """              - id: ran_begin
                run: echo begin
"""
SENTINEL_END = """              - id: ran_end
                if: always()
                run: echo end
"""
ROLLBACK_AUDIT_JOB = """          rollback-audit:
            needs: [advance-pin, redeploy-fleet, runtime-image-readiness, e2e-smoke, rollback-pin]
            if: always()
            steps:
              - env:
                  B: ${{ needs.rollback-pin.outputs.ran_begin }}
                  E: ${{ needs.rollback-pin.outputs.ran_end }}
                run: echo audit
"""
ROLLBACK_SENTINEL_READS = """              - env:
                  REDEPLOY_BEGIN: ${{ needs.redeploy-fleet.outputs.ran_begin }}
                  REDEPLOY_END: ${{ needs.redeploy-fleet.outputs.ran_end }}
                  READINESS_BEGIN: ${{ needs.runtime-image-readiness.outputs.ran_begin }}
                  READINESS_END: ${{ needs.runtime-image-readiness.outputs.ran_end }}
                  E2E_BEGIN: ${{ needs.e2e-smoke.outputs.ran_begin }}
                  E2E_END: ${{ needs.e2e-smoke.outputs.ran_end }}
                run: . scripts/deploy/ran-sentinel.sh
"""


def test_accepts_guarded_runtime_readiness_fleet_gate(tmp_path: Path):
    """The full canonical shape, including the ran-sentinel wiring, passes."""
    workflow = write_workflow(tmp_path, readiness_workflow())

    result = run_lint(workflow)

    assert result.returncode == 0, result.stdout
    assert "no CP deploy/reload path" in result.stdout
    assert "ran-sentinel" in result.stdout


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
{SENTINEL_OUTPUTS}            steps:
{SENTINEL_BEGIN}              - run: echo roll
{SENTINEL_END}          runtime-image-readiness:
            needs: [advance-pin]
            runs-on: {runner}
            timeout-minutes: 30
{SENTINEL_OUTPUTS}            steps:
{SENTINEL_BEGIN}              - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd
              - run: bash scripts/deploy/require-local-deploy-daemon.sh
              - env:
                  CP_BASE_URL: https://staging-api.moleculesai.app
                  INFISICAL_BASE: https://key.moleculesai.app
                  INFISICAL_ENV: staging
                  INFISICAL_CLIENT_ID: ${{{{ secrets.INFISICAL_CI_CLIENT_ID }}}}
                  INFISICAL_CLIENT_SECRET: ${{{{ secrets.INFISICAL_CI_CLIENT_SECRET }}}}
                  INFISICAL_PROJECT_ID: ${{{{ secrets.INFISICAL_CI_PROJECT_ID }}}}
                run: {action}
{SENTINEL_END}          e2e-smoke:
            needs: [redeploy-fleet, runtime-image-readiness]
{SENTINEL_OUTPUTS}            steps:
{SENTINEL_BEGIN}              - run: echo "E2E_EXPECT_TENANT_BUILD_SHA=$GITHUB_SHA" >> "$GITHUB_ENV"
              - run: go test -tags staging_e2e ./internal/staginge2e/
{SENTINEL_END}          rollback-pin:
            needs: [{rollback_needs}]
            if: always()
{SENTINEL_OUTPUTS}            steps:
{SENTINEL_BEGIN}{ROLLBACK_SENTINEL_READS}{SENTINEL_END}{ROLLBACK_AUDIT_JOB}    """


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


def test_workflow_env_cannot_redirect_infisical_credentials(tmp_path: Path):
    body = readiness_workflow().replace(
        "        jobs:\n",
        "        env:\n"
        "          INFISICAL_BASE: https://credential-sink.example.test\n"
        "          INFISICAL_ENV: prod\n"
        "        jobs:\n",
    )
    workflow = write_workflow(tmp_path, body)

    result = run_lint(workflow)

    assert result.returncode == 1
    assert "forbidden execution/endpoint key" in result.stdout


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


# ---------------------------------------------------------------------------
# ran-sentinel structural rules
# ---------------------------------------------------------------------------
# These prove the new rules can FAIL. The lint is only the structural half —
# that the wiring exists and cannot be quietly deleted or defanged. The
# BEHAVIOUR (a missing sentinel suppresses the rollback, reports loudly, and is
# non-vacuous on an empty token) is proven by executing the real step bodies in
# .gitea/scripts/tests/test_ran_sentinel.py.


def test_rejects_a_job_with_no_begin_marker(tmp_path: Path):
    body = readiness_workflow().replace(
        "              - id: ran_begin\n                run: echo begin\n"
        '              - run: echo "E2E_EXPECT_TENANT_BUILD_SHA=$GITHUB_SHA"',
        '              - run: echo "E2E_EXPECT_TENANT_BUILD_SHA=$GITHUB_SHA"',
    )
    result = run_lint(write_workflow(tmp_path, body))

    assert result.returncode == 1, result.stdout
    assert "ran-sentinel BEGIN" in result.stdout
    assert "e2e-smoke" in result.stdout


def test_rejects_a_begin_marker_that_is_not_the_first_step(tmp_path: Path):
    """A step ahead of BEGIN could fail and forge a phantom."""
    body = readiness_workflow().replace(
        "              - id: ran_begin\n"
        "                run: echo begin\n"
        "              - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd",
        "              - uses: actions/checkout@de0fac2e4500dabe0009e67214ff5f5447ce83dd\n"
        "              - id: ran_begin\n"
        "                run: echo begin",
    )
    result = run_lint(write_workflow(tmp_path, body))

    assert result.returncode == 1, result.stdout
    assert "FIRST step" in result.stdout


def test_rejects_a_conditional_begin_marker(tmp_path: Path):
    """A false `if:` on BEGIN would forge a phantom on demand."""
    body = readiness_workflow().replace(
        "              - id: ran_begin\n                run: echo begin\n",
        "              - id: ran_begin\n"
        "                if: github.event_name == 'never'\n"
        "                run: echo begin\n",
        1,
    )
    result = run_lint(write_workflow(tmp_path, body))

    assert result.returncode == 1, result.stdout
    assert "unconditional" in result.stdout


def test_rejects_an_end_marker_that_is_not_always(tmp_path: Path):
    """Without always(), a genuine FAILURE emits no END and looks like a phantom.

    That is the one direction the design may not take: it would SUPPRESS a
    rollback that is genuinely owed.
    """
    body = readiness_workflow().replace(
        "              - id: ran_end\n                if: always()\n",
        "              - id: ran_end\n                if: success()\n",
        1,
    )
    result = run_lint(write_workflow(tmp_path, body))

    assert result.returncode == 1, result.stdout
    assert "always()" in result.stdout
    assert "SUPPRESSES" in result.stdout


def test_rejects_a_sentinel_no_consumer_can_read(tmp_path: Path):
    body = readiness_workflow().replace(SENTINEL_OUTPUTS, "", 1)
    result = run_lint(write_workflow(tmp_path, body))

    assert result.returncode == 1, result.stdout
    assert "outputs:" in result.stdout


def test_rejects_a_rollback_that_decides_from_result_alone(tmp_path: Path):
    """The exact pre-sentinel shape: `result` consumed with no proof it ran."""
    body = readiness_workflow().replace(ROLLBACK_SENTINEL_READS, "              - run: echo rollback\n", 1)
    result = run_lint(write_workflow(tmp_path, body))

    assert result.returncode == 1, result.stdout
    assert "never reads" in result.stdout
    assert "phantom-red" in result.stdout


def test_rejects_a_rollback_that_reimplements_the_classifier(tmp_path: Path):
    body = readiness_workflow().replace(
        "                run: . scripts/deploy/ran-sentinel.sh\n",
        "                run: echo rollback\n",
        1,
    )
    result = run_lint(write_workflow(tmp_path, body))

    assert result.returncode == 1, result.stdout
    assert "ran-sentinel.sh" in result.stdout


def test_rejects_removing_the_rollback_auditor(tmp_path: Path):
    """rollback-pin's own sentinel needs a consumer or it proves nothing."""
    body = readiness_workflow().replace(ROLLBACK_AUDIT_JOB, "", 1)
    result = run_lint(write_workflow(tmp_path, body))

    assert result.returncode == 1, result.stdout
    assert "rollback-audit" in result.stdout
    assert "885373" in result.stdout


def test_rejects_an_auditor_that_audits_nothing(tmp_path: Path):
    body = readiness_workflow().replace(
        "                  B: ${{ needs.rollback-pin.outputs.ran_begin }}\n",
        "                  B: unused\n",
        1,
    )
    result = run_lint(write_workflow(tmp_path, body))

    assert result.returncode == 1, result.stdout
    assert "audit nothing" in result.stdout
