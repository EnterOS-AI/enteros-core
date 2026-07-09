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


def test_accepts_provider_agnostic_fleet_gate(tmp_path: Path):
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
          e2e-smoke:
            needs: [redeploy-fleet]
            steps:
              - run: go test -tags staging_e2e ./internal/staginge2e/
          rollback-pin:
            needs: [advance-pin, redeploy-fleet, e2e-smoke]
            if: always()
            steps:
              - run: |
                  bash scripts/deploy/advance-staging-tenant-pin.sh --image "$OLD_IMAGE"
                  bash scripts/deploy/redeploy-staging-fleet.sh --image "$OLD_IMAGE"
        """,
    )

    result = run_lint(workflow)

    assert result.returncode == 0, result.stdout
    assert "provider-agnostic" in result.stdout


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
    assert "`e2e-smoke` does not (transitively) `needs:` `redeploy-fleet`" in result.stdout
