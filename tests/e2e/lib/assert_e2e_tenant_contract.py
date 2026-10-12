#!/usr/bin/env python3
"""The e2e scripts must speak a contract the tenant actually IMPLEMENTS.

Three checks, one idea. A call is only real if the server is listening for it:

  1. ROUTES   — every `tenant_call` path must be a route the tenant registers.
  2. RAW URLS — every raw `$TENANT_URL/...` curl must hit a route too (any method).
                Plenty of call sites bypass tenant_call; a tenant_call-only scan
                would leave them exactly as unguarded as the bugs below.
  3. HEADERS  — every `X-*` header the scripts send must be one the tenant reads
                (a handler `GetHeader`/`Header.Get`) or declares (router.go CORS).

WHY THIS EXISTS
---------------
`test_staging_full_saas.sh` called `GET /activity?workspace_id=<id>` in TWO places
(steps 9b and 10). That route does not exist — the tenant serves
`/workspaces/:id/activity` and the handler reads the id from the PATH
(router.go: `wsAuth := r.Group("/workspaces/:id", ...)`). The call 404'd to the
canvas SPA, so a JSON assertion got Next.js HTML back, and step 10 polled that
404 for 60s before hard-failing with "delegation-provenance pipeline regression".
The pipeline was fine. The URL was not.

Nobody had ever seen it, because on staging step 9 (memory) 503'd and aborted the
run BEFORE 9b, on every pass — so those steps had literally never executed. A step
that has never once run is not coverage; it is a placeholder. The ephemeral gate is
the first environment to get past memory (core#4166 bundles the sidecar), which is
how both bugs surfaced at once.

The HEADER check is the same bug wearing a different hat, found one CI run later.
Step 10 sent `X-Source-Workspace-Id: $PARENT_ID` to identify the delegating parent.
No Go code reads that header — grep the tree, zero hits. The proxy reads exactly one
caller header, `X-Workspace-ID` (a2a_proxy.go), and that value is what becomes
`activity_logs.source_id`. So the caller was anonymous, source_id was NULL, and the
assertion "child activity records parent as source" could not ever be true. Same
signature as the routes bug: the assertion had been soft-logged, so a contract that
never once held read as green for months.

A header nobody reads is silence, and silence in a test looks exactly like success.

A wrong URL or a dead header should not cost a live 5-minute CI provision to
discover, and neither should be able to masquerade as a product regression. This is
the cheap structural guard: it needs no infra, no containers and no network — just
the server source and the scripts.

Non-goals: it does not check auth, method semantics, query params, or header VALUES.
It answers one question per check — "is this path routable at all?", "is this header
read at all?" — because those are the two that were silently wrong.
"""
from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
WS_SERVER = ROOT / "workspace-server"
ROUTER = WS_SERVER / "internal" / "router" / "router.go"
E2E_DIR = ROOT / "tests" / "e2e"

# A group's Go variable -> its path prefix, e.g. wsAuth := r.Group("/workspaces/:id", ...)
GROUP_RE = re.compile(r'(\w+)\s*:?=\s*\w+\.Group\(\s*"([^"]*)"')
# A route registration, e.g. wsAuth.GET("/activity", ...)
ROUTE_RE = re.compile(r'(\w+)\.(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|Any)\(\s*"([^"]*)"')
# A tenant_call in a shell script, e.g. tenant_call GET "/workspaces/$ID/activity?limit=5"
# The path is OPTIONALLY quoted — `tenant_call POST /workspaces` is just as common
# and just as breakable. A quotes-only regex silently skipped 12 live call sites,
# which meant the next `tenant_call GET /activity` (unquoted) — the exact bug class
# this file exists to kill — would have been waved through under an "OK: all N
# paths are routable" banner. A guard that reports on a subset it does not name is
# worse than no guard.
CALL_RE = re.compile(r'tenant_call\s+([A-Z]+)\s+"?([^"\s\\;)]+)')

# A RAW curl straight at the tenant, e.g. curl ... "$TENANT_URL/registry/register".
# tenant_call is the common path, but plenty of call sites bypass it (they need an
# extra header, or predate the helper) — and those were invisible to a tenant_call-
# only scan, which is exactly the kind of blind spot that lets a dead route live.
# The method is not reliably recoverable from a raw curl (it may be -X POST, or an
# implicit GET, or built in a variable), so these are checked for routability under
# ANY method: the question is still "does this path exist at all?"
RAW_URL_RE = re.compile(r'\$TENANT_URL(/[^"\'\s\\]*)')

# A header the server READS, e.g. c.GetHeader("X-Workspace-ID") / r.Header.Get("X-Timeout")
GO_HEADER_RE = re.compile(r'(?:GetHeader|Header\.Get)\(\s*"([^"]+)"')
# The CORS allow-list in router.go — headers the tenant DECLARES but the edge consumes
# (X-Molecule-Org-Slug is routing metadata: no Go handler reads it, yet it is real).
CORS_RE = re.compile(r"AllowHeaders:\s*\[\]string\{([^}]*)\}")
# A header a script SENDS, e.g. -H "X-Workspace-ID: $ws_id"
SH_HEADER_RE = re.compile(r'-H\s+"(X-[A-Za-z0-9-]+)\s*:')

# Headers addressed to something OTHER than the tenant, so the tenant is rightly deaf
# to them. Keep this list tiny and justified — each entry is a promise that the header
# has a real reader somewhere else.
NON_TENANT_HEADERS = {
    "X-Scope-OrgID",  # Grafana Loki multi-tenancy (tests/e2e/lib/obs.sh) — never our server
}


def registered_paths() -> set[tuple[str, str]]:
    """(METHOD, raw gin pattern) for every route the tenant registers."""
    src = ROUTER.read_text()
    prefixes: dict[str, str] = {}
    for var, prefix in GROUP_RE.findall(src):
        prefixes[var] = prefix
    out: set[tuple[str, str]] = set()
    for var, method, path in ROUTE_RE.findall(src):
        full = prefixes.get(var, "") + path
        methods = ("GET", "POST", "PUT", "PATCH", "DELETE") if method == "Any" else (method,)
        for m in methods:
            out.add((m, full))
    return out


def segments(path: str) -> list[str]:
    return [s for s in path.split("?", 1)[0].strip("/").split("/") if s != ""]


def matches(call: str, route: str) -> bool:
    """Does a call path match a gin route pattern?

    Gin semantics, honoured properly rather than flattened:
      ':param'  matches exactly ONE segment
      '*wild'   matches the REST of the path (must be last) — this is why
                `/files/config.yaml` is routable by `/files/*path`, and why a
                naive "collapse everything variable" normalizer reports a false
                positive on it.
    Shell side: a '$VAR' segment could hold anything, so it matches any single
    route segment. We are permissive there on purpose — the question is
    routability, and a false ALARM here would be worse than useless.
    """
    c, r = segments(call), segments(route)
    i = 0
    for j, rseg in enumerate(r):
        if rseg.startswith("*"):
            return True  # wildcard swallows the remainder (gin requires it be last)
        if i >= len(c):
            return False
        cseg = c[i]
        if rseg.startswith(":") or cseg.startswith("$"):
            i += 1
            continue
        if rseg != cseg:
            return False
        i += 1
    return i == len(c)


def normalize(path: str) -> str:
    """Display-only: what the call's shape looks like, for the error message."""
    return "/" + "/".join(":x" if s.startswith("$") else s for s in segments(path))


# Calls that are SUPPOSED to 404 — negative gates pinning a removed route.
# Keep this list tiny and justified; each entry is a deliberate assertion that a
# route stays gone (e.g. shared-context, removed in favour of memory v2's team:
# namespace — see RFC #2789 / task #304).
INTENTIONALLY_ABSENT = {
    ("GET", "/workspaces/:x/shared-context"),
}


# Build-tag-gated, test-ONLY routes: the tenant DOES serve these, but only in an
# E2E/debug build (registered behind a Go build tag), so they never appear in the
# default router.go text this parser reads. An e2e script that calls one is NOT
# hitting a dead route — the route exists in the image under test — so exempt it
# from the "path the tenant does NOT route" failure. Keep this list tiny and
# justified; each entry is a promise that the route is real in the test build.
#
#   POST /workspaces/:id/test-busy — busy-injection hook that pins a workspace's
#   active_tasks high so scenario 10b can force-hibernate a GENUINELY busy box
#   (core#4528, RFC #92). Never compiled into the production tenant binary.
KNOWN_TEST_ONLY_ROUTES = {
    ("POST", "/workspaces/:x/test-busy"),
}


def is_routable(method: str, path: str, routes: set[tuple[str, str]]) -> bool:
    return any(m == method and matches(path, r) for m, r in routes)


def known_headers() -> set[str]:
    """Headers the tenant reads in a handler, plus the ones it declares via CORS.

    Case-insensitive by contract: HTTP header names are, and Go's Header.Get
    canonicalizes. We fold to lowercase so `X-Workspace-Id` and `X-Workspace-ID`
    are the same header — because to the server, they are.
    """
    out: set[str] = set()
    for go in WS_SERVER.rglob("*.go"):
        if go.name.endswith("_test.go"):
            continue
        src = go.read_text(errors="replace")
        out.update(h.lower() for h in GO_HEADER_RE.findall(src))
        for block in CORS_RE.findall(src):
            out.update(h.strip().strip('"').lower() for h in block.split(",") if h.strip())
    return out


def scripts() -> list[Path]:
    return sorted([*E2E_DIR.glob("*.sh"), *(E2E_DIR / "lib").glob("*.sh")])


def check_routes(routes: set[tuple[str, str]]) -> tuple[int, list[str]]:
    bad: list[str] = []
    checked = 0
    for script in scripts():
        for lineno, line in enumerate(script.read_text().splitlines(), 1):
            if line.lstrip().startswith("#"):
                continue  # a comment quoting a dead shape is documentation, not a call
            for method, path in CALL_RE.findall(line):
                if path.startswith("$"):  # fully dynamic path, cannot be checked
                    continue
                checked += 1
                if (method, normalize(path)) in INTENTIONALLY_ABSENT:
                    continue
                if (method, normalize(path)) in KNOWN_TEST_ONLY_ROUTES:
                    continue  # build-tag-gated route: real in the test image, absent from router.go text
                if not is_routable(method, path, routes):
                    bad.append(f"  {script.relative_to(ROOT)}:{lineno}: {method} {path}\n"
                               f"      normalized: {normalize(path)}  -- not in router.go")
    return checked, bad


def check_raw_urls(routes: set[tuple[str, str]]) -> tuple[int, list[str]]:
    """Raw `$TENANT_URL/...` curls must hit a path the tenant routes (any method)."""
    any_method = {r for _, r in routes}
    bad: list[str] = []
    checked = 0
    for script in scripts():
        for lineno, line in enumerate(script.read_text().splitlines(), 1):
            if line.lstrip().startswith("#"):
                continue
            for path in RAW_URL_RE.findall(line):
                # "$TENANT_URL$path" — the path is a variable, nothing to check.
                if not segments(path) or path.startswith("/$"):
                    continue
                checked += 1
                if not any(matches(path, r) for r in any_method):
                    bad.append(f"  {script.relative_to(ROOT)}:{lineno}: raw curl to {path}\n"
                               f"      normalized: {normalize(path)}  -- no route in router.go "
                               f"under ANY method")
    return checked, bad


def check_headers(known: set[str]) -> tuple[int, list[str]]:
    bad: list[str] = []
    checked = 0
    for script in scripts():
        for lineno, line in enumerate(script.read_text().splitlines(), 1):
            if line.lstrip().startswith("#"):
                continue  # ditto — naming a dead header in prose must not fail CI
            for hdr in SH_HEADER_RE.findall(line):
                checked += 1
                if hdr in NON_TENANT_HEADERS:
                    continue
                if hdr.lower() not in known:
                    bad.append(f"  {script.relative_to(ROOT)}:{lineno}: sends {hdr}\n"
                               f"      no workspace-server handler reads it, and router.go's CORS "
                               f"does not declare it -- the server is DEAF to this header")
    return checked, bad


def main() -> int:
    routes = registered_paths()
    if len(routes) < 50:
        print(f"::error::only parsed {len(routes)} routes from {ROUTER} — the parser is "
              "probably broken, and a broken parser here would silently pass everything")
        return 2

    known = known_headers()
    # Same self-check as the route parser: an empty/tiny header set would pass
    # everything, which is the one failure mode a guard must never have.
    if "x-workspace-id" not in known or len(known) < 10:
        print(f"::error::only parsed {len(known)} request headers from {WS_SERVER} (and "
              "X-Workspace-ID was not among them) — the parser is probably broken, and a "
              "broken parser here would silently pass everything")
        return 2

    n_routes, bad_routes = check_routes(routes)
    n_raw, bad_raw = check_raw_urls(routes)
    n_headers, bad_headers = check_headers(known)

    if bad_routes or bad_raw:
        print("::error::e2e calls a path the tenant does NOT route (it will 404 to the "
              "canvas SPA, and a JSON assertion will get HTML back):")
        print("\n".join(bad_routes + bad_raw))
    if bad_headers:
        print("::error::e2e sends a header the tenant NEVER reads (it is silently dropped, "
              "so whatever it was supposed to convey — caller identity, provenance — is "
              "simply absent, and the assertion that depends on it cannot ever hold):")
        print("\n".join(bad_headers))
    if bad_routes or bad_raw or bad_headers:
        return 1

    print(f"OK: all {n_routes} tenant_call path(s) + {n_raw} raw $TENANT_URL path(s) are "
          f"routable ({len(routes)} routes in router.go), and all {n_headers} X-* header(s) "
          f"the e2e sends are read by the tenant ({len(known)} known).")
    return 0


if __name__ == "__main__":
    sys.exit(main())
