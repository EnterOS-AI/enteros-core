#!/usr/bin/env python3
"""Cross-repo SSOT drift gate: compare molecule-core canvas tokens to molecule-app.

Fetches molecule-ai/molecule-app/app/globals.css and byte-compares the shared
@theme tokens (surface/ink/line/accent/warm/good/bad, light + dark) against the
local canvas/src/app/globals.css. Real drift -> ::error:: + exit 1 with diffs.

Advisory by default: the calling CI job uses continue-on-error: true and is NOT
in all-required. The gate skips loud when APP_SSOT_READ_TOKEN is absent.

Mirrors molecule-ai/molecule-app/.gitea/scripts/check_canvas_token_drift.py.
"""

from __future__ import annotations

import base64
import json
import os
import re
import sys
import urllib.request
from typing import Dict, Tuple

SHARED_TOKEN_NAMES: Tuple[str, ...] = (
    "--color-surface",
    "--color-surface-elevated",
    "--color-surface-sunken",
    "--color-surface-card",
    "--color-line",
    "--color-line-soft",
    "--color-ink",
    "--color-ink-mid",
    "--color-ink-soft",
    "--color-accent",
    "--color-accent-strong",
    "--color-accent-wash",
    "--color-warm",
    "--color-good",
    "--color-bad",
)

APP_FILE_PATH = "app/globals.css"
APP_REPO = "molecule-ai/molecule-app"
GITEA_SERVER = os.environ.get("GITEA_SERVER_URL", "https://git.moleculesai.app")
CANVAS_FILE_PATH = "canvas/src/app/globals.css"


def fetch_app_css(token: str) -> str:
    """Fetch the molecule-app globals.css raw content."""
    url = f"{GITEA_SERVER}/api/v1/repos/{APP_REPO}/contents/{APP_FILE_PATH}"
    req = urllib.request.Request(
        url,
        headers={
            "Authorization": f"token {token}",
            "Accept": "application/json",
            # CF WAF 1010-bans the default Python-urllib UA; send a
            # non-urllib UA so this reaches Gitea (transport-only).
            "User-Agent": "molecule-ci-gate/1.0 (+gitea-api)",
        },
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        data = resp.read().decode("utf-8")

    payload = json.loads(data)
    content = payload.get("content")
    if not content:
        raise RuntimeError(f"empty content field from {url}")
    return base64.b64decode(content).decode("utf-8")


def extract_theme_block(css: str) -> str:
    """Extract the body of the first @theme block."""
    pattern = re.compile(r"@theme\s*\{([^}]*)\}", re.DOTALL)
    match = pattern.search(css)
    return match.group(1) if match else ""


def extract_data_theme_dark(css: str) -> str:
    """Extract the body of the first [data-theme=\"dark\"] block."""
    pattern = re.compile(r'\[data-theme="dark"\]\s*\{([^}]*)\}', re.DOTALL)
    match = pattern.search(css)
    return match.group(1) if match else ""


def parse_tokens(block: str) -> Dict[str, str]:
    """Parse --color-* tokens from a CSS block."""
    tokens: Dict[str, str] = {}
    for line in block.splitlines():
        line = line.strip()
        if not line or line.startswith("/*") or line.startswith("*"):
            continue
        match = re.match(r"(--color-[a-z-]+):\s*([^;]+);", line)
        if match:
            tokens[match.group(1)] = match.group(2).strip()
    return tokens


def extract_shared_tokens(css: str) -> Dict[str, Dict[str, str]]:
    """Return {light: tokens, dark: tokens} for the shared SSOT surface."""
    light_block = extract_theme_block(css)
    dark_block = extract_data_theme_dark(css)
    return {
        "light": parse_tokens(light_block),
        "dark": parse_tokens(dark_block),
    }


def compare_tokens(
    canvas: Dict[str, Dict[str, str]],
    app: Dict[str, Dict[str, str]],
) -> Tuple[bool, Dict[str, Dict[str, str]]]:
    """Compare canvas vs app tokens. Returns (ok, drift_by_mode)."""
    drift: Dict[str, Dict[str, str]] = {"light": {}, "dark": {}}
    ok = True
    for mode in ("light", "dark"):
        canvas_mode = canvas[mode]
        app_mode = app[mode]
        for name in SHARED_TOKEN_NAMES:
            canvas_val = canvas_mode.get(name)
            app_val = app_mode.get(name)
            if canvas_val != app_val:
                drift[mode][name] = f"app={app_val!r} canvas={canvas_val!r}"
                ok = False
    return ok, drift


def find_missing(
    tokens: Dict[str, Dict[str, str]], side: str
) -> list[str]:
    """Return `mode/name` for every declared shared token absent on `side`.

    Without this, a token missing from BOTH files compares equal (None ==
    None) and the gate reports "aligned" having verified nothing — and if
    the @theme regex ever fails to match, EVERY token is missing on both
    sides and the gate is a 100% vacuous pass. Presence is asserted
    separately from equality so that can never happen silently.
    """
    missing: list[str] = []
    for mode in ("light", "dark"):
        for name in SHARED_TOKEN_NAMES:
            if name not in tokens[mode]:
                missing.append(f"{side}:{mode}/{name}")
    return missing


def main() -> int:
    token = os.environ.get("APP_SSOT_READ_TOKEN")
    # The caller states whether this run is CREDENTIALLED. A trusted ref
    # (push / workflow_dispatch / same-repo PR) can reach the Infisical CI
    # machine identity, so an absent token there is a REAL misconfiguration
    # and must red — not silently exit 0. Only an untrusted-fork PR, which
    # structurally cannot hold secrets, is allowed the stated skip.
    #
    # This branch used to `return 0` unconditionally on a missing token.
    # APP_SSOT_READ_TOKEN was never provisioned on the repo or the org, so
    # that no-op was the ONLY path this gate ever took: it reported success
    # on every run without fetching, parsing or comparing anything.
    require_token = os.environ.get("DRIFT_GATE_REQUIRE_TOKEN", "") == "1"
    if not token:
        if require_token:
            print(
                "::error::APP_SSOT_READ_TOKEN is empty on a TRUSTED run. This "
                "gate cannot compare against the app SSOT without a "
                "molecule-app read credential, and a check that cannot run "
                "must not report success. Fix the credential wiring (the "
                "workflow sources DRIFT_BOT_TOKEN from Infisical prod "
                "/shared/drift-bot); do not re-add a skip-on-missing branch."
            )
            return 1
        print(
            "::notice::APP_SSOT_READ_TOKEN absent and DRIFT_GATE_REQUIRE_TOKEN "
            "is not set — untrusted-fork PR, which cannot hold secrets. "
            "Skipping the cross-repo comparison for that STATED reason."
        )
        return 0

    if not os.path.exists(CANVAS_FILE_PATH):
        print(f"::error::{CANVAS_FILE_PATH} not found in working tree")
        return 1

    try:
        app_css = fetch_app_css(token)
    except Exception as exc:  # noqa: BLE001
        print(f"::error::Failed to fetch app SSOT ({APP_REPO}/{APP_FILE_PATH}): {exc}")
        return 1

    with open(CANVAS_FILE_PATH, "r", encoding="utf-8") as f:
        canvas_css = f.read()

    canvas_tokens = extract_shared_tokens(canvas_css)
    app_tokens = extract_shared_tokens(app_css)

    # Assert PRESENCE before equality, and report the count that was
    # actually compared. A silent zero-comparison run is indistinguishable
    # from a clean one otherwise.
    missing = (
        find_missing(canvas_tokens, "canvas") + find_missing(app_tokens, "app")
    )
    expected = len(SHARED_TOKEN_NAMES) * 2  # light + dark
    if missing:
        print(
            f"::error::Declared shared design tokens are MISSING, so the SSOT "
            f"comparison would be vacuous: {len(missing)} of "
            f"{expected * 2} expected (canvas+app) lookups found nothing."
        )
        for m in missing:
            print(f"  missing: {m}")
        return 1

    ok, drift = compare_tokens(canvas_tokens, app_tokens)
    if ok:
        print(
            f"::notice::Canvas↔app token SSOT is aligned "
            f"({expected} token comparisons: "
            f"{len(SHARED_TOKEN_NAMES)} shared tokens x light+dark, "
            f"0 drifted)."
        )
        return 0

    print("::error::Canvas↔app token SSOT drift detected.")
    for mode in ("light", "dark"):
        if drift[mode]:
            print(f"\n[{mode} mode]")
            for name, detail in drift[mode].items():
                print(f"  {name}: {detail}")
    print(f"\n{len(drift['light']) + len(drift['dark'])} token(s) differ from app SSOT.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
