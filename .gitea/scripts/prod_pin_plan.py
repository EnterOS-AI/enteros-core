#!/usr/bin/env python3
"""Render the production molecule-tenant pin PLAN (current vs target).

Read-only. Used by promote-prod-tenant-pin.yml's plan step, which runs
unconditionally — including while the production freeze is on — so an operator
can review exactly what a promote WOULD do without anything mutating.

A script rather than an inline heredoc: a `run: |` block scalar cannot carry a
column-0 heredoc terminator without corrupting the YAML, and several core lints
fail closed on a workflow file they cannot parse.

MULTI-CONTROL-PLANE. The production fleet is NOT served by one control plane.
On the prod box `molecule-cp-prod` (:8092) and `molecule-cp-staging` (:8096)
each own a `runtime_image_pins` table, and tenants are assigned to one or the
other by their `cp-env` label. Verified live on 2026-08-08, the two disagreed:

    :8092  molecule-tenant  sha256:747d3df6…  git f3c9eaf244…
    :8096  molecule-tenant  sha256:f1972d8c…  git 1802ebe22e…

Reading only the first endpoint renders a plan that is true for at most half
the fleet. Worse, a promote against one endpoint returns 200 and the reconciler
reports the out-of-scope tenants converged *against their own pin* — so the miss
is silent and looks exactly like success. Every endpoint is therefore read, the
absent-row check is applied PER endpoint, and the summary states, per CP,
whether a promote is required there.

Usage:
    prod_pin_plan.py --target <image_ref> [--target-digest <sha256:...>] \
        <label>=<pins.json> [<label>=<pins.json> ...]

    # legacy single-endpoint form, retained so an older caller cannot break
    prod_pin_plan.py <pins.json> <target_image_ref>

Exit 1 if ANY endpoint's document is unreadable or is missing the
(molecule-tenant, global) row — an absent row must not render as a clean
"nothing to compare".
"""

from __future__ import annotations

import json
import sys

TEMPLATE_NAME = "molecule-tenant"
REGION = "global"


def find_tenant_pin(doc: object) -> dict | None:
    rows = doc if isinstance(doc, list) else (
        (doc or {}).get("pins") or (doc or {}).get("data") or (doc or {}).get("items") or []
    )
    for r in rows:
        if not isinstance(r, dict):
            continue
        if r.get("template_name") == TEMPLATE_NAME and r.get("region", REGION) == REGION:
            return r
    return None


def parse_args(argv: list[str]) -> tuple[str, str | None, list[tuple[str, str]]]:
    """Return (target_ref, target_digest, [(label, path), ...])."""
    # Legacy positional form: <pins.json> <target_image_ref>
    if len(argv) == 3 and not argv[1].startswith("--") and "=" not in argv[1]:
        return argv[2], None, [("control-plane", argv[1])]

    target: str | None = None
    target_digest: str | None = None
    endpoints: list[tuple[str, str]] = []
    i = 1
    while i < len(argv):
        arg = argv[i]
        if arg == "--target":
            i += 1
            if i >= len(argv):
                raise ValueError("--target requires a value")
            target = argv[i]
        elif arg == "--target-digest":
            i += 1
            if i >= len(argv):
                raise ValueError("--target-digest requires a value")
            target_digest = argv[i]
        elif "=" in arg and not arg.startswith("--"):
            label, path = arg.split("=", 1)
            if not label or not path:
                raise ValueError(f"malformed endpoint spec {arg!r} (want <label>=<path>)")
            endpoints.append((label, path))
        else:
            raise ValueError(f"unrecognised argument {arg!r}")
        i += 1

    if target is None:
        raise ValueError("--target <image_ref> is required")
    if not endpoints:
        raise ValueError("at least one <label>=<pins.json> endpoint is required")
    return target, target_digest, endpoints


def main(argv: list[str]) -> int:
    try:
        target, target_digest, endpoints = parse_args(argv)
    except ValueError as exc:
        print(f"::error::{exc}", file=sys.stderr)
        print(
            "usage: prod_pin_plan.py --target <image_ref> [--target-digest <sha256:...>] "
            "<label>=<pins.json> [...]",
            file=sys.stderr,
        )
        return 2

    # Read and validate EVERY endpoint before rendering anything. A partial plan
    # is worse than no plan: it is the shape an operator would sign off on.
    failures: list[str] = []
    parsed: list[tuple[str, dict]] = []
    for label, path in endpoints:
        try:
            with open(path, encoding="utf-8") as fh:
                doc = json.load(fh)
        except (OSError, json.JSONDecodeError) as exc:
            failures.append(
                f"::error::[{label}] cannot parse the pins document ({exc}) — refusing to "
                f"render a plan from an unreadable response"
            )
            continue
        row = find_tenant_pin(doc)
        if row is None:
            failures.append(
                f"::error::[{label}] no runtime_image_pins row for "
                f"({TEMPLATE_NAME}, {REGION}) in the CP response. That is not "
                f"'no change needed' — it means the reserved tenant pin row is "
                f"missing on this control plane, and a fresh provision there "
                f"would fail."
            )
            continue
        parsed.append((label, row))

    if failures:
        for line in failures:
            print(line, file=sys.stderr)
        return 1

    lines = [
        "## Production molecule-tenant pin — PLAN (no mutation)",
        "",
        f"* TARGET image    : `{target}`",
    ]
    if target_digest:
        lines.append(f"* TARGET digest   : `{target_digest}`")
    lines += [
        "",
        f"Control planes read: **{len(parsed)}** "
        f"({', '.join(label for label, _ in parsed)})",
        "",
    ]

    needed: list[str] = []
    for label, cur in parsed:
        cur_digest = cur.get("image_digest") or ""
        if target_digest:
            if cur_digest == target_digest:
                verdict = "already at target"
            else:
                verdict = "**PROMOTE REQUIRED**"
                needed.append(label)
        else:
            verdict = "(no --target-digest supplied — cannot compare)"
        lines += [
            f"### {label}",
            "",
            f"* current digest  : `{cur_digest or '(none)'}`",
            f"* current git_sha : `{cur.get('git_sha') or '(none)'}`",
            f"* promoted_at     : {cur.get('promoted_at', '(unknown)')}",
            f"* promoted_by     : {cur.get('promoted_by', '(unknown)')}",
            f"* verdict         : {verdict}",
            "",
        ]

    if target_digest:
        if needed:
            lines.append(
                f"**{len(needed)} of {len(parsed)} control plane(s) need the promote: "
                f"{', '.join(needed)}.** A promote must be executed against EACH of "
                f"them. Promoting one endpoint returns 200 while tenants on the other "
                f"stay on their own pin — the miss is silent."
            )
        else:
            lines.append(
                f"All {len(parsed)} control plane(s) are already at the target digest. "
                f"Nothing to promote."
            )
        lines.append("")

    lines += [
        "Applying would move BOTH SSOTs — the `runtime_image_pins` row AND the",
        "`LOCAL_TENANT_IMAGE` boot secret — via",
        "`scripts/deploy/advance-staging-tenant-pin.sh`, which verifies each write",
        "landed. Rolling the running containers is a SEPARATE step",
        "(`redeploy-staging-fleet.sh --cp-env production --tag <tag>`): promoting the",
        "pin changes what FRESH provisions get, not what is already running.",
        "",
        "Native plugin versions are compiled INTO the tenant binary, so a pin",
        "promote alone delivers no native-plugin change.",
    ]
    sys.stdout.write("\n".join(lines) + "\n")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
