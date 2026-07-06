#!/usr/bin/env bash
# create_org_with_retry.sh — SSOT helper for POST /cp/admin/orgs that is robust
# to the create-502 FALSE-READY class.
#
# WHY: the shared CP /health preflight is always 200 on the surviving instance
# during a staging-tenant-cd roll, so it is NOT the real "CP ready to create"
# signal. The initial org-create is then a single shot that fails-closed on ANY
# non-2xx — including a TRANSIENT 502/503/504 from the tenant edge/Caddy during
# a CP redeploy/boot window. Because staging-tenant-cd rolls on push:[main] and
# the live-staging harnesses also fire on push:[main], a create can land inside
# a roll → a deterministic red misread as "org creation failed". This is the
# same class CP#1129 fixed on the CP-side harness; this lib ports the fix to the
# core harnesses (SSOT so all of them share ONE implementation).
#
# CONTRACT: retry ONLY on the transient transport class (502/503/504 with a
# gateway-shaped body). NEVER retry a 4xx — a 409 slug-collision or a 400 bad
# payload is a real bug and must stay red (fail-closed). Bounded by
# CREATE_ORG_RETRIES; on success sets CREATE_ORG_RESP to the response BODY (the
# trailing HTTP_CODE marker stripped) and returns 0. On exhaustion / 4xx sets
# CREATE_ORG_RESP to the last body and returns non-zero (the caller `fail`s).
#
# USAGE:
#   source "$(dirname "$0")/lib/create_org_with_retry.sh"
#   create_org_with_retry "$CP_URL" "$ADMIN_TOKEN" "$body_json" || fail "..."
#   ORG_ID=$(printf '%s' "$CREATE_ORG_RESP" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))')

# CREATE_ORG_RESP is an OUTPUT variable: this lib is sourced and the caller reads
# it after create_org_with_retry returns. shellcheck can't see the cross-file use,
# so its SC2034 "appears unused" is a false positive here (same pattern as the
# sibling e2e libs' REGISTER_RESP / REGISTER_STATUS output vars).
# shellcheck disable=SC2034
#
# Bounded retry budget (transient window during a CP roll is seconds; 6×10s ≈ 1m
# is a generous safety net that still fails loud on a genuinely-down CP).
CREATE_ORG_RETRIES="${CREATE_ORG_RETRIES:-6}"
CREATE_ORG_RETRY_SLEEP="${CREATE_ORG_RETRY_SLEEP:-10}"

# create_org_with_retry <cp_url> <admin_token> <body_json>
create_org_with_retry() {
  local cp_url="$1" admin_token="$2" body="$3"
  local attempt code resp safe
  CREATE_ORG_RESP=""
  for attempt in $(seq 1 "$CREATE_ORG_RETRIES"); do
    set +e
    resp=$(curl -sS --max-time 60 -w "\nHTTP_CODE=%{http_code}" -X POST \
      "$cp_url/cp/admin/orgs" \
      -H "Authorization: Bearer $admin_token" \
      -H "Content-Type: application/json" \
      -d "$body")
    set -e
    code=$(printf '%s' "$resp" | sed -n 's/^HTTP_CODE=//p' | tail -n1)
    code=${code:-000}
    # Strip the trailing HTTP_CODE marker line to hand back the pure body.
    CREATE_ORG_RESP=$(printf '%s' "$resp" | sed '/^HTTP_CODE=[0-9]*$/d')
    if [ "$code" = "200" ] || [ "$code" = "201" ]; then
      return 0
    fi
    safe=$(printf '%s' "$resp" | tr -d '\000' | head -c 300)
    # Retry ONLY on a transient transport class (gateway 5xx). curl code 000 =
    # connection reset/refused mid-roll — also transient.
    if echo "$code" | grep -Eq '^(000|502|503|504)$' \
       && { [ "$code" = "000" ] || echo "$safe" | grep -Eqi 'Service Unavailable|Bad Gateway|Gateway Timeout|error code: 50[24]|no healthy upstream|connection refused|upstream connect error'; }; then
      echo "    [create_org_with_retry] transient $code attempt ${attempt}/${CREATE_ORG_RETRIES}: $safe" >&2
      [ "$attempt" -lt "$CREATE_ORG_RETRIES" ] && { sleep "$CREATE_ORG_RETRY_SLEEP"; continue; }
    fi
    # Non-transient (4xx, or an unrecognized 5xx body): stop and fail closed.
    echo "    [create_org_with_retry] non-transient HTTP $code — failing closed: $safe" >&2
    return 1
  done
  return 1
}
