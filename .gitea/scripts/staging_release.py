#!/usr/bin/env python3
"""Qualify the current staging image for production, without touching production.

The staging pin identifies the artifact; its provenance identifies the staging
run. Both the run and every release-validation job must have completed
successfully on that exact main commit. A published image alone is insufficient.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import sys
import urllib.error
import urllib.request

import pin_provenance as provenance

STAGING_WORKFLOW = "staging-tenant-cd.yml"
REQUIRED_JOBS = frozenset({
    "await-image", "advance-pin", "redeploy-fleet", "runtime-image-readiness",
    "e2e-smoke", "rollback-pin", "rollback-audit",
})


class ReleaseError(ValueError):
    """The candidate has no complete staging qualification."""


def tenant_pin(document: object) -> dict:
    pins = [p for p in provenance.rows_of(document)
            if p.get("template_name") == "molecule-tenant" and p.get("region", "global") == "global"]
    if len(pins) != 1:
        raise ReleaseError("staging must contain exactly one global molecule-tenant pin")
    return pins[0]


def qualify(pin: dict, run: dict, jobs: list[dict], repo: str) -> dict[str, str]:
    fields = provenance.parse(pin.get("notes") or "")
    provenance.resolve_repo(fields, repo)
    provenance.bind_stamp_to_run(fields, run)
    sha = pin.get("git_sha") or ""
    digest = pin.get("image_digest") or ""
    if not re.fullmatch(r"[0-9a-f]{40}", sha) or sha != run.get("head_sha"):
        raise ReleaseError("staging pin must identify the exact full commit tested by the staging run")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise ReleaseError("staging pin has no valid immutable image digest")
    if str(run.get("id")) != fields["run"]:
        raise ReleaseError("Actions returned a different run from the staging pin's provenance")
    if (provenance.workflow_file_of(run) != STAGING_WORKFLOW
            or run.get("head_branch") != "main"
            or run.get("path") != f"{STAGING_WORKFLOW}@refs/heads/main"
            or run.get("event") not in {"push", "workflow_dispatch"}):
        raise ReleaseError("release evidence must come from staging-tenant-cd on main")
    if run.get("status") != "completed" or run.get("conclusion") != "success":
        raise ReleaseError("staging validation has not completed successfully")
    by_name = {}
    for job in jobs:
        name = str(job.get("name") or "").split(" (", 1)[0]
        if name in by_name:
            raise ReleaseError(f"ambiguous staging job evidence: {name}")
        by_name[name] = job
    for name in sorted(REQUIRED_JOBS):
        job = by_name.get(name, {})
        if job.get("status") != "completed" or job.get("conclusion") != "success":
            raise ReleaseError(f"staging job {name} is missing or did not succeed")
        if not job.get("started_at") or str(job["started_at"]).startswith("1970-"):
            raise ReleaseError(f"staging job {name} has no execution evidence")
    return {"sha": sha, "sha7": sha[:7], "tag": f"staging-{sha[:7]}",
            "digest": digest, "staging_run": fields["run"]}


class RejectRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise urllib.error.HTTPError(req.full_url, code, "redirect refused", headers, fp)


def fetch(path: str, repo: str, base: str, token: str) -> object:
    request = urllib.request.Request(
        f"{base.rstrip('/')}/api/v1/repos/{repo}/{path}",
        headers={"Authorization": f"token {token}", "User-Agent": "curl/8.4.0"},
    )
    try:
        with urllib.request.build_opener(RejectRedirect).open(request, timeout=30) as response:
            body = response.read(1024 * 1024 + 1)
        if len(body) > 1024 * 1024:
            raise ReleaseError("Actions response exceeded the size limit")
        return json.loads(body)
    except urllib.error.HTTPError as exc:
        raise ReleaseError(f"Actions evidence unavailable: HTTP {exc.code}") from exc
    except (OSError, ValueError) as exc:
        raise ReleaseError(f"Actions evidence unreadable: {type(exc).__name__}") from exc


def read_jobs(run_id: str, repo: str, base: str, token: str) -> list[dict]:
    jobs = []
    for page in range(1, 41):
        response = fetch(f"actions/runs/{run_id}/jobs?limit=50&page={page}", repo, base, token)
        batch = response.get("jobs") if isinstance(response, dict) else None
        if not isinstance(batch, list) or any(not isinstance(j, dict) for j in batch):
            raise ReleaseError("Actions jobs response is invalid")
        if not batch:
            return jobs
        jobs.extend(batch)
    raise ReleaseError("Actions job pagination did not complete")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pins", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    try:
        repo = provenance.expected_repo()
        base = provenance.normalize_base(os.environ.get("GITHUB_SERVER_URL", ""))
        token = os.environ.get("GITEA_TOKEN", "")
        if not token:
            raise ReleaseError("GITEA_TOKEN is required to verify staging")
        pin = tenant_pin(json.loads(args.pins.read_text(encoding="utf-8")))
        fields = provenance.parse(pin.get("notes") or "")
        provenance.resolve_repo(fields, repo)
        run = fetch(f"actions/runs/{fields['run']}", repo, base, token)
        if not isinstance(run, dict):
            raise ReleaseError("Actions run response is invalid")
        release = qualify(pin, run, read_jobs(fields["run"], repo, base, token), repo)
        with args.output.open("w", encoding="utf-8") as output:
            for key, value in release.items():
                output.write(f"{key}={value}\n")
        print(f"Staging qualified {release['sha']} @ {release['digest']} (run {release['staging_run']})")
    except (ValueError, provenance.StampError, OSError) as exc:
        print(f"::error::{exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
