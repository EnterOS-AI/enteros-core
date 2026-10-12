import os
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts" / "deploy" / "reload-staging-controlplane.sh"


def run_script(*args: str, provider: str | None = None) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    env.pop("CONTROLPLANE_DEPLOY_PROVIDER", None)
    env.pop("MOLECULE_CONTROLPLANE_DEPLOY_PROVIDER", None)
    if provider is not None:
        env["CONTROLPLANE_DEPLOY_PROVIDER"] = provider
    return subprocess.run(
        ["bash", str(SCRIPT), *args],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )


def test_default_provider_is_external_noop():
    result = run_script("--tag", "staging-deadbee", "--dry-run")

    assert result.returncode == 0, result.stdout
    assert "CONTROLPLANE_DEPLOY_PROVIDER=none" in result.stdout
    assert "TARGET_IMAGE=registry.moleculesai.app/molecule-ai/molecule-tenant:staging-deadbee" in result.stdout
    assert "Railway" not in result.stdout


def test_invalid_provider_fails_closed():
    result = run_script("--tag", "staging-deadbee", provider="bogus")

    assert result.returncode == 2
    assert "unsupported CONTROLPLANE_DEPLOY_PROVIDER=bogus" in result.stdout


def test_retired_railway_provider_fails_closed():
    result = run_script("--tag", "staging-deadbee", "--dry-run", provider="railway")

    assert result.returncode == 2
    assert "unsupported CONTROLPLANE_DEPLOY_PROVIDER=railway" in result.stdout
    assert "supported providers: none, external, ci-on-merge" in result.stdout
