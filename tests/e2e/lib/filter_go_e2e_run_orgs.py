#!/usr/bin/env python3
"""Select exact run-and-tag-scoped Go E2E org identities from a CP roster."""

from __future__ import annotations

import argparse
import json
import re
import sys


RUN_ID_RE = re.compile(r"^[0-9]+$")
TAG_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,20}$")


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser()
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--tags", required=True)
    return parser


def _fail(message: str) -> int:
    print(f"invalid Go E2E teardown scope/roster: {message}", file=sys.stderr)
    return 2


def main() -> int:
    args = _parser().parse_args()
    run_id = args.run_id.strip()
    tags = args.tags.split()

    if not RUN_ID_RE.fullmatch(run_id):
        return _fail("run id must contain digits only")
    if not tags:
        return _fail("at least one teardown tag is required")
    if any(not TAG_RE.fullmatch(tag) for tag in tags):
        return _fail("tags must be lowercase alphanumeric DNS-label fragments")
    if len(tags) != len(set(tags)):
        return _fail("duplicate teardown tags are not allowed")

    try:
        roster = json.load(sys.stdin)
    except (json.JSONDecodeError, UnicodeDecodeError) as exc:
        return _fail(f"roster is not valid JSON: {exc}")
    if not isinstance(roster, dict) or not isinstance(roster.get("orgs"), list):
        return _fail("roster must be an object with an orgs array")

    tag_pattern = "|".join(re.escape(tag) for tag in sorted(tags))
    slug_re = re.compile(rf"^e2e-(?:{tag_pattern})-{re.escape(run_id)}-[0-9a-f]{{6}}$")

    matches: list[tuple[str, str]] = []
    identities: dict[str, str] = {}
    for row in roster["orgs"]:
        if not isinstance(row, dict):
            return _fail("every roster org entry must be an object")
        slug = row.get("slug")
        if not isinstance(slug, str) or not slug_re.fullmatch(slug):
            continue
        if row.get("instance_status") == "purged":
            continue
        org_id = row.get("id")
        if not isinstance(org_id, str) or not org_id.strip():
            return _fail(f"matching org {slug!r} has no string id")
        org_id = org_id.strip()
        previous = identities.get(slug)
        if previous is not None and previous != org_id:
            return _fail(f"matching slug {slug!r} has conflicting org ids")
        if previous is None:
            identities[slug] = org_id
            matches.append((slug, org_id))

    for slug, org_id in matches:
        print(f"{slug}\t{org_id}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
