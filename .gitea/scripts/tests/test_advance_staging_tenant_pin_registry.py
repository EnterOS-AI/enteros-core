"""Guards for DIGEST_SOURCE=registry in advance-staging-tenant-pin.sh.

WHY. The PRODUCTION promote lane must reach the prod control planes on
loopback, which only the `local-deploy` runner pods can do. Their label set is
exactly ['local-deploy'] and they have NO docker socket. `runs-on:` is an AND
over labels, so `[docker-host, local-deploy]` matched zero of the 29 registered
runners and the lane could never be scheduled — its one and only run (624317)
sat at started_at=1970-01-01T00:00:00Z until it was cancelled.

Relabelling cannot fix that: dropping `local-deploy` lands the job on a robot-1
runner with no loopback route to the prod CP, and dropping `docker-host` lands
it on a pod with no docker socket. Removing the docker DEPENDENCY is the fix, so
these tests exist to keep it removed.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts" / "deploy" / "advance-staging-tenant-pin.sh"


def _fake_curl_cp_only(bindir: Path, digest: str, git_sha: str) -> None:
    """A curl stub answering only the CP pin read (the SKIP_SSOT_WRITE=1 path)."""
    curl = bindir / "curl"
    payload = (
        '{"pins":[{"template_name":"molecule-tenant","region":"global",'
        f'"image_digest":"{digest}","git_sha":"{git_sha}"}}]}}'
    )
    curl.write_text(
        "#!/bin/sh\ncat <<'JSON'\n" + payload + "\nJSON\n",
        encoding="utf-8",
    )
    curl.chmod(0o755)


def _resolver(repo_root: Path, body: str) -> None:
    d = repo_root / ".gitea" / "scripts"
    d.mkdir(parents=True, exist_ok=True)
    (d / "registry-manifest-state.py").write_text(body, encoding="utf-8")


def _run(env_extra: dict, tmp_path: Path, path_value: str) -> subprocess.CompletedProcess:
    env = os.environ.copy()
    env.update(
        {
            "PATH": path_value,
            "CP_ADMIN_API_TOKEN": "test-token",
            "CP_BASE_URL": "https://prod-cp.test",
            "TENANT_IMAGE_NAME": "registry.test/molecule-tenant",
            "GITHUB_OUTPUT": str(tmp_path / "github-output"),
            "SKIP_SSOT_WRITE": "1",
        }
    )
    env.pop("DIGEST_SOURCE", None)
    env.pop("REG_USER", None)
    env.pop("REG_TOKEN", None)
    env.update(env_extra)
    return subprocess.run(
        ["bash", str(SCRIPT), "--tag", "staging-deadbee"],
        capture_output=True,
        text=True,
        env=env,
        check=False,
    )


def test_registry_digest_source_needs_no_docker_at_all(tmp_path: Path):
    """DIGEST_SOURCE=registry must resolve with NO docker binary on PATH.

    A docker stub that simply is never called would pass even if the code still
    called docker behind a fallback. Removing docker from PATH entirely is the
    only honest proof that this path is docker-free.
    """
    digest = "sha256:" + "c" * 64
    repo_root = tmp_path / "root"
    _resolver(
        repo_root,
        "import os, sys\n"
        'assert os.environ.get("REG_USER"), "REG_USER not passed through"\n'
        'assert os.environ.get("REG_TOKEN"), "REG_TOKEN not passed through"\n'
        'assert sys.argv[1] == "registry.test/molecule-tenant:staging-deadbee", sys.argv[1]\n'
        f'print("{digest}")\n',
    )

    binbox = tmp_path / "bin"
    binbox.mkdir()
    _fake_curl_cp_only(binbox, digest, "deadbeef")
    (binbox / "python3").write_text(
        f'#!/bin/sh\nexec "{sys.executable}" "$@"\n', encoding="utf-8"
    )
    (binbox / "python3").chmod(0o755)
    for tool in ("sh", "sed", "grep", "tr", "xargs", "head", "cat", "env",
                 "mktemp", "wc", "rm", "printf", "dirname", "cut"):
        src = shutil.which(tool)
        if src:
            try:
                (binbox / tool).symlink_to(src)
            except OSError:
                shutil.copy2(src, binbox / tool)

    assert shutil.which("docker", path=str(binbox)) is None, (
        "the test PATH must not expose docker, or this proves nothing"
    )

    r = _run(
        {
            "DIGEST_SOURCE": "registry",
            "REG_USER": "ci",
            "REG_TOKEN": "reg-token",
            "REPO_ROOT": str(repo_root),
            "GITHUB_SHA": "deadbeef1234567890abcdef1234567890abcdef",
        },
        tmp_path,
        str(binbox),
    )
    assert r.returncode == 0, f"rc={r.returncode}\nSTDOUT={r.stdout}\nSTDERR={r.stderr}"
    out = (tmp_path / "github-output").read_text(encoding="utf-8")
    assert f"new_digest={digest}" in out, out


def test_registry_absent_tag_fails_loud_and_never_promotes(tmp_path: Path):
    """Resolver exit 10 (tag absent) must abort, not resolve an empty digest.

    Promoting a pin to a tag the registry does not serve is exactly how prod was
    left pointing at `staging-7d78136`, a 404 on both registry hosts.
    """
    repo_root = tmp_path / "root"
    _resolver(repo_root, "import sys\nsys.exit(10)\n")
    binbox = tmp_path / "bin"
    binbox.mkdir()
    _fake_curl_cp_only(binbox, "sha256:" + "b" * 64, "cafebabe")

    r = _run(
        {
            "DIGEST_SOURCE": "registry",
            "REG_USER": "ci",
            "REG_TOKEN": "reg-token",
            "REPO_ROOT": str(repo_root),
        },
        tmp_path,
        f"{binbox}{os.pathsep}{os.environ['PATH']}",
    )
    assert r.returncode != 0, "an absent registry tag must fail the promote"
    assert "tag absent" in r.stderr, r.stderr


def test_registry_source_requires_registry_credentials(tmp_path: Path):
    """Missing REG_USER/REG_TOKEN must fail closed, never fall back to docker."""
    repo_root = tmp_path / "root"
    _resolver(repo_root, "print('sha256:' + 'd'*64)\n")
    binbox = tmp_path / "bin"
    binbox.mkdir()
    _fake_curl_cp_only(binbox, "sha256:" + "b" * 64, "cafebabe")

    r = _run(
        {"DIGEST_SOURCE": "registry", "REPO_ROOT": str(repo_root)},
        tmp_path,
        f"{binbox}{os.pathsep}{os.environ['PATH']}",
    )
    assert r.returncode != 0
    assert "REG_USER is required" in r.stderr, r.stderr


def test_an_unknown_digest_source_is_rejected(tmp_path: Path):
    """A typo must fail closed rather than silently taking the docker path."""
    repo_root = tmp_path / "root"
    _resolver(repo_root, "print('sha256:' + 'd'*64)\n")
    binbox = tmp_path / "bin"
    binbox.mkdir()
    _fake_curl_cp_only(binbox, "sha256:" + "b" * 64, "cafebabe")

    r = _run(
        {"DIGEST_SOURCE": "registries", "REPO_ROOT": str(repo_root)},
        tmp_path,
        f"{binbox}{os.pathsep}{os.environ['PATH']}",
    )
    assert r.returncode != 0
    assert "DIGEST_SOURCE must be" in r.stderr, r.stderr


def test_default_digest_source_is_still_docker(tmp_path: Path):
    """The staging lane's proven docker path must be untouched by default.

    Test the DEFAULT, not just the parameter: with DIGEST_SOURCE unset the
    script must still pull+inspect, so this change cannot silently re-route the
    staging lane onto an unproven path.
    """
    digest = "sha256:" + "e" * 64
    binbox = tmp_path / "bin"
    binbox.mkdir()
    pulled = tmp_path / "pulled"
    docker = binbox / "docker"
    docker.write_text(
        "#!/bin/sh\n"
        f'if [ "$1" = "pull" ]; then : > "{pulled}"; exit 0; fi\n'
        f'if [ "$1" = "image" ] && [ "$2" = "inspect" ] && [ -f "{pulled}" ]; '
        f'then echo "{digest}"; exit 0; fi\n'
        "exit 1\n",
        encoding="utf-8",
    )
    docker.chmod(0o755)
    _fake_curl_cp_only(binbox, digest, "deadbeef")

    r = _run({}, tmp_path, f"{binbox}{os.pathsep}{os.environ['PATH']}")
    assert r.returncode == 0, f"STDOUT={r.stdout}\nSTDERR={r.stderr}"
    assert pulled.exists(), "the default path must still docker pull"
