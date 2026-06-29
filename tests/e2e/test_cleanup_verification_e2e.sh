#!/usr/bin/env bash
# test_cleanup_verification_e2e.sh — the CLEANUP-VERIFICATION WRAPPER.
#
# Doubles a real org-lifecycle e2e as a TEARDOWN-CORRECTNESS gate. It provisions a
# throwaway tenant through the product's OWN control path (CP admin API + the
# platform-agent MCP), tears it down through the product's OWN teardown (CP admin
# DELETE /cp/admin/tenants/:slug → executeOrgPurge → provisioner.DeprovisionInstance,
# plus the concierge MCP where exercised), and then makes the NEW, REQUIRED
# assertion: scan EVERY resource class for anything still bearing this run and
# FAIL — naming the leaked resource + the teardown op that should have removed it —
# if the product's own teardown left ANYTHING behind. A leftover is a TEST
# FAILURE, not something a reaper silently mops up. The reaper (sweep/
# e2e_leak_reap.go #1021/#805) is the BACKSTOP for the one path a bash trap can't
# cover (SIGKILL / runner hard-cancel), not the primary cleanup.
#
# THE FIVE STAGES (run_footprint.sh implements the engine):
#   1. CAPTURE   unique RUN_ID per run (== the e2e- slug, the universal link); the
#                run's whole footprint is enumerable by org-id / e2e-run-id label,
#                derived name, and DB org_id.
#   2. RUN       create org via CP admin API → (best-effort) drive the concierge's
#                platform MCP create_workspace → capture footprint → DEPROVISION via
#                the CP admin API (the product's OWN teardown).
#   3. VERIFY    after teardown returns, scan docker containers/networks/volumes,
#                cloudflare dns/tunnels, and db org_instances/organizations/
#                tenant_resources for ANY survivor bearing the run. Survivor → FAIL.
#   4. FINALLY   force-remove anything left by RUN_ID (so the e2e never itself leaks,
#                even when it CATCHES a product bug). Trap registered BEFORE
#                provisioning; trap EXIT INT TERM covers success / assert-fail /
#                set -e / timeout / Ctrl-C.
#   5. RESIDUAL  a bash trap cannot run on SIGKILL / CI hard-cancel; that residual
#                is covered by force-by-label + the now-backstop e2e-leak reaper.
#
# TARGETS (provider-agnostic — same VERIFY both ways):
#   local-docker (default): MOLECULE_CP_URL=https://localhost:8090, tenants are local
#     docker containers (no CF/EC2 → those scans no-op), DB via RF_DB_EXEC.
#   staging EC2/CF: MOLECULE_CP_URL=https://staging-api.moleculesai.app, set RF_CF_*
#     to enable the DNS/tunnel scan; org-row + EC2 covered by the admin API.
#
# Env:
#   MOLECULE_CP_URL        default https://localhost:8090
#   MOLECULE_ADMIN_TOKEN   CP admin bearer (or ADMIN_TOKEN). REQUIRED for a live run.
#   E2E_CP_INSECURE        1 → curl -k (auto-on for localhost self-signed TLS).
#   RF_DB_EXEC             psql prefix for the DB-row scan (see run_footprint.sh).
#   RF_CF_API_TOKEN/RF_CF_ZONE_ID/RF_CF_ACCOUNT_ID/RF_APP_DOMAIN  enable CF scan.
#   E2E_DRIVE_CONCIERGE    1 (default) → best-effort drive the platform MCP; 0 → skip.
#   E2E_PROVISION_TIMEOUT_SECS   default 600 (tenant running)
#   E2E_CONCIERGE_ONLINE_SECS    default 300
#   E2E_AGENT_ACT_SECS           default 300
#   E2E_SIMULATE_LEAK      ""(default) | container | volume | network — inject ONE
#                          tagged resource AFTER teardown to PROVE the VERIFY bites.
#   E2E_RUN_ID             slug suffix seed (CI: ${GITHUB_RUN_ID}-${RUN_ATTEMPT}).
#
# Exit: 0 pass / honest skip-loud; 1 VERIFY-FAIL (leak) or assertion failure; 2 bad env.
set -euo pipefail

source "$(dirname "$0")/_lib.sh"
# shellcheck source=lib/run_footprint.sh
source "$(dirname "$0")/lib/run_footprint.sh"
# shellcheck source=lib/collision-proof-slug.sh
source "$(dirname "$0")/lib/collision-proof-slug.sh"

CP_URL="${MOLECULE_CP_URL:-https://localhost:8090}"
ADMIN_TOKEN="${MOLECULE_ADMIN_TOKEN:-${ADMIN_TOKEN:-}}"
PROVISION_TIMEOUT_SECS="${E2E_PROVISION_TIMEOUT_SECS:-600}"
CONCIERGE_ONLINE_SECS="${E2E_CONCIERGE_ONLINE_SECS:-300}"
AGENT_ACT_SECS="${E2E_AGENT_ACT_SECS:-300}"
DRIVE_CONCIERGE="${E2E_DRIVE_CONCIERGE:-1}"
SIMULATE_LEAK="${E2E_SIMULATE_LEAK:-}"

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] ❌ $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] ✅ $*"; }
skip_loud() { echo "[$(date +%H:%M:%S)] ⏭️  SKIP: $*" >&2; exit 0; }

# Auto-insecure for a localhost self-signed dev CP.
CURL_K=()
if [ "${E2E_CP_INSECURE:-}" = "1" ] || echo "$CP_URL" | grep -qE '://(localhost|127\.0\.0\.1)'; then
  CURL_K=(-k)
fi
CURL_COMMON=(-sS --max-time 30 "${CURL_K[@]}")

# Collision-proof, sweeper-/reaper-covered slug. THIS is the RUN_ID.
SLUG="e2e-clv-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 12)"
assert_collision_proof_slug "$SLUG" || fail "bad slug $SLUG"
RUN_ID="$SLUG"
WORKER_NAME="e2e-clv-w-$(make_collision_proof_slug_suffix "${E2E_RUN_ID:-}" 8)"
WORKER_NAME=$(echo "$WORKER_NAME" | tr -cd 'a-zA-Z0-9-' | head -c 48)
export WORKER_NAME

# ─── PR-mode / no-stack self-check (CI-safe; the live gate runs on push/dispatch) ─
# A required context must still resolve on a PR runner that has no local stack or
# creds. There we self-check bash -n (catches a merge that breaks the wrapper) and
# skip-loud; the REAL teardown-verify gate runs where a stack + admin token exist.
if [ -z "$ADMIN_TOKEN" ] || ! curl "${CURL_COMMON[@]}" --max-time 6 "$CP_URL/health" >/dev/null 2>&1; then
  log "no admin token or CP not reachable at $CP_URL — PR-mode self-check only."
  bash -n "$0" || fail "PR-mode self-check FAILED: bash -n on $0 has a syntax error"
  bash -n "$(dirname "$0")/lib/run_footprint.sh" || fail "PR-mode self-check FAILED: bash -n on lib/run_footprint.sh"
  ok "PR-mode self-check PASSED — $(basename "$0") + run_footprint.sh are bash-clean. Live teardown-verify runs where a CP + admin token exist."
  exit 0
fi

TMPDIR_E2E=$(mktemp -d -t clv-XXXXXX)

# ─── state for the teardown trap (registered BEFORE we provision anything) ──────
ORG_ID=""
NETWORK_NAME=""
PROVISIONED=0
CP_TEARDOWN_DONE=0
export RF_CAPTURED_VOLS=""
export RF_WORKER_IDS=""

admin_call() {  # METHOD PATH [curl args…]
  local method="$1" path="$2"; shift 2
  curl "${CURL_COMMON[@]}" -X "$method" "$CP_URL$path" \
    -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" "$@"
}

cp_teardown() {  # the product's OWN teardown — CP admin tenant delete cascade
  [ -z "$ORG_ID" ] && return 0
  [ "$CP_TEARDOWN_DONE" = "1" ] && return 0
  CP_TEARDOWN_DONE=1
  log "🧹 CP teardown: DELETE /cp/admin/tenants/$SLUG (executeOrgPurge cascade)…"
  admin_call DELETE "/cp/admin/tenants/$SLUG" --max-time 200 \
    -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1 || \
    log "  (delete returned non-2xx — may be mid-cascade / already gone)"
  # Eventual-consistency: wait for the org row to disappear from the admin list.
  local gone=0 i
  for i in $(seq 1 24); do
    if ! admin_call GET "/cp/admin/orgs" 2>/dev/null \
        | python3 -c "import json,sys
d=json.load(sys.stdin); sys.exit(0 if any(o.get('slug')=='$SLUG' and o.get('status')!='purged' for o in d.get('orgs',[])) else 1)" 2>/dev/null; then
      gone=1; break
    fi
    sleep 5
  done
  [ "$gone" = "1" ] && log "  org row gone from admin list" || log "  org row still present (the VERIFY/db scan will surface it)"
}

finalize() {
  local rc=$?
  rm -rf "$TMPDIR_E2E" 2>/dev/null || true
  if [ "$PROVISIONED" = "1" ]; then
    # If we exited before the product teardown ran (early failure), still exercise
    # it (the wrapper's job is to TEST that teardown), THEN force-clean.
    if [ "$CP_TEARDOWN_DONE" != "1" ]; then
      log "finalize: product teardown had not run — invoking it now before the safety belt."
      cp_teardown || true
    fi
    log "finalize: FINALLY safety belt — force-removing anything left by run_id=$RUN_ID."
    rf_force_clean "$ORG_ID" "$RUN_ID" "$NETWORK_NAME" || true
  fi
  exit "$rc"
}
trap finalize EXIT INT TERM   # success / assert-fail / set -e / timeout / Ctrl-C

# ─── 0. preflight ──────────────────────────────────────────────────────────────
log "═══ CLEANUP-VERIFICATION WRAPPER ═══  CP=$CP_URL  run_id(slug)=$SLUG"
admin_call GET "/cp/admin/orgs" >/dev/null 2>&1 || fail "CP admin API not reachable / token rejected at $CP_URL"
ok "CP admin API reachable"
rf_have_docker && log "docker daemon reachable — container/network/volume scan ACTIVE" \
              || log "no docker daemon — container/network/volume scan will SKIP (cloud target)"
[ -n "${RF_DB_EXEC:-}" ] && log "RF_DB_EXEC set — db-row scan ACTIVE" || log "RF_DB_EXEC unset — db-row scan SKIPs (org-row admin check is the fallback)"
rf_cf_enabled && log "RF_CF_* set — cloudflare dns/tunnel scan ACTIVE" || log "RF_CF_* unset — cloudflare scan SKIPs (local-docker creates none)"

# ─── 1. RUN: create org via the CP admin API (provisions the tenant) ────────────
log "1/5 Creating throwaway org via CP admin API…"
CREATE_RESP=$(admin_call POST "/cp/admin/orgs" \
  -d "{\"slug\":\"$SLUG\",\"name\":\"E2E cleanup-verify $SLUG\",\"owner_user_id\":\"e2e-runner:$SLUG\"}")
ORG_ID=$(echo "$CREATE_RESP" | python3 -c "import json,sys
try: print(json.load(sys.stdin).get('id',''))
except Exception: print('')" 2>/dev/null || echo "")
[ -z "$ORG_ID" ] && fail "org create returned no id: $(echo "$CREATE_RESP" | head -c 300)"
PROVISIONED=1
NETWORK_NAME="mol-$(echo "$ORG_ID" | tr -d - | cut -c1-12)"
ok "Org created id=$ORG_ID  (derived docker network=$NETWORK_NAME)"

log "    waiting for tenant provisioning (instance_status=running, up to ${PROVISION_TIMEOUT_SECS}s)…"
DEADLINE=$(( $(date +%s) + PROVISION_TIMEOUT_SECS )); LAST=""
while true; do
  [ "$(date +%s)" -gt "$DEADLINE" ] && fail "tenant never reached running within ${PROVISION_TIMEOUT_SECS}s"
  ST=$(admin_call GET "/cp/admin/orgs" 2>/dev/null | python3 -c "import json,sys
d=json.load(sys.stdin)
print(next((o.get('instance_status','') for o in d.get('orgs',[]) if o.get('slug')=='$SLUG'),''))" 2>/dev/null || echo "")
  [ "$ST" != "$LAST" ] && { log "    instance_status → ${ST:-<none>}"; LAST="$ST"; }
  case "$ST" in
    running) break ;;
    failed)  fail "tenant provisioning FAILED for $SLUG" ;;
    *) sleep 10 ;;
  esac
done
ok "Tenant provisioning complete (running)"

# ─── 2. RUN (best-effort): drive the platform-agent MCP create_workspace ────────
# Honest: the local concierge needs the platform-agent image + a working model to
# expose create_workspace. If it can't (tool/model absent), we log it and proceed —
# the org-level footprint (tenant trio + concierge + network + volumes + db rows)
# is already created by the CP admin API and is what the teardown must fully remove.
WORKER_ID=""
if [ "$DRIVE_CONCIERGE" = "1" ]; then
  log "2/5 Best-effort: drive the concierge's platform MCP to create_workspace '$WORKER_NAME'…"
  TENANT_TOKEN=$(admin_call GET "/cp/admin/orgs/$SLUG/admin-token" 2>/dev/null \
    | python3 -c "import json,sys
try: print(json.load(sys.stdin).get('admin_token',''))
except Exception: print('')" 2>/dev/null || echo "")
  # Tenant URL: local <slug>.lvh.me:<cp-port>, staging <slug>.staging.<domain>.
  CP_HOST=$(echo "$CP_URL" | sed -E 's#^https?://##; s#[:/].*$##')
  CP_PORT=$(echo "$CP_URL" | sed -E 's#^https?://[^:/]+##; s#^:##; s#/.*$##')
  case "$CP_HOST" in
    localhost|127.0.0.1) TENANT_URL="https://$SLUG.lvh.me${CP_PORT:+:$CP_PORT}" ;;
    staging-api.*)       TENANT_URL="https://$SLUG.staging.${CP_HOST#staging-api.}" ;;
    api.*)               TENANT_URL="https://$SLUG.${CP_HOST#api.}" ;;
    *)                   TENANT_URL="https://$SLUG.$CP_HOST" ;;
  esac
  tenant_call() { local m="$1" p="$2"; shift 2; curl "${CURL_COMMON[@]}" -X "$m" "$TENANT_URL$p" \
    -H "Authorization: Bearer $TENANT_TOKEN" -H "X-Molecule-Org-Id: $ORG_ID" -H "Origin: $TENANT_URL" "$@"; }

  if [ -z "$TENANT_TOKEN" ]; then
    log "    no tenant admin token — skipping the MCP drive (org-level lifecycle still proves teardown)."
  else
    # Wait for the concierge (kind=platform root) to be online, bounded.
    CONCIERGE_ID=""; DDL=$(( $(date +%s) + CONCIERGE_ONLINE_SECS ))
    while [ "$(date +%s)" -lt "$DDL" ]; do
      CONCIERGE_ID=$(tenant_call GET /workspaces 2>/dev/null | python3 -c "import json,sys
try: rows=json.load(sys.stdin)
except Exception: rows=[]
print(next((w.get('id','') for w in (rows if isinstance(rows,list) else []) if w.get('kind')=='platform' and not w.get('parent_id')),''))" 2>/dev/null || echo "")
      if [ -n "$CONCIERGE_ID" ]; then
        CST=$(tenant_call GET "/workspaces/$CONCIERGE_ID" 2>/dev/null | python3 -c "import json,sys
try: print(json.load(sys.stdin).get('status',''))
except Exception: print('')" 2>/dev/null || echo "")
        [ "$CST" = "online" ] && break
      fi
      sleep 8
    done
    HAS_TOOL=no
    if [ -n "$CONCIERGE_ID" ]; then
      HAS_TOOL=$(tenant_call POST "/workspaces/$CONCIERGE_ID/mcp" -H "Content-Type: application/json" \
        -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' 2>/dev/null | python3 -c "
import sys,json
try: d=json.load(sys.stdin)
except Exception: print('no'); sys.exit(0)
tools=(d.get('result') or {}).get('tools',[]) if isinstance(d,dict) else []
names={t.get('name','') for t in tools if isinstance(t,dict)}
print('yes' if any(n=='create_workspace' or n.endswith('create_workspace') for n in names) else 'no')" 2>/dev/null || echo no)
    fi
    if [ "$HAS_TOOL" != "yes" ]; then
      log "    concierge MCP create_workspace not available on this stack (image/model) — skipping the child-create (documented self-host caveat)."
    else
      ok "    concierge online + advertises create_workspace; sending the A2A create request…"
      AGENT_PROMPT="Create a new workspace in this org now using your platform tools. Use the create_workspace tool with name exactly ${WORKER_NAME} and role engineer. Do not ask clarifying questions. Reply with the new workspace id."
      export AGENT_PROMPT
      A2A_PAYLOAD=$(python3 -c "
import json,os,uuid
print(json.dumps({'jsonrpc':'2.0','method':'message/send','id':'clv-1','params':{'message':{'role':'user','messageId':f'e2e-{uuid.uuid4().hex[:8]}','parts':[{'kind':'text','text':os.environ['AGENT_PROMPT']}]}}}))")
      A2A_TMP="$TMPDIR_E2E/a2a"; : >"$A2A_TMP"
      set +e
      tenant_call POST "/workspaces/$CONCIERGE_ID/a2a" --max-time "$AGENT_ACT_SECS" \
        -H "Content-Type: application/json" -d "$A2A_PAYLOAD" -o "$A2A_TMP" >/dev/null 2>&1
      set -e
      # Assert the deterministic side effect: the named workspace now exists.
      ADL=$(( $(date +%s) + AGENT_ACT_SECS ))
      while [ "$(date +%s)" -lt "$ADL" ]; do
        WORKER_ID=$(tenant_call GET /workspaces 2>/dev/null | python3 -c "import json,sys,os
want=os.environ['WORKER_NAME']
try: rows=json.load(sys.stdin)
except Exception: rows=[]
print(next((w.get('id','') for w in (rows if isinstance(rows,list) else []) if w.get('name')==want),''))" 2>/dev/null || echo "")
        [ -n "$WORKER_ID" ] && break
        sleep 6
      done
      if [ -n "$WORKER_ID" ]; then
        ok "    MCP side effect confirmed: concierge created workspace '$WORKER_NAME' (id=$WORKER_ID) — now part of the run footprint."
        RF_WORKER_IDS="$WORKER_ID"; export RF_WORKER_IDS
      else
        log "    concierge replied but no '$WORKER_NAME' appeared — proceeding with the org-level footprint (teardown still verified)."
      fi
    fi
  fi
else
  log "2/5 E2E_DRIVE_CONCIERGE=0 — skipping the MCP drive; verifying the org-level teardown footprint."
fi

# ─── 3. CAPTURE the full footprint (so even unlabelled volumes are checked) ─────
log "3/5 Capturing the run footprint (containers/networks/volumes by org-id; volume NAMES pre-teardown)…"
RF_CAPTURED_VOLS=$(rf_capture_volumes "$ORG_ID" 2>/dev/null | sort -u | tr '\n' ' ')
export RF_CAPTURED_VOLS
if rf_have_docker; then
  CN=$(docker ps -aq --filter "label=molecule.org-id=$ORG_ID" 2>/dev/null | wc -l | tr -d ' ')
  log "    captured: $CN container(s) labelled molecule.org-id=$ORG_ID; volumes=[$RF_CAPTURED_VOLS]; network=$NETWORK_NAME"
  [ "$CN" = "0" ] && log "    (note: 0 containers labelled for this org — tenant may run on a different CP; VERIFY still checks db/derived-name)"
fi

# ─── 4. DEPROVISION via the product's OWN teardown (CP admin tenant delete) ─────
log "4/5 Deprovisioning via the product's OWN teardown (CP admin API)…"
cp_teardown
ok "CP teardown invoked"

# Optional: PROVE the VERIFY bites — inject ONE tagged resource the teardown 'missed'.
if [ -n "$SIMULATE_LEAK" ] && rf_have_docker; then
  log "    E2E_SIMULATE_LEAK=$SIMULATE_LEAK — injecting one resource bearing the run AFTER teardown to prove VERIFY catches it…"
  case "$SIMULATE_LEAK" in
    container)
      docker run -d --name "mol-ws-leak${SLUG##*-}" \
        --label "molecule.org-id=$ORG_ID" --label "molecule.e2e-run-id=$RUN_ID" \
        --label molecule.local-tenant=1 alpine sleep 600 >/dev/null 2>&1 || true ;;
    volume)
      docker volume create --label "molecule.org-id=$ORG_ID" --label "molecule.e2e-run-id=$RUN_ID" \
        "mol-ws-cfg-leak${SLUG##*-}" >/dev/null 2>&1 || true
      RF_CAPTURED_VOLS="$RF_CAPTURED_VOLS mol-ws-cfg-leak${SLUG##*-}"; export RF_CAPTURED_VOLS ;;
    network)
      docker network create --label "molecule.org-id=$ORG_ID" --label "molecule.e2e-run-id=$RUN_ID" \
        "mol-leak${SLUG##*-}" >/dev/null 2>&1 || true ;;
  esac
fi

# ─── 5. VERIFY (REQUIRED): zero resources may still bear the run ────────────────
log "5/5 VERIFY — zero-leftover assertion across all resource classes…"
if ! rf_verify "$ORG_ID" "$RUN_ID" "$NETWORK_NAME" "$SLUG"; then
  fail "ZERO-LEFTOVER VERIFY FAILED — the CP/MCP teardown leaked the resource(s) named above. This is a product teardown gap (the reaper would have hidden it). Failing the gate."
fi
ok "═══ CLEANUP-VERIFICATION PASSED ═══  the CP/MCP teardown removed the ENTIRE run footprint; the reaper stays a true backstop."
# finalize (EXIT trap) runs the FINALLY safety belt — a no-op on a clean run.
