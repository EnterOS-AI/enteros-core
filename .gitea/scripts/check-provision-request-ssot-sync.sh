#!/usr/bin/env bash
# SSOT sync-check for the provision-request wire contract (RFC core#3285 §10).
#
# Sibling of check-contract-ssot-sync.sh (which guards the mcp-plugin-delivery
# mirror). Same mechanism, different contract: molecule-contracts is the PUBLIC
# SSOT for the cross-repo provision-request wire shape, and core keeps a local
# MIRROR at workspace-server/internal/provisioner/provision_request.contract.json
# -- the producer-side copy that provision_request_contract_test.go pins
# cpProvisionRequest's json tags against. This check keeps that mirror honest
# against the SSOT: it fetches molecule-contracts' canonical copy and verifies
# core's mirror is the SAME CONTRACT.
#
# The provision-request contract was historically DUPLICATED byte-identical in
# molecule-core and molecule-controlplane with no shared source of truth -- the
# exact "two copies, silent drift" failure mode that dropped `template_assets`
# for weeks (RFC #2843). This gate (plus the matching one in CP) is the
# enforcement that dedups them onto the molecule-contracts SSOT without forcing
# a heavy code dependency.
#
# This is the transitional "vendored copy + sync-check" form (RFC §10, "one
# source, never two copies"). It is ADVISORY-FIRST per RFC §13/§14.2 (advisory
# -> soak -> required); the BP-required promotion is a separate post-soak step.
#
# INVARIANT (what reds): core's mirror and the molecule-contracts SSOT must be
# the SAME CONTRACT -- canonical-JSON identical (key-order- and whitespace-
# normalized via `jq -S -c`). Canonical compare reds only on a REAL content
# divergence, robust to any pure formatting delta between the copies (RFC §14.1:
# a gate must be GREEN on current main against a decorative delta).
#
# AUTH: molecule-contracts is a PUBLIC repo (RFC §10 visibility decision), so its
# canonical contract is readable over the raw endpoint WITHOUT a token. No secret
# is required; the check runs on EVERY context (incl. fork PRs). It is FAIL-CLOSED
# on a fetch error (non-200 / network) and on canonical drift.
#
# NOTE ON SEEDING ORDER: this gate fetches the SSOT from molecule-contracts main.
# Until the molecule-contracts SSOT seed (provision-request/) is merged, the fetch
# 404s and the gate fails closed by design -- merge the contracts seed first, then
# this advisory gate goes (and stays) green. It is NOT in branch protection, so it
# does not block during that window.
set -euo pipefail

# Public raw endpoint of the molecule-contracts SSOT (overridable for testing).
SSOT_URL="${SSOT_URL:-https://git.moleculesai.app/molecule-ai/molecule-contracts/raw/branch/main/provision-request/provision-request.contract.json}"
# Core's local mirror (relative to repo root; overridable for testing).
LOCAL="${LOCAL:-workspace-server/internal/provisioner/provision_request.contract.json}"

if [ ! -f "$LOCAL" ]; then
  echo "::error::Core's provision-request mirror $LOCAL is missing -- it must be present (provision_request_contract_test.go pins cpProvisionRequest's json tags against it)."
  exit 1
fi

TMP="$(mktemp)"
trap 'rm -f "$TMP" "${TMP}.local.canon" "${TMP}.ssot.canon"' EXIT

# Fetch the SSOT unauthenticated; fail-closed on any non-2xx / network error.
# molecule-contracts is public; send an explicit curl UA (the CF edge 403s the
# default python/urllib UA -- harmless here but keeps the contract uniform).
set +e
curl -fsS -A "curl/8.4.0" "$SSOT_URL" -o "$TMP"
curl_status=$?
set -e
if [ "$curl_status" -ne 0 ]; then
  echo "::error::Failed to fetch the molecule-contracts provision-request SSOT from ${SSOT_URL} (curl exit $curl_status). Fail-closed. (If the molecule-contracts seed has not merged yet, merge it first.)"
  exit 1
fi

# Both copies must be valid JSON (fail-closed on a corrupt SSOT or mirror).
if ! jq -e . "$TMP" >/dev/null 2>&1; then
  echo "::error::The molecule-contracts provision-request SSOT did not parse as JSON (fetched from ${SSOT_URL}). Fail-closed."
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
  echo "OK -- core's mirror $LOCAL is the SAME CONTRACT as the molecule-contracts provision-request SSOT (canonical-JSON identical)."
  if ! cmp -s "$LOCAL" "$TMP"; then
    echo "::notice::Mirror and SSOT are the same contract but NOT raw-byte-identical (formatting differs). Canonical equality is the enforced invariant."
  fi
  exit 0
fi

echo "::error::Core's mirror $LOCAL DRIFTED from the molecule-contracts provision-request SSOT -- they are NOT the same contract (canonical-JSON differs)."
echo "Canonical diff (local mirror vs SSOT):"
diff -u "${TMP}.local.canon" "${TMP}.ssot.canon" || true
echo "Re-sync: align core's mirror to the molecule-contracts SSOT at provision-request/provision-request.contract.json (the SSOT is canonical -- align the mirror to it, never the reverse)."
exit 1
