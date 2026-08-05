#!/usr/bin/env python3
"""Render the production molecule-tenant pin PLAN (current vs target).

Read-only. Used by promote-prod-tenant-pin.yml's plan step, which runs
unconditionally — including while the production freeze is on — so an operator
can review exactly what a promote WOULD do without anything mutating.

A script rather than an inline heredoc: a `run: |` block scalar cannot carry a
column-0 heredoc terminator without corrupting the YAML, and several core lints
fail closed on a workflow file they cannot parse.

Usage: prod_pin_plan.py <pins.json> <target_image_ref>
Exit 1 if the pins document is empty or the molecule-tenant/global row is absent
— an absent row must not render as a clean "nothing to compare".
"""

from __future__ import annotations

import json
import sys


def find_tenant_pin(doc: object) -> dict | None:
    rows = doc if isinstance(doc, list) else (
        (doc or {}).get("pins") or (doc or {}).get("data") or (doc or {}).get("items") or []
    )
    for r in rows:
        if r.get("template_name") == "molecule-tenant" and r.get("region") == "global":
            return r
    return None


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print("usage: prod_pin_plan.py <pins.json> <target_image_ref>", file=sys.stderr)
        return 2
    path, target = argv[1], argv[2]
    try:
        doc = json.load(open(path, encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        print(f"::error::cannot parse the pins document ({exc}) — refusing to render "
              f"a plan from an unreadable response", file=sys.stderr)
        return 1

    cur = find_tenant_pin(doc)
    if cur is None:
        print("::error::no runtime_image_pins row for (molecule-tenant, global) in the "
              "CP response. That is not 'no change needed' — it means the reserved "
              "tenant pin row is missing, and a fresh provision would fail.",
              file=sys.stderr)
        return 1

    lines = [
        "## Production molecule-tenant pin — PLAN (no mutation)",
        "",
        f"* current digest  : `{cur.get('image_digest', '(none)')}`",
        f"* current git_sha : `{cur.get('git_sha') or '(none)'}`",
        f"* promoted_at     : {cur.get('promoted_at', '(unknown)')}",
        f"* promoted_by     : {cur.get('promoted_by', '(unknown)')}",
        "",
        f"* TARGET image    : `{target}`",
        "",
        "Applying would move BOTH SSOTs — the `runtime_image_pins` row AND the",
        "`LOCAL_TENANT_IMAGE` boot secret — via",
        "`scripts/deploy/advance-staging-tenant-pin.sh`, which verifies each write",
        "landed. Rolling the running containers is a SEPARATE step",
        "(`redeploy-staging-fleet.sh --cp-env production --tag <tag>`): promoting the",
        "pin changes what FRESH provisions get, not what is already running.",
    ]
    out = "\n".join(lines) + "\n"
    sys.stdout.write(out)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
