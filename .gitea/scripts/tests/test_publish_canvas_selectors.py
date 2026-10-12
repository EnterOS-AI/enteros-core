import os
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / ".gitea" / "scripts" / "publish-canvas-selectors.sh"
SHA = "a" * 40
DIGEST = "sha256:" + "b" * 64
OLD_DIGEST = "sha256:" + "c" * 64
IMAGE = "registry.moleculesai.app/molecule-ai/canvas"
CANDIDATE = f"{IMAGE}:staging-{SHA}"
MOVING = f"{IMAGE}:staging-latest"
COMPAT = f"{IMAGE}:sha-{SHA[:7]}"


def write_executable(path: Path, body: str) -> None:
    path.write_text(body, encoding="utf-8")
    path.chmod(0o755)


def run_selector(
    tmp_path: Path,
    entries: dict[str, tuple[str, str]],
    *,
    main_sha: str = SHA,
    relevant_changed: bool = False,
    relevant_touched: bool | None = None,
) -> tuple[subprocess.CompletedProcess[str], dict[str, tuple[str, str]], str]:
    bindir = tmp_path / "bin"
    bindir.mkdir()
    state = tmp_path / "state.tsv"
    state.write_text(
        "".join(
            f"{ref}\t{digest}\t{revision}\n"
            for ref, (digest, revision) in entries.items()
        ),
        encoding="utf-8",
    )
    calls = tmp_path / "calls.log"
    output = tmp_path / "github-output"

    write_executable(
        bindir / "git",
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        'printf "git %s\\n" "$*" >> "$CALLS_FILE"\n'
        'if [ "$1 $2" = "rev-parse refs/remotes/origin/main" ]; then\n'
        'printf "%s\\n" "$MAIN_SHA"\n'
        'exit 0\n'
        "fi\n"
        'if [ "$1 $2" = "merge-base --is-ancestor" ]; then exit 0; fi\n'
        'if [ "$1 $2" = "diff --quiet" ]; then [ "$RELEVANT_CHANGED" != "true" ]; exit; fi\n'
        'if [ "$1 $2" = "rev-list --max-count=1" ]; then\n'
        '  [ "$RELEVANT_TOUCHED" != "true" ] || printf "%s\\n" "$MAIN_SHA"\n'
        '  exit 0\n'
        'fi\n'
        'echo "unexpected git invocation" >&2\n'
        'exit 98\n',
    )
    write_executable(
        bindir / "python3",
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        'printf "python3 %s\\n" "$*" >> "$CALLS_FILE"\n'
        '[[ "$1" = *.gitea/scripts/registry-manifest-state.py ]]\n'
        'ref="$2"\n'
        "line=$(awk -F '\\t' -v ref=\"$ref\" '$1 == ref {print; exit}' \"$STATE_FILE\")\n"
        '[ -n "$line" ] || exit 10\n'
        "printf '%s\\n' \"$line\" | awk -F '\\t' '{print $2}'\n",
    )
    write_executable(
        bindir / "timeout",
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        'printf "timeout %s\\n" "$*" >> "$CALLS_FILE"\n'
        "shift\n"
        'exec "$@"\n',
    )
    write_executable(
        bindir / "docker",
        "#!/usr/bin/env bash\n"
        "set -euo pipefail\n"
        'printf "docker %s\\n" "$*" >> "$CALLS_FILE"\n'
        'if [ "$1 ${2:-} ${3:-}" = "buildx imagetools create" ]; then\n'
        '  target="$6"; source="$7"; digest="${source##*@}"\n'
        "  revision=$(awk -F '\\t' -v digest=\"$digest\" '$2 == digest {print $3; exit}' \"$STATE_FILE\")\n"
        '  [ -n "$revision" ]\n'
        "  awk -F '\\t' -v target=\"$target\" '$1 != target' \"$STATE_FILE\" > \"$STATE_FILE.tmp\"\n"
        "  printf '%s\\t%s\\t%s\\n' \"$target\" \"$digest\" \"$revision\" >> \"$STATE_FILE.tmp\"\n"
        '  mv "$STATE_FILE.tmp" "$STATE_FILE"\n'
        "  exit 0\n"
        "fi\n"
        'if [ "$1" = "pull" ]; then exit 0; fi\n'
        'if [ "$1 ${2:-}" = "image inspect" ]; then\n'
        '  ref="$3"\n'
        "  awk -F '\\t' -v ref=\"$ref\" '$1 == ref {print $3; exit}' \"$STATE_FILE\"\n"
        "  exit 0\n"
        "fi\n"
        'echo "unexpected docker invocation" >&2\n'
        "exit 97\n",
    )

    env = os.environ.copy()
    env.update(
        {
            "PATH": f"{bindir}{os.pathsep}{env['PATH']}",
            "STATE_FILE": str(state),
            "CALLS_FILE": str(calls),
            "MAIN_SHA": main_sha,
            "RELEVANT_CHANGED": "true" if relevant_changed else "false",
            "RELEVANT_TOUCHED": "true"
            if (relevant_changed if relevant_touched is None else relevant_touched)
            else "false",
            "IMAGE_NAME": IMAGE,
            "CANDIDATE_TAG": f"staging-{SHA}",
            "SHA_TAG": f"sha-{SHA[:7]}",
            "EXPECTED_SHA": SHA,
            "REG_USER": "registry-user",
            "REG_TOKEN": "sensitive-registry-token",
            "REGISTRY_PULL_TIMEOUT_SECONDS": "180",
            "GITHUB_OUTPUT": str(output),
        }
    )
    result = subprocess.run(
        ["bash", str(SCRIPT)],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    final: dict[str, tuple[str, str]] = {}
    for line in state.read_text(encoding="utf-8").splitlines():
        ref, digest, revision = line.split("\t")
        final[ref] = (digest, revision)
    return result, final, calls.read_text(encoding="utf-8")


def test_partial_publication_repairs_and_proves_both_moving_selectors(
    tmp_path: Path,
) -> None:
    result, state, calls = run_selector(tmp_path, {CANDIDATE: (DIGEST, SHA)})

    assert result.returncode == 0, result.stderr
    assert state == {
        CANDIDATE: (DIGEST, SHA),
        MOVING: (DIGEST, SHA),
        COMPAT: (DIGEST, SHA),
    }
    assert result.stdout.count("verified selector") == 3
    assert "candidate_digest=" + DIGEST in (tmp_path / "github-output").read_text()
    assert "sensitive-registry-token" not in calls


def test_mismatched_selectors_are_repaired_to_candidate_digest_and_revision(
    tmp_path: Path,
) -> None:
    result, state, _calls = run_selector(
        tmp_path,
        {
            CANDIDATE: (DIGEST, SHA),
            MOVING: (OLD_DIGEST, "d" * 40),
            COMPAT: (OLD_DIGEST, "d" * 40),
        },
    )

    assert result.returncode == 0, result.stderr
    assert {state[ref] for ref in (CANDIDATE, MOVING, COMPAT)} == {(DIGEST, SHA)}


def test_superseded_rerun_fails_before_mutating_moving_selectors(
    tmp_path: Path,
) -> None:
    initial = {
        CANDIDATE: (DIGEST, SHA),
        MOVING: (OLD_DIGEST, "d" * 40),
        COMPAT: (OLD_DIGEST, "d" * 40),
    }
    result, state, calls = run_selector(
        tmp_path, initial, main_sha="e" * 40, relevant_changed=True
    )

    assert result.returncode != 0
    assert "superseded" in result.stderr
    assert state == initial
    assert "imagetools create" not in calls


def test_unrelated_main_advance_keeps_latest_canvas_candidate_current(
    tmp_path: Path,
) -> None:
    result, state, _calls = run_selector(
        tmp_path, {CANDIDATE: (DIGEST, SHA)}, main_sha="e" * 40
    )

    assert result.returncode == 0, result.stderr
    assert {state[ref] for ref in (CANDIDATE, MOVING, COMPAT)} == {(DIGEST, SHA)}
    assert "advanced only through unrelated paths" in result.stdout


def test_relevant_change_then_revert_still_supersedes_old_publisher_run(
    tmp_path: Path,
) -> None:
    initial = {CANDIDATE: (DIGEST, SHA)}
    result, state, calls = run_selector(
        tmp_path,
        initial,
        main_sha="e" * 40,
        relevant_changed=False,
        relevant_touched=True,
    )

    assert result.returncode != 0
    assert "superseded" in result.stderr
    assert state == initial
    assert "imagetools create" not in calls


def test_candidate_revision_mismatch_fails_before_selector_mutation(
    tmp_path: Path,
) -> None:
    result, state, calls = run_selector(
        tmp_path, {CANDIDATE: (DIGEST, "f" * 40)}
    )

    assert result.returncode != 0
    assert "revision" in result.stderr
    assert state == {CANDIDATE: (DIGEST, "f" * 40)}
    assert "imagetools create" not in calls
