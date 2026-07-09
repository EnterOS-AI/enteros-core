import os
import subprocess
import textwrap
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts" / "deploy" / "redeploy-staging-fleet.sh"


def write_fake_bin(tmp_path: Path, git_sha: str) -> Path:
    bindir = tmp_path / "bin"
    bindir.mkdir()
    docker_log = tmp_path / "docker.log"
    (bindir / "docker").write_text(
        textwrap.dedent(
            f"""\
            #!/usr/bin/env bash
            set -euo pipefail
            echo "$@" >> "{docker_log}"
            case "${{1:-}}" in
              info) exit 0;;
              pull) exit 0;;
              image)
                [ "${{2:-}}" = "inspect" ] && exit 0
                ;;
              ps)
                printf 'mol-tenant-canary\\n'
                exit 0
                ;;
              volume)
                [ "${{2:-}}" = "ls" ] && exit 0
                ;;
              inspect)
                name="${{2:-}}"
                fmt="${{4:-}}"
                case "$fmt" in
                  *NetworkSettings.Networks*) printf 'molecule-net\\n';;
                  *RestartPolicy.Name*) printf 'unless-stopped\\n';;
                  *Config.Env*) printf 'MOLECULE_ENV=staging\\n';;
                  *Config.Labels*) printf 'molecule.local-tenant=1\\nmolecule.cp-env=staging\\n';;
                  *HostConfig.ExtraHosts*) ;;
                  *Config.Image*) printf 'registry/old:staging-old\\n';;
                  *) printf 'stub-inspect:%s:%s\\n' "$name" "$fmt";;
                esac
                exit 0
                ;;
              rename|stop|rm|start) exit 0;;
              run)
                printf 'new-container-id\\n'
                exit 0
                ;;
              port)
                printf '127.0.0.1:39001\\n'
                exit 0
                ;;
            esac
            echo "unexpected docker args: $*" >&2
            exit 1
            """
        ),
        encoding="utf-8",
    )
    (bindir / "curl").write_text(
        textwrap.dedent(
            f"""\
            #!/usr/bin/env bash
            set -euo pipefail
            printf '{{"git_sha":"{git_sha}"}}\\n'
            """
        ),
        encoding="utf-8",
    )
    os.chmod(bindir / "docker", 0o755)
    os.chmod(bindir / "curl", 0o755)
    return bindir


def run_script(tmp_path: Path, git_sha: str, tag: str) -> subprocess.CompletedProcess[str]:
    bindir = write_fake_bin(tmp_path, git_sha)
    env = os.environ.copy()
    env["PATH"] = f"{bindir}:{env['PATH']}"
    env["TENANT_IMAGE"] = "registry.example/molecule-tenant"
    env["STAGING_CANVAS_APP_CONTAINER"] = ""
    env["HEALTH_GATE_ATTEMPTS"] = "1"
    env["HEALTH_GATE_SLEEP_SECS"] = "0"
    return subprocess.run(
        ["bash", str(SCRIPT), "--tag", tag],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )


def test_fleet_roll_accepts_buildinfo_matching_staging_tag(tmp_path: Path):
    result = run_script(tmp_path, git_sha="deadbee1234567890", tag="staging-deadbee")

    assert result.returncode == 0, result.stdout
    assert "/buildinfo git_sha=deadbee1234567890 matches deadbee" in result.stdout
    assert "staging fleet + shared canvas app redeploy complete" in result.stdout


def test_fleet_roll_rejects_buildinfo_mismatching_staging_tag(tmp_path: Path):
    result = run_script(tmp_path, git_sha="badcafe1234567890", tag="staging-deadbee")

    assert result.returncode == 1
    assert "git_sha=badcafe1234567890 did not match expected deadbee" in result.stdout
    assert "canary mol-tenant-canary failed" in result.stdout
