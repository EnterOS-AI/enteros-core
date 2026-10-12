#!/usr/bin/env python3
"""Parse the production control-plane endpoint table.

Each production control plane is THREE coupled facts, and they must travel
together:

    <label>=<base_url>=<infisical_env>

WHY THE INFISICAL ENV BELONGS HERE. `advance-staging-tenant-pin.sh` uses
INFISICAL_ENV for two things — fetching the CP admin token, and writing the
`LOCAL_TENANT_IMAGE` boot secret through write_ssot_pin. Those boot secrets are
per control plane: `molecule-cp-prod` boots with MOLECULE_ENV=production and
resolves LOCAL_TENANT_IMAGE from Infisical `prod`, while `molecule-cp-staging`
boots with MOLECULE_ENV=staging and resolves it from `staging`. Measured
2026-08-08, they held different values pointing at different registry hosts.

Setting INFISICAL_ENV once, outside the loop that varies the base URL, therefore
promotes the second control plane's DB pin and then rewrites the FIRST one's
boot secret. It is silent: write_ssot_pin re-reads the value it already wrote,
finds it correct, and logs `SSOT ok` — affirming a write it never made to the
right environment. That is the "pin rolled, boot SSOT stale, every fresh org
broke" mechanism the promote script exists to prevent.

There is deliberately NO default env. A missing third field is an error, not an
inherited `prod`, because inheriting is exactly the bug.

Usage:
    prod_cp_endpoints.py "<label>=<url>=<env>[,<label>=<url>=<env>...]"
Emits one TAB-separated `label<TAB>base_url<TAB>infisical_env` row per endpoint.
Exit 2 on any malformed or unknown-environment entry — fail closed, because a
half-parsed endpoint table silently drops a control plane.
"""

from __future__ import annotations

import sys

# The Infisical environment SLUGS. `production` is NOT one of them: a promote
# once wrote to a nonexistent environment because of exactly that confusion,
# so it is rejected by name rather than allowed to 404 at run time.
VALID_ENVS = {"prod", "staging", "dev"}


def parse_endpoints(spec: str) -> list[tuple[str, str, str]]:
    out: list[tuple[str, str, str]] = []
    seen_labels: set[str] = set()
    entries = [e.strip() for e in (spec or "").split(",") if e.strip()]
    if not entries:
        raise ValueError("the endpoint table is empty — at least one control plane is required")
    for entry in entries:
        parts = entry.split("=")
        if len(parts) != 3:
            raise ValueError(
                f"endpoint {entry!r} must be <label>=<base_url>=<infisical_env>; "
                f"the infisical env is REQUIRED and is never inherited, because "
                f"a shared env writes one control plane's LOCAL_TENANT_IMAGE for all of them"
            )
        label, base, env = (p.strip() for p in parts)
        if not label or not base or not env:
            raise ValueError(f"endpoint {entry!r} has an empty field")
        if not base.startswith(("http://", "https://")):
            raise ValueError(f"endpoint {entry!r} base url must be http(s)://")
        if env not in VALID_ENVS:
            raise ValueError(
                f"endpoint {entry!r} names Infisical environment {env!r}, which is not "
                f"one of {sorted(VALID_ENVS)}. Note the production slug is `prod`, "
                f"not `production`."
            )
        if label in seen_labels:
            raise ValueError(f"duplicate endpoint label {label!r}")
        seen_labels.add(label)
        out.append((label, base.rstrip("/"), env))
    return out


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(
            "usage: prod_cp_endpoints.py '<label>=<url>=<env>[,...]'",
            file=sys.stderr,
        )
        return 2
    try:
        rows = parse_endpoints(argv[1])
    except ValueError as exc:
        print(f"::error::{exc}", file=sys.stderr)
        return 2
    for label, base, env in rows:
        print(f"{label}\t{base}\t{env}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
