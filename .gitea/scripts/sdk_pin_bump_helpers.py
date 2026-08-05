#!/usr/bin/env python3
"""Helpers for the `sdk-pin-bump` workflow.

Deliberately a script rather than inline heredocs in the workflow: a `run: |`
block scalar with a column-0 heredoc terminator silently breaks the YAML, and
molecule-core has five separate lints (lint-workflow-yaml, lint_schedule_budget,
lint-publish-timeout, lint_no_coe_on_required, lint-required-context-exists-in-bp)
that can only fail-closed on a file they cannot parse — so an unparseable
workflow degrades several unrelated gates at once. Keeping the logic here also
makes it unit-testable.

Subcommands:
  registry-sha        print sha256 of the embedded SDK registry in the RESOLVED
                      module (the exact bytes llmregistry.RawYAML embeds)
  set-canonical-sha   rewrite the canonicalRegistrySHA256 constant in place
  pr-payload          write the Gitea create-PR JSON body
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys

SDK_MODULE = "go.moleculesai.app/sdk/gen/go"
CANONICAL_RE = re.compile(r'(const canonicalRegistrySHA256 = ")[0-9a-f]{64}(")')


def cmd_registry_sha(args: argparse.Namespace) -> int:
    """Hash the registry inside the resolved module.

    Computed from the module the build now links, NOT scraped out of a test
    failure message — scraping would make the constant a copy of whatever the
    test happened to print, which is circular.
    """
    try:
        out = subprocess.run(
            ["go", "list", "-m", "-f", "{{.Dir}}", SDK_MODULE],
            capture_output=True, text=True, check=True, cwd=args.module_dir or None,
        ).stdout.strip()
    except (subprocess.CalledProcessError, FileNotFoundError) as exc:
        print(f"::error::cannot locate {SDK_MODULE} in the module cache: {exc}", file=sys.stderr)
        return 1
    if not out:
        print(f"::error::`go list -m` returned no directory for {SDK_MODULE}", file=sys.stderr)
        return 1
    yaml_path = os.path.join(out, "llmregistry", "llm-registry.yaml")
    try:
        data = open(yaml_path, "rb").read()
    except OSError as exc:
        print(f"::error::cannot read embedded registry {yaml_path}: {exc}", file=sys.stderr)
        return 1
    if not data:
        # An empty file hashes to a perfectly valid sha — and would silently
        # become the new checkpoint. Refuse.
        print(f"::error::embedded registry {yaml_path} is EMPTY", file=sys.stderr)
        return 1
    sys.stdout.write(hashlib.sha256(data).hexdigest())
    return 0


def replace_canonical_sha(text: str, new_sha: str) -> str:
    """Return `text` with the canonical sha constant set to `new_sha`.

    Raises ValueError unless EXACTLY one constant was replaced — zero means the
    constant moved or was renamed (and a silent no-op would leave a stale
    checkpoint next to a bumped pin, which is the precise failure this whole
    lane exists to prevent); more than one means the file grew an ambiguity.
    """
    if not re.fullmatch(r"[0-9a-f]{64}", new_sha):
        raise ValueError(f"not a sha256 hex digest: {new_sha!r}")
    out, n = CANONICAL_RE.subn(lambda m: m.group(1) + new_sha + m.group(2), text)
    if n != 1:
        raise ValueError(
            f"expected exactly 1 canonicalRegistrySHA256 constant, replaced {n}"
        )
    return out


def cmd_set_canonical_sha(args: argparse.Namespace) -> int:
    try:
        text = open(args.path, encoding="utf-8").read()
        open(args.path, "w", encoding="utf-8").write(
            replace_canonical_sha(text, args.sha)
        )
    except (OSError, ValueError) as exc:
        print(f"::error::{exc}", file=sys.stderr)
        return 1
    return 0


PR_BODY_TEMPLATE = """Automated by the `sdk-pin-bump` lane.

Moves `go.moleculesai.app/sdk/gen/go` to molecule-ai-sdk@`{head}`, regenerates \
`internal/providers/gen/registry_gen.go`, and moves the paired \
`canonicalRegistrySHA256` checkpoint to `{sha256}`.

**Review the registry diff.** The regenerated projection shows exactly which \
model ids entered or left each runtime's arms. An id entering a `platform` arm \
becomes selectable at workspace-create; an id leaving stops being selectable. \
That is a product-visible change, which is why this lane opens a PR and does \
not merge it.

`go test ./internal/providers/...` passed on this branch before the PR was \
opened. If a test in this repo encoded the OLD registry state, the lane goes \
red instead of opening a PR — adoption then needs a human, which is the \
intended outcome.
"""


def cmd_pr_payload(args: argparse.Namespace) -> int:
    """Build the create-PR JSON.

    The body is templated HERE rather than as a multi-line shell string in the
    workflow: a `run: |` block scalar cannot carry column-0 continuation lines
    without terminating the block and corrupting the YAML.
    """
    body = PR_BODY_TEMPLATE.format(head=args.head, sha256=args.registry_sha)
    json.dump(
        {
            "base": args.base,
            "head": args.branch,
            "title": f"chore(sdk): bump sdk/gen/go pin to {args.sha12}",
            "body": body,
        },
        open(args.out, "w", encoding="utf-8"),
    )
    return 0


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("registry-sha")
    p.add_argument("--module-dir", default="")
    p.set_defaults(func=cmd_registry_sha)

    p = sub.add_parser("set-canonical-sha")
    p.add_argument("path")
    p.add_argument("sha")
    p.set_defaults(func=cmd_set_canonical_sha)

    p = sub.add_parser("pr-payload")
    p.add_argument("out")
    p.add_argument("branch")
    p.add_argument("sha12")
    p.add_argument("head")
    p.add_argument("registry_sha")
    p.add_argument("--base", default="main")
    p.set_defaults(func=cmd_pr_payload)

    args = ap.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
