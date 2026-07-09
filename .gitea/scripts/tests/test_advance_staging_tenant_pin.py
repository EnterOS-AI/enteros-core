import json
import os
import subprocess
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts" / "deploy" / "advance-staging-tenant-pin.sh"


def test_advance_staging_tenant_pin_promotes_cp_runtime_pin(tmp_path: Path):
    new_digest = "sha256:" + "a" * 64
    old_digest = "sha256:" + "b" * 64
    old_git = "cafebabe1234567890abcdef1234567890abcdef"
    github_sha = "deadbeef1234567890abcdef1234567890abcdef"
    state_dir = tmp_path / "state"
    state_dir.mkdir()

    docker = tmp_path / "docker"
    docker.write_text(
        f"""#!/bin/sh
if [ "$1" = "image" ] && [ "$2" = "inspect" ] && [ "$3" = "registry.test/molecule-tenant:staging-deadbee" ]; then
  echo "{new_digest}"
  exit 0
fi
exit 1
""",
        encoding="utf-8",
    )
    docker.chmod(0o755)

    curl = tmp_path / "curl"
    curl.write_text(
        f"""#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
method = "GET"
body = ""
url = ""
i = 0
while i < len(args):
    if args[i] == "-X":
        method = args[i + 1]
        i += 2
        continue
    if args[i] == "-d":
        body = args[i + 1]
        i += 2
        continue
    if args[i].startswith("http"):
        url = args[i]
    i += 1
state = os.environ["FAKE_CURL_STATE"]
if url.endswith("/cp/admin/runtime-image/promote") and method == "POST":
    open(os.path.join(state, "body.json"), "w").write(body)
    open(os.path.join(state, "promoted"), "w").write("1")
    print(body)
    sys.exit(0)
if url.endswith("/cp/admin/runtime-image"):
    promoted = os.path.exists(os.path.join(state, "promoted"))
    digest = "{new_digest}" if promoted else "{old_digest}"
    git = "{github_sha}" if promoted else "{old_git}"
    print(json.dumps({{"pins": [{{"template_name": "molecule-tenant", "region": "global", "image_digest": digest, "git_sha": git}}]}}))
    sys.exit(0)
print("unexpected curl call: " + " ".join(args), file=sys.stderr)
sys.exit(1)
""",
        encoding="utf-8",
    )
    curl.chmod(0o755)

    github_output = tmp_path / "github-output"
    env = os.environ.copy()
    env.update(
        {
            "PATH": f"{tmp_path}{os.pathsep}{env['PATH']}",
            "CP_ADMIN_API_TOKEN": "test-token",
            "CP_BASE_URL": "https://staging-api.test",
            "TENANT_IMAGE_NAME": "registry.test/molecule-tenant",
            "GITHUB_SHA": github_sha,
            "GITHUB_OUTPUT": str(github_output),
            "FAKE_CURL_STATE": str(state_dir),
        }
    )

    result = subprocess.run(
        ["bash", str(SCRIPT), "--tag", "staging-deadbee"],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )

    assert result.returncode == 0, result.stderr + result.stdout
    body = json.loads((state_dir / "body.json").read_text(encoding="utf-8"))
    assert body == {
        "template_name": "molecule-tenant",
        "image_digest": new_digest,
        "git_sha": github_sha,
        "notes": "staging tenant image registry.test/molecule-tenant:staging-deadbee",
    }
    out = github_output.read_text(encoding="utf-8")
    assert f"old_image=registry.test/molecule-tenant:staging-{old_git[:7]}" in out
    assert f"old_digest={old_digest}" in out
    assert f"old_git_sha={old_git}" in out
    assert "new_image=registry.test/molecule-tenant:staging-deadbee" in out
    assert f"new_digest={new_digest}" in out
    assert f"new_git_sha={github_sha}" in out
