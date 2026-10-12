"""Browser startup must fail closed and clean up failures, never use the host."""

import os
import subprocess
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[1] / "start-playwright-server.sh"


def run_server(tmp_path, mode="healthy", version="1.59.1"):
    scripts = {
        "docker": r'''#!/bin/bash
echo "$*" >> "$STATE/calls"
case "$1" in
  create) [ "$MODE" != create-fails ] || exit 1; echo test-container ;;
  cp) [ "$MODE" != copy-fails ] ;;
  start) [ "$MODE" != start-fails ] ;;
  inspect)
    if [[ "$*" == *NetworkMode* ]]; then
      if [ "$MODE" = host-network ]; then echo host; else echo bridge; fi
    else echo false; fi ;;
  port) if [ "$MODE" != no-port ]; then echo 127.0.0.1:45678; fi ;;
esac
''',
        "node": '#!/bin/bash\necho "$VERSION"\n',
        "curl": '#!/bin/bash\n[ "$MODE" != not-ready ]\n',
    }
    for name, content in scripts.items():
        executable = tmp_path / name
        executable.write_text(content, encoding="utf-8")
        executable.chmod(0o755)
    env = {**os.environ, "PATH": f"{tmp_path}{os.pathsep}{os.environ['PATH']}",
           "STATE": str(tmp_path), "MODE": mode, "VERSION": version}
    result = subprocess.run(["bash", str(SCRIPT), "browser-e2e-chat-test", str(tmp_path)],
                            env=env, capture_output=True, text=True, timeout=10)
    calls = tmp_path / "calls"
    return result, calls.read_text().splitlines() if calls.exists() else []


def test_ready_server_returns_only_the_endpoint_and_remains_running(tmp_path):
    result, calls = run_server(tmp_path)
    assert result.returncode == 0, result.stderr
    assert result.stdout == "ws://127.0.0.1:45678/\n"
    assert not any(call.startswith("rm ") for call in calls)


@pytest.mark.parametrize("mode", ["copy-fails", "start-fails", "host-network", "no-port", "not-ready"])
def test_failed_start_removes_only_its_container_and_emits_no_endpoint(tmp_path, mode):
    result, calls = run_server(tmp_path, mode)
    assert result.returncode != 0
    assert "ws://" not in result.stdout
    assert "rm -fv test-container" in calls
    assert not any("browser-e2e-chat-test" in call for call in calls if call.startswith("rm "))


def test_create_failure_does_not_remove_an_existing_container(tmp_path):
    result, calls = run_server(tmp_path, "create-fails")
    assert result.returncode != 0
    assert not any(call.startswith("rm ") for call in calls)


def test_invalid_version_fails_before_creating_resources(tmp_path):
    result, calls = run_server(tmp_path, version="latest")
    assert result.returncode != 0
    assert calls == []
