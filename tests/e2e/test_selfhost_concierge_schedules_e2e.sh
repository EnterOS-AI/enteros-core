#!/usr/bin/env bash
# HERMETIC self-host concierge default-schedules e2e (closes core#4555).
#
# WHAT THIS PROVES — the LIVE graft->deliver->seed->grid loop, end to end:
#   The self-host concierge (kind='platform' root, seeded UNCONDITIONALLY on a
#   MOLECULE_ORG_ID-unset boot) ships two default schedules
#   (daily-activity-report + plugin-auto-update). Those live in the platform-agent
#   template's `schedules:` node and are grafted onto the concierge's delivered
#   /configs/config.yaml ONLY on self-host (graftConciergeSchedules, self-host
#   gated: MOLECULE_ORG_ID unset). The unit tests
#   (concierge_default_schedules_payload_test.go) already prove the COMPOSE
#   function; #4555 is the LIVE loop this closes: real platform boot -> concierge
#   seed -> local-Docker provision onto the stub runtime -> composed config.yaml
#   delivered to the /configs volume -> stub seeds the grid -> core reads it via
#   GET /workspaces/:id/schedules.
#
# TWO ASSERTIONS, both HARD, most-specific first:
#   A. DELIVERY  — docker exec cat /configs/config.yaml carries BOTH default
#      schedule names. This is the direct proof the self-host graft fired and the
#      composed config reached the volume the runtime reads. Deterministic,
#      dependency-free.
#   B. GRID      — GET /workspaces/<concierge>/schedules returns BOTH by name.
#      Strictly stronger: it also proves the core->runtime schedules proxy
#      contract end-to-end (schedules_proxy -> stub GET /internal/schedules ->
#      volumeEntry shape). Retries while the runtime is still coming up; only an
#      empty-but-reachable grid is a real failure.
#
# NEGATIVE CONTROL (why this can't false-green): the ONLY reason both schedules
# appear is that graftConciergeSchedules ran, and it runs ONLY when
# SelfHostPlatformSeedEnabled() is true (MOLECULE_ORG_ID unset). In SaaS mode
# (MOLECULE_ORG_ID set) the SAME platform-agent template yields grafted=false ->
# zero schedules -> both assertions FAIL. An empty template schedules node yields
# the same. (The SaaS negative at compose level is pinned by the Go unit test
# TestConciergeDefaultSchedules_SaaSSeedsNeither; this lane runs the positive
# self-host loop live.) The count check (exactly the 2 defaults) means the assert
# cannot be satisfied by unrelated noise.
#
# BOOT CONTRACT (wired by the workflow — see selfhost-concierge-schedules-e2e.yml):
#   * MOLECULE_ORG_ID   UNSET  -> self-host seed + local Docker provisioner + the
#                                 graft gate. THE WHOLE POINT.
#   * MOLECULE_DEFAULT_RUNTIME=claude-code -> concierge seeded runtime=claude-code
#                                 so it resolves to the pre-tagged stub image
#                                 (RuntimeImages["claude-code"]) and its base
#                                 template is "claude-code-default".
#   * MOLECULE_LLM_DEFAULT_MODEL=claude-opus-4-7 -> ONLY gates the boot-time
#                                 auto-provision's eligibility
#                                 (platformAgentModelConfigured=true). It does NOT
#                                 feed the LLM router: the router derives the
#                                 provider from the EFFECTIVE model
#                                 (effectiveModelForBilling -> payload.Model, else
#                                 the MODEL/MOLECULE_MODEL workspace_secret;
#                                 workspace_provision.go:1468,1255), and
#                                 MOLECULE_LLM_DEFAULT_MODEL is only a LAST-resort
#                                 seed inside ensureConciergeModel, which runs
#                                 AFTER the router. Step 4 therefore sets the
#                                 concierge's ACTUAL routing model — the MODEL
#                                 workspace_secret = claude-opus-4-7 (a BARE
#                                 anthropic id => provider=anthropic-api => BYOK,
#                                 NOT the closed platform arm) — via the ensure
#                                 endpoint BEFORE the (re)provision.
#   * CONFIGS_DIR = $E2E_TEMPLATES_DIR -> where this script writes the two template
#                                 fixtures (claude-code-default + platform-agent).
#
# The stub carries no LLM; a dummy global ANTHROPIC_API_KEY satisfies the BYOK
# credential gate (global scope) and no real model call ever happens.
#
# Env contract:
#   BASE                 default http://localhost:8080
#   MOLECULE_ADMIN_TOKEN required admin bearer (matches ADMIN_TOKEN on the platform)
#   E2E_TEMPLATES_DIR    required; == the platform's CONFIGS_DIR (template root)
#   E2E_WS_MANIFEST      optional; concierge id is appended for run-scoped teardown
#   ONLINE_TIMEOUT       default 300 (concierge provision->online budget)
#   GRID_TIMEOUT         default 120 (schedules-proxy readiness budget)
#   CACHE_TAG            optional pin of the provisioner cache tag (else Gitea sha)
#
# Exit codes: 0 pass; 1 assertion failure; 2 environment/preflight error.
set -euo pipefail

: "${BASE:=http://localhost:8080}"
export BASE
# shellcheck disable=SC1091
# shellcheck source=_lib.sh
source "$(dirname "$0")/_lib.sh"

ADMIN_TOKEN="${ADMIN_TOKEN:-${MOLECULE_ADMIN_TOKEN:-}}"
export ADMIN_TOKEN MOLECULE_ADMIN_TOKEN="${ADMIN_TOKEN}"

ONLINE_TIMEOUT="${ONLINE_TIMEOUT:-300}"
GRID_TIMEOUT="${GRID_TIMEOUT:-120}"
RUNTIME="claude-code"
STUB_DIR="$(cd "$(dirname "$0")/stub-runtime" && pwd)"
CACHE_REPO="molecule-local/workspace-template-${RUNTIME}"
GITEA_BRANCH_API="${GITEA_BRANCH_API:-https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-ai-workspace-template-${RUNTIME}/branches/main}"
LATEST_TAG="${CACHE_REPO}:latest"
CACHE_TAG="${CACHE_TAG:-}"
ORIG_CACHE_IMAGE_ID=""

# The two default schedules the self-host concierge ships. This block is a
# VERBATIM copy of the platform-agent template's config.yaml `schedules:` node —
# the SAME content pinned by concierge_default_schedules_payload_test.go's
# conciergeRealDefaultSchedulesBlock. If the template's defaults change, update
# BOTH in lockstep (that is the point of pinning them).
DEFAULT_SCHEDULE_1="daily-activity-report"
DEFAULT_SCHEDULE_2="plugin-auto-update"

log()  { echo "[$(date +%H:%M:%S)] $*"; }
pass() { echo "[$(date +%H:%M:%S)] PASS: $*"; PASS=$((PASS + 1)); }
fail() { echo "[$(date +%H:%M:%S)] FAIL: $*" >&2; FAIL=$((FAIL + 1)); }
die()  { echo "[$(date +%H:%M:%S)] ERROR: $*" >&2; exit 2; }
PASS=0
FAIL=0

admin_curl() { local _a=(); e2e_admin_auth_args _a; curl -s "${_a[@]+"${_a[@]}"}" "$@"; }

_container_name() {  # canonical ws container name for an id
  if command -v e2e-names >/dev/null 2>&1; then e2e-names container "$1"; else echo "ws-$1"; fi
}

cleanup() {
  local rc=$?
  # Restore the provisioner cache tag to whatever it pointed at before, so a stub
  # run never leaves the real claude-code tag aliased to the lightweight stub.
  if [ -n "$ORIG_CACHE_IMAGE_ID" ] && [ -n "$CACHE_TAG" ]; then
    docker tag "$ORIG_CACHE_IMAGE_ID" "$CACHE_TAG" >/dev/null 2>&1 || true
  fi
  exit $rc
}
trap cleanup EXIT INT TERM

e2e_require_admin_token || die "admin token required (AdminAuth fails closed)"
[ -n "${E2E_TEMPLATES_DIR:-}" ] || die "E2E_TEMPLATES_DIR must be set (== the platform's CONFIGS_DIR)"

echo "=== Self-host concierge default-schedules E2E (closes #4555) ==="
echo "BASE=$BASE  runtime=$RUNTIME  templates_dir=$E2E_TEMPLATES_DIR"
echo ""

# Preflight.
docker info >/dev/null 2>&1 || die "docker daemon not reachable — this test provisions a local container"
curl -s -m 5 "$BASE/health" >/dev/null 2>&1 || die "platform not reachable at $BASE/health"

# ---------------------------------------------------------------------------
# Step 1 — deliver the two template fixtures into the platform's template root.
# ---------------------------------------------------------------------------
# The graft reads the platform-agent template's `schedules:` node;
# composeConciergeRuntimeConfig reads the claude-code base template
# ("claude-code-default"). Core ships NEITHER in-repo (templates live in separate
# repos, fetched into a cache at runtime), so this hermetic lane supplies both.
echo "--- Step 1: deliver template fixtures ---"
mkdir -p "$E2E_TEMPLATES_DIR/claude-code-default" \
         "$E2E_TEMPLATES_DIR/platform-agent/prompts"

cat > "$E2E_TEMPLATES_DIR/claude-code-default/config.yaml" <<'YAML'
name: Claude Code Default
runtime: claude-code
prompt_files:
- system-prompt.md
runtime_config:
  model: claude-opus-4-7
  required_env: [ANTHROPIC_API_KEY]
YAML

# platform-agent/config.yaml — the graft source. Its `schedules:` node is the two
# real defaults, VERBATIM (see conciergeRealDefaultSchedulesBlock).
cat > "$E2E_TEMPLATES_DIR/platform-agent/config.yaml" <<'YAML'
name: Org Concierge
runtime: claude-code
prompt_files:
- system-prompt.md
runtime_config:
  model: claude-opus-4-7
  required_env: [ANTHROPIC_API_KEY]
schedules:
  - name: daily-activity-report
    cron: "0 9 * * *"
    timezone: UTC
    enabled: true
    prompt: "Every morning, report what happened across this deployment in the last 24 hours. Retrieve recent activity (e.g. GET /workspaces/<your id>/activity?since_secs=86400, and /mail/summary if available), write a concise 'Yesterday's report' covering agents/tasks active, work completed, notable events, and any errors, then deliver it to the user with the send_message_to_user tool. If nothing of note happened, send a brief 'quiet day' note."

  - name: plugin-auto-update
    cron: "0 3 * * *"
    timezone: UTC
    enabled: true
    prompt: "Keep this self-hosted deployment up to date. Use check_plugin_updates to list plugins with a newer version available, and for each, use apply_plugin_update to apply it (re-pins and restarts the affected workspace). Also check whether a newer core or runtime version is available — you CANNOT apply those (operator deploy needed), so only report them. Then send the user (send_message_to_user) an audit: which plugins you auto-updated (name old->new) and any core/runtime updates available to deploy. If the update tools are not available yet, just report that update tooling is not yet installed and do nothing else."
YAML

# The concierge persona (resolveConciergePersonaBytes reads it; claude-code lands
# it at system-prompt.md). Content is immaterial to the schedules assertion.
printf '# You are %s\n\nYou are the org concierge.\n' '{{CONCIERGE_NAME}}' \
  > "$E2E_TEMPLATES_DIR/platform-agent/prompts/concierge.md"

pass "delivered claude-code-default + platform-agent templates (with the 2 default schedules)"
echo ""

# ---------------------------------------------------------------------------
# Step 2 — build the stub image and pre-tag it to the provisioner cache tag.
# ---------------------------------------------------------------------------
# runtime=claude-code resolves via RegistryModeLocal to
# molecule-local/workspace-template-claude-code:<gitea-HEAD-sha12>-<arch>. The
# concierge (kind=platform) resolves its image through the SAME per-runtime path
# (selectImage -> RuntimeImages["claude-code"]), so pre-tagging the stub to that
# cache tag makes the concierge land on the stub with no 2.5GB template build.
echo "--- Step 2: build + tag stub to the provisioner cache tag ---"
if [ -z "$CACHE_TAG" ]; then
  CACHE_SHA=$(curl -s -m 10 "$GITEA_BRANCH_API" 2>/dev/null \
    | python3 -c "import sys,json
try: print(json.load(sys.stdin)['commit']['id'][:12])
except Exception: print('')" 2>/dev/null)
  [ -n "$CACHE_SHA" ] || die "could not resolve template HEAD sha from $GITEA_BRANCH_API — set CACHE_TAG explicitly"
  CACHE_ARCH_SUFFIX="${MOLECULE_IMAGE_PLATFORM:-}"
  if [ -n "$CACHE_ARCH_SUFFIX" ]; then
    CACHE_ARCH_SUFFIX="${CACHE_ARCH_SUFFIX#*/}"; CACHE_ARCH_SUFFIX="${CACHE_ARCH_SUFFIX//\//-}"
  else
    CACHE_ARCH_SUFFIX="$(go env GOARCH)"
  fi
  CACHE_TAG="${CACHE_REPO}:${CACHE_SHA}-${CACHE_ARCH_SUFFIX}"
fi
log "provisioner cache tag: $CACHE_TAG"
ORIG_CACHE_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$CACHE_TAG" 2>/dev/null || true)"

if ! docker build --platform=linux/amd64 -t molecule-local/stub-runtime:latest "$STUB_DIR" >/tmp/stub_sched_build.log 2>&1; then
  echo "stub image build failed:"; tail -25 /tmp/stub_sched_build.log; die "stub build failed"
fi
docker tag molecule-local/stub-runtime:latest "$CACHE_TAG"
docker tag molecule-local/stub-runtime:latest "$LATEST_TAG"
pass "stub built + tagged -> $CACHE_TAG (+ :latest)"
echo ""

# ---------------------------------------------------------------------------
# Step 3 — discover the self-host-seeded concierge (kind='platform' root).
# ---------------------------------------------------------------------------
echo "--- Step 3: discover the concierge (kind='platform' root) ---"
find_concierge() {
  admin_curl --max-time 15 "$BASE/workspaces" | python3 -c "
import sys, json
try: rows = json.load(sys.stdin)
except Exception: print(''); sys.exit(0)
for w in rows if isinstance(rows, list) else []:
    if w.get('kind') == 'platform' and not w.get('parent_id'):
        print(w.get('id','')); break
else:
    print('')"
}
CONCIERGE_ID=""
DISCOVER_DEADLINE=$(( $(date +%s) + 60 ))
while true; do
  CONCIERGE_ID=$(find_concierge)
  [ -n "$CONCIERGE_ID" ] && break
  [ "$(date +%s)" -gt "$DISCOVER_DEADLINE" ] && \
    die "no kind='platform' concierge seeded — EnsureSelfHostedPlatformAgent should create it on a MOLECULE_ORG_ID-unset boot"
  sleep 2
done
pass "concierge (platform root) = $CONCIERGE_ID"
# Record for run-scoped teardown (the workflow removes ws-<id> by exact name).
if [ -n "${E2E_WS_MANIFEST:-}" ]; then echo "$CONCIERGE_ID" >> "$E2E_WS_MANIFEST" || true; fi
echo ""

# ---------------------------------------------------------------------------
# Step 4 — set the concierge's MODEL + a BYOK credential, then reprovision.
# ---------------------------------------------------------------------------
# The first live run (run 555247) died here. The boot-time auto-provision
# (MaybeProvisionPlatformAgentOnBoot — fired because MOLECULE_LLM_DEFAULT_MODEL is
# set => platformAgentModelConfigured=true) reached the LLM router with NO
# resolvable model and aborted:
#   workspace_provision: llm routing ... provider="" resolved="" route_to_platform=false
#   Provisioner: ABORT — BYOK workspace has no usable LLM credential (MISSING_BYOK_CREDENTIAL)
# Root cause: the router derives the provider from the EFFECTIVE model
# (effectiveModelForBilling -> payload.Model, else the MODEL/MOLECULE_MODEL
# workspace_secret; workspace_provision.go:1468,1255). On a concierge RESTART
# payload.Model is "", and there was NO MODEL workspace_secret, so effectiveModel=""
# => provider="" => the byok-with-no-cred abort. MOLECULE_LLM_DEFAULT_MODEL does NOT
# feed the router — it only gates boot eligibility and is a last-resort seed inside
# ensureConciergeModel, which runs in applyConciergeProvisionConfig AFTER the router
# (workspace_provision_shared.go:211 router vs :281 concierge config). Because the
# router aborted first, ensureConciergeModel never ran, the MODEL secret was never
# seeded, and every later restart re-saw provider="".
#
# The fix mirrors the proven lifecycle-stub lane
# (test_local_provision_lifecycle_e2e.sh): the workspace runs on a BARE anthropic id
# (claude-opus-4-7 => provider anthropic-api => BYOK, NOT the closed platform arm;
# providers_test.go:106) plus a dummy ANTHROPIC_API_KEY so byok resolves. Two
# concierge-specific twists vs the lifecycle lane:
#   * MODEL — the concierge is the org root, so a per-workspace secret write is
#     refused (conciergeSelfSecretWriteBlocked, core#2566). We instead set the MODEL
#     via the canonical onboarding path POST /admin/org/platform-agent/ensure {model}
#     (EnsurePlatformAgent -> ensurePlatformAgentFlow). That flow persists the MODEL
#     workspace_secret via setModelSecret BEFORE it triggers the provision — the
#     core#3496 ordering that exists precisely to beat this race — and reprovisions
#     via RestartByIDAfterMutation (bypasses RestartByID's self-fire debounce).
#     force:true makes it deterministically repair+reprovision regardless of the
#     failed/offline state the aborted boot-provision left behind (this is the
#     "reset status + reprovision after writing model+key" boot-race handling).
#   * KEY — set a GLOBAL ANTHROPIC_API_KEY: loadWorkspaceSecrets loads global_secrets
#     into the provision env and hasAnyPlatformManagedLLMKey accepts a
#     provider-matching cred at global OR workspace scope (workspace_provision.go:1695),
#     and SetGlobal has no bypass-key rejection on a self-host stack (no platform
#     proxy wired). Set it BEFORE the ensure call so the SINGLE reprovision the ensure
#     triggers already sees model + key together.
# The stub makes no LLM call, so the dummy key value only needs to exist.
echo "--- Step 4: set concierge model + global BYOK key, then reprovision ---"
SET_CODE=$(admin_curl -o /dev/null -w '%{http_code}' -X PUT "$BASE/settings/secrets" \
  -H "Content-Type: application/json" \
  -d '{"key":"ANTHROPIC_API_KEY","value":"sk-ant-e2e-selfhost-sched-dummy-not-a-real-key"}' || echo 000)
[ "$SET_CODE" -ge 200 ] && [ "$SET_CODE" -lt 300 ] || die "PUT /settings/secrets ANTHROPIC_API_KEY failed (http=$SET_CODE)"
pass "global ANTHROPIC_API_KEY set (http=$SET_CODE)"

# One canonical call writes the concierge's MODEL and reprovisions. The ensure flow
# validates claude-opus-4-7 against the concierge's runtime (claude-code), persists
# the MODEL workspace_secret BEFORE the provision trigger, then reprovisions with
# force. The resulting single provision resolves provider=anthropic-api, finds the
# global ANTHROPIC_API_KEY (HasUsableLLMCred), and runs the concierge path
# (prepareProvisionContext -> applyConciergeProvisionConfig ->
# composeConciergeRuntimeConfig -> graftConciergeSchedules) that composes + delivers
# config.yaml with the grafted schedules onto /configs. No runtime is passed: the
# flow preserves the existing root's runtime and only uses `runtime` on a fresh
# insert.
ENSURE_CODE=$(admin_curl -o /dev/null -w '%{http_code}' -X POST "$BASE/admin/org/platform-agent/ensure" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-opus-4-7","force":true}' || echo 000)
[ "$ENSURE_CODE" -ge 200 ] && [ "$ENSURE_CODE" -lt 300 ] || die "POST /admin/org/platform-agent/ensure (model + reprovision) failed (http=$ENSURE_CODE)"
pass "concierge MODEL set + provision dispatched via ensure (http=$ENSURE_CODE)"
echo ""

# ---------------------------------------------------------------------------
# Step 5 — wait for the concierge to reach online on the stub.
# ---------------------------------------------------------------------------
echo "--- Step 5: wait for the concierge to come online (<=${ONLINE_TIMEOUT}s) ---"
ws_status() { admin_curl --max-time 15 "$BASE/workspaces/$CONCIERGE_ID" | python3 -c "
import sys, json
try: print((json.load(sys.stdin) or {}).get('status',''))
except Exception: print('')"; }
ONLINE_DEADLINE=$(( $(date +%s) + ONLINE_TIMEOUT ))
LAST_STATUS=""
while true; do
  S=$(ws_status)
  [ "$S" != "$LAST_STATUS" ] && { log "concierge -> ${S:-<none>}"; LAST_STATUS="$S"; }
  [ "$S" = "online" ] && break
  if [ "$(date +%s)" -gt "$ONLINE_DEADLINE" ]; then
    echo "--- concierge container logs (tail) ---"
    docker logs "$(_container_name "$CONCIERGE_ID")" 2>&1 | tail -n 60 || true
    die "concierge never reached online within ${ONLINE_TIMEOUT}s (last='${S}')"
  fi
  sleep 5
done
CNAME="$(_container_name "$CONCIERGE_ID")"
docker ps --format '{{.Names}}' | grep -Fxq "$CNAME" || die "concierge online but container $CNAME not running"
pass "concierge online (container $CNAME running)"
echo ""

# ---------------------------------------------------------------------------
# ASSERTION A (DELIVERY) — the graft landed both schedules in /configs/config.yaml.
# ---------------------------------------------------------------------------
echo "--- Assertion A: grafted schedules delivered to the concierge config volume ---"
CFG=$(docker exec "$CNAME" cat /configs/config.yaml 2>/dev/null || echo "")
[ -n "$CFG" ] || fail "could not read /configs/config.yaml from $CNAME"
for name in "$DEFAULT_SCHEDULE_1" "$DEFAULT_SCHEDULE_2"; do
  if echo "$CFG" | grep -qF "$name"; then
    pass "config.yaml carries default schedule '$name'"
  else
    fail "config.yaml MISSING default schedule '$name' — self-host graft did not fire / not delivered"
  fi
done
echo ""

# ---------------------------------------------------------------------------
# ASSERTION B (GRID) — core reads both schedules back through the proxy.
# ---------------------------------------------------------------------------
# GET /workspaces/:id/schedules -> listVolume -> proxy to the stub's
# GET /internal/schedules -> {"schedules":[volumeEntry]}. Retry while the runtime
# is still coming up (a 5xx/000 from a not-yet-ready proxy is transient); an
# empty-but-reachable grid, or a grid missing a default, is a REAL failure.
echo "--- Assertion B: schedules grid via GET /workspaces/:id/schedules ---"
grid_names() {  # -> newline-separated schedule names, or empty on non-200
  local body code
  body=$(admin_curl --max-time 20 -w $'\n%{http_code}' "$BASE/workspaces/$CONCIERGE_ID/schedules" 2>/dev/null || printf '\n000')
  code="${body##*$'\n'}"; body="${body%$'\n'*}"
  [ "$code" = "200" ] || { echo "__HTTP_${code}__"; return; }
  echo "$body" | python3 -c "
import sys, json
try: rows = json.load(sys.stdin)
except Exception: sys.exit(0)
for r in rows if isinstance(rows, list) else []:
    n = r.get('name') if isinstance(r, dict) else None
    if n: print(n)"
}
GRID_DEADLINE=$(( $(date +%s) + GRID_TIMEOUT ))
NAMES=""
while true; do
  NAMES=$(grid_names)
  case "$NAMES" in
    __HTTP_*__)
      # proxy/runtime not ready yet — keep polling until the deadline.
      if [ "$(date +%s)" -gt "$GRID_DEADLINE" ]; then
        fail "schedules grid never returned 200 within ${GRID_TIMEOUT}s (last=${NAMES}) — core<->runtime proxy leg unreachable"
        break
      fi
      sleep 5; continue ;;
  esac
  # 200 with a body: this is authoritative (reachable). Assert content now.
  break
done

if [ "${NAMES#__HTTP_}" = "$NAMES" ]; then  # got a real 200 grid
  GRID_COUNT=$(printf '%s\n' "$NAMES" | grep -c . || true)
  GRID_FLAT=$(printf '%s' "$NAMES" | tr '\n' ' ')
  for name in "$DEFAULT_SCHEDULE_1" "$DEFAULT_SCHEDULE_2"; do
    if printf '%s\n' "$NAMES" | grep -qFx "$name"; then
      pass "grid returns default schedule '$name'"
    else
      fail "grid MISSING default schedule '$name' (grid had: ${GRID_FLAT})"
    fi
  done
  # Exactly the two defaults — the assert can't be satisfied by unrelated noise.
  if [ "${GRID_COUNT:-0}" -eq 2 ]; then
    pass "grid returns exactly the 2 default schedules"
  else
    fail "grid returned ${GRID_COUNT} schedule(s), want exactly 2 (had: ${GRID_FLAT})"
  fi
fi
echo ""

# ---------------------------------------------------------------------------
echo "=== RESULT: PASS=$PASS FAIL=$FAIL ==="
if [ "$FAIL" -ne 0 ]; then
  echo "FAILED: the self-host default schedules did not flow through the live graft->deliver->grid loop." >&2
  exit 1
fi
echo "Proven live: MOLECULE_ORG_ID-unset boot -> concierge seed -> local-Docker provision onto the stub ->"
echo "self-host graft delivered BOTH default schedules to /configs/config.yaml -> core read them back via the grid."
