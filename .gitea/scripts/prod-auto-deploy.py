#!/usr/bin/env python3
"""Production auto-deploy helpers for Gitea Actions.

The workflow keeps network side effects in shell/curl, but centralizes the
release decision shape here so it has unit coverage: disable flag parsing,
target tag selection, CP payload construction, and status-context selection.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
from urllib.parse import quote


TRUE_VALUES = {"1", "true", "yes", "on", "disabled", "disable"}
PROD_CP_URL = "https://api.moleculesai.app"
DEFAULT_REQUIRED_CONTEXTS = [
    "CI / all-required (push)",
    "Secret scan / Scan diff for credential-shaped strings (push)",
]
TERMINAL_FAILURE_STATES = {"failure", "error", "cancelled", "canceled", "skipped"}
REDEPLOY_PATH = "/cp/admin/tenants/redeploy-fleet"


def truthy_flag(value: str | None) -> bool:
    if value is None:
        return False
    return value.strip().lower() in TRUE_VALUES


def _int_env(env: dict[str, str], name: str, default: int, minimum: int = 1) -> int:
    raw = env.get(name, "")
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError as exc:
        raise ValueError(f"{name} must be an integer, got {raw!r}") from exc
    if value < minimum:
        raise ValueError(f"{name} must be >= {minimum}, got {value}")
    return value


def build_plan(env: dict[str, str]) -> dict:
    sha = env.get("GITHUB_SHA", "").strip()
    if not sha:
        raise ValueError("GITHUB_SHA is required")

    disabled_value = env.get("PROD_AUTO_DEPLOY_DISABLED", "")
    if truthy_flag(disabled_value):
        return {
            "enabled": False,
            "sha": sha,
            "disabled_reason": f"PROD_AUTO_DEPLOY_DISABLED={disabled_value}",
        }

    short_sha = sha[:7]
    target_tag = env.get("PROD_AUTO_DEPLOY_TARGET_TAG", "").strip() or f"staging-{short_sha}"
    canary_slug = env.get("PROD_AUTO_DEPLOY_CANARY_SLUG", "hongming").strip()
    body = {
        "target_tag": target_tag,
        "soak_seconds": _int_env(env, "PROD_AUTO_DEPLOY_SOAK_SECONDS", 60, minimum=0),
        "batch_size": _int_env(env, "PROD_AUTO_DEPLOY_BATCH_SIZE", 3),
        "dry_run": truthy_flag(env.get("PROD_AUTO_DEPLOY_DRY_RUN", "")),
        # confirm:true ack required by CP /cp/admin/tenants/redeploy-fleet
        # contract (cp#228 / task #308) for fleet-wide intent. Empty body
        # / {confirm:false} / {only_slugs:[]} → 400. This caller is the
        # production auto-deploy step that rolls every live tenant (canary
        # + fan-out), no slug scoping, so confirm:true is correct.
        "confirm": True,
    }
    if canary_slug:
        body["canary_slug"] = canary_slug

    cp_url = env.get("CP_URL", "").strip() or PROD_CP_URL
    if cp_url != PROD_CP_URL and not truthy_flag(env.get("PROD_ALLOW_NON_PROD_CP_URL", "")):
        raise ValueError(
            f"Refusing production deploy to CP_URL={cp_url!r}; "
            f"set PROD_ALLOW_NON_PROD_CP_URL=true for an explicit non-prod drill"
        )

    return {
        "enabled": True,
        "sha": sha,
        "short_sha": short_sha,
        "target_tag": target_tag,
        "cp_url": cp_url,
        "body": body,
    }


def latest_status_for_context(statuses: list[dict], context: str) -> dict | None:
    """Return the first matching status.

    Gitea's combined-status response is newest-first in practice. The merge
    queue relies on the same contract; keeping the selector explicit makes
    stale duplicate contexts easy to test.
    """

    for status in statuses:
        if status.get("context") == context:
            return status
    return None


def ci_context_state(statuses: list[dict], context: str) -> str:
    status = latest_status_for_context(statuses, context)
    if not status:
        return "missing"
    return str(status.get("status") or status.get("state") or "missing").lower()


def context_is_satisfied(state: str) -> bool:
    return state == "success"


def context_is_terminal_failure(state: str) -> bool:
    return state in TERMINAL_FAILURE_STATES


def required_contexts(env: dict[str, str]) -> list[str]:
    raw = env.get("PROD_AUTO_DEPLOY_REQUIRED_CONTEXTS", "")
    if not raw.strip():
        return DEFAULT_REQUIRED_CONTEXTS
    return [line.strip() for line in raw.replace(",", "\n").splitlines() if line.strip()]


def chunks(items: list[str], size: int) -> list[list[str]]:
    return [items[i : i + size] for i in range(0, len(items), size)]


class RolloutFailed(RuntimeError):
    def __init__(self, message: str, response: dict):
        super().__init__(message)
        self.response = response


def slugs_from_redeploy_response(body: dict) -> list[str]:
    slugs: list[str] = []
    for row in body.get("results") or []:
        slug = str(row.get("slug") or "").strip()
        if slug:
            slugs.append(slug)
    return slugs


def scoped_redeploy_body(base: dict, slugs: list[str]) -> dict:
    body = dict(base)
    body.pop("canary_slug", None)
    body["only_slugs"] = slugs
    body["soak_seconds"] = 0
    body["batch_size"] = max(1, len(slugs))
    return body


def cp_api_json(method: str, url: str, token: str, body: dict | None = None) -> tuple[int, dict]:
    data = None
    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/json",
    }
    if body is not None:
        data = json.dumps(body).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", errors="replace")
        try:
            parsed = json.loads(raw)
        except json.JSONDecodeError:
            parsed = {"error": raw[:500]}
        return exc.code, parsed


def plan_rollout_slugs(cp_url: str, token: str, body: dict, redeploy=None) -> list[str]:
    if redeploy is None:
        redeploy = redeploy_scoped
    dry_run_body = dict(body)
    dry_run_body["dry_run"] = True
    status, resp = redeploy(cp_url, token, dry_run_body)
    if status != 200:
        raise RuntimeError(f"dry-run redeploy-fleet returned HTTP {status}: {resp.get('error', '')}")
    if resp.get("ok") is not True:
        raise RuntimeError(f"dry-run redeploy-fleet reported ok={resp.get('ok')}: {resp.get('error', '')}")
    slugs = slugs_from_redeploy_response(resp)
    if not slugs:
        raise RuntimeError("dry-run redeploy-fleet returned no rollout candidates")
    return slugs


def redeploy_scoped(cp_url: str, token: str, body: dict) -> tuple[int, dict]:
    return cp_api_json("POST", f"{cp_url}{REDEPLOY_PATH}", token, body)


def _raise_for_redeploy_result(status: int, body: dict, slugs: list[str]) -> None:
    if status != 200 or body.get("ok") is not True:
        raise RuntimeError(
            "redeploy scoped call failed for "
            f"{','.join(slugs)}: HTTP {status}, ok={body.get('ok')}"
        )


def execute_scoped_rollout(
    plan: dict,
    token: str,
    list_slugs=plan_rollout_slugs,
    redeploy=redeploy_scoped,
    sleep=time.sleep,
) -> dict:
    cp_url = plan["cp_url"]
    base_body = plan["body"]
    all_slugs = list_slugs(cp_url, token, base_body)
    batch_size = int(base_body.get("batch_size") or 1)
    canary_slug = str(base_body.get("canary_slug") or "").strip()
    dry_run = bool(base_body.get("dry_run"))
    aggregate = {"ok": True, "results": []}

    if canary_slug:
        if canary_slug not in all_slugs:
            raise RuntimeError(f"configured canary slug {canary_slug!r} is not a running tenant")
        body = scoped_redeploy_body(base_body, [canary_slug])
        print(f"POST {cp_url}{REDEPLOY_PATH} only_slugs={','.join(body['only_slugs'])}")
        status, resp = redeploy(cp_url, token, body)
        aggregate["results"].extend(resp.get("results") or [])
        try:
            _raise_for_redeploy_result(status, resp, [canary_slug])
        except RuntimeError as exc:
            aggregate["ok"] = False
            aggregate["error"] = str(exc)
            raise RolloutFailed(str(exc), aggregate) from exc
        soak_seconds = int(base_body.get("soak_seconds") or 0)
        if soak_seconds > 0 and not dry_run:
            print(f"Canary passed; soaking locally for {soak_seconds}s")
            sleep(soak_seconds)

    remaining = [slug for slug in all_slugs if slug != canary_slug]
    for group in chunks(remaining, batch_size):
        body = scoped_redeploy_body(base_body, group)
        print(f"POST {cp_url}{REDEPLOY_PATH} only_slugs={','.join(group)}")
        status, resp = redeploy(cp_url, token, body)
        aggregate["results"].extend(resp.get("results") or [])
        try:
            _raise_for_redeploy_result(status, resp, group)
        except RuntimeError as exc:
            aggregate["ok"] = False
            aggregate["error"] = str(exc)
            raise RolloutFailed(str(exc), aggregate) from exc

    return aggregate


def rollout_from_plan_file(plan_path: str, response_path: str, env: dict[str, str]) -> None:
    token = env.get("CP_ADMIN_API_TOKEN", "").strip()
    if not token:
        raise ValueError("CP_ADMIN_API_TOKEN is required for production auto-deploy")
    with open(plan_path, "r", encoding="utf-8") as fh:
        plan = json.load(fh)
    if not plan.get("enabled"):
        raise RuntimeError("production auto-deploy plan is disabled")
    try:
        response = execute_scoped_rollout(plan, token)
    except RolloutFailed as exc:
        response = exc.response
        with open(response_path, "w", encoding="utf-8") as fh:
            json.dump(response, fh, sort_keys=True)
            fh.write("\n")
        raise
    with open(response_path, "w", encoding="utf-8") as fh:
        json.dump(response, fh, sort_keys=True)
        fh.write("\n")


def _api_json(url: str, token: str) -> dict:
    req = urllib.request.Request(url, headers={"Authorization": f"token {token}"})
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")[:500]
        raise RuntimeError(f"GET {url} -> HTTP {exc.code}: {body}") from exc


def _api_json_optional(url: str, token: str) -> tuple[int, dict | None]:
    req = urllib.request.Request(url, headers={"Authorization": f"token {token}"})
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            return resp.status, json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        if exc.code == 404:
            return exc.code, None
        body = exc.read().decode("utf-8", errors="replace")[:300]
        print(f"::warning::GET {url} -> HTTP {exc.code}: {body}", file=sys.stderr)
        return exc.code, None


def live_disable_flag(env: dict[str, str]) -> str:
    """Return a live disable value from Gitea variables when readable.

    Gitea evaluates `${{ vars.* }}` once when the job starts. This API read is
    the emergency re-check immediately before production side effects.
    """

    token = env.get("GITEA_TOKEN", "").strip()
    if not token:
        return ""
    host = env.get("GITEA_HOST", "git.moleculesai.app")
    repo = env.get("GITHUB_REPOSITORY", "molecule-ai/molecule-core")
    variable = quote("PROD_AUTO_DEPLOY_DISABLED", safe="")
    url = f"https://{host}/api/v1/repos/{repo}/actions/variables/{variable}"
    status, body = _api_json_optional(url, token)
    if status != 200 or not isinstance(body, dict):
        return ""
    return str(body.get("data") or body.get("value") or "")


def assert_not_disabled(env: dict[str, str]) -> None:
    plan = build_plan(env)
    if not plan.get("enabled"):
        raise RuntimeError(plan.get("disabled_reason", "production auto-deploy disabled"))
    live_value = live_disable_flag(env)
    if truthy_flag(live_value):
        raise RuntimeError(f"PROD_AUTO_DEPLOY_DISABLED={live_value} (live Gitea variable)")


def wait_for_ci_context(env: dict[str, str]) -> str:
    host = env.get("GITEA_HOST", "git.moleculesai.app")
    repo = env.get("GITHUB_REPOSITORY", "molecule-ai/molecule-core")
    sha = env.get("GITHUB_SHA", "").strip()
    token = env.get("GITEA_TOKEN", "").strip()
    contexts = required_contexts(env)
    interval = _int_env(env, "CI_STATUS_POLL_INTERVAL_SECONDS", 15)
    timeout = _int_env(env, "CI_STATUS_TIMEOUT_SECONDS", 1800)

    if not sha:
        raise ValueError("GITHUB_SHA is required")
    if not token:
        raise ValueError("GITEA_TOKEN is required to wait for CI status")

    url = f"https://{host}/api/v1/repos/{repo}/commits/{sha}/status"
    deadline = time.time() + timeout
    last_states: dict[str, str] = {}
    while time.time() <= deadline:
        body = _api_json(url, token)
        statuses = body.get("statuses") or []
        states = {context: ci_context_state(statuses, context) for context in contexts}
        for context, state in states.items():
            if state != last_states.get(context):
                print(f"CI context {context!r}: {state}", file=sys.stderr)
        last_states = states

        failures = [
            f"{context}={state}"
            for context, state in states.items()
            if context_is_terminal_failure(state)
        ]
        if failures:
            raise RuntimeError(
                "Required CI context failed; refusing production deploy: "
                + ", ".join(failures)
            )
        if all(context_is_satisfied(state) for state in states.values()):
            return "success"
        time.sleep(interval)
    last = ", ".join(f"{context}={state}" for context, state in last_states.items()) or "none"
    raise TimeoutError(f"Timed out waiting {timeout}s for required CI contexts; last_states={last}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    sub.add_parser("plan", help="print production deploy plan as JSON")
    sub.add_parser("assert-enabled", help="fail if production deploy is currently disabled")
    sub.add_parser("wait-ci", help="block until required CI context is green")
    rollout_parser = sub.add_parser("rollout", help="execute canary-first scoped production rollout")
    rollout_parser.add_argument("--plan", required=True, help="path to prod-auto-deploy plan JSON")
    rollout_parser.add_argument("--response", required=True, help="path to write aggregate response JSON")
    args = parser.parse_args()

    try:
        if args.command == "plan":
            print(json.dumps(build_plan(dict(os.environ)), sort_keys=True))
            return 0
        if args.command == "assert-enabled":
            assert_not_disabled(dict(os.environ))
            return 0
        if args.command == "wait-ci":
            wait_for_ci_context(dict(os.environ))
            return 0
        if args.command == "rollout":
            rollout_from_plan_file(args.plan, args.response, dict(os.environ))
            return 0
    except Exception as exc:  # noqa: BLE001 - CLI should render operator-friendly errors.
        print(f"::error::{exc}", file=sys.stderr)
        return 1
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
