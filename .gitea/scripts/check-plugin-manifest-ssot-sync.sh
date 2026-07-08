#!/usr/bin/env bash
# SSOT sync-check for the plugin-manifest schema (install-time manifest gate).
#
# molecule-ai-sdk's contracts/plugin-manifest/plugin-manifest.schema.json is
# the LIVE SSOT for the marketplace plugin-manifest contract (core#3383; RFC
# core#3285 lineage — the schema's $id still carries the molecule-contracts
# URL it was authored under, but molecule-ai-sdk is where the contract LIVES,
# molecule-contracts is archived). Core keeps a VENDORED copy at
# workspace-server/internal/plugins/contracts/plugin-manifest.schema.json --
# embedded (go:embed) by internal/plugins/manifest_ssot.go, which validates
# staged plugin.yaml manifests at install time (advisory phase). This script
# keeps that vendored copy honest against the SSOT: it fetches the canonical
# schema and verifies the vendored copy is BYTE-IDENTICAL.
#
# This is the SAME transitional "vendored copy + sync-check" shape core uses for
# the provision-request contract. Together with the Go embed it closes the loop:
# the install validator can't silently run against a drifted contract without
# this check going red.
#
# INVARIANT (what reds): the vendored schema and the molecule-ai-sdk SSOT
# copy must be BYTE-IDENTICAL (the copy is vendored byte-for-byte, see
# workspace-server/internal/plugins/contracts/PROVENANCE.md), so ANY
# divergence -- content or formatting -- reds until re-vendored.
#
# AUTH: NONE. molecule-ai-sdk is a PUBLIC repo, so the canonical schema is
# readable over the raw endpoint without a token; this runs on EVERY context
# (incl. fork PRs) and is FAIL-CLOSED on a fetch error (non-200 / network) and
# on drift. The explicit curl UA is REQUIRED: the Cloudflare edge in front of
# git.moleculesai.app 403s default non-browser UAs.
set -euo pipefail

# Public raw endpoint of the molecule-ai-sdk SSOT (overridable for testing).
SSOT_URL="${SSOT_URL:-https://git.moleculesai.app/molecule-ai/molecule-ai-sdk/raw/branch/main/contracts/plugin-manifest/plugin-manifest.schema.json}"
# Vendored copy in core (relative to repo root; overridable for testing).
LOCAL_PATH="${LOCAL_PATH:-workspace-server/internal/plugins/contracts/plugin-manifest.schema.json}"

if [ ! -f "$LOCAL_PATH" ]; then
  echo "::error::Vendored plugin-manifest schema $LOCAL_PATH is missing -- manifest_ssot.go embeds it; re-vendor from molecule-ai-sdk (see the PROVENANCE.md next to it)."
  exit 1
fi

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

set +e
curl -fsS -A "curl/8.4.0" "$SSOT_URL" -o "$tmp"
curl_status=$?
set -e
if [ "$curl_status" -ne 0 ]; then
  echo "::error::Failed to fetch the molecule-ai-sdk SSOT schema from ${SSOT_URL} (curl exit $curl_status). Fail-closed."
  exit 1
fi

if ! jq -e . "$tmp" >/dev/null 2>&1; then
  echo "::error::The molecule-ai-sdk SSOT schema did not parse as JSON (likely an edge/error page, not the schema). Fail-closed."
  exit 1
fi

if cmp -s "$LOCAL_PATH" "$tmp"; then
  echo "OK -- vendored plugin-manifest.schema.json is BYTE-IDENTICAL to the molecule-ai-sdk SSOT."
else
  echo "::error::Vendored plugin-manifest.schema.json DRIFTED from the molecule-ai-sdk SSOT (not byte-identical)."
  echo "Diff (vendored vs SSOT):"
  diff -u "$LOCAL_PATH" "$tmp" || true
  echo "Re-sync: align ${LOCAL_PATH} to ${SSOT_URL} (the SSOT is canonical -- align the vendored copy to it, never the reverse; update workspace-server/internal/plugins/contracts/PROVENANCE.md pins)."
  exit 1
fi
