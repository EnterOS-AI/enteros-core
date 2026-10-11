#!/usr/bin/env python3
"""Consumer-pin drift gate: molecule-core's SDK `go.mod` pin vs the SDK SSOT.

WHY THIS EXISTS
---------------
The runtime -> template `.runtime-version` axis has all three legs of a pin
pipeline:

  * an OPENER   (workspace-runtime propagate_runtime_version.py, on release),
  * a MERGER    (workspace-runtime merge_runtime_version_bumps.py, non-author),
  * a DETECTOR  (workspace-runtime check_consumer_runtime_drift.py).

The SDK -> molecule-core `go.moleculesai.app/sdk/gen/go` axis had NONE of them.
Nothing opened a bump PR when the SDK merged, nothing merged one, and — the part
this file fixes — nothing ever went red when the pin fell behind. The two gates
that LOOK adjacent do not cover it:

  * internal/providers/sync_canonical_test.go pins the sha256 of whatever the
    binary already embeds. It is hermetic and INVERTED: it fires on an
    UNEXPECTED registry change, never on staleness. A pin five releases old
    stays green forever.
  * verify-providers-gen.yml asserts the generated projection matches THE PINNED
    MODULE, whatever version that happens to be. Also green on a stale pin.

That blind spot is not hypothetical. On 2026-08-05 core was still pinned to
sdk#203, which had narrowed every runtime's `platform` arm to minimax-only.
sdk#204 corrected it hours later by restoring five HEALTHY model ids — and core
kept serving the narrowed menu, so `anthropic/claude-opus-4-7`, `-4-8`,
`anthropic/claude-sonnet-4-6`, `openai/gpt-5.4` and `openai/gpt-5.4-mini` were
healed for existing pins but NOT re-selectable for new workspaces. Every check in
the repo was green throughout.

DISPOSITION — deliberately not "red on any lag"
-----------------------------------------------
molecule-core's branch protection is `contexts: ["*"]`, so ANY context this gate
posts is merge-blocking for EVERY PR in the repo. A naive "red whenever the pin
is not head" would wedge the whole repo on a routine SDK merge. So this mirrors
check_consumer_runtime_drift.classify_pin_drift exactly:

  * CURRENT (pin == SDK main head)                          -> OK, exit 0
  * LAGGING but within the window                           -> ADVISORY, exit 0
  * LAGGING beyond the window, bump PR IN FLIGHT            -> ADVISORY, exit 0
        (transient / self-healing — the opener filed it, it awaits review)
  * LAGGING beyond the window, NO open bump PR = STUCK      -> BLOCKING, exit 1
  * UNDETERMINABLE (no token, API error, network)           -> ADVISORY, exit 0
        (fail-soft: never block core on a signal we could not read)

The window is measured from the OLDEST UNADOPTED SDK commit — "how long has a
change been sitting unadopted", not "how far behind in commit count" — because a
burst of ten SDK commits in one hour is not a staleness problem, and one commit
ignored for a week is.

Only the STUCK case is red, which is precisely the condition a human must act on.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.request
from dataclasses import dataclass
from datetime import datetime, timezone

ORG = "molecule-ai"
SDK_REPO = "molecule-ai-sdk"
CORE_REPO = "molecule-core"
SDK_MODULE = "go.moleculesai.app/sdk/gen/go"
DEFAULT_GITEA = "https://git.moleculesai.app"

# Stated window. A change sitting unadopted longer than this is a real lag, not
# propagation latency. 72h spans a weekend, so a Friday SDK merge does not go red
# on Saturday.
DEFAULT_MAX_LAG_HOURS = 72.0

# How far back to walk SDK main looking for the pinned commit. If the pin is not
# in this many commits it is unambiguously stale — no need to keep paging.
COMMIT_SCAN_PAGES = 5
COMMIT_SCAN_PER_PAGE = 50

# The Cloudflare edge 1010-blocks python-urllib's default UA (same finding as the
# runtime scripts). Every request here must look like curl.
USER_AGENT = "curl/8.4.0"

# go.mod pseudo-version: v0.0.0-<utc-timestamp>-<12-hex>
PSEUDO_RE = re.compile(
    r"^\s*" + re.escape(SDK_MODULE) + r"\s+(v[0-9A-Za-z.\-+]*-(\d{14})-([0-9a-f]{12}))\s*$",
    re.MULTILINE,
)

# Title grammar the SDK-pin opener uses (sdk-pin-bump.yml). Kept in lockstep.
BUMP_BRANCH_PREFIX = "bump/sdk-"


class StatusUnavailable(RuntimeError):
    """A signal we could not read. Always degrades to ADVISORY, never blocking."""


@dataclass(frozen=True)
class PinState:
    pinned_sha12: str
    pinned_version: str
    head_sha: str
    # None when the pin IS head.
    oldest_unadopted_sha: str | None
    oldest_unadopted_iso: str | None
    commits_behind: int
    lag_hours: float


def normalize_base_url(url: str) -> str:
    """Return a scheme-qualified, trailing-slash-free base URL.

    `GITEA_HOST` is set schemeless (`git.moleculesai.app`) in several places in
    this org, including the operator shell profile. urllib raises a bare
    ValueError on such a URL, which escaped as an unhandled traceback and an
    exit-1 — i.e. the fail-soft contract was silently inverted into "block every
    PR in core" by an env var. Normalize instead of trusting the caller.
    """
    url = (url or "").strip().rstrip("/")
    if not url:
        return DEFAULT_GITEA
    if "://" not in url:
        url = "https://" + url
    return url


def _get_json(url: str, token: str = "") -> object:
    try:
        req = urllib.request.Request(url, headers={"User-Agent": USER_AGENT})
    except ValueError as exc:
        # Malformed URL (e.g. a schemeless GITEA_HOST that slipped past
        # normalization). Unreadable signal, not a drift verdict.
        raise StatusUnavailable(f"malformed URL {url!r}: {exc}") from exc
    if token:
        req.add_header("Authorization", f"token {token}")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            raw = resp.read()
    except (urllib.error.URLError, urllib.error.HTTPError, OSError, ValueError) as exc:
        raise StatusUnavailable(f"GET {url}: {exc}") from exc
    try:
        return json.loads(raw)
    except json.JSONDecodeError as exc:
        # A Cloudflare / gateway HTML error page must never parse "clean" into a
        # false verdict — the cron-fixtures-ssot-drift parse-guard lesson.
        raise StatusUnavailable(f"GET {url}: non-JSON response ({exc})") from exc


def parse_go_mod_pin(text: str) -> tuple[str, str]:
    """Return (pseudo_version, sha12) for the SDK require line.

    Raises SystemExit(1) — a go.mod with no SDK pin is a real defect in a repo
    whose registry SSOT *is* the SDK, not an unreadable-signal case.
    """
    m = PSEUDO_RE.search(text)
    if not m:
        print(
            f"::error::no `{SDK_MODULE}` pseudo-version require line found in go.mod. "
            f"This gate cannot verify the registry SSOT pin. If the dependency moved "
            f"to a tagged release, update PSEUDO_RE in this script to match.",
            file=sys.stderr,
        )
        raise SystemExit(1)
    return m.group(1), m.group(3)


def _parse_iso(ts: str) -> datetime:
    # Gitea emits RFC3339 with an offset, e.g. 2026-08-05T01:57:41Z / +00:00.
    return datetime.fromisoformat(ts.replace("Z", "+00:00"))


def resolve_pin_state(
    pinned_version: str,
    pinned_sha12: str,
    *,
    gitea_url: str,
    token: str,
    now: datetime | None = None,
) -> PinState:
    now = now or datetime.now(timezone.utc)
    api = f"{normalize_base_url(gitea_url)}/api/v1/repos/{ORG}/{SDK_REPO}"

    head = _get_json(f"{api}/branches/main", token)
    try:
        head_sha = head["commit"]["id"]  # type: ignore[index]
    except (KeyError, TypeError) as exc:
        raise StatusUnavailable(f"unexpected branches/main shape: {exc}") from exc

    if head_sha.startswith(pinned_sha12):
        return PinState(pinned_sha12, pinned_version, head_sha, None, None, 0, 0.0)

    # Walk main newest-first until we meet the pinned commit; everything seen
    # before it is unadopted. The LAST one seen is the oldest unadopted commit.
    unadopted: list[dict] = []
    found = False
    for page in range(1, COMMIT_SCAN_PAGES + 1):
        batch = _get_json(
            f"{api}/commits?sha=main&limit={COMMIT_SCAN_PER_PAGE}&page={page}", token
        )
        if not isinstance(batch, list) or not batch:
            break
        for c in batch:
            sha = c.get("sha", "")
            if sha.startswith(pinned_sha12):
                found = True
                break
            unadopted.append(c)
        if found:
            break

    if not unadopted:
        # Head != pin, yet nothing unadopted: the pin is not an ancestor of main
        # (force-push, or a pin from a branch). Not a staleness verdict we can
        # defend, so refuse to guess.
        raise StatusUnavailable(
            f"pin {pinned_sha12} is not head but no unadopted commits were found on "
            f"main — the pin may not be an ancestor of main"
        )

    oldest = unadopted[-1]
    oldest_sha = oldest.get("sha", "")
    try:
        oldest_iso = oldest["commit"]["committer"]["date"]
    except (KeyError, TypeError) as exc:
        raise StatusUnavailable(f"unexpected commit shape: {exc}") from exc

    lag_hours = (now - _parse_iso(oldest_iso)).total_seconds() / 3600.0

    if not found:
        # Pin fell off the scan horizon: definitively stale. Report the commits
        # we counted as a floor, and flag it by making the count negative-proof.
        print(
            f"::warning::pinned commit {pinned_sha12} not found within the last "
            f"{len(unadopted)} commits on {SDK_REPO}@main — the pin is at least "
            f"that far behind.",
            file=sys.stderr,
        )

    return PinState(
        pinned_sha12,
        pinned_version,
        head_sha,
        oldest_sha,
        oldest_iso,
        len(unadopted),
        lag_hours,
    )


def open_bump_pr_target(*, gitea_url: str, token: str) -> str | None:
    """Return the head branch of an open SDK-pin bump PR on core, or None.

    Raises StatusUnavailable when the query could not be made — the caller
    degrades to ADVISORY rather than blocking on an unread signal.
    """
    if not token:
        raise StatusUnavailable("no token available to query open PRs")
    api = f"{normalize_base_url(gitea_url)}/api/v1/repos/{ORG}/{CORE_REPO}/pulls"
    for page in range(1, 6):
        batch = _get_json(f"{api}?state=open&limit=50&page={page}", token)
        if not isinstance(batch, list) or not batch:
            return None
        for pr in batch:
            head = ((pr.get("head") or {}).get("ref")) or ""
            if head.startswith(BUMP_BRANCH_PREFIX):
                return head
    return None


def classify(
    state: PinState,
    *,
    max_lag_hours: float,
    gitea_url: str,
    token: str,
) -> tuple[bool, str]:
    """Return (is_blocking, message). See the module docstring for disposition."""
    if state.oldest_unadopted_sha is None:
        return False, (
            f"OK — core pins {SDK_REPO}@{state.pinned_sha12}, which IS "
            f"{SDK_REPO}@main head ({state.head_sha[:12]}). No drift."
        )

    base = (
        f"core pins {SDK_MODULE} at {state.pinned_version} ({state.pinned_sha12}); "
        f"{SDK_REPO}@main is {state.head_sha[:12]} — {state.commits_behind} commit(s) "
        f"ahead. Oldest unadopted commit {(state.oldest_unadopted_sha or '')[:12]} "
        f"landed {state.lag_hours:.1f}h ago (window {max_lag_hours:.0f}h)."
    )

    if state.lag_hours <= max_lag_hours:
        return False, f"ADVISORY (within window) — {base}"

    try:
        in_flight = open_bump_pr_target(gitea_url=gitea_url, token=token)
    except StatusUnavailable as exc:
        return False, (
            f"ADVISORY (fail-soft) — {base} Could not determine whether a bump PR "
            f"is in flight ({exc}); refusing to block core on an unread signal."
        )

    if in_flight:
        return False, (
            f"ADVISORY (propagation in flight) — {base} An open bump PR "
            f"(branch {in_flight}) is advancing the pin; transient / self-healing."
        )

    return True, (
        f"STUCK — {base} No open bump PR is advancing it, so this will not "
        f"self-heal.\n"
        f"Remediation: run the `sdk-pin-bump` workflow in molecule-core "
        f"(Actions -> sdk-pin-bump -> Run workflow). It bumps go.mod, runs "
        f"`go generate`, moves canonicalRegistrySHA256 and opens the PR.\n"
        f"Manual equivalent, from workspace-server/:\n"
        f"  go get {SDK_MODULE}@{state.head_sha}\n"
        f"  go generate ./...\n"
        f"  go test ./internal/providers -run TestSyncedYAMLMatchesCanonicalSHA\n"
        f"  # paste the observed sha into canonicalRegistrySHA256"
    )


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--go-mod", default="workspace-server/go.mod")
    ap.add_argument("--gitea-url", default=os.environ.get("GITEA_HOST", DEFAULT_GITEA))
    ap.add_argument("--token-env", default="SDK_PIN_DRIFT_TOKEN")
    ap.add_argument(
        "--max-lag-hours",
        type=float,
        default=float(os.environ.get("SDK_PIN_MAX_LAG_HOURS", DEFAULT_MAX_LAG_HOURS)),
    )
    args = ap.parse_args(argv)

    try:
        text = open(args.go_mod, encoding="utf-8").read()
    except OSError as exc:
        print(f"::error::cannot read {args.go_mod}: {exc}", file=sys.stderr)
        return 1

    pinned_version, pinned_sha12 = parse_go_mod_pin(text)
    token = os.environ.get(args.token_env, "")

    try:
        state = resolve_pin_state(
            pinned_version, pinned_sha12, gitea_url=args.gitea_url, token=token
        )
    except StatusUnavailable as exc:
        # Fail-soft. An unreachable SSOT must never wedge every PR in core.
        print(
            f"::warning::SDK pin drift undeterminable ({exc}) — treating as ADVISORY "
            f"(fail-soft, not blocking).",
            file=sys.stderr,
        )
        return 0

    blocking, message = classify(
        state,
        max_lag_hours=args.max_lag_hours,
        gitea_url=args.gitea_url,
        token=token,
    )
    if blocking:
        print(f"::error::SDK consumer pin is STUCK.\n{message}", file=sys.stderr)
        return 1
    print(message)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
