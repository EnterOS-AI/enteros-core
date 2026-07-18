#!/usr/bin/env bash

# Verify the current staging teardown contract without pretending that a generic
# Gitea runner can enumerate provider resources directly. DELETE is synchronous:
# a 200 receipt is returned only after executeOrgPurge has completed its
# provider-specific cleanup and DB-row step. We corroborate that receipt against
# the exact purge audit row, then require the authoritative exact-tenant endpoint
# to return its identity-bearing 404 JSON for the same slug.

e2e_cp_require_local_backend() {
  local backend="${E2E_INFRA_BACKEND:-<unset>}"
  if [ "$backend" != "local-docker" ]; then
    echo "[cp-purge] unsupported E2E_INFRA_BACKEND=$backend; active staging is local-docker only" >&2
    return 2
  fi
}

e2e_cp_require_staging_origin() {
  local cp_url="${1:-}"
  local port
  if [ "$cp_url" = "https://staging-api.moleculesai.app" ]; then
    return 0
  fi
  if [ "${E2E_CP_ALLOW_EPHEMERAL_LOOPBACK:-0}" = "1" ] \
    && [[ "$cp_url" =~ ^http://127\.0\.0\.1:([1-9][0-9]{0,4})$ ]]; then
    port="${BASH_REMATCH[1]}"
    if [ "$((10#$port))" -le 65535 ]; then
      return 0
    fi
  fi
  echo "[cp-purge] refusing to send the admin bearer outside the staging control-plane origin or explicit numeric-loopback test origin" >&2
  return 2
}

e2e_cp_validate_org_id() {
  local org_id="${1:-}"
  [[ "$org_id" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]
}

e2e_cp_publish_creation_identity() {
  local slug="${1:-}"
  local org_id="${2:-}"

  case "$slug" in
    e2e-*|rt-e2e-*) ;;
    *) return 2 ;;
  esac
  case "$slug" in
    e2e-|rt-e2e-|*[!a-z0-9-]*) return 2 ;;
  esac
  e2e_cp_validate_org_id "$org_id" || return 2
  [ -n "${GITHUB_ENV:-}" ] || return 0
  printf 'E2E_CREATED_SLUG=%s\nE2E_CREATED_ORG_ID=%s\n' \
    "$slug" "$org_id" >> "$GITHUB_ENV"
}

# Set E2E_CP_TENANT_STATE to present, absent, or inconclusive and preserve the
# exact org identity in E2E_CP_TENANT_ORG_ID when present. The control plane's
# boot-events handler first resolves the exact organizations.slug row. Only its
# current `{"error":"org not found","slug":"<exact slug>"}` 404 contract is
# authoritative absence proof; a generic edge/route 404 or roster page is not.
_e2e_cp_probe_tenant_identity() {
  local cp_url="$1"
  local admin_token="$2"
  local slug="$3"
  local ua="${MOLECULE_UA:-curl/8.4.0}"
  local tmpdir body code org_id

  E2E_CP_TENANT_STATE=inconclusive
  E2E_CP_TENANT_ORG_ID=""
  tmpdir=$(mktemp -d -t cp-tenant-probe-XXXXXX) || return 4
  body="$tmpdir/tenant.json"

  if ! code=$(curl -sS -A "$ua" --max-time 30 \
      -o "$body" -w '%{http_code}' \
      "$cp_url/cp/admin/tenants/$slug/boot-events?limit=1" \
      -H "Authorization: Bearer $admin_token" 2>/dev/null); then
    echo "[cp-purge] exact-tenant identity check is inconclusive for slug=$slug (request failed)" >&2
    rm -rf "$tmpdir"
    return 4
  fi

  case "$code" in
    404)
      if ! python3 -c '
import json, sys
expected_slug = sys.argv[1]
try:
    data = json.load(sys.stdin)
except Exception:
    raise SystemExit(1)
valid = (
    isinstance(data, dict)
    and set(data) == {"error", "slug"}
    and data.get("error") == "org not found"
    and data.get("slug") == expected_slug
)
raise SystemExit(0 if valid else 1)
' "$slug" < "$body" 2>/dev/null; then
        echo "[cp-purge] exact-tenant identity check is inconclusive for slug=$slug (malformed or mismatched HTTP 404)" >&2
        rm -rf "$tmpdir"
        return 4
      fi
      E2E_CP_TENANT_STATE=absent
      rm -rf "$tmpdir"
      return 0
      ;;
    200)
      org_id=$(python3 -c '
import json, sys, uuid
expected_slug = sys.argv[1]
try:
    data = json.load(sys.stdin)
    org_id = data.get("org_id", "") if isinstance(data, dict) else ""
    valid = data.get("slug") == expected_slug and isinstance(org_id, str)
    uuid.UUID(org_id)
except Exception:
    valid = False
if not valid:
    raise SystemExit(1)
print(org_id)
' "$slug" < "$body" 2>/dev/null) || {
        echo "[cp-purge] exact-tenant identity check is inconclusive for slug=$slug (malformed or mismatched HTTP 200)" >&2
        rm -rf "$tmpdir"
        return 4
      }
      E2E_CP_TENANT_STATE=present
      E2E_CP_TENANT_ORG_ID="$org_id"
      rm -rf "$tmpdir"
      return 0
      ;;
    *)
      echo "[cp-purge] exact-tenant identity check is inconclusive for slug=$slug (HTTP $code)" >&2
      rm -rf "$tmpdir"
      return 4
      ;;
  esac
}

# Validate the safety-net roster before its contents are used to discover this
# run's slugs. This endpoint is discovery only; exact absence is proven above.
e2e_cp_fetch_org_roster_json() {
  local cp_url="$1"
  local admin_token="$2"
  local backend="${E2E_INFRA_BACKEND:-<unset>}"
  local ua="${MOLECULE_UA:-curl/8.4.0}"
  local tmpdir body code validated

  E2E_INFRA_BACKEND="$backend" e2e_cp_require_local_backend || return $?
  e2e_cp_require_staging_origin "$cp_url" || return $?
  if [ -z "$admin_token" ]; then
    echo "[cp-purge] safety-net roster discovery is inconclusive: admin token is required" >&2
    return 2
  fi

  tmpdir=$(mktemp -d -t cp-roster-discovery-XXXXXX) || return 4
  body="$tmpdir/orgs.json"
  if ! code=$(curl -sS -A "$ua" --max-time 30 \
      -o "$body" -w '%{http_code}' \
      "$cp_url/cp/admin/orgs?limit=500" \
      -H "Authorization: Bearer $admin_token" 2>/dev/null); then
    echo "[cp-purge] safety-net roster discovery is inconclusive: request failed" >&2
    rm -rf "$tmpdir"
    return 4
  fi
  if [ "$code" != "200" ]; then
    echo "[cp-purge] safety-net roster discovery is inconclusive: HTTP $code" >&2
    rm -rf "$tmpdir"
    return 4
  fi

  validated=$(python3 -c '
import json, sys
try:
    data = json.load(sys.stdin)
    rows = data.get("orgs") if isinstance(data, dict) else None
    if not isinstance(rows, list):
        raise ValueError("orgs is not a list")
    if any(not isinstance(row, dict) or not isinstance(row.get("slug"), str) for row in rows):
        raise ValueError("invalid org row")
except Exception:
    raise SystemExit(1)
print(json.dumps(data, separators=(",", ":")))
' < "$body" 2>/dev/null) || {
    echo "[cp-purge] safety-net roster discovery is inconclusive: malformed JSON response" >&2
    rm -rf "$tmpdir"
    return 4
  }
  printf '%s\n' "$validated"
  rm -rf "$tmpdir"
}

# e2e_cp_delete_and_verify_purge <cp_url> <admin_token> <slug> <creation_org_id>
# The creation-returned ID is mandatory and must still own the exact slug before
# this helper will issue DELETE.
e2e_cp_delete_and_verify_purge() {
  local cp_url="$1"
  local admin_token="$2"
  local slug="$3"
  local expected_org_id="${4:-}"
  local backend="${E2E_INFRA_BACKEND:-<unset>}"
  local ua="${MOLECULE_UA:-curl/8.4.0}"
  local poll_secs="${E2E_CP_PURGE_POLL_SECS:-60}"
  local poll_interval="${E2E_CP_PURGE_POLL_INTERVAL:-5}"
  # Dedicated, independently-tunable budget for the 409 "active lifecycle
  # operation" DELETE-retry phase. This is ADDITIVE to the audit-poll and
  # absence-poll budgets (each poll_secs), so worst-case teardown ≈
  # delete_retry_secs + 2*poll_secs; keep this modest so a slow lifecycle-drain
  # cannot blow a job cleanup timeout. Observed drain in CI is ~30s.
  local delete_retry_secs="${E2E_CP_PURGE_DELETE_RETRY_SECS:-60}"
  local tmpdir delete_body audit_body
  local delete_code audit_code purge_id receipt_org_id receipt_fields
  local audit_state audit_fields audit_purge_id predelete_org_id deadline
  local delete_started_epoch delete_deadline
  local delete_response_lost=0

  E2E_CP_PURGE_RESULT=""

  E2E_INFRA_BACKEND="$backend" e2e_cp_require_local_backend || return $?
  e2e_cp_require_staging_origin "$cp_url" || return $?
  case "$slug" in
    e2e-*|rt-e2e-*) ;;
    *)
      echo "[cp-purge] refusing teardown for non-E2E slug" >&2
      return 2
      ;;
  esac
  case "$slug" in
    e2e-|rt-e2e-|*[!a-z0-9-]*)
      echo "[cp-purge] refusing teardown for malformed E2E slug" >&2
      return 2
      ;;
  esac
  if [ -z "$admin_token" ]; then
    echo "[cp-purge] admin token is required" >&2
    return 2
  fi
  if ! e2e_cp_validate_org_id "$expected_org_id"; then
    echo "[cp-purge] a valid creation-returned org_id is required before cleanup" >&2
    return 2
  fi
  if ! [[ "$poll_secs" =~ ^(0|[1-9][0-9]*)$ ]]; then
    echo "[cp-purge] purge poll seconds must be a canonical non-negative integer" >&2
    return 2
  fi
  if ! [[ "$poll_interval" =~ ^(0|[1-9][0-9]*)$ ]]; then
    echo "[cp-purge] purge poll interval must be a canonical non-negative integer" >&2
    return 2
  fi
  poll_secs=$((10#$poll_secs))
  poll_interval=$((10#$poll_interval))

  _e2e_cp_probe_tenant_identity "$cp_url" "$admin_token" "$slug" || return $?
  case "$E2E_CP_TENANT_STATE" in
    absent)
      E2E_CP_PURGE_RESULT=already_absent
      echo "[cp-purge] exact org already absent (slug=$slug; exact-tenant HTTP 404); no DELETE or purge audit required"
      return 0
      ;;
    present)
      if [ "$E2E_CP_TENANT_ORG_ID" != "$expected_org_id" ]; then
        echo "[cp-purge] refusing DELETE because exact slug org_id mismatch (slug=$slug expected=$expected_org_id observed=$E2E_CP_TENANT_ORG_ID)" >&2
        return 4
      fi
      predelete_org_id="$expected_org_id"
      ;;
    *)
      echo "[cp-purge] exact-tenant identity check is inconclusive for slug=$slug" >&2
      return 4
      ;;
  esac

  tmpdir=$(mktemp -d -t cp-purge-verify-XXXXXX) || return 2
  delete_body="$tmpdir/delete.json"
  audit_body="$tmpdir/audit.json"
  # DELETE is synchronous. A 409 is the control plane's "organization has an
  # active lifecycle operation" — claimOrgPurge could not claim the lifecycle
  # parent row because the org's OWN in-flight workspace op (provision or
  # deprovision) has not drained yet. It is transient and self-resolving: no
  # purge audit row is created on 409 (nothing to poll), so the only correct
  # recovery is to wait for the lifecycle claim to free and re-issue the DELETE.
  # Without this, a teardown that lands while the org is still settling hard-
  # fails and LEAKS the tenant (the staging-provisioning-outage leak class). The
  # retry is bounded by its OWN delete_retry_secs budget (additive to the later
  # audit/absence polls — see the local declaration). Any OTHER non-200 is a hard
  # failure: it is not a self-resolving conflict, and retrying it would only mask
  # a real teardown defect.
  delete_deadline=$(( $(date +%s) + delete_retry_secs ))
  while true; do
    delete_started_epoch=$(date +%s)
    if ! delete_code=$(curl -sS -A "$ua" --max-time 120 \
        -o "$delete_body" -w '%{http_code}' \
        -X DELETE "$cp_url/cp/admin/tenants/$slug" \
        -H "Authorization: Bearer $admin_token" \
        -H "Content-Type: application/json" \
        -d "{\"confirm\":\"$slug\"}"); then
      # The synchronous handler may complete the purge and then lose its response
      # while local-Docker detaches the tenant network. That is an ambiguous
      # transport result, not proof of failure or success. Recover only if the CP
      # subsequently exposes a completed audit for this exact creation-returned
      # org ID and the exact tenant endpoint proves structured absence below.
      delete_response_lost=1
      purge_id=""
      receipt_org_id="$predelete_org_id"
      echo "[cp-purge] DELETE response was lost for slug=$slug; requiring exact completed purge audit plus exact-org absence" >&2
      break
    fi

    if [ "$delete_code" = "409" ]; then
      if [ "$(date +%s)" -ge "$delete_deadline" ]; then
        echo "[cp-purge] DELETE still 409 (active lifecycle operation) after ${delete_retry_secs}s for slug=$slug" >&2
        rm -rf "$tmpdir"
        return 4
      fi
      echo "[cp-purge] DELETE 409 (active lifecycle operation) for slug=$slug; lifecycle op still draining, retrying in ${poll_interval}s"
      sleep "$poll_interval"
      continue
    fi

    if [ "$delete_code" != "200" ]; then
      echo "[cp-purge] DELETE returned HTTP $delete_code without a completed purge receipt for slug=$slug" >&2
      rm -rf "$tmpdir"
      return 4
    fi

    receipt_fields=$(python3 -c '
import json, sys, uuid
expected_slug, expected_org_id = sys.argv[1:3]
try:
    data = json.load(sys.stdin)
    purge_id = data.get("purge_id", "")
    org_id = data.get("org_id", "")
    valid = (
        data.get("deleted") is True
        and data.get("slug") == expected_slug
        and isinstance(purge_id, str)
        and isinstance(org_id, str)
        and org_id == expected_org_id
    )
    uuid.UUID(purge_id)
    uuid.UUID(org_id)
except Exception:
    valid = False
if not valid:
    raise SystemExit(1)
print(purge_id, org_id, sep="\t")
' "$slug" "$predelete_org_id" < "$delete_body" 2>/dev/null) || {
      echo "[cp-purge] DELETE returned a malformed or mismatched purge receipt for slug=$slug" >&2
      rm -rf "$tmpdir"
      return 4
    }
    IFS=$'\t' read -r purge_id receipt_org_id <<< "$receipt_fields"
    break
  done

  deadline=$(( $(date +%s) + poll_secs ))
  while true; do
    audit_code=""
    if audit_code=$(curl -sS -A "$ua" --max-time 30 \
        -o "$audit_body" -w '%{http_code}' \
        "$cp_url/cp/admin/purges?limit=500" \
        -H "Authorization: Bearer $admin_token"); then
      if [ "$audit_code" = "200" ]; then
        audit_fields=$(python3 -c '
import datetime, json, sys, uuid
purge_id, slug, org_id, delete_started_raw = sys.argv[1:5]
delete_started = int(delete_started_raw)
try:
    data = json.load(sys.stdin)
    rows = data.get("purges", []) if isinstance(data, dict) else []
except Exception:
    print("invalid")
    raise SystemExit(0)
if not isinstance(rows, list):
    print("invalid")
    raise SystemExit(0)

def completion_epoch(row):
    row_id = row.get("id")
    completed_at = row.get("completed_at")
    try:
        uuid.UUID(row_id)
        if not isinstance(completed_at, str) or not completed_at.strip():
            return None
        stamp = datetime.datetime.fromisoformat(completed_at.replace("Z", "+00:00"))
        if stamp.tzinfo is None:
            return None
    except Exception:
        return None
    if row.get("last_step") != "completed":
        return None
    return stamp.timestamp()

if purge_id:
    row = next((r for r in rows if isinstance(r, dict) and r.get("id") == purge_id), None)
    if row is None:
        print("missing")
    elif row.get("org_slug") != slug or row.get("org_id") != org_id:
        print("mismatch")
    elif row.get("status") == "completed":
        if completion_epoch(row) is not None:
            print("completed", row["id"], sep="\t")
        else:
            print("invalid")
    elif row.get("status") == "failed":
        print("failed")
    else:
        print("pending")
else:
    # A lost DELETE response supplies no trustworthy purge_id. The
    # creation-returned org ID is still collision-safe authority: accept only a
    # completed, internally consistent audit for that exact slug/ID pair. Exact
    # structured absence is checked separately after this loop.
    matching = [
        r for r in rows
        if isinstance(r, dict)
        and r.get("org_slug") == slug
        and r.get("org_id") == org_id
    ]
    done = [
        r for r in matching
        if r.get("status") == "completed"
        and (completion_epoch(r) or -1) >= delete_started
    ]
    if done:
        row = max(done, key=lambda r: r.get("completed_at", ""))
        print("completed", row["id"], sep="\t")
    elif any(r.get("status") == "completed" for r in matching):
        print("invalid")
    elif any(r.get("status") == "failed" for r in matching):
        print("failed")
    elif matching:
        print("pending")
    else:
        print("missing")
' "$purge_id" "$slug" "$receipt_org_id" "$delete_started_epoch" < "$audit_body" 2>/dev/null || echo invalid)
        IFS=$'\t' read -r audit_state audit_purge_id <<< "$audit_fields"
        case "$audit_state" in
          completed)
            if [ "$delete_response_lost" = "1" ]; then
              purge_id="$audit_purge_id"
              echo "[cp-purge] recovered exact completed purge audit after lost DELETE response (purge_id=$purge_id org_id=$receipt_org_id slug=$slug)"
            fi
            break
            ;;
          failed|mismatch|invalid)
            echo "[cp-purge] exact purge audit did not complete safely for slug=$slug (state=$audit_state)" >&2
            rm -rf "$tmpdir"
            return 4
            ;;
        esac
      fi
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "[cp-purge] exact purge audit was not completed within ${poll_secs}s for slug=$slug" >&2
      rm -rf "$tmpdir"
      return 4
    fi
    sleep "$poll_interval"
  done

  deadline=$(( $(date +%s) + poll_secs ))
  while true; do
    if ! _e2e_cp_probe_tenant_identity "$cp_url" "$admin_token" "$slug"; then
      rm -rf "$tmpdir"
      return 4
    fi
    if [ "$E2E_CP_TENANT_STATE" = "absent" ]; then
      # Caller-visible success classification used by harness/safety-net logs.
      # shellcheck disable=SC2034
      E2E_CP_PURGE_RESULT=purged
      echo "[cp-purge] CP purge completed (purge_id=$purge_id org_id=$receipt_org_id) and exact org absent (slug=$slug; exact-tenant HTTP 404); direct provider enumeration is not performed by this runner"
      rm -rf "$tmpdir"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "[cp-purge] exact org remained present within ${poll_secs}s for slug=$slug" >&2
      rm -rf "$tmpdir"
      return 4
    fi
    sleep "$poll_interval"
  done
}
