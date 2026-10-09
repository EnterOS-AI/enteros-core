import json
import os
import shutil
import subprocess
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts" / "deploy" / "advance-staging-tenant-pin.sh"

# Resolve bash through PATH ourselves instead of letting the OS do it.
# On the Linux CI runner this is just /usr/bin/bash. On a Windows dev box it is
# the difference between running the tests and not: CreateProcess searches
# System32 BEFORE PATH, so a bare "bash" resolves to System32\bash.exe — the WSL
# launcher — and every test times out against a distro that isn't there.
BASH = shutil.which("bash") or "bash"

sys.path.insert(0, str(ROOT / ".gitea" / "scripts"))
import pin_provenance  # noqa: E402


def test_digest_resolution_is_independent_of_a_previous_runner_cache(tmp_path: Path):
    """A downstream job must pull the tag before inspecting its local daemon.

    Gitea may schedule ``await-image`` and ``advance-pin`` on different runners.
    The registry tag is the shared handoff; a Docker image cached by the first
    runner is not. Reproduce that boundary with an initially empty fake daemon.
    """
    digest = "sha256:" + "9" * 64
    github_sha = "deadbeef1234567890abcdef1234567890abcdef"
    pulled = tmp_path / "pulled"

    docker = tmp_path / "docker"
    docker.write_text(
        f"""#!/bin/sh
if [ "$1" = "pull" ] && [ "$2" = "registry.test/molecule-tenant:staging-deadbee" ]; then
  : > "{pulled}"
  exit 0
fi
if [ "$1" = "image" ] && [ "$2" = "inspect" ] && [ -f "{pulled}" ]; then
  echo "{digest}"
  exit 0
fi
exit 1
""",
        encoding="utf-8",
    )
    docker.chmod(0o755)

    curl = tmp_path / "curl"
    curl.write_text(
        f"""#!/bin/sh
# Emulates curl's -w for the CP calls: the client reads the body and the HTTP
# status from ONE stdout stream (body, newline, %{{http_code}}). A shim that
# printed only the body would hand the script an empty status, which it now
# correctly refuses to read as success. Infisical calls pass no -w and get the
# bare body, exactly as the real curl would.
fmt=""
prev=""
for a in "$@"; do
  [ "$prev" = "-w" ] && fmt="$a"
  prev="$a"
done
printf '%s' '{{"pins":[{{"template_name":"molecule-tenant","region":"global","image_digest":"{digest}","git_sha":"{github_sha}"}}]}}'
if [ -n "$fmt" ]; then printf '\\n200'; fi
exit 0
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
            "GITHUB_SHA": github_sha,
            "GITHUB_OUTPUT": str(tmp_path / "github-output"),
            "SKIP_SSOT_WRITE": "1",
        }
    )

    result = subprocess.run(
        [BASH, str(SCRIPT), "--tag", "staging-deadbee"],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )

    assert result.returncode == 0, result.stderr + result.stdout
    assert pulled.exists(), "the downstream runner never pulled the registry tag"


def test_failed_pull_cannot_fall_back_to_a_stale_local_tag(tmp_path: Path):
    stale_digest = "sha256:" + "8" * 64
    called = tmp_path / "curl-called"

    docker = tmp_path / "docker"
    docker.write_text(
        f"""#!/bin/sh
if [ "$1" = "pull" ]; then
  exit 42
fi
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then
  echo "{stale_digest}"
  exit 0
fi
exit 1
""",
        encoding="utf-8",
    )
    docker.chmod(0o755)

    curl = tmp_path / "curl"
    curl.write_text(
        f"""#!/bin/sh
: > "{called}"
printf '%s\\n' '{{"pins":[]}}'
""",
        encoding="utf-8",
    )
    curl.chmod(0o755)

    env = os.environ.copy()
    env.update(
        {
            "PATH": f"{tmp_path}{os.pathsep}{env['PATH']}",
            "CP_ADMIN_API_TOKEN": "test-token",
            "TENANT_IMAGE_NAME": "registry.test/molecule-tenant",
            "SKIP_SSOT_WRITE": "1",
        }
    )

    result = subprocess.run(
        [BASH, str(SCRIPT), "--tag", "staging-deadbee"],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )

    assert result.returncode != 0, "a failed registry pull reused a stale local image"
    assert not called.exists(), "the script mutated/read CP state after digest resolution failed"


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
if [ "$1" = "pull" ]; then
  exit 0
fi
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
wfmt = ""
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
    if args[i] == "-w":
        wfmt = args[i + 1]
        i += 2
        continue
    if args[i].startswith("http"):
        url = args[i]
    i += 1

def respond(payload, code=200):
    # Emulate curl's -w: the CP client reads body AND status from one stdout
    # stream. A shim that printed only the body would hand the script an empty
    # status, which it now (correctly) refuses to treat as success.
    sys.stdout.write(payload)
    if wfmt:
        sys.stdout.write(wfmt.replace("%{{http_code}}", str(code)))
    sys.exit(0)

state = os.environ["FAKE_CURL_STATE"]
if url.endswith("/cp/admin/runtime-image/promote") and method == "POST":
    open(os.path.join(state, "body.json"), "w").write(body)
    open(os.path.join(state, "promoted"), "w").write("1")
    respond(body)
if url.endswith("/cp/admin/runtime-image"):
    promoted = os.path.exists(os.path.join(state, "promoted"))
    digest = "{new_digest}" if promoted else "{old_digest}"
    git = "{github_sha}" if promoted else "{old_git}"
    respond(json.dumps({{"pins": [{{"template_name": "molecule-tenant", "region": "global", "image_digest": digest, "git_sha": git}}]}}))
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
            # The promote now refuses to run without a CI run id to attribute
            # the pin write to (see pin_provenance.py). Supplying the same
            # variables Actions supplies is what makes this test exercise the
            # real path; the ABSENT case is covered by
            # test_promote_refused_without_ci_provenance below.
            "GITHUB_RUN_ID": "900001",
            "GITHUB_REPOSITORY": "molecule-ai/molecule-core",
            "GITHUB_WORKFLOW": "staging-tenant-cd",
            "GITHUB_JOB": "advance-pin",
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
        [BASH, str(SCRIPT), "--tag", "staging-deadbee"],
        cwd=ROOT,
        env=env,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )

    assert result.returncode == 0, result.stderr + result.stdout
    body = json.loads((state_dir / "body.json").read_text(encoding="utf-8"))
    assert body["template_name"] == "molecule-tenant"
    assert body["image_digest"] == new_digest
    assert body["git_sha"] == github_sha
    # The note is now a PROVENANCE STAMP followed by the free text. Assert the
    # stamp by parsing it with the same module the auditor uses, not by matching
    # a formatted string — a guard that only matches text drifts silently the
    # first time the grammar gains a field.
    fields = pin_provenance.parse(body["notes"])
    assert fields["run"] == "900001", body["notes"]
    assert fields["repo"] == "molecule-ai/molecule-core", body["notes"]
    assert "registry.test/molecule-tenant:staging-deadbee" in body["notes"]
    assert len(body["notes"]) <= 500, "the CP rejects notes longer than 500 chars"
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
if [ "$1" = "pull" ]; then
  exit 0
fi
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
        [BASH, str(SCRIPT), "--tag", "staging-deadbee"],
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
if [ "$1" = "pull" ]; then
  exit 0
fi
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
method, body, url, wfmt = "GET", "", "", ""
i = 0
while i < len(args):
    if args[i] == "-X":
        method = args[i + 1]; i += 2; continue
    if args[i] == "-d":
        body = args[i + 1]; i += 2; continue
    if args[i] == "-w":
        wfmt = args[i + 1]; i += 2; continue
    if args[i].startswith("http"):
        url = args[i]
    i += 1

def respond(payload, code=200):
    # Emulate curl's -w. The CP client reads body AND status from one stdout
    # stream; the Infisical calls pass no -w and get the bare body, exactly as
    # the real curl would.
    sys.stdout.write(payload)
    if wfmt:
        sys.stdout.write(wfmt.replace("%{{http_code}}", str(code)))
    sys.exit(0)

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
    respond(body)
if p.path.endswith("/cp/admin/runtime-image"):
    promoted = os.path.exists(os.path.join(state, "promoted"))
    respond(json.dumps({{"pins": [{{"template_name": "molecule-tenant", "region": "global",
        "image_digest": "{resolved}" if promoted else "sha256:" + "b" * 64,
        "git_sha": "{'e' * 40}"}}]}}))

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
        [BASH, str(SCRIPT), "--tag", "staging-deadbee"],
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


# ---------------------------------------------------------------------------
# POST /cp/admin/runtime-image/promote — 409 is DOCUMENTED-RETRYABLE
# ---------------------------------------------------------------------------
#
# The CP's molecule-tenant pin UPSERT is guarded by
#   INSERT ... SELECT ... WHERE $1 <> 'molecule-tenant'
#                            OR pg_try_advisory_xact_lock(<TenantImageRolloutAdvisoryLockID>)
# (molecule-controlplane internal/handlers/pin_runtime_image.go). Three actors
# contend on that advisory lock (internal/provisioner/tenant_rollout_lock.go): a
# FRESH-TENANT PROVISION holds pg_advisory_lock_shared for the whole
# orchestration, a fleet rollout holds pg_try_advisory_lock, and this promote
# takes an exclusive xact TRY. A held fence makes the try fail, the SELECT
# yields no row, ScanRow returns sql.ErrNoRows, and admin_pin_handler.go maps
# that to HTTP 409 carrying PromoteConflictOnNoRows:
#   {"error":"molecule-tenant pin mutation conflicts with an active fleet
#             rollout; retry after the rollout completes"}
# The fence is session-scoped with NO TTL and NO table row — i.e. transient, and
# the CP's own message says "retry".
#
# On 2026-08-07T21:11:26Z that 409 froze the molecule-core merge train
# (staging-tenant-cd run 628740, job 928053) over an ~11s condition:
#   >> [tenant-pin] target  pin digest=sha256:f1fc68...
#   curl: (22) The requested URL returned error: 409
#   exitcode '22': failure
# `curl -f` discarded the body, so the one actionable sentence never reached the
# log, and nothing retried.
#
# These tests drive the REAL script with the REAL curl binary against a REAL
# local HTTP server. A fake `curl` on PATH cannot prove this fix: the defect IS
# curl's own -f semantics (exit 22 + discarded body), so a shim would only test
# our transcription of curl, not curl.

import re
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


class _CPStub:
    """A real HTTP server speaking the two CP admin endpoints this script uses.

    ``promote_plan`` is a list of ``(status, body_dict)`` consumed one per POST;
    the LAST entry repeats forever, so ``[(409, ...)]`` is a permanent conflict
    and ``[(409, ...), (409, ...), (200, ...)]`` is a conflict that clears.
    """

    def __init__(self, promote_plan, old_digest, new_digest, old_git, new_git):
        self.promote_plan = list(promote_plan)
        self.old_digest, self.new_digest = old_digest, new_digest
        self.old_git, self.new_git = old_git, new_git
        self.promote_attempts = 0
        self.promoted = False
        self.get_attempts = 0
        stub = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *a):  # keep pytest output readable
                pass

            def _json(self, status, payload):
                raw = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            def do_POST(self):
                # EXACT match, never endswith: a test that mis-points the base
                # URL (e.g. .../wrong/cp/admin/runtime-image) must get a 404,
                # not a silent 200 from a suffix match.
                if self.path != "/cp/admin/runtime-image/promote":
                    self._json(404, {"error": "no such route"})
                    return
                length = int(self.headers.get("Content-Length") or 0)
                self.rfile.read(length)
                stub.promote_attempts += 1
                idx = min(stub.promote_attempts - 1, len(stub.promote_plan) - 1)
                status, payload = stub.promote_plan[idx]
                if 200 <= status < 300:
                    stub.promoted = True
                self._json(status, payload)

            def do_GET(self):
                if self.path != "/cp/admin/runtime-image":
                    self._json(404, {"error": "no such route"})
                    return
                stub.get_attempts += 1
                digest = stub.new_digest if stub.promoted else stub.old_digest
                git = stub.new_git if stub.promoted else stub.old_git
                self._json(200, {"pins": [{
                    "template_name": "molecule-tenant", "region": "global",
                    "image_digest": digest, "git_sha": git,
                }]})

        self._srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.base_url = "http://127.0.0.1:%d" % self._srv.server_address[1]
        self._thread = threading.Thread(target=self._srv.serve_forever, daemon=True)
        self._thread.start()

    def close(self):
        self._srv.shutdown()
        self._srv.server_close()


CONFLICT_MESSAGE = (
    "molecule-tenant pin mutation conflicts with an active fleet rollout; "
    "retry after the rollout completes"
)
_NEW_DIGEST = "sha256:" + "1" * 64
_OLD_DIGEST = "sha256:" + "2" * 64
_NEW_GIT = "3" * 40
_OLD_GIT = "4" * 40


def test_the_runner_provides_a_real_curl_for_these_tests_to_exercise():
    """Fail CLOSED, never skip, when curl is missing.

    Everything below is only meaningful against the genuine curl binary — the
    defect was curl's own ``-f`` behaviour. On an image without curl the script
    dies at "curl: command not found", which the status check correctly reports
    as an unusable response, and several of these tests would then pass while
    proving nothing about HTTP handling at all. A `pytest.skip` here would be
    the same hole with a friendlier colour: a gate that covers nothing must go
    RED, not green-by-no-op.
    """
    path = shutil.which("curl")
    assert path, (
        "no curl on PATH: the promote-client tests cannot exercise the real "
        "HTTP client, so this suite is not testing what it claims to. Install "
        "curl on the runner image rather than skipping."
    )
    version = subprocess.run(
        [path, "--version"], text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
    ).stdout.splitlines()[0]
    # Recorded in the CI log on failure; the fix deliberately uses only -o/-w,
    # which every curl 7.x+ supports, so no minimum version is asserted here.
    assert version.startswith("curl "), f"unrecognised curl: {version!r}"


def _run_pin_script(tmp_path: Path, base_url: str, **extra_env):
    """Invoke the script exactly as the runner does: ``bash <script> ...``.

    See .gitea/workflows/staging-tenant-cd.yml (advance-pin) and
    .gitea/workflows/promote-prod-tenant-pin.yml — BOTH shell out to this same
    file, so this covers the staging and the production plane.

    ``--image <name>@sha256:<digest>`` short-circuits resolve_digest, so no
    docker is needed; SKIP_SSOT_WRITE=1 plus CP_ADMIN_API_TOKEN keeps Infisical
    out of it. The CP calls are the whole subject here.
    """
    env = os.environ.copy()
    env.update({
        "CP_ADMIN_API_TOKEN": "test-token",
        "CP_BASE_URL": base_url,
        "TENANT_IMAGE_NAME": "registry.test/molecule-tenant",
        "GITHUB_OUTPUT": str(tmp_path / "github-output"),
        "SKIP_SSOT_WRITE": "1",
        # The promote refuses without a CI run id to attribute the pin write to
        # (pin_provenance.py). Set it EXPLICITLY rather than inheriting whatever
        # the runner exports, so these tests reach the CP calls they are about
        # on a developer box too. The absent case is
        # test_promote_refused_without_ci_provenance.
        "GITHUB_RUN_ID": "900003",
        "GITHUB_REPOSITORY": "molecule-ai/molecule-core",
    })
    for k in ("INFISICAL_CLIENT_ID", "INFISICAL_CLIENT_SECRET", "INFISICAL_PROJECT_ID"):
        env.pop(k, None)
    env.update({k: str(v) for k, v in extra_env.items()})
    started = time.monotonic()
    try:
        result = subprocess.run(
            [BASH, str(SCRIPT),
             "--image", "registry.test/molecule-tenant@" + _NEW_DIGEST,
             "--git-sha", _NEW_GIT],
            cwd=ROOT, env=env, text=True,
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False,
            # A hard wall, far above every budget these tests set. Without it, a
            # regression that removes the bound turns "the retry is unbounded"
            # into a HUNG test that eventually trips the job timeout, instead of
            # a named failure. The bound must fail loudly, not slowly.
            timeout=120,
        )
    except subprocess.TimeoutExpired as exc:
        raise AssertionError(
            "the script never terminated (120s wall). The promote retry is "
            "unbounded — a conflict that does not clear must give up inside its "
            "stated budget.\n" + (exc.stdout or "") + (exc.stderr or "")
        ) from None
    return result, time.monotonic() - started


def test_a_documented_retryable_409_is_retried_until_the_fence_clears(tmp_path: Path):
    """NEGATIVE CONTROL 1/3 — status 409 (twice) then 200. Must RETRY and succeed.

    RED before the fix: ``curl -fsS`` exits 22 on the first 409, ``set -e`` kills
    the script, one promote attempt, rc != 0.
    """
    stub = _CPStub(
        [(409, {"error": CONFLICT_MESSAGE}),
         (409, {"error": CONFLICT_MESSAGE}),
         (200, {"template_name": "molecule-tenant", "region": "global",
                "image_digest": _NEW_DIGEST, "git_sha": _NEW_GIT})],
        _OLD_DIGEST, _NEW_DIGEST, _OLD_GIT, _NEW_GIT,
    )
    try:
        result, _ = _run_pin_script(
            tmp_path, stub.base_url,
            PROMOTE_CONFLICT_RETRY_BUDGET_SECONDS=30,
            PROMOTE_CONFLICT_RETRY_INTERVAL_SECONDS=1,
        )
    finally:
        stub.close()

    out = result.stdout + result.stderr
    assert result.returncode == 0, (
        "a transient, CP-documented-retryable 409 was treated as fatal\n" + out
    )
    assert stub.promote_attempts == 3, (
        f"expected 3 promote attempts (409, 409, 200), got {stub.promote_attempts}\n" + out
    )
    # The BODY must reach the log — that sentence is the entire actionable part.
    assert CONFLICT_MESSAGE in out, (
        "the CP's 409 body never reached the operator; only an exit code did\n" + out
    )
    assert "curl: (22)" not in out, "still surfacing the bare curl exit code\n" + out


def test_a_400_is_terminal_and_is_never_retried(tmp_path: Path):
    """NEGATIVE CONTROL 2/3 — same code path, ONE input varied: 409 -> 400.

    The CP returns 400 for a validation failure (admin_pin_handler.go
    handlePromote -> ValidatePromoteRequest). Retrying that spins against a
    request that can never succeed, so it must fail on the FIRST response.
    """
    bad = "image_digest must be sha256:<64 hex>"
    stub = _CPStub([(400, {"error": bad})], _OLD_DIGEST, _NEW_DIGEST, _OLD_GIT, _NEW_GIT)
    try:
        result, elapsed = _run_pin_script(
            tmp_path, stub.base_url,
            PROMOTE_CONFLICT_RETRY_BUDGET_SECONDS=30,
            PROMOTE_CONFLICT_RETRY_INTERVAL_SECONDS=1,
        )
    finally:
        stub.close()

    out = result.stdout + result.stderr
    assert result.returncode != 0, "a 400 validation error passed as success\n" + out
    assert stub.promote_attempts == 1, (
        f"a terminal 400 was retried {stub.promote_attempts} times — retry must key "
        "on the retryable conflict, not on 'any failure'\n" + out
    )
    assert bad in out, "the CP's 400 body was discarded\n" + out
    assert elapsed < 20, f"a terminal 400 burned {elapsed:.1f}s of retry budget"


def test_a_503_no_database_is_terminal_and_is_never_retried(tmp_path: Path):
    """Same control, other terminal status: 503 = ``no database wired`` (nil db)."""
    msg = "runtime-image promotion unavailable: no database wired"
    stub = _CPStub([(503, {"error": msg})], _OLD_DIGEST, _NEW_DIGEST, _OLD_GIT, _NEW_GIT)
    try:
        result, _ = _run_pin_script(
            tmp_path, stub.base_url,
            PROMOTE_CONFLICT_RETRY_BUDGET_SECONDS=30,
            PROMOTE_CONFLICT_RETRY_INTERVAL_SECONDS=1,
        )
    finally:
        stub.close()

    out = result.stdout + result.stderr
    assert result.returncode != 0, "a 503 passed as success\n" + out
    assert stub.promote_attempts == 1, (
        f"a 503 was retried {stub.promote_attempts} times\n" + out
    )
    assert msg in out, "the CP's 503 body was discarded\n" + out


def test_a_first_try_200_is_not_retried_and_logs_no_conflict(tmp_path: Path):
    """NEGATIVE CONTROL 3/3 — status 200 on the first attempt.

    Exactly one promote, and the conflict sentence must be ABSENT. Without that
    absence check, "the message appears on a non-2xx" could be satisfied by a
    script that prints it unconditionally.
    """
    stub = _CPStub(
        [(200, {"template_name": "molecule-tenant", "region": "global",
                "image_digest": _NEW_DIGEST, "git_sha": _NEW_GIT})],
        _OLD_DIGEST, _NEW_DIGEST, _OLD_GIT, _NEW_GIT,
    )
    try:
        result, elapsed = _run_pin_script(
            tmp_path, stub.base_url,
            PROMOTE_CONFLICT_RETRY_BUDGET_SECONDS=30,
            PROMOTE_CONFLICT_RETRY_INTERVAL_SECONDS=1,
        )
    finally:
        stub.close()

    out = result.stdout + result.stderr
    assert result.returncode == 0, out
    assert stub.promote_attempts == 1, (
        f"a clean 200 was promoted {stub.promote_attempts} times — the retry loop "
        "must not re-POST a write that already landed\n" + out
    )
    assert CONFLICT_MESSAGE not in out, (
        "the conflict message is printed on a SUCCESS path — the 'body is captured' "
        "assertion in the 409 test would then be vacuous\n" + out
    )
    assert elapsed < 20, f"a clean 200 slept {elapsed:.1f}s"


def test_a_permanent_409_terminates_inside_a_stated_budget(tmp_path: Path):
    """The retry is BOUNDED: a fence that never clears must stop, not spin."""
    stub = _CPStub([(409, {"error": CONFLICT_MESSAGE})],
                   _OLD_DIGEST, _NEW_DIGEST, _OLD_GIT, _NEW_GIT)
    try:
        result, elapsed = _run_pin_script(
            tmp_path, stub.base_url,
            PROMOTE_CONFLICT_RETRY_BUDGET_SECONDS=6,
            PROMOTE_CONFLICT_RETRY_INTERVAL_SECONDS=2,
        )
    finally:
        stub.close()

    out = result.stdout + result.stderr
    assert result.returncode != 0, "a permanent conflict reported success\n" + out
    assert elapsed < 60, f"the retry loop is unbounded — ran {elapsed:.1f}s on a 6s budget"
    # It really retried (not a disguised first-failure), and it really stopped.
    assert 2 <= stub.promote_attempts <= 8, (
        f"expected a handful of attempts inside a 6s/2s budget, got {stub.promote_attempts}\n" + out
    )
    # The terminal message must NAME the budget, so the operator knows the knob.
    assert re.search(r"\b6s\b", out) and "budget" in out.lower(), (
        "the give-up message does not state the retry budget\n" + out
    )
    assert CONFLICT_MESSAGE in out, "the final failure dropped the CP's own explanation\n" + out


def test_an_unreachable_cp_is_not_read_as_success(tmp_path: Path):
    """NON-VACUITY — an EMPTY body and a non-HTTP status must NOT pass.

    Capturing status/body by hand (``-o`` + ``-w '%{http_code}'``) removes curl's
    own ``-f`` failure signal, so whatever replaces it has to be real. On a
    refused connection curl writes NOTHING to the body file and ``%{http_code}``
    is ``000``. A ``grep -q ""``-shaped check, an unquoted ``[ $status = 200 ]``,
    or a step without ``set -u`` would all wave that through as success.

    The port is bound and immediately closed, so the address is routable and the
    connection is REFUSED — not a hostname that fails to resolve.
    """
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    dead_port = s.getsockname()[1]
    s.close()

    result, elapsed = _run_pin_script(
        tmp_path, "http://127.0.0.1:%d" % dead_port,
        PROMOTE_CONFLICT_RETRY_BUDGET_SECONDS=6,
        PROMOTE_CONFLICT_RETRY_INTERVAL_SECONDS=2,
    )
    out = result.stdout + result.stderr
    assert result.returncode != 0, (
        "an unreachable CP (empty body, http_code 000) read as SUCCESS\n" + out
    )
    assert "OLD_IMAGE=" not in result.stdout, (
        "the script printed its success summary despite never reaching the CP\n" + out
    )
    # A non-zero exit ALONE proves nothing: with the status check weakened to a
    # 4xx/5xx denylist, `000` passes as success and the run still dies moments
    # later on a JSONDecodeError parsing the empty body. That is failing by
    # accident, and it is precisely how a guard rots into decoration. Assert the
    # STATUS CHECK is what fired — it names the method, the path and the code —
    # and that no downstream parser blew up instead.
    assert "returned HTTP 000" in out, (
        "the run failed, but NOT at the status check — nothing reported an "
        "unusable HTTP status. A weakened check that lets `000` through would "
        "still fail downstream and still satisfy a bare returncode assertion.\n" + out
    )
    assert "/cp/admin/runtime-image" in out, (
        "the failure does not say WHICH CP call was unusable\n" + out
    )
    assert "Traceback" not in out and "JSONDecodeError" not in out, (
        "the status check let an unusable response through and a downstream "
        "parser crashed on it instead\n" + out
    )
    assert elapsed < 60, f"an unreachable CP was retried as if it were a conflict ({elapsed:.1f}s)"


def test_a_non_2xx_on_the_pin_READ_also_surfaces_the_body(tmp_path: Path):
    """Body capture is a property of the CP client, not a special case for 409.

    Every error on this path used to be reduced to ``curl: (22) ... error: NNN``.
    Point the GET at a route the stub 404s and assert the server's own words
    reach the log.
    """
    stub = _CPStub([(200, {})], _OLD_DIGEST, _NEW_DIGEST, _OLD_GIT, _NEW_GIT)
    try:
        # /cp/admin/runtime-image now resolves under /wrong, which the stub
        # answers 404 {"error":"no such route"}.
        result, _ = _run_pin_script(tmp_path, stub.base_url + "/wrong")
    finally:
        stub.close()

    out = result.stdout + result.stderr
    assert result.returncode != 0, out
    assert "no such route" in out, (
        "a non-2xx on the pin READ still reduces to an exit code\n" + out
    )
    assert stub.promote_attempts == 0, "promoted despite an unreadable current pin\n" + out


def test_a_zero_retry_interval_is_refused_rather_than_looping_forever(tmp_path: Path):
    """The interval is the loop's only progress term.

    ``waited`` advances by the interval and the budget is compared against it,
    so an interval of 0 leaves ``waited`` at 0 forever and the "bounded" retry
    never terminates. Refuse it at startup, before anything is mutated.
    """
    stub = _CPStub([(409, {"error": CONFLICT_MESSAGE})],
                   _OLD_DIGEST, _NEW_DIGEST, _OLD_GIT, _NEW_GIT)
    try:
        result, _ = _run_pin_script(
            tmp_path, stub.base_url,
            PROMOTE_CONFLICT_RETRY_BUDGET_SECONDS=6,
            PROMOTE_CONFLICT_RETRY_INTERVAL_SECONDS=0,
        )
    finally:
        stub.close()

    out = result.stdout + result.stderr
    assert result.returncode != 0, "a zero retry interval was accepted\n" + out
    assert "PROMOTE_CONFLICT_RETRY_INTERVAL_SECONDS" in out, (
        "rejected, but not with a message naming the bad knob\n" + out
    )
    assert stub.promote_attempts == 0, (
        "the CP was called before the retry configuration was validated\n" + out
    )


def _fake_promote_env(tmp_path: Path, state_dir: Path, digest: str, git_sha: str) -> dict:
    """Fakes for a promote that WOULD succeed: a docker daemon that resolves the
    digest and a curl that records the promote body.

    Shared by the two provenance tests so the ONLY difference between them is
    the presence of GITHUB_RUN_ID. A negative control that also varies the fakes
    proves nothing about the variable under test.
    """
    docker = tmp_path / "docker"
    docker.write_text(
        f"""#!/bin/sh
if [ "$1" = "pull" ]; then exit 0; fi
if [ "$1" = "image" ] && [ "$2" = "inspect" ]; then echo "{digest}"; exit 0; fi
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
method, body, url, wfmt = "GET", "", "", ""
i = 0
while i < len(args):
    if args[i] == "-X":
        method = args[i + 1]; i += 2; continue
    if args[i] == "-d":
        body = args[i + 1]; i += 2; continue
    if args[i] == "-w":
        wfmt = args[i + 1]; i += 2; continue
    if args[i].startswith("http"):
        url = args[i]
    i += 1

def respond(payload, code=200):
    # Emulate curl's -w. The CP client reads body AND status from one stdout
    # stream; a shim that printed only the body would hand the script an empty
    # status, which it (correctly) refuses to treat as success.
    sys.stdout.write(payload)
    if wfmt:
        sys.stdout.write(wfmt.replace("%{{http_code}}", str(code)))
    sys.exit(0)

state = os.environ["FAKE_CURL_STATE"]
if url.endswith("/cp/admin/runtime-image/promote") and method == "POST":
    open(os.path.join(state, "body.json"), "w").write(body)
    open(os.path.join(state, "promoted"), "w").write("1")
    respond(body)
if url.endswith("/cp/admin/runtime-image"):
    promoted = os.path.exists(os.path.join(state, "promoted"))
    d = "{digest}" if promoted else "sha256:" + "0" * 64
    g = "{git_sha}" if promoted else "0" * 40
    respond(json.dumps({{"pins": [{{"template_name": "molecule-tenant", "region": "global", "image_digest": d, "git_sha": g}}]}}))
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
            "GITHUB_SHA": git_sha,
            "GITHUB_OUTPUT": str(tmp_path / "gh-output"),
            "FAKE_CURL_STATE": str(state_dir),
            "SKIP_SSOT_WRITE": "1",
        }
    )
    for leaked in ("GITHUB_RUN_ID", "GITHUB_REPOSITORY", "GITHUB_WORKFLOW", "GITHUB_JOB"):
        env.pop(leaked, None)
    return env


def test_promote_refused_without_ci_provenance(tmp_path: Path):
    """No GITHUB_RUN_ID => no stamp => NO PIN WRITE. Executed, not grepped.

    This is the mechanism that replaces the convention. The hand path was
    literally endorsed in a workflow comment ("Reconcile that case by running
    scripts/deploy/advance-staging-tenant-pin.sh directly"), and a hand promote
    is indistinguishable from a CI one once written: `promoted_by` is the CP
    admin token's identity either way.

    The assertion that matters is not the exit code — it is that `body.json`
    was never written. A guard that fails AFTER the POST has already landed
    protects nothing.
    """
    digest = "sha256:" + "a" * 64
    git_sha = "b" * 40
    state_dir = tmp_path / "state"
    state_dir.mkdir()
    env = _fake_promote_env(tmp_path, state_dir, digest, git_sha)

    result = subprocess.run(
        [BASH, str(SCRIPT), "--tag", "staging-deadbee"],
        cwd=ROOT, env=env, text=True,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False,
    )

    assert result.returncode != 0, result.stdout + result.stderr
    assert not (state_dir / "body.json").exists(), (
        "the promote POST was sent despite having no CI provenance"
    )
    assert "refusing to promote" in result.stderr, result.stderr


def test_promote_allowed_with_ci_provenance(tmp_path: Path):
    """The POSITIVE control: the SAME fakes plus GITHUB_RUN_ID promote fine.

    Varies exactly one input against the test above. Without this, a script
    that refused every promote for an unrelated reason would still pass the
    negative test.
    """
    digest = "sha256:" + "a" * 64
    git_sha = "b" * 40
    state_dir = tmp_path / "state"
    state_dir.mkdir()
    env = _fake_promote_env(tmp_path, state_dir, digest, git_sha)
    env["GITHUB_RUN_ID"] = "900002"
    env["GITHUB_REPOSITORY"] = "molecule-ai/molecule-core"

    result = subprocess.run(
        [BASH, str(SCRIPT), "--tag", "staging-deadbee"],
        cwd=ROOT, env=env, text=True,
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False,
    )

    assert result.returncode == 0, result.stdout + result.stderr
    body = json.loads((state_dir / "body.json").read_text(encoding="utf-8"))
    assert pin_provenance.parse(body["notes"])["run"] == "900002", body["notes"]
