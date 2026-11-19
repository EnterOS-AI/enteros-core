#!/usr/bin/env bash
# lint_cleanup_traps.sh — regression gate for the OSS-shape program's
# "all E2E tests must have proper cleanup" bar (RFC #2873).
#
# Asserts:
#   1. every shell file under tests/e2e/ that calls `mktemp` ALSO
#      installs an `EXIT` trap somewhere in the file.
#   2. every staging tenant E2E script that provisions a real org uses a
#      slug prefix caught by sweep-stale-e2e-orgs.yml and installs an
#      EXIT trap.
#
# These are the minimum-viable guarantees that scratch files and real
# staging EC2 tenants converge back to zero when an assertion or curl
# exits the script non-zero.
#
# Why this lints (instead of the test runner enforcing): shell scripts
# can't easily be wrapped by an outer harness without breaking the
# `WSID=… ./test_x.sh` invocation contract. Static gate is the cheap
# defense.
#
# Usage:
#   tests/e2e/lint_cleanup_traps.sh
#
# Exits non-zero if any test_*.sh has unmatched mktemp/trap. CI invokes
# it from the existing Shellcheck (E2E scripts) workflow.

set -euo pipefail

cd "$(dirname "$0")"
repo_root="$(cd ../.. && pwd)"

violations=0
for f in test_*.sh; do
  if grep -qE '\bmktemp\b' "$f"; then
    if ! grep -qE 'trap[[:space:]]+.*EXIT' "$f"; then
      echo "::error file=tests/e2e/$f::has 'mktemp' but no 'trap … EXIT' — scratch will leak when test exits non-zero. Pattern: TMPDIR_E2E=\$(mktemp -d -t prefix-XXX); trap 'rm -rf \"\$TMPDIR_E2E\"' EXIT INT TERM"
      violations=$((violations + 1))
    fi
  fi
  # Wire the zero-leftover VERIFY into the gate: any wrapper that sources the
  # run-footprint engine (lib/run_footprint.sh) MUST actually call rf_verify —
  # the REQUIRED post-teardown assertion — AND rf_force_clean (the FINALLY safety
  # belt). Without this lint a refactor could quietly drop the leak-catch and the
  # test would go green while the product's teardown silently leaks (exactly the
  # failure this wrapper exists to prevent).
  if grep -qE 'run_footprint\.sh' "$f"; then
    if ! grep -qE '\brf_verify\b' "$f"; then
      echo "::error file=tests/e2e/$f::sources lib/run_footprint.sh but never calls rf_verify — the zero-leftover VERIFY assertion is REQUIRED. Call rf_verify and FAIL the test when it returns non-zero."
      violations=$((violations + 1))
    fi
    if ! grep -qE '\brf_force_clean\b' "$f"; then
      echo "::error file=tests/e2e/$f::sources lib/run_footprint.sh but never calls rf_force_clean — the FINALLY safety belt is REQUIRED so the e2e never itself leaks when it catches a teardown bug."
      violations=$((violations + 1))
    fi
  fi
done

if ! python3 - "$repo_root" <<'PY'
import re
import sys
from pathlib import Path

repo = Path(sys.argv[1])
e2e_dir = repo / "tests" / "e2e"
sweeper = repo / ".gitea" / "workflows" / "sweep-stale-e2e-orgs.yml"

errors: list[str] = []
sweeper_text = sweeper.read_text()

required_sweeper_prefixes = ('"e2e-"', '"rt-e2e-"')
for prefix in required_sweeper_prefixes:
    if prefix not in sweeper_text:
        errors.append(
            f"::error file=.gitea/workflows/sweep-stale-e2e-orgs.yml::"
            f"missing stale-org sweeper prefix {prefix}"
        )

slug_assignment_re = re.compile(r'^\s*SLUG=(["\'])(?P<value>.+?)\1', re.MULTILINE)
covered_prefixes = ("e2e-", "rt-e2e-")

for path in sorted(e2e_dir.glob("test_*staging*.sh")):
    text = path.read_text()
    creates_org = "/cp/admin/orgs" in text and re.search(r"\bPOST\b", text)
    deletes_org = "/cp/admin/tenants" in text and re.search(r"\bDELETE\b", text)
    if not (creates_org or deletes_org):
        continue

    rel = path.relative_to(repo)
    if not re.search(r"trap\s+.*\bEXIT\b", text):
        errors.append(
            f"::error file={rel}::staging tenant E2E touches CP org lifecycle "
            "but has no EXIT trap for teardown"
        )

    assignments = [m.group("value") for m in slug_assignment_re.finditer(text)]
    if not assignments:
        errors.append(
            f"::error file={rel}::staging tenant E2E touches CP org lifecycle "
            "but has no quoted SLUG=... assignment for scoped cleanup"
        )
        continue

    for value in assignments:
        literal_prefix = re.split(r"[$`]", value, maxsplit=1)[0]
        if not literal_prefix:
            errors.append(
                f"::error file={rel}::SLUG assignment starts with dynamic data "
                f"({value!r}); use a fixed e2e-* or rt-e2e-* prefix so "
                "sweep-stale-e2e-orgs can reap orphans"
            )
            continue
        if not literal_prefix.startswith(covered_prefixes):
            errors.append(
                f"::error file={rel}::SLUG prefix {literal_prefix!r} is not "
                "covered by sweep-stale-e2e-orgs.yml; use e2e-* or rt-e2e-*"
            )

if errors:
    print("\n".join(errors))
    raise SystemExit(1)

print("✓ staging tenant E2E slug prefixes are covered by sweep-stale-e2e-orgs and use EXIT traps")
PY
then
  violations=$((violations + 1))
fi

# ─── SSOT teardown gate ────────────────────────────────────────────────────────
# Every `DELETE /cp/admin/tenants/:slug` must go through the SSOT helper
# e2e_cp_delete_and_verify_purge (tests/e2e/lib/cp_purge_receipt.sh). This is
# the guard that turns "we fixed seven scripts" into "an eighth cannot regress".
#
# WHY IT EXISTS. The control plane fences org teardown: claimOrgPurge returns an
# immediate HTTP 409 "organization has an active lifecycle operation" while any
# org-scoped mutation lease is unexpired — including the CP's OWN async
# concierge/platform-agent workspace provision for the org a test just created.
# A 409 writes NO purge audit row, so a fire-and-forget DELETE followed by a
# presence poll waits out its full timeout for an org nothing will ever delete:
# the tenant leaks and the job reds (or, in an advisory-tier lane, leaks
# SILENTLY, which is worse). 3dd3675e2 fixed exactly this — in the shared
# library only. Seven hand-rolled copies were left behind and stayed broken for
# twelve days, because nothing made the class enforceable.
#
# The helper's contract is what must be preserved, not merely "a retry": exact
# purge receipt + exact completed audit row + exact-org absence, with a BOUNDED
# 409 retry that still fails CLOSED on a persistent 409, any other non-200, a
# malformed or mismatched receipt, or a surviving org. Blind-retrying a DELETE,
# or discarding its status code (`>/dev/null 2>&1` — what hid this for twelve
# days), is the failure mode this lint exists to prevent.
#
# SCOPE: shell under tests/ and scripts/, plus .gitea/ workflows and scripts.
# Go harnesses (workspace-server/internal/staginge2e/*.go) drive their own
# t.Cleanup teardown and cannot call a bash function; they are out of scope here
# and carry their own coverage.
if ! python3 - "$repo_root" <<'PY'
import re
import sys
from pathlib import Path

repo = Path(sys.argv[1])

SSOT_HELPER = "e2e_cp_delete_and_verify_purge"
SSOT_LIB = "tests/e2e/lib/cp_purge_receipt.sh"

# The ONLY sanctioned hand-rolled DELETEs, each with the structural reason the
# receipt contract cannot apply. Adding an eighth script means editing THIS list
# in a reviewed diff — which is the entire point. Do not add a script here
# because routing it through the helper was inconvenient.
ALLOWLIST = {
    # THE SSOT itself — this is where the one true DELETE lives.
    SSOT_LIB: "defines e2e_cp_delete_and_verify_purge",
    # Janitor + SIGKILL backstops. Both act on a slug DISCOVERED after the fact,
    # with no creation-returned org id — and the helper REQUIRES that id as its
    # collision-safe authority before it will issue a destructive DELETE. They
    # are reaping orphans precisely because the owning run died without proving
    # anything, so there is no receipt contract to honor.
    ".gitea/workflows/sweep-stale-e2e-orgs.yml":
        "stale-org janitor: reaps discovered slugs with no creation-returned org id",
    ".gitea/workflows/template-delivery-e2e-staging.yml":
        "always() SIGKILL backstop for a run whose EXIT trap never fired",
}

# A DELETE request, not prose about one. Requires the verb and the admin tenant
# path in the SAME logical statement (line continuations are joined first).
DELETE_RE = re.compile(r"(?:-X|--request)\s+DELETE\b|\bDELETE\b\s+[\"']?\S*/cp/admin/tenants")
TENANT_PATH_RE = re.compile(r"/cp/admin/tenants/")
# redeploy-fleet is a POST fan-out on the same path prefix — never a teardown.
REDEPLOY_RE = re.compile(r"/cp/admin/tenants/redeploy-fleet")
# Diagnostic output that merely NAMES the request it is about to make.
NARRATION_RE = re.compile(
    r"^\s*(?:log|echo|ok|fail|warn|printf|>&2)\b"
    r"|::(?:error|warning|notice)::"
)


def logical_lines(text: str):
    """Join shell line-continuations so a multi-line curl is one statement."""
    out, buf, start = [], "", 1
    for i, raw in enumerate(text.splitlines(), start=1):
        if not buf:
            start = i
        stripped = raw.rstrip()
        if stripped.endswith("\\"):
            buf += stripped[:-1] + " "
            continue
        out.append((start, buf + stripped))
        buf = ""
    if buf:
        out.append((start, buf))
    return out


def scan(path: Path):
    rel = path.relative_to(repo).as_posix()
    try:
        text = path.read_text(encoding="utf-8")
    except (UnicodeDecodeError, OSError):
        return []
    if not TENANT_PATH_RE.search(text):
        return []
    hits = []
    for lineno, line in logical_lines(text):
        if line.lstrip().startswith("#"):
            continue
        if not TENANT_PATH_RE.search(line) or REDEPLOY_RE.search(line):
            continue
        if not DELETE_RE.search(line):
            continue
        if SSOT_HELPER in line:
            continue
        if NARRATION_RE.search(line):
            continue
        hits.append((rel, lineno, line.strip()[:160]))
    return hits


targets: list[Path] = []
for sub, patterns in (
    ("tests", ("**/*.sh",)),
    ("scripts", ("**/*.sh",)),
    (".gitea", ("**/*.yml", "**/*.yaml", "**/*.sh")),
):
    base = repo / sub
    if not base.is_dir():
        continue
    for pattern in patterns:
        targets.extend(sorted(base.glob(pattern)))

errors: list[str] = []
seen_allowlisted: set[str] = set()

for path in targets:
    for rel, lineno, snippet in scan(path):
        if rel in ALLOWLIST:
            seen_allowlisted.add(rel)
            continue
        errors.append(
            f"::error file={rel},line={lineno}::hand-rolled DELETE /cp/admin/tenants "
            f"— route it through {SSOT_HELPER} ({SSOT_LIB}). A raw DELETE cannot survive "
            "the CP's org-lifecycle fence: claimOrgPurge answers an immediate 409 "
            "'organization has an active lifecycle operation' while the org's own async "
            "provision is in flight, and a 409 writes NO purge audit row — so nothing "
            "re-issues the DELETE and the tenant LEAKS. The helper retries the 409 "
            "bounded and still fails CLOSED (persistent 409, other non-200, bad receipt, "
            f"surviving org). Offending statement: {snippet}"
        )

# The SSOT must actually exist and export the helper — an allowlist entry is
# worthless if the thing it defers to was renamed or deleted.
ssot = repo / SSOT_LIB
if not ssot.is_file():
    errors.append(f"::error file={SSOT_LIB}::the SSOT teardown helper file is missing")
elif f"{SSOT_HELPER}()" not in ssot.read_text(encoding="utf-8"):
    errors.append(
        f"::error file={SSOT_LIB}::does not define {SSOT_HELPER}() — every allowlisted "
        "hand-rolled DELETE and every migrated caller depends on it"
    )

# A stale allowlist is a hole that grows quietly: if an entry no longer contains
# a hand-rolled DELETE, it must be removed so it cannot silently re-authorize a
# future one.
for rel, reason in sorted(ALLOWLIST.items()):
    if rel == SSOT_LIB:
        continue
    if rel not in seen_allowlisted:
        errors.append(
            f"::error file=tests/e2e/lint_cleanup_traps.sh::stale SSOT-teardown allowlist "
            f"entry '{rel}' ({reason}) no longer contains a hand-rolled DELETE — remove it "
            "so it cannot silently re-authorize one"
        )

if errors:
    print("\n".join(errors))
    raise SystemExit(1)

print(
    f"✓ every DELETE /cp/admin/tenants routes through {SSOT_HELPER} "
    f"({len(ALLOWLIST) - 1} reviewed backstop exception(s))"
)
PY
then
  violations=$((violations + 1))
fi

if [ "$violations" -gt 0 ]; then
  echo "::error::$violations shell E2E file(s) leak scratch on early exit. See above."
  exit 1
fi

echo "✓ all $(grep -lE '\bmktemp\b' test_*.sh | wc -l | tr -d ' ') shell E2E files with mktemp also install an EXIT trap"
