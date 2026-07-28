#!/usr/bin/env bash
# template-asset-delivery — the PR-BUILD merge gate for RFC #2843 / #206 / #3478.
#
# Provisions a FRESH seo-agent workspace THROUGH the harness tenant (which runs
# workspace-server/Dockerfile.tenant built FROM THE PR under test, as a real
# molecules-server tenant: MOLECULE_ORG_ID set → CPProvisioner, docker-less)
# and asserts the tenant SERVES BACK the FULL rendered /configs bundle via the
# Files API — the #206 host-side /configs mirror read-back.
#
# WHY (root-fix of the deploy-ordering deadlock): the old gate provisioned
# against DEPLOYED staging and asserted config.yaml, so a delivery-surface PR
# (e.g. the #206 read-back fix) could NEVER green its own gate until it was
# deployed — but deploying needed the merge the red gate blocked. This gate
# validates the PR's OWN freshly-built image instead, with no deployed
# dependency and without touching any staging org or pin.
#
# Assertions (merge-blocking — a delivery regression FAILS the PR):
#   C. config.yaml SERVED + REAL (>1 KiB, contains the rendered runtime/model
#      keys) from the host-side mirror — NOT the 59-byte "container offline, no
#      template" stub the pre-#3478 docker-less read-back returned.
#   D. prompts/ SERVED (the identity prompt) from the same mirror.
#
# Empirically, on the #3478 image, seo-agent serves config.yaml=9316B +
# prompts=[seo-agent.md]; on a pre-#3478 image the read-back 500s / returns the
# 59B stub → best size <=1 KiB → this gate FAILS (proven discriminating).
#
# The POST-ONLINE plugin channel (seo-all reconcile) needs a live runtime the
# harness cp-stub never brings online; it lives in template-delivery-e2e-staging.yml.
#
# Exit: 0 = both channels served real bytes | 1 = delivery regression.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HARNESS_ROOT="$HERE"
cd "$HARNESS_ROOT"
# shellcheck source=_curl.sh
source "$HARNESS_ROOT/_curl.sh"

TEMPLATE="${DELIVERY_TEMPLATE:-seo-agent}"
RUNTIME="${DELIVERY_RUNTIME:-claude-code}"
MODEL="${DELIVERY_MODEL:-moonshot/kimi-k2.6}"
# config.yaml minimum size that proves the FULL bundle was served (not the ~59B
# stub / an error envelope). seo-agent's rendered config.yaml is ~9.3 KiB; 1 KiB
# is a generous floor that the stub can never reach.
MIN_CONFIG_BYTES="${DELIVERY_MIN_CONFIG_BYTES:-1024}"
# The mirror is written during the async provision (CPProvisioner.Start), so the
# read-back becomes real within a couple of seconds of create. Poll the REAL
# signal (served bytes), never a fixed sleep. Generous ceiling for a cold box.
DEADLINE_SECS="${DELIVERY_READBACK_TIMEOUT_SECS:-150}"

log()  { echo "[$(date +%H:%M:%S)] $*"; }
fail() { echo "[$(date +%H:%M:%S)] FAIL: $*" >&2; exit 1; }
ok()   { echo "[$(date +%H:%M:%S)] PASS: $*"; }

RESP_FILE="$(mktemp)"
BEST_FILE="$(mktemp)"
cleanup() { rm -f "$RESP_FILE" "$BEST_FILE" /tmp/tmpl_c2.out; }
trap cleanup EXIT

log "=== template-asset-delivery gate — tenant image = PR build; template=$TEMPLATE ==="

# ─── 0. satisfy the template's OWN required_env before provisioning ──────────
#
# The seo-agent template declares `runtime_config.required_env` (TENANT_NAME,
# TENANT_DOMAIN, TENANT_DOMAIN_APEX, TENANT_DOMAIN_FULL, TENANT_TIMEZONE).
# Preflight #5 (workspace_provision_shared.go) ABORTS the provision when any of
# them is unset, so cpProv.Start never runs and
# PersistConfigBundleHostSide (cp_provisioner.go) never writes the host-side
# /configs mirror — the Files API then 404s and this gate reported
# "config.yaml served only 0B ... delivery REGRESSION", which is a TRUE symptom
# attached to the WRONG cause. There was no delivery regression: the workspace
# never got as far as delivering.
#
# The self-host escape hatch that fills these with placeholders
# (applySelfHostTenantDefaults) is gated on !PlatformManagedProxyConfigured(),
# and the harness tenant sets MOLECULE_LLM_BASE_URL + MOLECULE_LLM_USAGE_TOKEN
# to satisfy assertManagedTenantHasLLMEnv at boot — so the harness is a
# PLATFORM-MANAGED tenant and must supply tenant identity itself, exactly as a
# real operator does.
#
# This was invisible until now because the lane is PATH-GATED and had been
# emitting a no-op pass without executing; the template gained required_env
# while the gate was never actually running.
#
# DERIVED, not hardcoded: the list is read from the template's own config.yaml
# (the manifest-pinned clone the workflow's "Pre-clone manifest deps" step
# produces), so the next required var a template author adds is satisfied
# automatically instead of re-breaking this gate. Values are obvious harness
# fixtures — this gate asserts DELIVERY, not SEO behaviour.
REPO_ROOT="$(cd "$HARNESS_ROOT/../.." && pwd)"
TEMPLATE_CONFIG="${DELIVERY_TEMPLATE_CONFIG:-$REPO_ROOT/.tenant-bundle-deps/workspace-configs-templates/$TEMPLATE/config.yaml}"

seed_required_env() {
  if [ ! -f "$TEMPLATE_CONFIG" ]; then
    log "  WARN: $TEMPLATE_CONFIG absent — cannot derive required_env; a provision abort will be reported below if the template needs any"
    return 0
  fi
  local names
  names="$(python3 -c '
import sys, yaml
try:
    d = yaml.safe_load(open(sys.argv[1])) or {}
except Exception as e:
    print("", end=""); sys.exit(0)
req = ((d.get("runtime_config") or {}).get("required_env") or [])
print("\n".join(n for n in req if isinstance(n, str) and n.strip()))
' "$TEMPLATE_CONFIG" 2>/dev/null)"
  if [ -z "$names" ]; then
    log "  template declares no runtime_config.required_env — nothing to seed"
    return 0
  fi
  local n seeded=0
  while IFS= read -r n; do
    [ -n "$n" ] || continue
    # A global secret is the operator-facing channel the preflight's own error
    # message names ("or as Global secrets"), and it lands in envVars for every
    # workspace this tenant provisions.
    curl_alpha_admin -X POST "$BASE/admin/secrets" \
      -d "{\"key\":\"$n\",\"value\":\"harness-$(echo "$n" | tr '[:upper:]_' '[:lower:]-')\"}" \
      >/dev/null 2>&1 || log "  WARN: could not seed $n"
    seeded=$((seeded+1))
  done <<< "$names"
  log "  seeded $seeded required_env value(s) from $TEMPLATE_CONFIG: $(echo "$names" | tr '\n' ' ')"
}
seed_required_env

# provision_failure prints WHY the workspace never delivered, so a provision
# abort can never again be misread as a delivery regression. Reads the DB
# columns markProvisionFailed writes (status + last_sample_error) rather than
# trusting an API field to be exposed.
provision_failure() {
  local wid="$1"
  echo "--- workspace $wid provision state ---" >&2
  psql_exec_alpha -c "SELECT status, coalesce(last_sample_error,'(none)') FROM workspaces WHERE id = '$wid'" 2>&1 | head -5 >&2 || true
}

# ─── 1. provision a fresh workspace THROUGH the tenant ───────────────────────
BODY="{\"name\":\"tmpl-delivery-gate-$$\",\"tier\":2,\"runtime\":\"$RUNTIME\",\"template\":\"$TEMPLATE\",\"model\":\"$MODEL\"}"
log "POST /workspaces  $BODY"
RESP="$(curl_alpha_admin -X POST "$BASE/workspaces" -d "$BODY")"
WID="$(printf '%s' "$RESP" | python3 -c 'import sys,json
try: print(json.load(sys.stdin).get("id",""))
except Exception: print("")')"
[ -n "$WID" ] || fail "create returned no workspace id — provision rejected. body: $(printf '%s' "$RESP" | head -c 400)"
log "workspace id=$WID — polling Files API /configs read-back (<=${DEADLINE_SECS}s)"

# ─── 2. C: poll config.yaml read-back until the FULL bundle is served ─────────
# GET /workspaces/:id/files/config.yaml (root defaults to /configs → the #206
# host-side mirror). Success envelope is {path,content,size}; an error/stub
# yields a small or error body. Keep the RAW response of the largest read so far
# in BEST_FILE and run the content assertions on the FULL JSON — never split the
# multi-line YAML content through shell `read`, which truncates it to line 1.
size_of() {  # size_of <response-file> -> integer bytes of the served config
  python3 -c 'import sys,json
try:
  d=json.load(open(sys.argv[1]))
  c=d.get("content","")
  print(int(d.get("size", len(c) if isinstance(c,str) else 0)))
except Exception:
  print(0)' "$1"
}

deadline=$(( $(date +%s) + DEADLINE_SECS ))
best=0; attempt=0
: > "$BEST_FILE"
while [ "$(date +%s)" -lt "$deadline" ]; do
  attempt=$((attempt+1))
  curl_alpha_admin "$BASE/workspaces/$WID/files/config.yaml" > "$RESP_FILE" 2>/dev/null || true
  size="$(size_of "$RESP_FILE")"
  size="${size:-0}"
  if [ "$size" -gt "$best" ] 2>/dev/null; then best="$size"; cp "$RESP_FILE" "$BEST_FILE"; fi
  log "  attempt=$attempt config.yaml served size=$size (best=$best)"
  [ "$size" -gt "$MIN_CONFIG_BYTES" ] 2>/dev/null && break
  sleep 5
done

# C.1 — size floor (the load-bearing #206 assertion). Print the provision state
# FIRST: a 0B read-back has two very different causes — delivery regressed, or
# the provision aborted and there was nothing to deliver — and the gate must
# not name the wrong one.
if ! [ "$best" -gt "$MIN_CONFIG_BYTES" ] 2>/dev/null; then
  provision_failure "$WID"
  fail "C: config.yaml served only ${best}B (<= ${MIN_CONFIG_BYTES}B) after ${DEADLINE_SECS}s. Read the workspace provision state printed above BEFORE concluding delivery regressed: status=failed with a last_sample_error means the provision aborted (e.g. missing required_env) and never reached the mirror; status=online/provisioning with no error means the #206 host-side mirror genuinely is not being served (delivery REGRESSION)."
fi

# C.2 — the served bytes are the REAL rendered config, not a stub message. Parse
# the FULL JSON content in python (handles the multi-line YAML body correctly).
python3 -c '
import sys, json
d = json.load(open(sys.argv[1]))
c = d.get("content", "")
if not isinstance(c, str):
    print("content-not-a-string"); sys.exit(3)
low = c.lower()
if ("container offline" in low) or ("no template" in low) or ("file not found" in low):
    print("stub-or-error-text: " + c[:160]); sys.exit(4)
missing = [k for k in ("name:", "runtime:", "model:") if k not in c]
if missing:
    print("missing-keys " + ",".join(missing) + " head=" + c[:160]); sys.exit(5)
print("ok")
' "$BEST_FILE" >/tmp/tmpl_c2.out 2>&1 || \
  fail "C: config.yaml served ${best}B but is NOT a real rendered config bundle: $(head -c 200 /tmp/tmpl_c2.out 2>/dev/null)"
ok "C: config.yaml SERVED real + full from the #206 mirror (${best}B, has name:/runtime:/model:)"

# ─── 3. D: prompts/ served from the same mirror ──────────────────────────────
PROMPTS="$(curl_alpha_admin "$BASE/workspaces/$WID/files?path=prompts" 2>/dev/null)"
PCOUNT="$(printf '%s' "$PROMPTS" | python3 -c 'import sys,json
try:
  d=json.load(sys.stdin); print(len(d) if isinstance(d,list) else 0)
except Exception: print(0)')"
[ "${PCOUNT:-0}" -gt 0 ] 2>/dev/null || \
  fail "D: prompts/ served EMPTY ([]) — identity prompt NOT delivered via the asset channel. body: $(printf '%s' "$PROMPTS" | head -c 200)"
ok "D: prompts/ SERVED from the mirror (${PCOUNT} file(s): $(printf '%s' "$PROMPTS" | head -c 160))"

echo ""
ok "template-asset-delivery gate GREEN — PR's OWN tenant image serves the full /configs bundle (config.yaml=${best}B + prompts=${PCOUNT}) via the #206 docker-less read-back. tenant=alpha ws=$WID"
exit 0
