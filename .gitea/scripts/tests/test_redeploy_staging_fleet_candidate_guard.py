import os
import subprocess
import textwrap
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts" / "deploy" / "redeploy-staging-fleet.sh"

# Mixed-generation rtstate volume fixtures (Enter OS rebrand, internal#1089).
# LITERAL strings on purpose — never derived from the script's BRAND_PREFIXES —
# they are the negative controls proving the guard sees BOTH the legacy mol-*
# generation and the enteros-* generation (and ignores unrelated volumes).
VOLS_MIXED = "mol-ws-rtstate-legacy1\\nenteros-ws-rtstate-new1\\nunrelated-cache\\n"
VOLS_MIXED_WITHOUT_ENTEROS = "mol-ws-rtstate-legacy1\\nunrelated-cache\\n"
VOLS_MIXED_WITHOUT_MOL = "enteros-ws-rtstate-new1\\nunrelated-cache\\n"


def write_fake_bin(
    tmp_path: Path,
    git_sha: str,
    tenant_names: str = "mol-tenant-canary\\n",
    vols_before: str = VOLS_MIXED,
    vols_after: str | None = None,
) -> Path:
    bindir = tmp_path / "bin"
    bindir.mkdir()
    docker_log = tmp_path / "docker.log"
    # `docker volume ls` is called twice: the BEFORE snapshot and the AFTER
    # guard. A state file distinguishes the calls so removal of a volume
    # mid-roll can be simulated (vols_after defaults to vols_before = intact).
    vol_state = tmp_path / "volume-ls.called"
    if vols_after is None:
        vols_after = vols_before
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
                printf '{tenant_names}'
                exit 0
                ;;
              volume)
                if [ "${{2:-}}" = "ls" ]; then
                  if [ ! -f "{vol_state}" ]; then
                    : > "{vol_state}"
                    printf '{vols_before}'
                  else
                    printf '{vols_after}'
                  fi
                fi
                exit 0
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
                  *State.Running*) printf 'true\\n';;
                  *) printf 'stub-inspect:%s:%s\\n' "$name" "$fmt";;
                esac
                exit 0
                ;;
              rename|stop|rm|start) exit 0;;
              run)
                printf 'new-container-id\\n'
                exit 0
                ;;
              wait)
                printf '0\\n'
                exit 0
                ;;
              logs)
                printf '{{"git_sha":"{git_sha}"}}\\n'
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


def run_script(
    tmp_path: Path,
    git_sha: str,
    tag: str,
    tenant_names: str = "mol-tenant-canary\\n",
    vols_before: str = VOLS_MIXED,
    vols_after: str | None = None,
) -> subprocess.CompletedProcess[str]:
    bindir = write_fake_bin(
        tmp_path, git_sha,
        tenant_names=tenant_names, vols_before=vols_before, vols_after=vols_after,
    )
    env = os.environ.copy()
    env["PATH"] = f"{bindir}:{env['PATH']}"
    env["TENANT_IMAGE"] = "registry.example/molecule-tenant"
    env["STAGING_CANVAS_APP_CONTAINER"] = ""
    env["HEALTH_GATE_ATTEMPTS"] = "1"
    env["HEALTH_GATE_SLEEP_SECS"] = "0"
    # The script FAILS CLOSED on an unset TENANT_FLAGS: it strips every managed
    # rollout flag from the inherited tenant env and re-applies only what this
    # names, so a caller that never declares it would silently turn those flags off
    # across the fleet. Empty = "all managed flags dark", which is what these tests
    # want. See test_fleet_roll_refuses_to_run_without_declared_flag_state below.
    env["TENANT_FLAGS"] = ""
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
    # Dual-prefix rtstate guard (Enter OS rebrand, internal#1089): the fake
    # daemon lists one mol-ws-rtstate-*, one enteros-ws-rtstate-* and one
    # unrelated volume, so the ONLY correct count is 2 — a single-prefix
    # matcher counts 1 (guard blinded to one generation), an over-broad one 3.
    assert "session (rtstate) volumes present before roll: 2" in result.stdout
    assert "session-preservation OK: all 2 rtstate volume(s) intact after roll" in result.stdout


def test_fleet_roll_rejects_buildinfo_mismatching_staging_tag(tmp_path: Path):
    result = run_script(tmp_path, git_sha="badcafe1234567890", tag="staging-deadbee")

    assert result.returncode == 1
    assert "git_sha=badcafe1234567890 did not match expected deadbee" in result.stdout
    assert "canary mol-tenant-canary failed" in result.stdout


def test_fleet_roll_is_prefix_agnostic_across_mixed_tenant_fleet(tmp_path: Path):
    """A mixed mol-*/enteros-* fleet rolls in full (Enter OS rebrand, internal#1089).

    Tenant discovery is label-driven (molecule.local-tenant / molecule.cp-env),
    so container-name branding must not matter. Both names here are LITERAL
    negative controls: if any name-shaped filter creeps into the enumeration,
    one generation drops out of the roll and this goes red. (enteros-* sorts
    before mol-*, so the enteros container is the canary.)
    """
    result = run_script(
        tmp_path, git_sha="deadbee1234567890", tag="staging-deadbee",
        tenant_names="enteros-tenant-canary\\nmol-tenant-legacy\\n",
    )

    assert result.returncode == 0, result.stdout
    assert "tenants to roll (2): enteros-tenant-canary mol-tenant-legacy" in result.stdout
    assert "enteros-tenant-canary rolled to" in result.stdout
    assert "mol-tenant-legacy rolled to" in result.stdout
    assert "session-preservation OK: all 2 rtstate volume(s) intact after roll" in result.stdout


def test_fleet_roll_detects_removed_enteros_rtstate_volume(tmp_path: Path):
    """Losing an enteros-ws-rtstate-* session volume must FAIL the roll.

    This is the arm the future prefix flip would break: a guard still matching
    only ^mol-ws-rtstate- silently ignores the new generation's volumes, the
    removal goes unnoticed, and the roll reports success — exactly the hollow
    pass this fixture exists to make impossible.
    """
    result = run_script(
        tmp_path, git_sha="deadbee1234567890", tag="staging-deadbee",
        vols_before=VOLS_MIXED, vols_after=VOLS_MIXED_WITHOUT_ENTEROS,
    )

    assert result.returncode == 1, result.stdout
    assert (
        "session volume enteros-ws-rtstate-new1 was REMOVED by the fleet roll"
        in result.stdout
    )


def test_fleet_roll_detects_removed_mol_rtstate_volume(tmp_path: Path):
    """Losing a LEGACY mol-ws-rtstate-* session volume must still FAIL the roll.

    Negative control for the other direction: dual-prefix support must not cost
    the mol-* generation its guard (an enteros-only matcher would pass here).
    """
    result = run_script(
        tmp_path, git_sha="deadbee1234567890", tag="staging-deadbee",
        vols_before=VOLS_MIXED, vols_after=VOLS_MIXED_WITHOUT_MOL,
    )

    assert result.returncode == 1, result.stdout
    assert (
        "session volume mol-ws-rtstate-legacy1 was REMOVED by the fleet roll"
        in result.stdout
    )


def test_fleet_roll_refuses_to_run_without_declared_flag_state(tmp_path: Path):
    """UNSET TENANT_FLAGS must abort the roll, not default to empty.

    The script strips every managed rollout flag from the inherited tenant env and
    re-applies only what TENANT_FLAGS declares. A call site that forgets it does not
    inherit the old behaviour — it silently turns those flags OFF on every tenant,
    which would end an in-flight staging burn-in with no log line. Defaulting to
    empty here is the bug; aborting is the fix.
    """
    bindir = write_fake_bin(tmp_path, git_sha="deadbee1234567890")
    env = os.environ.copy()
    env["PATH"] = f"{bindir}:{env['PATH']}"
    env["TENANT_IMAGE"] = "registry.example/molecule-tenant"
    env["STAGING_CANVAS_APP_CONTAINER"] = ""
    env.pop("TENANT_FLAGS", None)

    result = subprocess.run(
        ["bash", str(SCRIPT), "--tag", "staging-deadbee"],
        cwd=ROOT, env=env, text=True,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False,
    )

    assert result.returncode == 1, result.stdout
    assert "TENANT_FLAGS is not set" in result.stdout
