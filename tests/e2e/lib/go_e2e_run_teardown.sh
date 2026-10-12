#!/usr/bin/env bash
# Advisory timeout/cancel safety net for Go staging E2Es.
#
# The Go tests retain their fail-closed t.Cleanup exact-delete path. This helper
# handles only the process-death gap where t.Cleanup cannot run. It discovers
# exact e2e-<tag>-<run_id>-<six-hex> roster rows and delegates every mutation
# and purge proof to cp_purge_receipt.sh. All workflows call this one SSOT and
# provide only the exact tag set their job creates.

set +e

GO_E2E_TEARDOWN_LIB_DIR=$(dirname -- "${BASH_SOURCE[0]}")
GO_E2E_TEARDOWN_LIB_DIR=$(cd -- "$GO_E2E_TEARDOWN_LIB_DIR" && pwd)
# shellcheck source=/dev/null
source "$GO_E2E_TEARDOWN_LIB_DIR/cp_purge_receipt.sh"

go_e2e_teardown_warn() {
  echo "::warning::go-e2e teardown $*"
}

if ! e2e_cp_require_local_backend \
  || ! e2e_cp_require_staging_origin "${CP_BASE_URL:-}"; then
  go_e2e_teardown_warn "safety net rejected its staging target/backend before bearer use"
  exit 0
fi

if [[ ! "${GITHUB_RUN_ID:-}" =~ ^[0-9]+$ ]]; then
  go_e2e_teardown_warn "discovery inconclusive: GITHUB_RUN_ID must contain digits only; no roster discovery or tenant DELETE was attempted"
  exit 0
fi
if [ -z "${ADMIN_TOKEN:-}" ] && [ -n "${CP_ADMIN_API_TOKEN:-}" ]; then
  ADMIN_TOKEN=$CP_ADMIN_API_TOKEN
fi
if [ -z "${ADMIN_TOKEN:-}" ]; then
  go_e2e_teardown_warn "discovery inconclusive: admin token unavailable; no roster discovery or tenant DELETE was attempted"
  exit 0
fi
if [ -z "${TEARDOWN_TAGS:-}" ]; then
  go_e2e_teardown_warn "discovery inconclusive: TEARDOWN_TAGS unset; no roster discovery or tenant DELETE was attempted"
  exit 0
fi
tag_count=0
for tag in $TEARDOWN_TAGS; do
  tag_count=$((tag_count + 1))
  if [[ ! "$tag" =~ ^[a-z0-9][a-z0-9-]{0,20}$ ]]; then
    go_e2e_teardown_warn "discovery inconclusive: invalid teardown tag; no roster discovery or tenant DELETE was attempted"
    exit 0
  fi
done
if [ "$tag_count" -eq 0 ]; then
  go_e2e_teardown_warn "discovery inconclusive: no teardown tags; no roster discovery or tenant DELETE was attempted"
  exit 0
fi

roster_rc=0
roster_json=$(e2e_cp_fetch_org_roster_json "$CP_BASE_URL" "$ADMIN_TOKEN") || roster_rc=$?
if [ "$roster_rc" != "0" ]; then
  go_e2e_teardown_warn "discovery inconclusive (roster rc=$roster_rc); no tenant DELETE was attempted"
  exit 0
fi

filter_rc=0
pairs=$(printf '%s' "$roster_json" | python3 \
  "$GO_E2E_TEARDOWN_LIB_DIR/filter_go_e2e_run_orgs.py" \
  --run-id "$GITHUB_RUN_ID" --tags "$TEARDOWN_TAGS") || filter_rc=$?
if [ "$filter_rc" != "0" ]; then
  go_e2e_teardown_warn "discovery inconclusive (run-scope filter rc=$filter_rc); no tenant DELETE was attempted"
  exit 0
fi

leaks=()
while IFS=$'\t' read -r slug org_id; do
  [ -n "$slug" ] || continue
  echo "Safety-net teardown: $slug"
  E2E_CP_PURGE_RESULT=""
  purge_rc=0
  e2e_cp_delete_and_verify_purge \
    "$CP_BASE_URL" "$ADMIN_TOKEN" "$slug" "$org_id" || purge_rc=$?
  if [ "$purge_rc" = "0" ] && [ "$E2E_CP_PURGE_RESULT" = "purged" ]; then
    echo "[teardown] verified exact CP purge receipt/audit and exact-tenant HTTP 404 for $slug"
  elif [ "$purge_rc" = "0" ] && [ "$E2E_CP_PURGE_RESULT" = "already_absent" ]; then
    echo "[teardown] exact tenant already absent by HTTP 404 for $slug; no DELETE or purge audit was required"
  else
    go_e2e_teardown_warn "verification for $slug failed or was inconclusive (rc=$purge_rc result=${E2E_CP_PURGE_RESULT:-unset}) - the automatic main-push janitor can retry only after its 90m age floor"
    leaks+=("$slug")
  fi
done <<< "$pairs"

if [ "${#leaks[@]}" -gt 0 ]; then
  go_e2e_teardown_warn "left ${#leaks[@]} leak(s); investigate now rather than waiting for the next main-push janitor after its 90m age floor: ${leaks[*]}"
fi
exit 0
