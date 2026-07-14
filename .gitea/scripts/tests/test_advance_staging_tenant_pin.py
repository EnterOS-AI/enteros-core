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
            # This test covers CP runtime-pin promotion, not the boot-default SSOT
            # write this PR adds. That write now needs real Infisical universal-auth
            # creds, which a unit test has no business holding — so opt out of it
            # explicitly. The fail-closed gate itself is pinned by
            # test_ssot_write_fails_closed_without_infisical below; without that,
            # setting this flag here would be silencing the new behaviour rather
            # than scoping around it.
            "SKIP_SSOT_WRITE": "1",
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


def test_ssot_write_fails_closed_without_infisical(tmp_path: Path):
    """The boot-default SSOT write must FAIL CLOSED when Infisical creds are absent.

    This PR makes advance-staging-tenant-pin.sh write the LOCAL_TENANT_IMAGE boot
    default into Infisical (the credentials SSOT). If that write could be skipped
    silently whenever creds happen to be missing, the CP's boot default would drift
    away from the pin we just promoted — the tenant would come back on a stale image
    and the whole point of the pin would be lost. So: no creds and no explicit
    SKIP_SSOT_WRITE=1 => hard exit, not a warning.

    Pins the gate at scripts/deploy/advance-staging-tenant-pin.sh:106.
    """
    # Stub `docker image inspect` so the script gets PAST digest resolution and
    # actually reaches the Infisical gate. Without this it dies at "cannot resolve
    # a sha256 image id/digest" and the test would pass for the wrong reason —
    # asserting a non-zero exit that has nothing to do with fail-closed behaviour.
    resolved = "sha256:" + "c" * 64
    docker = tmp_path / "docker"
    docker.write_text(
        f"""#!/bin/sh
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
  echo "{resolved}"
  exit 0
fi
exit 1
""",
        encoding="utf-8",
    )
    docker.chmod(0o755)

    env = os.environ.copy()
    env.update(
        {
            "PATH": f"{tmp_path}{os.pathsep}{env['PATH']}",
            "CP_ADMIN_API_TOKEN": "test-token",
            "CP_BASE_URL": "https://staging-api.test",
            "TENANT_IMAGE_NAME": "registry.test/molecule-tenant",
            "GITHUB_SHA": "a" * 40,
            "GITHUB_OUTPUT": str(tmp_path / "github-output"),
        }
    )
    # Explicitly ensure no Infisical universal-auth creds leak in from the runner.
    for k in ("INFISICAL_CLIENT_ID", "INFISICAL_CLIENT_SECRET", "INFISICAL_ACCESS_TOKEN"):
        env.pop(k, None)
    env.pop("SKIP_SSOT_WRITE", None)

    result = subprocess.run(
        ["bash", str(SCRIPT), "--tag", "staging-deadbee"],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )

    assert result.returncode != 0, (
        "SSOT write ran (or was silently skipped) with no Infisical creds — it must fail closed.\n"
        + result.stdout
        + result.stderr
    )
    assert "INFISICAL_CLIENT_ID is required" in (result.stderr + result.stdout), (
        "failed, but not with the fail-closed Infisical message:\n" + result.stderr + result.stdout
    )


def test_ssot_write_lands_the_ref_at_the_boot_default_path(tmp_path: Path):
    """EXECUTE write_ssot_pin against a fake Infisical and assert WHERE the value lands.

    Until this existed, write_ssot_pin had zero executed coverage — it was guarded only
    by source-greps in staging_tenant_pin_script_test.go, and those greps were too loose
    to catch the mutations that matter. Repointing the write to /shared/controlplane-admin
    (the CP ADMIN TOKEN path) passed, because `Contains(src, "CP_SSOT_PATH:-/shared/controlplane")`
    prefix-matches it. Renaming the secret passed, because `Contains(src, "LOCAL_TENANT_IMAGE")`
    matches the comments.

    So assert the OUTCOME, not the source text: which secret name, at which path, in which
    environment, with which value. A grep can be fooled by a comment; a landed value cannot.
    """
    resolved = "sha256:" + "d" * 64
    image_tag = "registry.test/molecule-tenant:staging-deadbee"
    state_dir = tmp_path / "state"
    state_dir.mkdir()

    docker = tmp_path / "docker"
    docker.write_text(
        f"""#!/bin/sh
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
  echo "{resolved}"
  exit 0
fi
exit 1
""",
        encoding="utf-8",
    )
    docker.chmod(0o755)

    # Fake curl speaking BOTH the CP admin API and the Infisical v3 raw-secrets API.
    # Every Infisical write is journalled to writes.jsonl so the test can assert exactly
    # what landed and where.
    curl = tmp_path / "curl"
    curl.write_text(
        f"""#!/usr/bin/env python3
import json, os, sys
from urllib.parse import urlparse, parse_qs, unquote

args = sys.argv[1:]
method, body, url = "GET", "", ""
i = 0
while i < len(args):
    if args[i] == "-X":
        method = args[i + 1]; i += 2; continue
    if args[i] == "-d":
        body = args[i + 1]; i += 2; continue
    if args[i].startswith("http"):
        url = args[i]
    i += 1

state = os.environ["FAKE_CURL_STATE"]
store = os.path.join(state, "secrets.json")
secrets = json.load(open(store)) if os.path.exists(store) else {{}}
p = urlparse(url)

# --- Infisical: universal-auth login
if p.path == "/api/v1/auth/universal-auth/login" and method == "POST":
    print(json.dumps({{"accessToken": "fake-inf-token"}}))
    sys.exit(0)

# --- Infisical: raw secret read/write
if p.path.startswith("/api/v3/secrets/raw/"):
    name = unquote(p.path.rsplit("/", 1)[1])
    if method == "GET":
        q = parse_qs(p.query)
        path = q.get("secretPath", [""])[0]
        env = q.get("environment", [""])[0]
        key = (env, path, name)
        val = secrets.get("|".join(key))
        if val is None:
            print(json.dumps({{"message": "not found"}})); sys.exit(0)
        print(json.dumps({{"secret": {{"secretValue": val}}}})); sys.exit(0)
    if method in ("PATCH", "POST"):
        d = json.loads(body)
        key = (d.get("environment", ""), d.get("secretPath", ""), name)
        secrets["|".join(key)] = d.get("secretValue", "")
        json.dump(secrets, open(store, "w"))
        with open(os.path.join(state, "writes.jsonl"), "a") as f:
            f.write(json.dumps({{
                "verb": method, "name": name,
                "environment": d.get("environment"), "secretPath": d.get("secretPath"),
                "secretValue": d.get("secretValue"),
            }}) + "\\n")
        print(json.dumps({{"secret": {{"secretValue": d.get("secretValue")}}}})); sys.exit(0)

# --- CP admin API
if p.path.endswith("/cp/admin/runtime-image/promote") and method == "POST":
    open(os.path.join(state, "promoted"), "w").write("1")
    print(body); sys.exit(0)
if p.path.endswith("/cp/admin/runtime-image"):
    promoted = os.path.exists(os.path.join(state, "promoted"))
    print(json.dumps({{"pins": [{{"template_name": "molecule-tenant", "region": "global",
        "image_digest": "{resolved}" if promoted else "sha256:" + "b" * 64,
        "git_sha": "{'e' * 40}"}}]}}))
    sys.exit(0)

print("unexpected curl call: " + " ".join(args), file=sys.stderr)
sys.exit(1)
""",
        encoding="utf-8",
    )
    curl.chmod(0o755)

    env = os.environ.copy()
    env.update(
        {
            "PATH": f"{tmp_path}{os.pathsep}{env['PATH']}",
            "CP_ADMIN_API_TOKEN": "test-token",
            "CP_BASE_URL": "https://staging-api.test",
            "TENANT_IMAGE_NAME": "registry.test/molecule-tenant",
            "GITHUB_SHA": "f" * 40,
            "GITHUB_OUTPUT": str(tmp_path / "github-output"),
            "FAKE_CURL_STATE": str(state_dir),
            # Real Infisical creds are required now — supply fakes so the write RUNS.
            "INFISICAL_CLIENT_ID": "fake-id",
            "INFISICAL_CLIENT_SECRET": "fake-secret",
            "INFISICAL_PROJECT_ID": "fake-project",
        }
    )
    env.pop("SKIP_SSOT_WRITE", None)

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

    writes_file = state_dir / "writes.jsonl"
    assert writes_file.exists(), (
        "write_ssot_pin never wrote to Infisical at all\n" + result.stdout + result.stderr
    )
    writes = [json.loads(l) for l in writes_file.read_text().splitlines() if l.strip()]
    assert len(writes) == 1, f"expected exactly one SSOT write, got {writes}"
    w = writes[0]

    # The secret NAME — a rename must not slip past because a comment mentions it.
    assert w["name"] == "LOCAL_TENANT_IMAGE", f"wrote the wrong secret name: {w}"

    # The PATH — /shared/controlplane-admin is the CP ADMIN TOKEN folder. The boot
    # default must never be written there. This is the mutation the old prefix-matching
    # grep let through.
    assert w["secretPath"] == "/shared/controlplane", (
        f"boot default landed at {w['secretPath']!r} — it must be exactly /shared/controlplane "
        f"(/shared/controlplane-admin is the CP admin-token path)"
    )

    assert w["environment"] == "staging", f"wrote to the wrong Infisical env: {w}"

    # The secret did not exist in this fake store, so the create verb is POST. The script
    # deliberately does NOT fall PATCH->POST: a transient PATCH failure on an existing key
    # would then POST and draw a false "already exists" 4xx that masks the real error.
    assert w["verb"] == "POST", (
        f"created an absent secret with {w['verb']} — an absent key is a POST (create), "
        f"an existing one a PATCH (update); the verb is chosen from the read-back, not guessed"
    )

    # The VALUE must be the fully-qualified PULLABLE ref, not the bare digest — a digest
    # alone is unpullable and re-breaks fresh-org boot, the exact drift this script exists
    # to prevent.
    assert w["secretValue"] == image_tag, (
        f"wrote {w['secretValue']!r}; expected the pullable ref {image_tag!r}"
    )
