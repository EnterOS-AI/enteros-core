#!/usr/bin/env bash
# MANDATORY local Docker-provisioner lifecycle e2e.
#
# Why this exists: every other e2e exercises the SaaS/EC2 (control-plane)
# provisioner. NOTHING mandatory exercises the LOCAL Docker provisioner
# (MOLECULE_ENV=development, docker.sock) — the path self-hosters and dev runs
# use. A config-volume bug where a restarted workspace couldn't find its
# config.yaml (and wedged in 'failed' with "config volume is empty") went
# undetected for exactly this reason. This test provisions a REAL workspace via
# the LOCAL provisioner and asserts the full lifecycle, INCLUDING the
# restart-survival assertion that would have caught that bug.
#
# Steps (each asserts loudly):
#   1. Build + tag the stub runtime image to the provisioner's RegistryModeLocal
#      cache tag so runtime=claude-code resolves to the stub (cache-hit, no
#      2.5GB build).
#   2. POST /workspaces (runtime=claude-code) — capture id.
#   3. Poll GET /workspaces/{id} until status==online (<=90s); assert a ws-<id>
#      container is running.
#   4. RESTART-SURVIVAL: POST /workspaces/{id}/restart, poll until online AGAIN
#      (<=90s); assert the container is back and the workspace did NOT wedge in
#      failed / "config volume is empty". <-- the key assertion.
#   5. PROXY REACH: POST an A2A message/send through the PLATFORM proxy
#      (/workspaces/{id}/a2a); assert 200 + the stub's canned reply (proves the
#      ws-<id>:8000 Docker-DNS rewrite path works end-to-end).
#   6. Cleanup: delete the workspace (trap removes its container + volumes).
#
# Parameterizable: LIFECYCLE_RUNTIME_IMAGE selects which image the provisioner
# resolves to. Default = the freshly-built stub. Point it at the real image
# (e.g. molecule-local/workspace-template-claude-code:2ac9678422a5) for an
# advisory lifecycle-only run (the proxy-reach step then asserts reachability,
# not the canned text — a real LLM-less runtime can't produce "STUB OK").
#
# Run:
#   BASE=http://localhost:8080 ADMIN_TOKEN=dev-local-admin-token \
#     bash tests/e2e/test_local_provision_lifecycle_e2e.sh
set -euo pipefail

source "$(dirname "$0")/_lib.sh"  # sets BASE default + admin-auth + cleanup helpers

# ---- config -----------------------------------------------------------------
ADMIN_TOKEN="${ADMIN_TOKEN:-${MOLECULE_ADMIN_TOKEN:-}}"
export ADMIN_TOKEN MOLECULE_ADMIN_TOKEN="${ADMIN_TOKEN}"

ONLINE_TIMEOUT="${ONLINE_TIMEOUT:-90}"          # seconds to wait for online
A2A_TIMEOUT="${A2A_TIMEOUT:-30}"
STUB_DIR="$(cd "$(dirname "$0")/stub-runtime" && pwd)"
RUNTIME="claude-code"

# The provisioner's RegistryModeLocal resolves runtime=claude-code by checking
# the local image store for molecule-local/workspace-template-claude-code:<sha12>
# (the Gitea HEAD sha12 of the template repo's `main` branch — see
# provisioner/localbuild.go EnsureLocalImage). If that tag is missing it
# clones+builds the real 2.5GB template (slow + can OOM-kill in CI). We pre-tag
# our chosen image to that EXACT cache tag so the cache-check (dockerHasTag)
# hits and resolves to our image with no clone/build.
#
# The sha MOVES as the template repo advances, so we DISCOVER it at runtime from
# the same Gitea branch API the provisioner uses (CACHE_SHA), and only fall back
# to a pinned default (or an explicit CACHE_TAG override) when Gitea is
# unreachable. This keeps the test correct without an annual sha bump.
CACHE_REPO="molecule-local/workspace-template-${RUNTIME}"
GITEA_BRANCH_API="${GITEA_BRANCH_API:-https://git.moleculesai.app/api/v1/repos/molecule-ai/molecule-ai-workspace-template-${RUNTIME}/branches/main}"
# Model + credential choice — three coupled constraints from workspace-server:
#   * Create rejects a model NOT registered for the runtime
#     (UNREGISTERED_MODEL_FOR_RUNTIME, provider-registry SSOT).
#   * The SLASH form (anthropic/claude-opus-4-7) derives provider=platform =>
#     platform_managed billing, which ABORTS provisioning in a dev stack with
#     no CP proxy env (MISSING_PLATFORM_PROXY, #2162).
#   * The BARE form (claude-opus-4-7) derives provider=anthropic-api => BYOK,
#     which then FAILS CLOSED unless the workspace has a usable LLM credential
#     (MISSING_BYOK_CREDENTIAL). anthropic-api's auth_env is
#     [ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN] — so we pass a DUMMY
#     ANTHROPIC_API_KEY secret. The stub never makes an LLM call, so the dummy
#     value is fine; it only needs to exist so byok resolves with a usable cred.
# This keeps the test self-contained (no platform-proxy env required) — exactly
# the portable shape the CI required job needs.
LIFECYCLE_MODEL="${LIFECYCLE_MODEL:-claude-opus-4-7}"
LIFECYCLE_LLM_KEY="${LIFECYCLE_LLM_KEY:-ANTHROPIC_API_KEY}"
LIFECYCLE_LLM_VALUE="${LIFECYCLE_LLM_VALUE:-sk-ant-e2e-stub-dummy-not-a-real-key}"
LATEST_TAG="${CACHE_REPO}:latest"

# Image the provisioner should actually run. Default: build the stub. Override
# to a real image (a pre-built tag) for the advisory lifecycle-only run.
LIFECYCLE_RUNTIME_IMAGE="${LIFECYCLE_RUNTIME_IMAGE:-__BUILD_STUB__}"

# LIFECYCLE_PROVISIONER_BUILDS=1: do NOT pre-tag any image — let the provisioner
# resolve runtime=claude-code itself via RegistryModeLocal (clone + docker build
# the real template). This exercises the GENUINE local image-resolution path end
# to end. Used by the advisory CI job. Implies the real (LLM-less) runtime, so
# the proxy-reach step asserts reachability, not a canned reply.
LIFECYCLE_PROVISIONER_BUILDS="${LIFECYCLE_PROVISIONER_BUILDS:-0}"

# When NOT running the stub we cannot assert the canned "STUB OK" text (no LLM);
# we assert reachability/registration instead.
USING_STUB=1
[ "$LIFECYCLE_RUNTIME_IMAGE" != "__BUILD_STUB__" ] && USING_STUB=0
[ "$LIFECYCLE_PROVISIONER_BUILDS" = "1" ] && USING_STUB=0

PASS=0
FAIL=0
WSID=""
# May be pre-pinned via env; otherwise resolved from the Gitea HEAD sha in Step 1.
CACHE_TAG="${CACHE_TAG:-}"
# Remember the tags/images we mutated so the trap can restore the cache tag to
# the real image (so a stub run never leaves the real claude-code tag pointing
# at the lightweight stub for the next developer/CI job).
ORIG_CACHE_IMAGE_ID=""

check() {
  local desc="$1" expected="$2" actual="$3"
  if echo "$actual" | grep -qF -- "$expected"; then
    echo "PASS: $desc"; PASS=$((PASS + 1))
  else
    echo "FAIL: $desc"
    echo "  expected to contain: $expected"
    echo "  got: $(echo "$actual" | head -5)"
    FAIL=$((FAIL + 1))
  fi
}

pass() { echo "PASS: $1"; PASS=$((PASS + 1)); }
fail() { echo "FAIL: $1"; [ -n "${2:-}" ] && echo "  $2"; FAIL=$((FAIL + 1)); }

admin_curl() {
  local _a=(); e2e_admin_auth_args _a
  curl -s "${_a[@]+"${_a[@]}"}" "$@"
}

ws_field() {  # ws_field <workspace-json> <field>
  echo "$1" | python3 -c "import sys,json
try:
  d=json.load(sys.stdin); print(d.get('$2',''))
except Exception:
  print('')"
}

container_running() {  # container_running <ws-id>  -> echoes name if running
  local short="${1:0:12}"
  docker ps --filter "name=ws-${short}" --filter "status=running" --format '{{.Names}}' 2>/dev/null | head -1
}

cleanup() {
  local rc=$?
  echo ""
  echo "--- cleanup ---"
  if [ -n "$WSID" ]; then
    # SCOPED teardown — only the workspace this test created. Never a blanket
    # sweep (other dev workspaces may be live on this shared daemon).
    e2e_delete_workspace "$WSID" "" >/dev/null 2>&1 || true
    local short="${WSID:0:12}"
    docker rm -f "ws-${short}" >/dev/null 2>&1 || true
    # Volume naming is split in the provisioner: configs + claude-sessions use the
    # 12-char short id (ConfigVolumeName/ClaudeSessionVolumeName), but the
    # /workspace volume uses the FULL UUID (buildWorkspaceMount: ws-<id>-workspace).
    # Remove BOTH forms so neither leaks.
    docker volume rm -f \
      "ws-${short}-configs" "ws-${short}-claude-sessions" \
      "ws-${short}-workspace" "ws-${WSID}-workspace" >/dev/null 2>&1 || true
    echo "cleaned workspace $WSID + ws-${short} container/volumes"
  fi
  # Restore the cache tag to whatever it pointed at before we retagged it, so a
  # stub run doesn't leave the real claude-code tag aliased to the stub.
  if [ -n "$ORIG_CACHE_IMAGE_ID" ]; then
    docker tag "$ORIG_CACHE_IMAGE_ID" "$CACHE_TAG" >/dev/null 2>&1 || true
    echo "restored $CACHE_TAG -> ${ORIG_CACHE_IMAGE_ID:0:19}"
  fi
  exit $rc
}
trap cleanup EXIT INT TERM

echo "=== Local Docker-Provisioner Lifecycle E2E ==="
echo "BASE=$BASE  runtime=$RUNTIME  using_stub=$USING_STUB  cache_tag=${CACHE_TAG:-<resolve-in-step-1>}"
echo ""

# Preflight: docker must be reachable and the platform must be up.
if ! docker info >/dev/null 2>&1; then
  echo "ERROR: docker daemon not reachable — this test provisions local containers."
  exit 2
fi
if ! curl -s -m 5 "$BASE/workspaces" >/dev/null 2>&1; then
  echo "ERROR: platform not reachable at $BASE"
  exit 2
fi

# ----------------------------------------------------------------------------
# Step 1 — build/tag the image the provisioner will resolve to.
# ----------------------------------------------------------------------------
echo "--- Step 1: resolve runtime image to the chosen target ---"
# Resolve the EXACT cache tag the provisioner will look up: <repo>:<gitea-HEAD-
# sha12>. Discover the sha from the Gitea branch API (same source the provisioner
# uses). An explicit CACHE_TAG env overrides discovery; if Gitea is unreachable
# AND no override is set, bail loudly — silently tagging the wrong sha would let
# the provisioner clone+build the real 2.5GB template (slow / OOM).
if [ -n "${CACHE_TAG:-}" ]; then
  echo "Using operator-pinned CACHE_TAG=$CACHE_TAG"
else
  CACHE_SHA=$(curl -s -m 10 "$GITEA_BRANCH_API" 2>/dev/null \
    | python3 -c "import sys,json
try:
  print(json.load(sys.stdin)['commit']['id'][:12])
except Exception:
  print('')" 2>/dev/null)
  if [ -z "$CACHE_SHA" ]; then
    echo "ERROR: could not resolve the template HEAD sha from $GITEA_BRANCH_API"
    echo "       set CACHE_TAG=$CACHE_REPO:<sha12> explicitly (the tag the provisioner expects)."
    exit 2
  fi
  CACHE_TAG="${CACHE_REPO}:${CACHE_SHA}"
  echo "Resolved provisioner cache tag: $CACHE_TAG (gitea HEAD sha)"
fi

# Record what the cache tag points at NOW (if anything) so cleanup can restore.
ORIG_CACHE_IMAGE_ID="$(docker image inspect --format '{{.Id}}' "$CACHE_TAG" 2>/dev/null || true)"

if [ "$LIFECYCLE_PROVISIONER_BUILDS" = "1" ]; then
  # No pre-tag — the provisioner resolves + builds the real template itself via
  # RegistryModeLocal. Disarm the cache-tag restore (we never touched it).
  ORIG_CACHE_IMAGE_ID=""
  pass "provisioner-builds mode: leaving image resolution to RegistryModeLocal (real template build)"
elif [ "$USING_STUB" -eq 1 ]; then
  echo "Building stub image from $STUB_DIR ..."
  if ! docker build --platform=linux/amd64 -t molecule-local/stub-runtime:latest "$STUB_DIR" >/tmp/stub_build.log 2>&1; then
    echo "FAIL: stub image build failed"; tail -20 /tmp/stub_build.log; exit 1
  fi
  pass "stub image built"
  TARGET_IMAGE="molecule-local/stub-runtime:latest"
  # Point BOTH the sha-pinned cache tag and :latest at the stub so the
  # provisioner's RegistryModeLocal cache-check (dockerHasTag) resolves to it
  # instead of cloning+building the template.
  docker tag "$TARGET_IMAGE" "$CACHE_TAG"
  docker tag "$TARGET_IMAGE" "$LATEST_TAG"
  pass "tagged $TARGET_IMAGE -> $CACHE_TAG (+ :latest)"
else
  TARGET_IMAGE="$LIFECYCLE_RUNTIME_IMAGE"
  if ! docker image inspect "$TARGET_IMAGE" >/dev/null 2>&1; then
    echo "Real image $TARGET_IMAGE not present locally — pulling ..."
    docker pull "$TARGET_IMAGE" >/dev/null 2>&1 || { echo "FAIL: cannot obtain $TARGET_IMAGE"; exit 1; }
  fi
  pass "using real runtime image $TARGET_IMAGE"
  docker tag "$TARGET_IMAGE" "$CACHE_TAG"
  docker tag "$TARGET_IMAGE" "$LATEST_TAG"
  pass "tagged $TARGET_IMAGE -> $CACHE_TAG (+ :latest)"
fi
echo ""

# ----------------------------------------------------------------------------
# Step 2 — provision a workspace via the real create endpoint.
# ----------------------------------------------------------------------------
echo "--- Step 2: provision workspace (POST /workspaces) ---"
# Provision-time billing on this dev stack (no CP proxy env):
#   * A claude-code workspace with a BARE model id derives provider=anthropic-api
#     => BYOK, which FAILS CLOSED in prepare unless a usable LLM credential
#     exists (MISSING_BYOK_CREDENTIAL).
#   * The per-workspace secret-write guard blocks a vendor key while the
#     workspace still resolves platform-managed (the MODEL secret isn't stored
#     until AFTER payload.secrets are written at create time) — so we can't pass
#     the key in the create payload.
# So: create WITHOUT secrets, flip the workspace to byok (explicit override wins
# in BOTH the guard's resolver and the provision resolver), then write the dummy
# vendor key — now permitted. We do NOT rely on Create's first provision to seed
# the config volume (it aborts byok-no-cred BEFORE Start, leaving the volume
# empty). Instead we SEED config.yaml directly into the named config volume and
# then trigger ONE clean provision via /restart. Seeding the volume is also what
# makes the restart-survival assertion meaningful: the restart path reuses the
# volume rather than any template.
CREATE_BODY=$(cat <<JSON
{"name":"Lifecycle E2E Stub","tier":2,"runtime":"$RUNTIME","model":"$LIFECYCLE_MODEL"}
JSON
)
RESP=$(admin_curl -X POST "$BASE/workspaces" -H "Content-Type: application/json" -d "$CREATE_BODY")
WSID=$(ws_field "$RESP" "id")
if [ -z "$WSID" ]; then
  fail "create returned no workspace id" "$RESP"
  echo "=== Results: $PASS passed, $((FAIL+1)) failed ==="
  exit 1
fi
pass "workspace created: $WSID"
SHORT="${WSID:0:12}"
CONFIG_VOL="ws-${SHORT}-configs"

# Mint a workspace bearer for the WorkspaceAuth-gated secret + /restart calls.
WTOKEN=$(e2e_mint_workspace_token "$WSID" || true)
if [ -z "$WTOKEN" ]; then
  fail "could not mint workspace token"
  echo "=== Results: $PASS passed, $FAIL failed ==="; exit 1
fi

# Flip to byok BEFORE writing the vendor key (explicit override unblocks the
# secret-write guard AND makes the provision resolver pick byok).
BM=$(admin_curl -X PUT "$BASE/admin/workspaces/$WSID/llm-billing-mode" \
  -H "Content-Type: application/json" -d '{"mode":"byok"}')
check "billing mode set to byok" "byok" "$BM"

# Write the dummy LLM credential (now allowed on a byok workspace). Inert — the
# stub never calls an LLM; it only needs to exist so byok has a usable cred.
SEC=$(curl -s -X POST "$BASE/workspaces/$WSID/secrets" \
  -H "Authorization: Bearer $WTOKEN" -H "Content-Type: application/json" \
  -d "{\"key\":\"$LIFECYCLE_LLM_KEY\",\"value\":\"$LIFECYCLE_LLM_VALUE\"}")
echo "  secret write: $(echo "$SEC" | head -c 120)"

# Seed config.yaml directly into the named config volume so the provision (and
# every later restart) has a config source. Create's byok-no-cred abort never
# wrote it, and this dev stack ships no claude-code template in the platform's
# configsDir for the empty-volume auto-recover to fall back to. The provisioner
# created the volume on its first (aborted) Start attempt; ensure it exists,
# then drop a minimal valid config.yaml in via a throwaway alpine container.
docker volume create "$CONFIG_VOL" >/dev/null 2>&1 || true
CFG_YAML="name: ${WSID}
description: stub lifecycle e2e
version: 1.0.0
tier: 2
runtime: ${RUNTIME}
model: ${LIFECYCLE_MODEL}
runtime_config:
  model: ${LIFECYCLE_MODEL}
  timeout: 0
"
if docker run --rm -v "${CONFIG_VOL}:/configs" alpine:3 sh -c "cat > /configs/config.yaml" <<EOF >/dev/null 2>&1
${CFG_YAML}
EOF
then pass "seeded config.yaml into $CONFIG_VOL"; else fail "could not seed config.yaml into $CONFIG_VOL"; fi
echo ""

# ----------------------------------------------------------------------------
# Step 3 — provision (via restart) and wait for online; assert container.
# ----------------------------------------------------------------------------
echo "--- Step 3: provision + wait for first online (<=${ONLINE_TIMEOUT}s) ---"
# Kick ONE clean provision now that byok + cred + config.yaml are all in place.
curl -s -X POST "$BASE/workspaces/$WSID/restart" \
  -H "Authorization: Bearer $WTOKEN" -H "Content-Type: application/json" -d '{}' >/dev/null
STATUS=""; LAST=""; failed_since=0
for _ in $(seq 1 "$ONLINE_TIMEOUT"); do
  WS=$(admin_curl "$BASE/workspaces/$WSID")
  STATUS=$(ws_field "$WS" "status")
  LAST=$(ws_field "$WS" "last_sample_error")
  if [ "$STATUS" = "online" ]; then break; fi
  if [ "$STATUS" = "failed" ]; then
    failed_since=$((failed_since + 1))
    # A restart re-kicks provisioning; give the coalescing pipeline room to
    # converge. Only bail if it stays failed for 20s straight.
    if [ "$failed_since" -ge 20 ]; then
      fail "workspace STUCK in 'failed' during initial provision" "last_sample_error: $LAST"
      echo "=== Results: $PASS passed, $FAIL failed ==="; exit 1
    fi
  else
    failed_since=0
  fi
  sleep 1
done
check "workspace reached online (status=$STATUS)" "online" "$STATUS"
RUN=$(container_running "$WSID")
if [ -n "$RUN" ]; then pass "container running: $RUN"; else fail "no running ws-${WSID:0:12} container" "docker ps shows none"; fi
echo ""

# ----------------------------------------------------------------------------
# Step 4 — RESTART-SURVIVAL (the assertion that would have caught the bug).
# ----------------------------------------------------------------------------
echo "--- Step 4: restart-survival (POST /workspaces/$WSID/restart) ---"
# Re-mint the workspace bearer: every (re)provision rotates the workspace token
# (issueAndInjectToken -> RevokeAllForWorkspace + IssueToken), so the Step-2
# token is now stale. /restart is WorkspaceAuth-gated, so mint a fresh one.
WTOKEN=$(e2e_mint_workspace_token "$WSID" || true)
if [ -z "$WTOKEN" ]; then
  fail "could not mint fresh workspace token for restart"
else
  RR=$(curl -s -X POST "$BASE/workspaces/$WSID/restart" \
        -H "Authorization: Bearer $WTOKEN" -H "Content-Type: application/json" -d '{}')
  check "restart accepted (provisioning)" "provisioning" "$RR"

  # Poll until online AGAIN. Restart reuses the EXISTING config volume (no
  # template/configFiles passed) — so this passes ONLY if the config volume
  # survived the stop and still has config.yaml. A regression (volume reaped /
  # emptied) surfaces as status=failed with the "config volume is empty" error.
  STATUS=""; LAST=""
  for _ in $(seq 1 "$ONLINE_TIMEOUT"); do
    WS=$(admin_curl "$BASE/workspaces/$WSID")
    STATUS=$(ws_field "$WS" "status")
    LAST=$(ws_field "$WS" "last_sample_error")
    case "$STATUS" in
      online) break ;;
      failed)
        fail "workspace wedged in 'failed' AFTER restart (the config-volume bug class)" "last_sample_error: $LAST"
        break ;;
    esac
    sleep 1
  done
  check "workspace back online after restart (status=$STATUS)" "online" "$STATUS"
  # Explicit negative on the exact bug signature.
  if echo "$LAST" | grep -qiF "config volume is empty"; then
    fail "restart hit 'config volume is empty' — restart-survival REGRESSION" "$LAST"
  else
    pass "no 'config volume is empty' error after restart"
  fi
  RUN=$(container_running "$WSID")
  if [ -n "$RUN" ]; then pass "container back after restart: $RUN"; else fail "container missing after restart"; fi
fi
echo ""

# ----------------------------------------------------------------------------
# Step 5 — proxy reach (ws-<id>:8000 Docker-DNS rewrite, end to end).
# ----------------------------------------------------------------------------
echo "--- Step 5: proxy reach (POST /workspaces/$WSID/a2a) ---"
A2A=$(curl -s --max-time "$A2A_TIMEOUT" -X POST "$BASE/workspaces/$WSID/a2a" \
  -H "Content-Type: application/json" \
  -d '{"method":"message/send","params":{"message":{"role":"user","parts":[{"type":"text","text":"ping"}]}}}')
if [ "$USING_STUB" -eq 1 ]; then
  check "proxy returned a result envelope" '"result"' "$A2A"
  check "proxy reached stub (canned reply)" 'STUB OK' "$A2A"
  # Parse the envelope so whitespace/key-ordering doesn't break the assertion.
  ROLE=$(echo "$A2A" | python3 -c "import sys,json
try:
  print(json.load(sys.stdin).get('result',{}).get('role',''))
except Exception:
  print('')")
  check "reply has agent role" "agent" "$ROLE"
else
  # Real LLM-less image: we can't get a canned text, but a reachable runtime
  # must answer with EITHER a result OR a structured JSON-RPC error — NOT a
  # proxy-level "workspace agent unreachable" / "no URL". Assert reachability.
  if echo "$A2A" | grep -qiE 'unreachable|workspace has no URL|restarting'; then
    fail "real runtime not reachable through proxy" "$A2A"
  else
    pass "real runtime reachable through proxy (got a JSON-RPC response)"
    echo "  response: $(echo "$A2A" | head -c 200)"
  fi
fi
echo ""

echo "=== Results: $PASS passed, $FAIL failed ==="
exit "$FAIL"
