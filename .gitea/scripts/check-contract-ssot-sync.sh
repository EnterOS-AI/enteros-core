#!/usr/bin/env bash
# SSOT sync-check for the MCP-plugin delivery contract (RFC core#3285 §10).
#
# molecule-ai-sdk is the SSOT for the cross-repo tool contract (RFC §10,
# "Contract-repo visibility — DECIDED: Public"). Core keeps a local MIRROR at
# contracts/mcp-plugin-delivery.contract.json (the producer-side copy that the
# degrade gate derives conciergePlatformMCPRequiredTool from, asserted by
# TestSSOT_DegradeGateToolDerivesFromContract). This check keeps that mirror
# honest against the SSOT: it fetches molecule-ai-sdk' canonical copy and
# verifies core's mirror is the SAME CONTRACT.
#
# This is the transitional "vendored copy + sync-check" form (RFC §10, "one
# source, never two copies") and the "run the new import alongside the old
# local file's drift test through a soak, proving they agree" step (RFC §10
# migration step 3). It is ADVISORY-FIRST per RFC §13/§14.2 (advisory -> soak
# -> required); the BP-required promotion is a separate post-soak step.
#
# INVARIANT (what reds): core's mirror and the molecule-ai-sdk SSOT must be
# the SAME CONTRACT -- canonical-JSON identical (key-order- and whitespace-
# normalized via `jq -S -c`). This is the "proving they agree" predicate of RFC
# §10 and is robust to the formatting difference that exists TODAY: the SSOT was
# seeded pretty-printed while core's mirror is minified, so the two are NOT
# raw-byte-identical even though they are the same contract. A raw-byte compare
# would red on current main against a pure formatting delta -- a decorative
# failure (RFC §14.1: a gate must be GREEN on current main). Canonical compare
# reds only on a REAL content divergence -- the defect this gate exists to catch.
# (A non-failing note still surfaces the byte-formatting delta so the eventual
# byte-identical re-seed / published-package migration of RFC §10 is visible.)
#
# AUTH: molecule-ai-sdk is a PUBLIC repo (RFC §10 visibility decision), so its
# canonical contract is readable over the raw endpoint WITHOUT a token. No secret
# is required; the check runs on EVERY context (incl. fork PRs). It is FAIL-CLOSED
# on a fetch error (non-200 / network) and on canonical drift.
set -euo pipefail

# Public raw endpoint of the molecule-ai-sdk SSOT (overridable for testing).
SSOT_URL="${SSOT_URL:-https://git.moleculesai.app/molecule-ai/molecule-ai-sdk/raw/branch/main/contracts/mcp/mcp-plugin-delivery.contract.json}"
# Core's local mirror (relative to repo root; overridable for testing).
LOCAL="${LOCAL:-contracts/mcp-plugin-delivery.contract.json}"

if [ ! -f "$LOCAL" ]; then
  echo "::error::Core's contract mirror $LOCAL is missing -- it must be present (the degrade gate derives its required tool from it)."
  exit 1
fi

TMP="$(mktemp)"
trap 'rm -f "$TMP" "${TMP}.local.canon" "${TMP}.ssot.canon"' EXIT

# Fetch the SSOT unauthenticated; fail-closed on any non-2xx / network error.
set +e
curl -fsS "$SSOT_URL" -o "$TMP"
curl_status=$?
set -e
if [ "$curl_status" -ne 0 ]; then
  echo "::error::Failed to fetch the molecule-ai-sdk SSOT from ${SSOT_URL} (curl exit $curl_status). Fail-closed."
  exit 1
fi

# Both copies must be valid JSON (fail-closed on a corrupt SSOT or mirror).
if ! jq -e . "$TMP" >/dev/null 2>&1; then
  echo "::error::The molecule-ai-sdk SSOT did not parse as JSON (fetched from ${SSOT_URL}). Fail-closed."
  exit 1
fi
if ! jq -e . "$LOCAL" >/dev/null 2>&1; then
  echo "::error::Core's mirror $LOCAL did not parse as JSON. Fail-closed."
  exit 1
fi

# Canonicalize (sorted keys, compact) and compare -- the "same contract" invariant.
jq -S -c . "$LOCAL" > "${TMP}.local.canon"
jq -S -c . "$TMP"   > "${TMP}.ssot.canon"

if cmp -s "${TMP}.local.canon" "${TMP}.ssot.canon"; then
  echo "OK -- core's mirror $LOCAL is the SAME CONTRACT as the molecule-ai-sdk SSOT (canonical-JSON identical)."
  # Non-failing visibility into the byte-formatting delta (RFC §10 byte-identical
  # re-seed / published-package migration tracking).
  if ! cmp -s "$LOCAL" "$TMP"; then
    echo "::notice::Mirror and SSOT are the same contract but NOT raw-byte-identical (formatting differs: mirror is minified, SSOT pretty-printed). This is expected today and tracked by RFC core#3285 §10 (byte-identical re-seed / published-package migration). Canonical equality is the enforced invariant."
  fi
  exit 0
fi

echo "::error::Core's mirror $LOCAL DRIFTED from the molecule-ai-sdk SSOT -- they are NOT the same contract (canonical-JSON differs)."
echo "Canonical diff (local mirror vs SSOT):"
diff -u "${TMP}.local.canon" "${TMP}.ssot.canon" || true
echo "Re-sync: align core's mirror to the molecule-ai-sdk SSOT at contracts/mcp/mcp-plugin-delivery.contract.json (the SSOT is canonical -- align the mirror to it, never the reverse)."
exit 1
