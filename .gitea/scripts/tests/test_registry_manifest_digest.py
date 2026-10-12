import hashlib
import os
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / ".gitea" / "scripts" / "registry-manifest-digest.sh"


def run_digest(
    tmp_path: Path,
    docker_body: str,
    *,
    max_bytes: int | None = None,
    ref: str = "registry.moleculesai.app/molecule-ai/image:staging-deadbee",
) -> subprocess.CompletedProcess[str]:
    bindir = tmp_path / "bin"
    bindir.mkdir()
    docker = bindir / "docker"
    docker.write_text(docker_body, encoding="utf-8")
    docker.chmod(0o755)
    env = os.environ.copy()
    env["PATH"] = f"{bindir}:{env['PATH']}"
    env["CALLED_FILE"] = str(tmp_path / "docker-called")
    if max_bytes is not None:
        env["REGISTRY_MANIFEST_MAX_BYTES"] = str(max_bytes)
    return subprocess.run(
        ["bash", str(SCRIPT), ref],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )


def test_reports_sha256_of_exact_registry_manifest_bytes(tmp_path: Path) -> None:
    manifest = b'{"schemaVersion":2,"config":{"digest":"sha256:abc"}}'
    result = run_digest(
        tmp_path,
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        "[ \"$1 $2 $3 $5\" = \"buildx imagetools inspect --raw\" ]\n"
        f"printf '%s' '{manifest.decode()}'\n",
    )

    assert result.returncode == 0, result.stderr
    assert result.stdout.strip() == "sha256:" + hashlib.sha256(manifest).hexdigest()


def test_fails_when_registry_inspect_fails(tmp_path: Path) -> None:
    result = run_digest(tmp_path, "#!/usr/bin/env bash\nexit 23\n")

    assert result.returncode != 0
    assert "could not read registry manifest" in result.stderr


def test_fails_on_empty_manifest(tmp_path: Path) -> None:
    result = run_digest(tmp_path, "#!/usr/bin/env bash\nexit 0\n")

    assert result.returncode != 0
    assert "empty registry manifest" in result.stderr


def test_bounds_registry_manifest_output_before_hashing(tmp_path: Path) -> None:
    result = run_digest(
        tmp_path,
        "#!/usr/bin/env bash\nprintf '0123456789abcdef'\n",
        max_bytes=8,
    )

    assert result.returncode != 0
    assert "exceeded 8-byte response bound" in result.stderr


def test_rejects_noncanonical_registry_before_docker_can_use_credentials(
    tmp_path: Path,
) -> None:
    result = run_digest(
        tmp_path,
        "#!/usr/bin/env bash\ntouch \"$CALLED_FILE\"\nprintf '{}'\n",
        ref="attacker.invalid/molecule-ai/image:tag",
    )

    assert result.returncode != 0
    assert not (tmp_path / "docker-called").exists()
