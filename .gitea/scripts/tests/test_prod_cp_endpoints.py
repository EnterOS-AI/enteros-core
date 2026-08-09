"""Guards for the production control-plane endpoint table and the apply loop.

THE DEFECT THESE EXIST FOR. The apply step promotes each control plane in turn
by varying CP_BASE_URL, but `INFISICAL_ENV` was pinned once at STEP level to
`prod`. `advance-staging-tenant-pin.sh` uses INFISICAL_ENV for two different
things: fetching the CP admin token, and — via write_ssot_pin — writing the
`LOCAL_TENANT_IMAGE` boot secret. So the second iteration promoted
`cp_staging`'s DB pin and then wrote the **prod** boot secret a second time.

The two boot secrets are genuinely distinct, measured 2026-08-08:

    cp_prod     api.moleculesai.app          Infisical env `prod`
    cp_staging  staging-api.moleculesai.app  Infisical env `staging`

`molecule-cp-prod` boots with MOLECULE_ENV=production and `molecule-cp-staging`
with MOLECULE_ENV=staging; each resolves LOCAL_TENANT_IMAGE from its OWN
Infisical environment. staging-tenant-cd.yml pairs staging-api with
INFISICAL_ENV=staging (its default). Pairing staging-api with `prod` is unique to
this lane and is wrong.

It also FAILS SILENTLY: iteration 2's write_ssot_pin re-reads the prod value that
iteration 1 just wrote, finds it already correct, and logs `SSOT ok` — affirming
a write it never made to the right environment. That is the same silent
half-fleet shape the multi-CP planner exists to prevent, reappearing on the
second SSOT, and it is precisely the "pin rolled, boot SSOT stale, every fresh
org broke" mechanism the script header says it exists to stop.
"""

from __future__ import annotations

import os
import re
import subprocess
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parents[3]
PARSER = ROOT / ".gitea" / "scripts" / "prod_cp_endpoints.py"
WORKFLOW = ROOT / ".gitea" / "workflows" / "promote-prod-tenant-pin.yml"


def _run(args: list[str]) -> subprocess.CompletedProcess:
    return subprocess.run(
        [sys.executable, str(PARSER), *args], capture_output=True, text=True, check=False
    )


def _apply_step() -> dict:
    doc = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    for step in doc["jobs"]["promote"]["steps"]:
        if str(step.get("name", "")).startswith("Apply"):
            return step
    raise AssertionError("no Apply step found in promote-prod-tenant-pin.yml")


def _default_endpoints() -> str:
    doc = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    raw = doc["jobs"]["promote"]["env"]["PROD_CP_BASE_URLS"]
    m = re.search(r"\|\|\s*'([^']+)'", raw)
    assert m, f"could not extract the default endpoint table from {raw!r}"
    return m.group(1)


# --------------------------------------------------------------------------
# The pairing itself
# --------------------------------------------------------------------------

def test_each_endpoint_carries_its_own_infisical_env():
    r = _run(["cp_prod=https://api.example=prod,cp_staging=https://stg.example=staging"])
    assert r.returncode == 0, r.stderr
    rows = [line.split("\t") for line in r.stdout.strip().splitlines()]
    assert rows == [
        ["cp_prod", "https://api.example", "prod"],
        ["cp_staging", "https://stg.example", "staging"],
    ], rows


def test_the_shipped_default_pairs_staging_api_with_the_staging_env():
    """The exact regression: staging-api must NOT be paired with `prod`."""
    r = _run([_default_endpoints()])
    assert r.returncode == 0, r.stderr
    by_base = {row.split("\t")[1]: row.split("\t")[2] for row in r.stdout.strip().splitlines()}
    assert "https://staging-api.moleculesai.app" in by_base, by_base
    assert by_base["https://staging-api.moleculesai.app"] == "staging", (
        "staging-api is paired with Infisical env %r — writing the wrong "
        "LOCAL_TENANT_IMAGE boot secret" % by_base["https://staging-api.moleculesai.app"]
    )
    assert by_base["https://api.moleculesai.app"] == "prod", by_base


def test_an_endpoint_without_an_explicit_env_is_rejected():
    """No implicit default: a 2-field entry must fail, not silently inherit."""
    r = _run(["cp_staging=https://stg.example"])
    assert r.returncode != 0, "a 2-field endpoint must be rejected, not defaulted"
    assert "infisical" in (r.stderr + r.stdout).lower()


def test_an_unknown_infisical_env_is_rejected():
    """`production` is not a valid slug — the slug is `prod`."""
    r = _run(["cp_prod=https://api.example=production"])
    assert r.returncode != 0, "the slug is `prod`, not `production`"
    assert "production" in (r.stderr + r.stdout)


def test_a_malformed_entry_is_rejected():
    r = _run(["not-an-entry"])
    assert r.returncode != 0


# --------------------------------------------------------------------------
# The apply step must actually USE the per-endpoint env
# --------------------------------------------------------------------------

def test_apply_step_does_not_pin_a_single_infisical_env():
    """A step-level INFISICAL_ENV applies to EVERY iteration — that is the bug."""
    env = _apply_step().get("env") or {}
    assert "INFISICAL_ENV" not in env, (
        "the Apply step pins INFISICAL_ENV=%r at step level, so every control "
        "plane in the loop writes that one environment's LOCAL_TENANT_IMAGE"
        % env.get("INFISICAL_ENV")
    )


def test_apply_loop_sets_infisical_env_per_endpoint():
    body = _apply_step()["run"]
    assert re.search(r"INFISICAL_ENV=\"?\$\{?\w*env\w*", body, re.IGNORECASE), (
        "the Apply loop never assigns INFISICAL_ENV from the per-endpoint value"
    )


def test_apply_step_passes_the_tenant_image_name_through():
    """`TENANT_IMAGE` is the job env var; the SCRIPT reads `TENANT_IMAGE_NAME`.

    Without the hand-off the script falls back to its hardcoded repository, so a
    set `vars.MOLECULE_TENANT_IMAGE` would have the plan and digest computed for
    one repository while the apply mutates another. staging-tenant-cd.yml does
    this correctly.
    """
    env = _apply_step().get("env") or {}
    assert "TENANT_IMAGE_NAME" in env, (
        "the Apply step never sets TENANT_IMAGE_NAME, so advance-staging-tenant-pin.sh "
        "silently uses its hardcoded default repository instead of TENANT_IMAGE"
    )
    assert "TENANT_IMAGE" in str(env["TENANT_IMAGE_NAME"]), env["TENANT_IMAGE_NAME"]


def _plan_step() -> dict:
    doc = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    for step in doc["jobs"]["promote"]["steps"]:
        if str(step.get("name", "")).startswith("Plan"):
            return step
    raise AssertionError("no Plan step found in promote-prod-tenant-pin.yml")


def test_plan_writes_a_machine_readable_verdict():
    body = _plan_step()["run"]
    assert "--needed-out" in body, "the Plan step emits no machine-readable verdict"
    assert "needed.txt" in body, body


def test_apply_is_gated_on_the_plan_verdict():
    """An armed dispatch with nothing to do must not mutate anything.

    Arming is sticky and repo-scoped, and lint-workflow-yaml forbids dispatch
    inputs, so there is no per-run confirmation: without this gate an armed
    operator who dispatches wanting only a plan gets a fleet-global mutation.

    Asserted structurally, not by keyword: the apply body must read the SAME
    verdict file the plan writes, and must contain a non-mutating early exit that
    precedes the promote invocation.
    """
    body = _apply_step()["run"]
    assert "needed.txt" in body, (
        "the Apply step never reads the plan's verdict file, so it promotes every "
        "control plane even when the plan says all are already at the target digest"
    )
    assert "exit 0" in body, "the Apply step has no non-mutating early exit"
    # Anchor on the INVOCATION (`./scripts/...`), not on the script's name, which
    # also appears in the operator prose above it.
    invoke = body.index("./scripts/deploy/advance-staging-tenant-pin.sh")
    assert body.index("needed.txt") < invoke, (
        "the verdict is consulted AFTER the promote — that is not a gate"
    )
    assert body.index("exit 0") < invoke, (
        "the early exit cannot short-circuit the promote it follows"
    )


# --------------------------------------------------------------------------
# No live credential material in fixtures
# --------------------------------------------------------------------------

def test_no_test_fixture_embeds_a_live_admin_token_fingerprint():
    """The fixture `api-token-<8 hex>` matched the LIVE prod CP admin token.

    Verified 2026-08-08: those 8 characters were byte-for-byte the first 8 of
    `CP_ADMIN_API_TOKEN` at Infisical prod /shared/controlplane-admin. Not
    exploitable on its own, but a committed fragment of a live production
    credential does not belong in a fixture. The needle is assembled at run time
    so this guard does not itself embed it.
    """
    needle = "2e19" + "e" + "375"
    gitea = ROOT / ".gitea"
    offenders = []
    for p in sorted(list(gitea.rglob("*.py")) + list(gitea.rglob("*.yml"))):
        if p.resolve() == Path(__file__).resolve():
            continue
        if needle in p.read_text(encoding="utf-8", errors="ignore"):
            offenders.append(str(p.relative_to(ROOT)))
    assert not offenders, (
        "file(s) %s embed 8 hex characters of the LIVE production CP admin "
        "token; use a synthetic value" % offenders
    )


# --------------------------------------------------------------------------
# Behavioural: the boot-secret write must land in the env it was told to use
# --------------------------------------------------------------------------

SCRIPT = ROOT / "scripts" / "deploy" / "advance-staging-tenant-pin.sh"

# On the Linux runner this is simply `bash`. TEST_BASH exists so the same test can
# be executed on a Windows dev box, where the bare name resolves to the WSL stub.
# It is an override, never a skip: a skipped shell test is a vacuous pass.
BASH = os.environ.get("TEST_BASH") or "bash"


def _ssot_harness(tmp_path: Path, initial: dict[str, str]) -> tuple[Path, Path]:
    """A fake curl modelling PER-ENVIRONMENT Infisical secrets. Returns (bin, state)."""
    state = tmp_path / "state"
    state.mkdir()
    for env, value in initial.items():
        (state / f"ssot_{env}").write_text(value, encoding="utf-8")
    (state / "pin_cp.test").write_text("sha256:" + "a" * 64, encoding="utf-8")

    body = r'''
import os, sys, json, urllib.parse
S = os.environ["FAKE_STATE"]
args = sys.argv[1:]
method, body, url = "GET", "", ""
i = 0
while i < len(args):
    a = args[i]
    if a == "-X": method = args[i+1]; i += 2; continue
    if a == "-d": body = args[i+1]; i += 2; continue
    if a in ("-H", "--doh-url", "-A", "-o", "-w"): i += 2; continue
    if a.startswith("http"): url = a
    i += 1
u = urllib.parse.urlparse(url)
env = (urllib.parse.parse_qs(u.query).get("environment") or [""])[0]
P = lambda f: os.path.join(S, f)
if "/auth/universal-auth/login" in url:
    print(json.dumps({"accessToken": "t"})); sys.exit(0)
if "/api/v3/secrets/raw/CP_ADMIN_API_TOKEN" in url:
    print(json.dumps({"secret": {"secretValue": "cp"}})); sys.exit(0)
if "/api/v3/secrets/raw/LOCAL_TENANT_IMAGE" in url:
    if method in ("PATCH", "POST"):
        p = json.loads(body); env = p.get("environment") or env
        open(P("ssot_%s" % env), "w").write(p["secretValue"])
        open(P("writes.log"), "a").write("WROTE %s\n" % env)
        print("{}"); sys.exit(0)
    try: v = open(P("ssot_%s" % env)).read()
    except OSError: v = ""
    print(json.dumps({"secret": {"secretValue": v}})); sys.exit(0)
if url.endswith("/cp/admin/runtime-image"):
    print(json.dumps({"pins": [{"template_name": "molecule-tenant", "region": "global",
                                "image_digest": open(P("pin_cp.test")).read(), "git_sha": "old"}]})); sys.exit(0)
if url.endswith("/cp/admin/runtime-image/promote"):
    open(P("pin_cp.test"), "w").write(json.loads(body)["image_digest"]); print("{}"); sys.exit(0)
sys.exit(22)
'''
    (tmp_path / "curl.py").write_text(body, encoding="utf-8")
    bindir = tmp_path / "bin"
    bindir.mkdir()
    (bindir / "curl").write_text(
        f'#!/bin/sh\nexec "{sys.executable}" "{tmp_path / "curl.py"}" "$@"\n', encoding="utf-8"
    )
    (bindir / "curl").chmod(0o755)
    (bindir / "python3").write_text(
        f'#!/bin/sh\nexec "{sys.executable}" "$@"\n', encoding="utf-8"
    )
    (bindir / "python3").chmod(0o755)

    repo_root = tmp_path / "root"
    (repo_root / ".gitea" / "scripts").mkdir(parents=True)
    (repo_root / ".gitea" / "scripts" / "registry-manifest-state.py").write_text(
        'print("sha256:" + "d"*64)\n', encoding="utf-8"
    )
    return bindir, state


def test_boot_secret_is_written_to_the_env_it_was_given(tmp_path: Path):
    """INFISICAL_ENV must steer the LOCAL_TENANT_IMAGE write, per invocation.

    This is the behavioural half of the blocking defect. When the apply loop
    pinned INFISICAL_ENV=prod outside the loop, the second control plane's
    iteration promoted the STAGING DB pin and then re-wrote the PROD boot
    secret — and because write_ssot_pin re-read the value the first iteration
    had just written, it logged `SSOT ok` and reported success for a write it
    never made to the staging environment. Only an assertion on WHICH
    environment received the write can catch that; a log line cannot.
    """
    bindir, state = _ssot_harness(
        tmp_path,
        {"prod": "registry.test/molecule-tenant:OLD-PROD",
         "staging": "registry.test/molecule-tenant:OLD-STAGING"},
    )
    env = os.environ.copy()
    env.update({
        "PATH": f"{bindir}{os.pathsep}{env['PATH']}",
        "FAKE_STATE": str(state),
        "REPO_ROOT": str(tmp_path / "root"),
        "DIGEST_SOURCE": "registry", "REG_USER": "u", "REG_TOKEN": "t",
        "INFISICAL_CLIENT_ID": "id", "INFISICAL_CLIENT_SECRET": "s",
        "INFISICAL_PROJECT_ID": "ws",
        "INFISICAL_ENV": "staging",           # <- the CP being promoted
        "TENANT_IMAGE_NAME": "registry.test/molecule-tenant",
        "CP_BASE_URL": "https://cp.test",
        "GITHUB_OUTPUT": str(tmp_path / "gho"),
    })
    r = subprocess.run(
        [BASH, str(SCRIPT), "--tag", "staging-deadbee", "--git-sha", "deadbeef"],
        capture_output=True, text=True, env=env, check=False,
    )
    assert r.returncode == 0, f"{r.stdout}\n{r.stderr}"

    assert (state / "ssot_staging").read_text(encoding="utf-8") == \
        "registry.test/molecule-tenant:staging-deadbee", "the staging boot secret was not updated"
    assert (state / "ssot_prod").read_text(encoding="utf-8") == \
        "registry.test/molecule-tenant:OLD-PROD", (
            "INFISICAL_ENV=staging wrote the PROD boot secret — that is the blocking defect"
        )
    assert (state / "writes.log").read_text(encoding="utf-8").split() == ["WROTE", "staging"]


def test_plan_and_apply_resolve_the_SAME_repository():
    """A one-line env addition with nothing pinning it is one refactor from regressing.

    The digest/plan steps interpolate the job-level `TENANT_IMAGE`; the apply
    hands the script `TENANT_IMAGE_NAME`. If those two ever diverge, the plan and
    the digest describe one repository while the apply mutates another — silently,
    because both halves succeed.
    """
    doc = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    job = doc["jobs"]["promote"]
    assert "TENANT_IMAGE" in job["env"], "TENANT_IMAGE must be a job-level env var"

    apply_env = _apply_step().get("env") or {}
    assert apply_env.get("TENANT_IMAGE_NAME") == "${{ env.TENANT_IMAGE }}", (
        "the apply must take the image name from the SAME job-level TENANT_IMAGE "
        "the plan and digest steps use, got %r" % apply_env.get("TENANT_IMAGE_NAME")
    )
    # Both the digest step and the plan step must reference that same variable.
    for step in doc["jobs"]["promote"]["steps"]:
        name = str(step.get("name", ""))
        if name.startswith(("Resolve the target digest", "Plan")):
            assert "${TENANT_IMAGE}" in step["run"], (
                "step %r does not derive its ref from the job-level TENANT_IMAGE" % name
            )
