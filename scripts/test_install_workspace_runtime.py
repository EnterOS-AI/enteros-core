import os
import shlex
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
INSTALLER = ROOT / "scripts" / "install-workspace-runtime.sh"


class InstallWorkspaceRuntimeTest(unittest.TestCase):
    def _run(self, fail_install: bool = False):
        with tempfile.TemporaryDirectory() as td:
            tmp = Path(td)
            log = tmp / "pip.log"
            fake_python = tmp / "python3"
            fake_python.write_text(
                textwrap.dedent(
                    """\
                    #!/usr/bin/env bash
                    set -eu
                    printf '%s\\n' "$*" >> "$FAKE_PIP_LOG"
                    case " $* " in
                      *" pip download "*)
                        dest=""
                        previous=""
                        for arg in "$@"; do
                          if [ "$previous" = "--dest" ]; then dest="$arg"; fi
                          previous="$arg"
                        done
                        mkdir -p "$dest"
                        : > "$dest/molecules_workspace_runtime-0.4.36-py3-none-any.whl"
                        ;;
                      *" pip install "*)
                        if [ "${FAKE_INSTALL_FAIL:-0}" = "1" ]; then exit 23; fi
                        ;;
                    esac
                    """
                )
            )
            fake_python.chmod(0o755)
            env = os.environ.copy()
            env.update(
                {
                    "PYTHON_BIN": str(fake_python),
                    "FAKE_PIP_LOG": str(log),
                    "FAKE_INSTALL_FAIL": "1" if fail_install else "0",
                }
            )
            result = subprocess.run(
                ["bash", str(INSTALLER)],
                env=env,
                text=True,
                capture_output=True,
                check=False,
            )
            lines = log.read_text().splitlines() if log.exists() else []
            return result, lines

    def test_private_wheel_is_pinned_then_public_pypi_resolves_dependencies(self):
        result, lines = self._run()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(lines), 2, lines)

        download = shlex.split(lines[0])
        install = shlex.split(lines[1])
        self.assertIn("download", download)
        self.assertIn("--no-deps", download)
        self.assertIn(
            "https://git.moleculesai.app/api/packages/molecule-ai/pypi/simple/",
            download,
        )
        self.assertIn("molecules-workspace-runtime==0.4.36", download)
        self.assertNotIn("--extra-index-url", download + install)

        self.assertIn("install", install)
        self.assertIn("https://pypi.org/simple/", install)
        wheels = [arg for arg in install if arg.endswith(".whl")]
        self.assertEqual(len(wheels), 1, install)
        self.assertFalse(Path(wheels[0]).parent.exists(), "temporary wheel directory leaked")

    def test_install_failure_is_nonzero_and_still_cleans_the_wheel(self):
        result, lines = self._run(fail_install=True)
        self.assertEqual(result.returncode, 23, result.stderr)
        install = shlex.split(lines[1])
        wheel = next(arg for arg in install if arg.endswith(".whl"))
        self.assertFalse(Path(wheel).parent.exists(), "temporary wheel directory leaked")


if __name__ == "__main__":
    unittest.main()
