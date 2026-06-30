#!/usr/bin/env bash
# SSOT sync-check for the workspace-comms schemas (producer-side comms gate).
#
# molecule-core's workspace-server/internal/models/workspace.go is the WIRE
# AUTHORITY the molecule-contracts `workspace-comms/*.schema.json` SSOT schemas
# were DERIVED FROM. The Go gate
# workspace-server/internal/models/workspace_comms_ssot_test.go asserts those
# structs stay field-compatible with a VENDORED copy of the schemas at
# workspace-server/internal/models/testdata/workspace-comms/. This script keeps
# that vendored copy honest against the molecule-contracts SSOT: it fetches each
# canonical schema and verifies the vendored copy is the SAME CONTRACT.
#
# This is the SAME transitional "vendored copy + sync-check" shape core already
# uses for the MCP-plugin-delivery mirror (check-contract-ssot-sync.sh); it is
# the workspace-comms sibling. Together with the Go gate it closes the producer
# enforcement loop: a workspace.go edit can't drift the SSOT without either the
# Go gate (struct <-> vendored schema) or this check (vendored schema <-> SSOT)
# going red.
#
# INVARIANT (what reds): each vendored schema and its molecule-contracts SSOT
# counterpart must be the SAME CONTRACT -- canonical-JSON identical (key-order-
# and whitespace-normalized via `jq -S -c`), so a pure formatting delta does NOT
# red but a real content divergence does.
#
# AUTH: NONE. molecule-contracts is a PUBLIC repo, so its canonical schemas are
# readable over the raw endpoint without a token; this runs on EVERY context
# (incl. fork PRs) and is FAIL-CLOSED on a fetch error (non-200 / network) and on
# canonical drift.
set -euo pipefail

# Public raw endpoint base of the molecule-contracts SSOT (overridable for testing).
SSOT_BASE="${SSOT_BASE:-https://git.moleculesai.app/molecule-ai/molecule-contracts/raw/branch/main/workspace-comms}"
# Vendored copies in core (relative to repo root; overridable for testing).
LOCAL_DIR="${LOCAL_DIR:-workspace-server/internal/models/testdata/workspace-comms}"

SCHEMAS=("register.schema.json" "heartbeat.schema.json" "agent-card.schema.json")

fail=0
for name in "${SCHEMAS[@]}"; do
  local_path="${LOCAL_DIR}/${name}"
  ssot_url="${SSOT_BASE}/${name}"

  if [ ! -f "$local_path" ]; then
    echo "::error::Vendored workspace-comms schema $local_path is missing -- the Go gate (workspace_comms_ssot_test.go) embeds it; re-sync from molecule-contracts."
    fail=1
    continue
  fi

  tmp="$(mktemp)"
  set +e
  curl -fsS -A "curl/8.4.0" "$ssot_url" -o "$tmp"
  curl_status=$?
  set -e
  if [ "$curl_status" -ne 0 ]; then
    echo "::error::Failed to fetch the molecule-contracts SSOT schema from ${ssot_url} (curl exit $curl_status). Fail-closed."
    rm -f "$tmp"
    fail=1
    continue
  fi

  if ! jq -e . "$tmp" >/dev/null 2>&1; then
    echo "::error::The molecule-contracts SSOT schema ${name} did not parse as JSON. Fail-closed."
    rm -f "$tmp"
    fail=1
    continue
  fi
  if ! jq -e . "$local_path" >/dev/null 2>&1; then
    echo "::error::Vendored $local_path did not parse as JSON. Fail-closed."
    rm -f "$tmp"
    fail=1
    continue
  fi

  if cmp -s <(jq -S -c . "$local_path") <(jq -S -c . "$tmp"); then
    echo "OK -- vendored ${name} is the SAME CONTRACT as the molecule-contracts SSOT (canonical-JSON identical)."
    if ! cmp -s "$local_path" "$tmp"; then
      echo "::notice::Vendored ${name} and the SSOT are the same contract but not raw-byte-identical (formatting differs). Canonical equality is the enforced invariant."
    fi
  else
    echo "::error::Vendored ${name} DRIFTED from the molecule-contracts SSOT -- NOT the same contract (canonical-JSON differs)."
    echo "Canonical diff (vendored vs SSOT):"
    diff -u <(jq -S -c . "$local_path") <(jq -S -c . "$tmp") || true
    echo "Re-sync: align ${local_path} to ${ssot_url} (the SSOT is canonical -- align the vendored copy to it, never the reverse)."
    fail=1
  fi
  rm -f "$tmp"
done

exit "$fail"
