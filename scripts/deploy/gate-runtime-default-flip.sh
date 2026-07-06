#!/usr/bin/env bash
# gate-runtime-default-flip.sh — Guard B config/pin-flip coupling.
#
# A change to the platform default runtime (KMS MOLECULE_DEFAULT_RUNTIME) or an
# openclaw runtime-image pin advance must NOT take effect until the fresh-org
# platform-agent CALLABLE gate is GREEN against the proposed value — otherwise an
# out-of-gate openclaw advance or a default-flip skew (the class that hit
# task#225/#226/test123) reaches users uncaught. This wrapper makes the flip go
# THROUGH the gate:
#
#   1. read the current Infisical value (so we can revert);
#   2. WRITE the proposed value (advance) — only with --apply;
#   3. run the CALLABLE e2e (TestPlatformAgentMgmtMCP_Staging with
#      E2E_ASSERT_MGMT_MCP_CALLABLE=1 and E2E_DEFAULT_RUNTIME=<proposed>) against
#      staging — a fresh org provisions a concierge and a REAL A2A
#      provision_workspace turn must create a workspace;
#   4. GREEN  → keep the flip (gated, applied);
#      RED    → REVERT to the prior value (the flip is blocked) and exit non-zero.
#
# ORDERING NOTE: like the staging tenant-pin, the control plane reads these
# values at boot, so step 3 only exercises the proposed value once the CP has
# adopted it. Run this AFTER the CP has adopted the advance (or point CP_BASE_URL
# at an env already serving it); the gate's own E2E_EXPECT_TENANT_BUILD_SHA guard
# (in the Go test) hard-fails if it detects it is exercising a stale image, so a
# premature run REDs rather than false-greens.
#
# Usage:
#   gate-runtime-default-flip.sh --runtime openclaw            # gate a default-runtime flip
#   gate-runtime-default-flip.sh --key RUNTIME_IMAGE_PIN_OPENCLAW --value <ref>
#   gate-runtime-default-flip.sh --runtime openclaw --apply    # write, gate, keep/revert
#   gate-runtime-default-flip.sh --runtime openclaw --dry-run  # read + plan only (default)
#
# Required env (Infisical machine identity + staging CP admin for the e2e):
#   INFISICAL_CLIENT_ID  INFISICAL_CLIENT_SECRET  INFISICAL_PROJECT_ID
#   CP_BASE_URL          (default https://staging-api.moleculesai.app)
#   CP_ADMIN_API_TOKEN   (or fetched from Infisical /shared/controlplane-admin)
# Optional:
#   INFISICAL_BASE  (default https://key.moleculesai.app)
#   INFISICAL_ENV   (default staging)
#   INFISICAL_PATH  (default /shared/controlplane)
#
# SAFETY: --dry-run (the DEFAULT) performs ZERO writes; --apply writes exactly one
# Infisical secret and self-REVERTS it on a red gate; prints no credential.
set -euo pipefail
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL='*'

INFISICAL_BASE="${INFISICAL_BASE:-https://key.moleculesai.app}"
INFISICAL_ENV="${INFISICAL_ENV:-staging}"
INFISICAL_PATH="${INFISICAL_PATH:-/shared/controlplane}"
CP_BASE_URL="${CP_BASE_URL:-https://staging-api.moleculesai.app}"
KEY="" ; VALUE="" ; RUNTIME="" ; APPLY=0 ; DRY_RUN=1

usage() { sed -n '2,45p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --runtime) RUNTIME="$2"; shift 2;;
    --key)     KEY="$2";   shift 2;;
    --value)   VALUE="$2"; shift 2;;
    --apply)   APPLY=1; DRY_RUN=0; shift;;
    --dry-run) DRY_RUN=1; APPLY=0; shift;;
    -h|--help) usage 0;;
    *) echo "unknown arg: $1" >&2; usage 2;;
  esac
done
log() { printf '>> [flip-gate] %s\n' "$*" >&2; }

# A --runtime <name> is sugar for KEY=MOLECULE_DEFAULT_RUNTIME VALUE=<name>.
if [ -n "$RUNTIME" ]; then
  KEY="${KEY:-MOLECULE_DEFAULT_RUNTIME}"
  VALUE="${VALUE:-$RUNTIME}"
fi
[ -n "$KEY" ]   || { echo "FATAL: --runtime or --key is required" >&2; usage 2; }
[ -n "$VALUE" ] || { echo "FATAL: --value (or --runtime) is required" >&2; usage 2; }

for v in INFISICAL_CLIENT_ID INFISICAL_CLIENT_SECRET INFISICAL_PROJECT_ID; do
  [ -n "${!v:-}" ] || { echo "FATAL: $v is required (Infisical machine identity)" >&2; exit 2; }
done

# curl via DoH so *.moleculesai.app resolves through a hijacked :53 resolver.
CURL=(curl -fsS --doh-url https://cloudflare-dns.com/dns-query)
enc_path="$(printf '%s' "$INFISICAL_PATH" | sed 's#/#%2F#g')"

log "authenticating to Infisical ($INFISICAL_BASE)"
TOKEN="$("${CURL[@]}" -X POST "$INFISICAL_BASE/api/v1/auth/universal-auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"clientId\":\"$INFISICAL_CLIENT_ID\",\"clientSecret\":\"$INFISICAL_CLIENT_SECRET\"}" \
  | python3 -c 'import sys,json; v=(json.load(sys.stdin) or {}).get("accessToken"); sys.stdout.write(v if isinstance(v,str) else "")')"
[ -n "$TOKEN" ] || { echo "FATAL: Infisical auth returned empty accessToken" >&2; exit 1; }

read_secret() {
  "${CURL[@]}" "$INFISICAL_BASE/api/v3/secrets/raw/$KEY?workspaceId=$INFISICAL_PROJECT_ID&environment=$INFISICAL_ENV&secretPath=$enc_path" \
    -H "Authorization: Bearer $TOKEN" 2>/dev/null \
    | python3 -c 'import sys,json;
d=json.load(sys.stdin);
s=d.get("secret") if isinstance(d,dict) else None;
sys.stdout.write((s or {}).get("secretValue","") if s else "")'
}

write_secret() {  # write_secret <value>
  local val="$1" method body now
  local cur; cur="$(read_secret || true)"
  if [ "$cur" = "$val" ]; then log "value already '$val' — no write needed"; return 0; fi
  method=PATCH; [ -n "$cur" ] || method=POST
  body="$(python3 -c 'import json,sys; print(json.dumps({"environment":sys.argv[1],"secretPath":sys.argv[2],"secretValue":sys.argv[3],"workspaceId":sys.argv[4]}))' \
    "$INFISICAL_ENV" "$INFISICAL_PATH" "$val" "$INFISICAL_PROJECT_ID")"
  "${CURL[@]}" -X "$method" "$INFISICAL_BASE/api/v3/secrets/raw/$KEY" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "$body" >/dev/null
  now="$(read_secret || true)"
  [ "$now" = "$val" ] || { echo "FATAL: write of $KEY did not take (is '$now', want '$val')" >&2; exit 1; }
  log "wrote $KEY = '$val' (verified)"
}

OLD="$(read_secret || true)"
log "current $INFISICAL_ENV $INFISICAL_PATH::$KEY = '${OLD:-<unset>}'"
log "proposed value = '$VALUE'"

# The gate runner: run the CALLABLE fresh-org e2e against staging with the
# proposed runtime, requiring the real A2A provision_workspace turn.
run_callable_gate() {
  local rt="$RUNTIME"; [ -n "$rt" ] || rt="$VALUE"
  log "running CALLABLE gate (TestPlatformAgentMgmtMCP_Staging, runtime=$rt) against $CP_BASE_URL"
  ( cd "$(dirname "$0")/../../workspace-server" \
    && STAGING_E2E=1 \
       CP_BASE_URL="$CP_BASE_URL" \
       CP_ADMIN_API_TOKEN="${CP_ADMIN_API_TOKEN:-}" \
       E2E_PROVIDER="${E2E_PROVIDER:-molecules-server}" \
       E2E_DEFAULT_RUNTIME="$rt" \
       E2E_ASSERT_MGMT_MCP_CALLABLE=1 \
       go test -tags staging_e2e ./internal/staginge2e/ \
         -run TestPlatformAgentMgmtMCP_Staging -count=1 -v -timeout 40m )
}

if [ "$DRY_RUN" = "1" ]; then
  log "DRY-RUN (default): would advance $KEY '${OLD:-<unset>}' -> '$VALUE', then run the CALLABLE gate,"
  log "                   keeping it on GREEN and reverting to '${OLD:-<unset>}' on RED. No writes performed."
  exit 0
fi

# --apply: advance, gate, keep-or-revert.
write_secret "$VALUE"
if run_callable_gate; then
  log "GATE GREEN — flip of $KEY -> '$VALUE' is applied (gated)."
  echo "FLIP_APPLIED key=$KEY value=$VALUE"
  exit 0
fi
log "GATE RED — reverting $KEY -> '${OLD:-<unset>}' (flip BLOCKED)."
if [ -n "$OLD" ]; then
  write_secret "$OLD"
else
  # Nothing to revert to (key was unset). Delete the just-written key to restore
  # the prior 'unset' state.
  "${CURL[@]}" -X DELETE "$INFISICAL_BASE/api/v3/secrets/raw/$KEY" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
    -d "$(python3 -c 'import json,sys; print(json.dumps({"environment":sys.argv[1],"secretPath":sys.argv[2],"workspaceId":sys.argv[3]}))' "$INFISICAL_ENV" "$INFISICAL_PATH" "$INFISICAL_PROJECT_ID")" >/dev/null 2>&1 || true
  log "reverted by deleting $KEY (was unset before the flip)"
fi
echo "FLIP_BLOCKED key=$KEY value=$VALUE reverted_to=${OLD:-<unset>}"
exit 1
