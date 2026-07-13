#!/usr/bin/env python3
"""Shared path-filter helper for Gitea Actions workflows.

Computes changed files against the PR base SHA or push-before SHA and writes
boolean outputs to GITHUB_OUTPUT. If the diff base is missing or untrusted, the
helper fails open by setting every output in the selected profile to true.
"""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
from pathlib import Path

PROFILES: dict[str, dict[str, str]] = {
    "ci": {
        "platform": r"^workspace-server/",
        "canvas": r"^canvas/",
        "python": r"^workspace/",
        "scripts": r"^tests/e2e/|^scripts/|^infra/scripts/",
    },
    "handlers-postgres": {
        "handlers": (
            r"^workspace-server/internal/handlers/"
            r"|^workspace-server/internal/wsauth/"
            # #2148: registry-auth real-PG integration tests (CanCommunicate
            # parent_id hierarchy lives in internal/registry; org-admin token
            # revoke/validate lives in internal/orgtoken) run in this same
            # workflow, so a regression in either package MUST trigger the job.
            r"|^workspace-server/internal/registry/"
            r"|^workspace-server/internal/orgtoken/"
            # #2149: the scheduler real-PG integration tests run in this same
            # workflow (they reuse its migrated Postgres), so changes to the
            # scheduler package must trigger the job too.
            r"|^workspace-server/internal/scheduler/"
            # #2150: the db package's real-PG migration-replay-from-scratch
            # + InitPostgres ping tests also run in this same workflow (they
            # reuse its sibling Postgres, against a separate `molecule_replay`
            # database). Changes to db must trigger the job too.
            r"|^workspace-server/internal/db/"
            r"|^workspace-server/migrations/"
            r"|^\.gitea/workflows/handlers-postgres-integration\.yml$"
        ),
    },
    "e2e-api": {
        "api": r"^workspace-server/|^tests/e2e/|^\.gitea/workflows/e2e-api\.yml$",
    },
    # ── e2e-ephemeral: the ONE profile that is INVERTED, on purpose ──────────
    #
    # Every other profile above is an ALLOW-list: "run only if a changed path
    # matches." That encodes a bet — that we can enumerate everything able to
    # break the lane. For the ephemeral happy-path gate we LOST that bet: its
    # `on: paths:` filter listed workspace-server/, three e2e scripts and
    # tests/e2e/lib/, but the workflow also RUNS `bash tests/harness/dind.sh`
    # (the per-job isolation the gate's correctness depends on) — and
    # tests/harness/** was NOT in the filter. A PR editing the gate's own
    # harness therefore did not run the gate.
    #
    # So this profile is a DENY-list: run UNLESS every changed path is provably
    # inert. The negative lookahead matches any path that is NOT docs/** and NOT
    # a *.md — so `any(...)` in classify() is True the moment ONE substantive
    # file is touched, and False only for a docs-only diff.
    #
    # The point is the DIRECTION OF THE FAILURE. Forget a path in an allow-list
    # and the gate silently does not run: a coverage hole that ships bugs.
    # Forget one here and the gate runs when it needn't: ~6 wasted minutes.
    # Only one of those two mistakes is allowed to be the cheap one.
    "e2e-ephemeral": {
        "happy": r"^(?!docs/)(?!.*\.md$)",
    },
    # #1296: the E2E Peer Visibility gate is being flipped to a REQUIRED
    # status check. A required-check workflow may NOT carry an `on: paths:`
    # filter (lint-required-no-paths.py / feedback_path_filtered_workflow_
    # cant_be_required would wedge docs-only PRs). So the path-scoping that
    # used to live in e2e-peer-visibility.yml's `on:` block moves here and
    # is applied per-step inside the always-running job (mirrors the
    # handlers-postgres shape). The pattern set MUST mirror the old `on:
    # paths:` list, PLUS the wsauth + workspace_provision surface the
    # token-kinds fix (PR#2682) introduced — a regression in either would
    # break the literal list_peers assertion the gate exists to protect.
    "peer-visibility": {
        "peervis": (
            r"^workspace-server/internal/handlers/mcp\.go$"
            r"|^workspace-server/internal/handlers/mcp_tools\.go$"
            r"|^workspace-server/internal/middleware/"
            r"|^workspace-server/internal/handlers/registry\.go$"
            r"|^workspace-server/internal/handlers/workspace\.go$"
            r"|^workspace-server/internal/handlers/workspace_provision\.go$"
            r"|^workspace-server/internal/wsauth/"
            r"|^tests/e2e/test_peer_visibility_mcp_staging\.sh$"
            r"|^tests/e2e/test_peer_visibility_token_mint_staging\.sh$"
            r"|^tests/e2e/test_peer_visibility_mcp_local\.sh$"
            r"|^tests/e2e/lib/peer_visibility_assert\.sh$"
            r"|^\.gitea/workflows/e2e-peer-visibility\.yml$"
        ),
    },
    # mc#2996 / RFC#2843 #37: the template-delivery e2e is being flipped to a
    # REQUIRED status check. A required-check workflow may NOT carry an `on:
    # paths:` filter (lint-required-no-paths.py / feedback_path_filtered_
    # workflow_cant_be_required would wedge docs-only PRs), so the path-scoping
    # that used to live in template-delivery-e2e.yml's `on:` block moves here
    # and is applied per-step inside the always-running job (mirrors the
    # peer-visibility shape). This set MUST mirror the old `on: paths:` list —
    # the delivery surface (provisioning asset channel + post-online plugin
    # reconcile) whose regressions this gate exists to catch.
    "template-delivery": {
        "delivery": (
            r"^workspace-server/internal/provisioner/template_assets\.go$"
            r"|^workspace-server/internal/provisioner/gitea_template_assets\.go$"
            r"|^workspace-server/internal/provisioner/cp_provisioner\.go$"
            # #206 docker-less read-back surface (host-side /configs mirror +
            # Files-API serve) — the exact code the PR-build gate exercises.
            r"|^workspace-server/internal/provisioner/hostside_config\.go$"
            r"|^workspace-server/internal/handlers/templates\.go$"
            r"|^workspace-server/internal/router/router\.go$"
            r"|^workspace-server/internal/handlers/platform_agent\.go$"
            r"|^workspace-server/cmd/server/main\.go$"
            r"|^workspace-server/internal/handlers/org_import\.go$"
            r"|^workspace-server/internal/handlers/workspace\.go$"
            r"|^workspace-server/internal/handlers/template_plugins\.go$"
            r"|^workspace-server/internal/handlers/plugins_reconcile\.go$"
            r"|^workspace-server/internal/handlers/registry\.go$"
            r"|^workspace-server/internal/handlers/plugins_install_pipeline\.go$"
            r"|^workspace-server/internal/handlers/plugins_tracking\.go$"
            r"|^workspace-server/internal/plugins/source\.go$"
            r"|^manifest\.json$"
            # ROOT-FIX of the deploy-ordering deadlock: the gate's OWN plumbing
            # is DELIBERATELY excluded from the delivery trigger — the workflow
            # file `.gitea/workflows/template-delivery-e2e.yml`, the delivery
            # gate script `tests/harness/template-asset-delivery-gate.sh`, the
            # harness compose, and the retired deployed-staging script
            # `tests/e2e/test_template_delivery_e2e.sh` (now dispatch-only in
            # template-delivery-e2e-staging.yml). Keeping any of them in the
            # trigger re-created the circular block: a delivery PR could never
            # green its own gate until deployed, but deploying needed the merge
            # the red gate blocked — AND a gate-plumbing PR (which changes the
            # workflow/replay while main still lacks the read-back fix) would
            # build a stale main image and 59B-fail its own no-op change.
            # Gate-plumbing changes self-certify via code review; only real
            # workspace-server delivery-surface changes re-provision + assert
            # the served /configs bundle (>1KiB, not the 59B stub).
        ),
    },
}


def classify(profile: str, paths: list[str]) -> dict[str, bool]:
    patterns = PROFILES[profile]
    return {
        name: any(re.search(pattern, path) for path in paths)
        for name, pattern in patterns.items()
    }


def all_true(profile: str) -> dict[str, bool]:
    return {name: True for name in PROFILES[profile]}


def resolve_base(event_name: str, pr_base_sha: str, push_before: str) -> str:
    if event_name == "pull_request" and pr_base_sha:
        return pr_base_sha
    return push_before


def is_zero_sha(value: str) -> bool:
    return not value or bool(re.fullmatch(r"0+", value))


def run_git(args: list[str], *, timeout: int = 30) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["git", *args],
        check=False,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
    )


def base_exists(base: str) -> bool:
    return run_git(["cat-file", "-e", base]).returncode == 0


def fetch_base(base: str, base_ref: str) -> None:
    # Gitea may reject fetching an arbitrary unadvertised SHA from a shallow
    # PR checkout. Fetch the advertised base branch first, then fall back to
    # the SHA for hosts that allow it.
    if base_ref:
        run_git(["fetch", "--depth=1", "origin", base_ref])
    if not base_exists(base):
        run_git(["fetch", "--depth=1", "origin", base])


def deepen_base_ref(base_ref: str) -> None:
    if base_ref:
        run_git(["fetch", "--deepen=200", "origin", base_ref], timeout=60)


def merge_base(base: str) -> str | None:
    proc = run_git(["merge-base", base, "HEAD"])
    if proc.returncode != 0:
        return None
    value = proc.stdout.strip()
    return value or None


def changed_paths(base: str, *, use_merge_base: bool) -> list[str] | None:
    compare_base = base
    if use_merge_base:
        compare_base = merge_base(base) or ""
        if not compare_base:
            return None

    proc = run_git(["diff", "--name-only", compare_base, "HEAD"])
    if proc.returncode != 0:
        return None
    return [line for line in proc.stdout.splitlines() if line]


def write_outputs(values: dict[str, bool], output_path: str | None) -> None:
    lines = [f"{name}={'true' if value else 'false'}" for name, value in values.items()]
    if output_path:
        with Path(output_path).open("a", encoding="utf-8") as fh:
            for line in lines:
                fh.write(line + "\n")
    else:
        for line in lines:
            print(line)


def detect(
    profile: str,
    event_name: str,
    pr_base_sha: str,
    push_before: str,
    base_ref: str = "",
) -> dict[str, bool]:
    base = resolve_base(event_name, pr_base_sha, push_before)
    if is_zero_sha(base):
        return all_true(profile)

    if not base_exists(base):
        fetch_base(base, base_ref)
    if not base_exists(base):
        return all_true(profile)

    use_merge_base = event_name == "pull_request"
    if use_merge_base and base_ref and merge_base(base) is None:
        deepen_base_ref(base_ref)

    paths = changed_paths(base, use_merge_base=use_merge_base)
    if paths is None:
        return all_true(profile)
    return classify(profile, paths)


def parse_args(argv: list[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--profile", required=True, choices=sorted(PROFILES))
    parser.add_argument("--event-name", default=os.environ.get("GITHUB_EVENT_NAME", ""))
    parser.add_argument("--pr-base-sha", default="")
    parser.add_argument("--base-ref", default="")
    parser.add_argument(
        "--push-before",
        default=os.environ.get("GITHUB_EVENT_BEFORE", ""),
    )
    return parser.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    values = detect(
        args.profile,
        args.event_name,
        args.pr_base_sha,
        args.push_before,
        args.base_ref,
    )
    write_outputs(values, os.environ.get("GITHUB_OUTPUT"))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

