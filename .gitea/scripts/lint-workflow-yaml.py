#!/usr/bin/env python3
"""lint-workflow-yaml — enforce this repository's Gitea workflow policy.

The first rules originated on Gitea 1.22.6. The live server is newer and now
documents `workflow_run` plus richer dispatch support, but those shapes remain
disabled here until a deliberate fleet migration validates runner behavior,
status-context emission, dispatch callers, and the existing compensating bots.
The lint must describe that local compatibility policy, not claim the current
server lacks features it now exposes.

Rules (4 fatal + 1 fatal cross-file + 1 heuristic-warn):
  1. `workflow_dispatch.inputs:` block — legacy flat-dispatch policy retained
     pending a reviewed migration of dispatch callers and fixtures.
     Memory: feedback_gitea_workflow_dispatch_inputs_unsupported.
     Origin: 2026-05-11 PyPI freeze (publish-runtime).
  2. `on: workflow_run:` event — supported by the current server but not yet
     adopted by this repository's status/reaper topology.
  3. `name:` containing `/` — breaks the
     `<workflow> / <job> (<event>)` commit-status context convention;
     downstream parsers (sop-checklist, status-reaper) tokenize on `/`.
  4. `name:` collision across files — Gitea routes commit-status updates
     by `name` and behavior on collision is undefined (status-reaper
     rev1 fail-loud).
  5. Cross-repo `uses: org/repo/path@ref` — blocked while
     `[actions].DEFAULT_ACTIONS_URL=github` is the server default;
     resolves to github.com/<org-suspended>/... and 404s.
     Memory: feedback_gitea_cross_repo_uses_blocked. Cross-link: task #109.
  6. (HEURISTIC, warn-not-fail) Steps reference `https://api.github.com`
     or `https://github.com/.../releases/download` without a
     workflow-level `env.GITHUB_SERVER_URL` set to the Gitea instance.
     Memory: feedback_act_runner_github_server_url.
  7. Production deploy/redeploy workflows may not rely only on Gitea
     `concurrency.cancel-in-progress: false` for serialization. The guard
     remains defense in depth after historical queued-run cancellation.
  8. Production deploy/redeploy workflows may not dump raw CP responses or
     raw `.error` fields into CI logs/summaries.
  9. Production deploy/redeploy workflows must expose an operational control:
     kill switch for auto deploys or rollback tag for manual deploys.
  10. Docker health checks must not run `docker info | head` under pipefail.
      `head` closes the pipe early, `docker info` can exit nonzero from
      SIGPIPE, and the step can falsely report Docker daemon failure.
  11. DUPLICATE MAPPING KEYS anywhere in a workflow file. Gitea's workflow
      parser REJECTS a duplicate key and refuses to schedule the file at
      all, but `yaml.safe_load` treats duplicates as legal and silently
      keeps the LAST one. That asymmetry is the dangerous part: the file
      reads fine to every Python-side check while the workflow simply stops
      existing in CI.
      Observed 2026-08-10 on `local-provision-e2e.yml`: a second `env:` on a
      step that already had one. Gitea never scheduled the workflow, so BOTH
      of its contexts stopped being posted; the PR read 43/43 success with an
      entire workflow deleted from CI, and `Lint workflow YAML`,
      lint-phantom-gate, lint-bp-context-emit-match,
      lint-required-context-exists-in-bp and lint-required-no-paths ALL
      posted success on that commit. Meanwhile safe_load had dropped three
      secrets from the step's env without a word.
      A missing status is invisible under branch protection `['*']` ("every
      POSTED context must be success"), so absence is not merely undetected —
      it is MORE likely to look green. Hence: fatal, and checked with a
      loader that rejects duplicates rather than through `safe_load`.

Per `feedback_smoke_test_vendor_truth_not_shape_match`, legacy fixtures retain
the exact incident shapes. They test the repository policy, not a claim about
the live server version.

Usage:
  python3 .gitea/scripts/lint-workflow-yaml.py
    Lint every `*.yml` in `.gitea/workflows/`.

  python3 .gitea/scripts/lint-workflow-yaml.py --workflow-dir <path>
    Lint a custom directory (used by tests/test_lint_workflow_yaml.py).

Exit codes:
  0 — clean OR only heuristic-warnings emitted.
  1 — at least one fatal rule (1-5) violated.
  2 — YAML parse error or argv usage error.
"""
from __future__ import annotations

import argparse
import collections
import glob
import os
import re
import sys
from pathlib import Path
from typing import Any, Iterable

try:
    import yaml
except ImportError:
    print("::error::PyYAML is required. Install with: pip install PyYAML", file=sys.stderr)
    sys.exit(2)


# YAML quirk: bare `on:` at the top level parses to the Python `True`
# (because `on` is a YAML 1.1 boolean alias). Handle both keys.
def _get_on(d: dict) -> Any:
    if not isinstance(d, dict):
        return None
    if "on" in d:
        return d["on"]
    if True in d:
        return d[True]
    return None


# ---------------------------------------------------------------------------
# Rule 1 — legacy flat-dispatch repository policy
# ---------------------------------------------------------------------------

def check_workflow_dispatch_inputs(filename: str, doc: Any) -> list[str]:
    """Return per-violation error lines if `workflow_dispatch.inputs` is set."""
    errors: list[str] = []
    on = _get_on(doc)
    if not isinstance(on, dict):
        return errors
    wd = on.get("workflow_dispatch")
    if isinstance(wd, dict) and wd.get("inputs"):
        errors.append(
            f"::error file={filename}::Rule 1 (FATAL): "
            f"`on.workflow_dispatch.inputs:` block detected. This repository "
            f"retains the legacy flat-dispatch policy until dispatch callers, "
            f"runner behavior, and CI fixtures are migrated together. Drop the "
            f"`inputs:` block and derive parameters from tag name / env / "
            f"external query, or land a dedicated policy-migration PR."
        )
    return errors


# ---------------------------------------------------------------------------
# Rule 2 — workflow_run not yet adopted by this repository
# ---------------------------------------------------------------------------

def check_workflow_run_event(filename: str, doc: Any) -> list[str]:
    """Return per-violation error lines if `on: workflow_run:` is used."""
    errors: list[str] = []
    on = _get_on(doc)
    if isinstance(on, dict) and "workflow_run" in on:
        errors.append(
            f"::error file={filename}::Rule 2 (FATAL): `on: workflow_run:` "
            f"event used. The current Gitea server supports this event, but "
            f"this repository has not migrated its status/reaper topology to "
            f"it. Use the established `schedule:`/external dispatch or `push:` "
            f"trigger until a dedicated end-to-end migration removes Rule 2."
        )
    elif isinstance(on, list) and "workflow_run" in on:
        errors.append(
            f"::error file={filename}::Rule 2 (FATAL): `on: workflow_run` "
            f"in event list. Repository policy has not adopted this current "
            f"Gitea feature; migrate Rule 2 and its fixtures deliberately."
        )
    return errors


# ---------------------------------------------------------------------------
# Rule 3 — name: contains "/" (breaks status-context tokenization)
# ---------------------------------------------------------------------------

def check_name_with_slash(filename: str, doc: Any) -> list[str]:
    """Return per-violation error lines if workflow `name:` contains a slash."""
    errors: list[str] = []
    if not isinstance(doc, dict):
        return errors
    name = doc.get("name")
    if isinstance(name, str) and "/" in name:
        errors.append(
            f"::error file={filename}::Rule 3 (FATAL): workflow `name: "
            f"{name!r}` contains `/`. The commit-status context convention "
            f"is `<workflow> / <job> (<event>)`; embedding `/` in the "
            f"workflow name makes downstream parsers (sop-checklist, "
            f"status-reaper) tokenize ambiguously. Rename to use `-` or "
            f"` ` instead."
        )
    return errors


# ---------------------------------------------------------------------------
# Rule 4 — cross-file name collision
# ---------------------------------------------------------------------------

def check_name_collision_across_files(
    docs_by_file: dict[str, Any],
) -> list[str]:
    """Return per-collision error lines if two files share the same `name:`."""
    errors: list[str] = []
    by_name: dict[str, list[str]] = collections.defaultdict(list)
    for filename, doc in docs_by_file.items():
        if isinstance(doc, dict):
            n = doc.get("name")
            if isinstance(n, str) and n:
                by_name[n].append(filename)
    for n, files in sorted(by_name.items()):
        if len(files) > 1:
            errors.append(
                f"::error::Rule 4 (FATAL): workflow `name: {n!r}` collision "
                f"across {len(files)} files: {files}. Gitea routes "
                f"commit-status updates by `name`; collision yields "
                f"undefined behavior. Give each workflow a unique `name:`."
            )
    return errors


# ---------------------------------------------------------------------------
# Rule 5 — cross-repo `uses: org/repo/path@ref`
# ---------------------------------------------------------------------------

# `uses: <foo>@<ref>` — match the value form Gitea/act actually parse.
# We need to distinguish:
#   - `actions/checkout@<sha>`           OK (bare org/repo@ref, no subpath)
#   - `./.gitea/actions/foo`             OK (local path)
#   - `docker://image:tag`               OK (docker-image form)
#   - `molecule-ai/molecule-ci/.gitea/actions/audit-force-merge@main`  BAD
USES_CROSS_REPO_RE = re.compile(
    r"""^
    (?P<owner>[A-Za-z0-9_.\-]+)
    /
    (?P<repo>[A-Za-z0-9_.\-]+)
    /                       # mandatory subpath separator => cross-repo composite/reusable
    (?P<path>[^@\s]+)
    @
    (?P<ref>\S+)
    $""",
    re.VERBOSE,
)


def _iter_uses(doc: Any) -> Iterable[str]:
    """Yield every `uses:` string from job steps in a workflow document."""
    if not isinstance(doc, dict):
        return
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        return
    for job in jobs.values():
        if not isinstance(job, dict):
            continue
        # reusable workflow: `uses:` at the job level
        if isinstance(job.get("uses"), str):
            yield job["uses"]
        steps = job.get("steps")
        if not isinstance(steps, list):
            continue
        for step in steps:
            if isinstance(step, dict) and isinstance(step.get("uses"), str):
                yield step["uses"]


def _iter_run_blocks(doc: Any) -> Iterable[str]:
    """Yield every shell `run:` block from job steps in a workflow document."""
    if not isinstance(doc, dict):
        return
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        return
    for job in jobs.values():
        if not isinstance(job, dict):
            continue
        steps = job.get("steps")
        if not isinstance(steps, list):
            continue
        for step in steps:
            if isinstance(step, dict) and isinstance(step.get("run"), str):
                yield step["run"]


def check_cross_repo_uses(filename: str, doc: Any) -> list[str]:
    """Return per-violation error lines for cross-repo `uses:` references."""
    errors: list[str] = []
    for uses in _iter_uses(doc):
        # Skip docker:// and local ./
        if uses.startswith(("docker://", "./", "../")):
            continue
        m = USES_CROSS_REPO_RE.match(uses.strip())
        if m:
            errors.append(
                f"::error file={filename}::Rule 5 (FATAL): cross-repo "
                f"`uses: {uses}` detected. This repository's current "
                f"`[actions].DEFAULT_ACTIONS_URL=github` policy resolves it to "
                f"github.com/{m.group('owner')}/{m.group('repo')} which "
                f"404s (org suspended 2026-05-06). Inline the shared bash "
                f"into `.gitea/scripts/` until task #109 (actions mirror) "
                f"ships. Memory: feedback_gitea_cross_repo_uses_blocked."
            )
    return errors


# ---------------------------------------------------------------------------
# Rule 6 — heuristic: github.com/api refs without workflow-level
#          GITHUB_SERVER_URL (WARN-not-FAIL per halt-condition 3)
# ---------------------------------------------------------------------------

# Match `https://api.github.com/...` (API call) — that's the actionable
# pattern. We intentionally do NOT match `https://github.com/.../releases/
# download/...` (jq-release pin) nor `https://github.com/${{ github.repository
# }}` (OCI label) because those are documented benign references on current
# main and would 100% false-positive (3 hits, per Phase 1 audit).
GITHUB_API_REF_RE = re.compile(
    r"https://api\.github\.com\b|https://github\.com/api/",
    re.IGNORECASE,
)


PROD_CP_URL_RE = re.compile(r"https://api\.moleculesai\.app\b")
REDEPLOY_FLEET_RE = re.compile(r"\b/cp/admin/tenants/redeploy-fleet\b")
RUN_SETS_PIPEFAIL_RE = re.compile(r"(?m)^\s*set\s+-[^\n]*o\s+pipefail\b")
DOCKER_INFO_HEAD_PIPE_RE = re.compile(
    r"(?m)^\s*docker\s+info\b[^\n|]*\|\s*head\b"
)
RAW_CP_RESPONSE_RE = re.compile(
    r"""(?x)
    (?:\bjq\s+\.\s+["']?\$HTTP_RESPONSE["']?)
    |
    (?:\bcat\s+["']?\$HTTP_RESPONSE["']?)
    |
    (?:\|\s*\.error\b)
    """
)


def _has_workflow_level_server_url(doc: Any) -> bool:
    if not isinstance(doc, dict):
        return False
    env = doc.get("env")
    if isinstance(env, dict) and "GITHUB_SERVER_URL" in env:
        return True
    return False


def check_github_server_url_missing(filename: str, doc: Any, raw: str) -> list[str]:
    """Return warn-lines (NOT errors) if api.github.com is referenced without
    workflow-level GITHUB_SERVER_URL. Heuristic — false-positives possible.
    """
    warns: list[str] = []
    if not GITHUB_API_REF_RE.search(raw):
        return warns
    if _has_workflow_level_server_url(doc):
        return warns
    warns.append(
        f"::warning file={filename}::Rule 6 (WARN, heuristic): file "
        f"references `https://api.github.com` without a workflow-level "
        f"`env.GITHUB_SERVER_URL: https://git.moleculesai.app`. The "
        f"act_runner default for `${{{{ github.server_url }}}}` is "
        f"github.com, which can break actions that auth-condition on "
        f"server_url (e.g. actions/setup-go). If this curl is "
        f"intentionally hitting GitHub (e.g. public release pin), ignore. "
        f"Memory: feedback_act_runner_github_server_url."
    )
    return warns


# ---------------------------------------------------------------------------
# Rule 7-9 — production CI/CD hardening rules
# ---------------------------------------------------------------------------

def _is_production_redeploy_workflow(raw: str) -> bool:
    """Heuristic production-side-effect detector.

    We intentionally key on the production CP host plus the redeploy-fleet
    endpoint. Staging workflows call the same endpoint on staging-api and are
    governed by looser staging verification policy.
    """

    return bool(PROD_CP_URL_RE.search(raw) and REDEPLOY_FLEET_RE.search(raw))


def _iter_concurrency_blocks(doc: Any) -> Iterable[dict[str, Any]]:
    if not isinstance(doc, dict):
        return
    top = doc.get("concurrency")
    if isinstance(top, dict):
        yield top
    jobs = doc.get("jobs")
    if not isinstance(jobs, dict):
        return
    for job in jobs.values():
        if isinstance(job, dict) and isinstance(job.get("concurrency"), dict):
            yield job["concurrency"]


def check_production_concurrency(filename: str, doc: Any, raw: str) -> list[str]:
    errors: list[str] = []
    if not _is_production_redeploy_workflow(raw):
        return errors
    for block in _iter_concurrency_blocks(doc):
        if block.get("cancel-in-progress") is False:
            errors.append(
                f"::error file={filename}::Rule 7 (FATAL): production deploy "
                f"workflow uses `concurrency.cancel-in-progress: false`. "
                f"historical runner behavior cancelled queued runs despite "
                f"that setting, so it is not sufficient as the only production "
                f"serialization primitive. "
                f"Use an external queue/lock or make the deploy idempotent."
            )
    return errors


def check_production_raw_response_logging(filename: str, raw: str) -> list[str]:
    errors: list[str] = []
    if not _is_production_redeploy_workflow(raw):
        return errors
    if RAW_CP_RESPONSE_RE.search(raw):
        errors.append(
            f"::error file={filename}::Rule 8 (FATAL): production deploy "
            f"workflow appears to print a raw production CP response or raw "
            f"`.error` field. CI logs are persistent and broad-read. Redact "
            f"runtime/SSM error details; print counts, booleans, status "
            f"codes, and links to restricted observability instead."
        )
    return errors


def check_production_operational_control(filename: str, raw: str) -> list[str]:
    errors: list[str] = []
    if not _is_production_redeploy_workflow(raw):
        return errors
    has_kill_switch = "PROD_AUTO_DEPLOY_DISABLED" in raw
    has_rollback = "PROD_MANUAL_REDEPLOY_TARGET_TAG" in raw
    if not (has_kill_switch or has_rollback):
        errors.append(
            f"::error file={filename}::Rule 9 (FATAL): production deploy "
            f"workflow calls redeploy-fleet without an operational control. "
            f"Auto deploys need a `PROD_AUTO_DEPLOY_DISABLED` kill switch; "
            f"manual deploys need a `PROD_MANUAL_REDEPLOY_TARGET_TAG` "
            f"rollback/pin path."
        )
    return errors


# ---------------------------------------------------------------------------
# Rule 10 — docker info piped to head under pipefail
# ---------------------------------------------------------------------------

def check_docker_info_head_pipefail(filename: str, doc: Any) -> list[str]:
    errors: list[str] = []
    for run_block in _iter_run_blocks(doc):
        if not (
            RUN_SETS_PIPEFAIL_RE.search(run_block)
            and DOCKER_INFO_HEAD_PIPE_RE.search(run_block)
        ):
            continue
        errors.append(
            f"::error file={filename}::Rule 10 (FATAL): workflow runs "
            f"`docker info | head` after enabling `pipefail`. `head` can "
            f"close the pipe early, making `docker info` exit nonzero and "
            f"falsely fail the Docker daemon health check. Capture "
            f"`docker_info=\"$(docker info 2>&1)\"` first, then print a "
            f"bounded preview with `printf ... | sed -n '1,5p'`."
        )
        break
    return errors


# ---------------------------------------------------------------------------
# Driver
# ---------------------------------------------------------------------------

class DuplicateKeyError(yaml.YAMLError):
    """Raised when a workflow mapping declares the same key twice.

    Subclasses YAMLError so any caller that already handles "this file does
    not parse" keeps working — a duplicate key IS a parse failure as far as
    Gitea is concerned, even though PyYAML tolerates it.
    """

    def __init__(self, key: object, line: int, column: int) -> None:
        self.key = key
        self.line = line
        self.column = column
        super().__init__(
            f"duplicate mapping key {key!r} at line {line}, column {column}"
        )


class NoDuplicateKeyLoader(yaml.SafeLoader):
    """SafeLoader that REJECTS duplicate mapping keys instead of taking the last.

    Still a SafeLoader subclass, so it cannot construct arbitrary Python
    objects; it only makes the mapping constructor strict. Deliberately NOT
    `yaml.safe_load`, whose permissive mapping constructor is exactly what
    hides this defect.
    """


def _construct_mapping_no_duplicates(
    loader: yaml.SafeLoader, node: yaml.MappingNode, deep: bool = False
) -> dict:
    mapping: dict = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node, deep=deep)
        try:
            duplicate = key in mapping
        except TypeError:
            # Unhashable key — malformed YAML; let the normal loader path
            # surface it rather than masking it as a duplicate.
            duplicate = False
        if duplicate:
            raise DuplicateKeyError(
                key, key_node.start_mark.line + 1, key_node.start_mark.column + 1
            )
        mapping[key] = loader.construct_object(value_node, deep=deep)
    return mapping


NoDuplicateKeyLoader.add_constructor(
    yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG,
    _construct_mapping_no_duplicates,
)


def load_workflow_strict(raw: str):
    """Parse a workflow, rejecting duplicate mapping keys.

    Returns the parsed document. Raises DuplicateKeyError (a YAMLError) on a
    duplicate, or yaml.YAMLError on any other parse failure.

    On `yaml.load` with an explicit Loader: this is NOT unsafe load. The
    generic advice "use safe_load instead" does not apply and would defeat
    the rule outright, because safe_load's permissive mapping constructor is
    the exact behaviour being guarded against — it accepts duplicates and
    keeps the last. `NoDuplicateKeyLoader` derives from `yaml.SafeLoader`, so
    it inherits SafeLoader's constructor table and still refuses
    `!!python/object` and friends; the sole override makes mappings STRICTER.
    Same pattern as `_LineLoader(yaml.SafeLoader)` in
    lint_continue_on_error_tracking.py.
    """
    return yaml.load(raw, Loader=NoDuplicateKeyLoader)  # noqa: S506 — SafeLoader subclass, stricter not looser


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        description="Lint Gitea Actions workflow YAML for repository-policy violations."
    )
    p.add_argument(
        "--workflow-dir",
        default=".gitea/workflows",
        help="Directory of workflow *.yml files (default: .gitea/workflows).",
    )
    args = p.parse_args(argv)

    wf_dir = Path(args.workflow_dir)
    if not wf_dir.exists():
        # Empty / missing dir = nothing to lint, not a failure.
        print(f"::notice::No workflow directory at {wf_dir}; skipping.")
        return 0

    yml_paths = sorted(
        glob.glob(str(wf_dir / "*.yml")) + glob.glob(str(wf_dir / "*.yaml"))
    )
    if not yml_paths:
        print(f"::notice::No workflow files in {wf_dir}; nothing to lint.")
        return 0

    fatal_errors: list[str] = []
    warnings: list[str] = []
    docs_by_file: dict[str, Any] = {}

    for path in yml_paths:
        rel = os.path.relpath(path)
        try:
            raw = Path(path).read_text()
            # Rule 11: parse with the duplicate-rejecting loader, NOT
            # safe_load. Gitea refuses to schedule a file with a duplicate
            # key, while safe_load silently keeps the last one — so a
            # safe_load-based lint reports success on a workflow that has
            # vanished from CI. Parsed once here and reused by every check
            # below, so the strict rule cannot be bypassed.
            doc = load_workflow_strict(raw)
        except DuplicateKeyError as e:
            fatal_errors.append(
                f"::error file={rel},line={e.line}::duplicate mapping key "
                f"{e.key!r} (line {e.line}, column {e.column}). Gitea's "
                f"workflow parser REJECTS duplicate keys and will not "
                f"schedule this file, so every status it posts silently "
                f"disappears — which reads as green under branch protection "
                f"['*'], not as a failure. Note `yaml.safe_load` accepts "
                f"this and keeps only the LAST {e.key!r}, dropping the "
                f"earlier one's contents. Merge the blocks into one key."
            )
            continue
        except yaml.YAMLError as e:
            fatal_errors.append(
                f"::error file={rel}::YAML parse error: {e}. Cannot lint "
                f"a file the parser rejects."
            )
            continue
        docs_by_file[rel] = doc

        # Per-file checks
        fatal_errors.extend(check_workflow_dispatch_inputs(rel, doc))
        fatal_errors.extend(check_workflow_run_event(rel, doc))
        fatal_errors.extend(check_name_with_slash(rel, doc))
        fatal_errors.extend(check_cross_repo_uses(rel, doc))
        fatal_errors.extend(check_production_concurrency(rel, doc, raw))
        fatal_errors.extend(check_production_raw_response_logging(rel, raw))
        fatal_errors.extend(check_production_operational_control(rel, raw))
        fatal_errors.extend(check_docker_info_head_pipefail(rel, doc))
        warnings.extend(check_github_server_url_missing(rel, doc, raw))

    # Cross-file checks
    fatal_errors.extend(check_name_collision_across_files(docs_by_file))

    # Emit warnings first (non-blocking)
    for w in warnings:
        print(w)

    if not fatal_errors:
        n = len(yml_paths)
        print(
            f"::notice::lint-workflow-yaml: {n} workflow file(s) checked, "
            f"no fatal repository-policy violations. "
            f"({len(warnings)} heuristic warning(s) emitted.)"
        )
        return 0

    # Emit fatal errors
    print(
        f"::error::lint-workflow-yaml: {len(fatal_errors)} fatal violation(s) "
        f"across {len(yml_paths)} workflow file(s). See rule documentation "
        f"in .gitea/scripts/lint-workflow-yaml.py docstring."
    )
    for e in fatal_errors:
        print(e)
    return 1


if __name__ == "__main__":
    sys.exit(main())
