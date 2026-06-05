#!/usr/bin/env bash
# E2E test: every maintained runtime template works end-to-end.
#
# Self-contained happy-path smoke per runtime. Provisions a fresh
# workspace, waits for status=online, sends a real A2A message, and
# asserts a non-error reply. Pins the contract so the post-#87 template
# extraction (and ongoing template work) can't silently break any
# runtime.
#
# Runtimes covered: claude-code, codex, hermes, openclaw.
# claude-code + hermes have unique
# provisioning quirks (claude-code OAuth, hermes 15-min cold-boot)
# and stay first-class with their own run_<runtime> functions; the
# OpenAI-backed runtimes share run_openai_runtime. Each phase skips cleanly
# if its prerequisite secret is missing.
#
# What this proves:
#   1. Provisioning + container boot works for each runtime.
#   2. The runtime reaches status=online within its expected cold-boot
#      window (claude-code: ~60s, hermes: up to 15min on cold apt).
#   3. A real A2A message/send produces a non-empty, non-error reply.
#   4. The activity_logs row for the call is well-formed.
#
# Each phase skips cleanly when its prerequisite secret is absent so a
# partially-keyed env (e.g. CI without an OpenAI key) doesn't false-fail.
#
# REQUIRE-LIVE (false-green guard, mirrors CP serving-e2e's
# SERVING_E2E_REQUIRE_LIVE semantics)
# ------------------------------------------------------------------
# Without a guard, an env with NO live secrets makes every phase SKIP,
# leaving PASS=0 FAIL=0 — and the historical `[ "$FAIL" -eq 0 ]` gate
# exits 0 (GREEN) while validating ZERO runtimes. That made the REQUIRED
# `E2E API Smoke Test` merge gate pass without exercising a single
# runtime (false-green).
#
# Fix: a real "validated arm" counter (VALIDATED) tracks runtimes that
# actually ran AND produced a non-error A2A reply. In CI, set
# E2E_REQUIRE_LIVE=1: if zero arms validated, the run exits NON-zero with
# a loud message — the gate goes red until at least one live arm is wired
# (secrets present). Locally (E2E_REQUIRE_LIVE unset/0), a fully-skipped
# run stays a LOUD skip + exit 0 for dev convenience.
#
# The CI live arm is MiniMax (E2E_MINIMAX_API_KEY, fed from the existing
# MOLECULE_STAGING_MINIMAX_API_KEY Gitea secret): it drives the
# claude-code runtime against MiniMax (BYOK) — the same key + path the
# staging-smoke / continuous-synth canaries use. No new credential.
#
# Usage:
#   # CI live arm — MiniMax (existing MOLECULE_STAGING_MINIMAX_API_KEY):
#   E2E_REQUIRE_LIVE=1 E2E_MINIMAX_API_KEY=... \
#     tests/e2e/test_priority_runtimes_e2e.sh
#
#   # Other live arms (if their secrets are configured):
#   CLAUDE_CODE_OAUTH_TOKEN=... E2E_OPENAI_API_KEY=... \
#     tests/e2e/test_priority_runtimes_e2e.sh
#
#   # CI / enforced mode — zero-validated is RED:
#   E2E_REQUIRE_LIVE=1 E2E_MINIMAX_API_KEY=... \
#     tests/e2e/test_priority_runtimes_e2e.sh
#
#   # Run only one runtime
#   E2E_RUNTIMES=minimax     tests/e2e/test_priority_runtimes_e2e.sh
#   E2E_RUNTIMES=claude-code tests/e2e/test_priority_runtimes_e2e.sh
#   E2E_RUNTIMES=hermes      tests/e2e/test_priority_runtimes_e2e.sh
#
# Prereqs:
#   - workspace-server on http://localhost:8080
#   - AdminAuth bootstrap or `MOLECULE_ADMIN_TOKEN` for token minting
#   - For claude-code: CLAUDE_CODE_OAUTH_TOKEN
#   - For hermes:      E2E_OPENAI_API_KEY  (other providers also OK if you
#                       set MODEL_SLUG_HERMES + matching secrets directly)

set -euo pipefail

source "$(dirname "$0")/_lib.sh"

PASS=0
FAIL=0
SKIP=0
# VALIDATED counts runtimes that ACTUALLY ran end-to-end (provisioned,
# reached online, AND returned a non-error A2A reply). Distinct from PASS,
# which also counts sub-assertions like activity-log rows. This is the
# signal the REQUIRE-LIVE gate keys off: VALIDATED==0 means we proved
# nothing about any runtime, regardless of how many sub-asserts "passed".
VALIDATED=0
CREATED_WSIDS=()

cleanup() {
  # `set -u` + empty array would error on "${CREATED_WSIDS[@]}"; the
  # ${VAR[@]+"…"} form expands to nothing when the array is unset/empty
  # so the loop body is skipped cleanly. Hits the skip-no-keys path.
  for wid in ${CREATED_WSIDS[@]+"${CREATED_WSIDS[@]}"}; do
    [ -n "$wid" ] && e2e_delete_workspace "$wid" ""
  done
}
trap cleanup EXIT

pass()      { echo "  PASS — $1"; PASS=$((PASS + 1)); }
fail()      { echo "  FAIL — $1"; echo "         $2"; FAIL=$((FAIL + 1)); }
skip()      { echo "  SKIP — $1"; SKIP=$((SKIP + 1)); }
# Mark a runtime as having been validated end-to-end (online + non-error
# A2A reply). Also emits a PASS line so it shows in the results tally.
validated() { echo "  PASS — $1"; PASS=$((PASS + 1)); VALIDATED=$((VALIDATED + 1)); }

# Pre-sweep any prior runs that left workspaces behind (same defence as
# test_notify_attachments_e2e.sh: trap fires on normal exit, but a
# SIGPIPE / kill -9 can bypass it).
PRIOR=$(curl -s "$BASE/workspaces" | python3 -c '
import json, sys
try:
    print(" ".join(w["id"] for w in json.load(sys.stdin) if w.get("name","").startswith("Priority E2E ")))
except Exception:
    pass
')
for _wid in $PRIOR; do
  echo "Sweeping prior workspace: $_wid"
  e2e_delete_workspace "$_wid" ""
done

# Block until $1 reaches one of $2 (space-separated states), or $3 sec elapse.
wait_for_status() {
  local wsid="$1" want="$2" budget="$3"
  local start=$SECONDS
  while [ $((SECONDS - start)) -lt "$budget" ]; do
    local s
    s=$(curl -s "$BASE/workspaces/$wsid" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("status",""))' 2>/dev/null || echo "")
    for w in $want; do [ "$s" = "$w" ] && { echo "$s"; return 0; }; done
    sleep 4
  done
  echo "$s"
  return 1
}

# Send "What is 2+2?" via A2A, return the reply text on stdout. Fails
# (non-zero exit + empty stdout) if the platform returns an error envelope
# or the reply is empty / sentinel-error.
send_test_prompt() {
  local wsid="$1" token="$2"
  local resp
  resp=$(curl -s --max-time 180 -X POST "$BASE/workspaces/$wsid/a2a" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $token" \
    -d '{
      "method": "message/send",
      "params": {
        "message": {
          "role": "user",
          "messageId": "e2e-priority-runtime",
          "parts": [{"kind": "text", "text": "Reply with exactly the word: PONG"}]
        }
      }
    }') || return 1
  # Walk a few common A2A reply shapes; stop at the first non-empty text.
  echo "$resp" | python3 -c '
import json, sys
try:
    d = json.loads(sys.stdin.read())
except Exception:
    sys.exit(1)
texts = []
def walk(node):
    if isinstance(node, dict):
        for v in node.values(): walk(v)
    elif isinstance(node, list):
        for v in node: walk(v)
    elif isinstance(node, str):
        texts.append(node)
walk(d.get("result") or d)
joined = "\n".join(t for t in texts if t.strip())
if not joined.strip():
    sys.exit(2)
# Surface a known error sentinel so the caller can tell apart "empty" from "explicit error"
low = joined.lower()
for needle in ("a2a_error", "agent error", "could not resolve authentication", "401",
               "no provider api key", "missing api", "model_not_found"):
    if needle in low:
        print("ERROR: " + joined[:200])
        sys.exit(3)
print(joined)
'
}

assert_activity_logged() {
  # After a successful A2A round-trip, the platform's a2a_proxy logs
  # an a2a_receive row with method=message/send. Pin the contract so a
  # silent regression in LogActivity (e.g. dropped status field, broken
  # broadcaster) shows up here. Polls briefly because LogActivity is
  # detached-goroutine — the row may land a few hundred ms after the
  # POST returns.
  local label="$1" wsid="$2" token="$3"
  local start=$SECONDS
  while [ $((SECONDS - start)) -lt 10 ]; do
    local act
    act=$(curl -s -H "Authorization: Bearer $token" "$BASE/workspaces/$wsid/activity?type=a2a_receive&limit=10")
    local found
    found=$(echo "$act" | python3 -c '
import json, sys
try:
    rows = json.load(sys.stdin) or []
except Exception:
    sys.exit(1)
for r in rows:
    if r.get("method") == "message/send" and r.get("status") in ("ok", "error"):
        print("ok")
        sys.exit(0)
sys.exit(2)
' 2>/dev/null) && true
    if [ "$found" = "ok" ]; then
      pass "$label activity_logs row written for the A2A turn"
      return 0
    fi
    sleep 1
  done
  fail "$label activity_logs row" "no a2a_receive row with method=message/send appeared in 10s"
}

run_claude_code() {
  echo ""
  echo "=== claude-code happy path ==="
  if [ -z "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
    skip "CLAUDE_CODE_OAUTH_TOKEN not set"
    return 0
  fi
  local secrets
  secrets=$(python3 -c "
import json, os
print(json.dumps({'CLAUDE_CODE_OAUTH_TOKEN': os.environ['CLAUDE_CODE_OAUTH_TOKEN']}))
")
  local resp wsid
  # model required (CTO 2026-05-22 SSOT) — pass the deleted DefaultModel("claude-code") value.
  resp=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
    -d "{\"name\":\"Priority E2E (claude-code)\",\"runtime\":\"claude-code\",\"model\":\"sonnet\",\"tier\":1,\"secrets\":$secrets}")
  wsid=$(echo "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))') || true
  if [ -z "$wsid" ]; then
    fail "create claude-code workspace" "$resp"
    return 0
  fi
  CREATED_WSIDS+=("$wsid")
  echo "  workspace=$wsid"

  # claude-code typical cold boot: 30-90s (image already pulled)
  local final
  final=$(wait_for_status "$wsid" "online failed" 240) || true
  if [ "$final" != "online" ]; then
    fail "claude-code workspace reaches online" "final status: $final"
    return 0
  fi
  pass "claude-code workspace reaches online"

  local token
  token=$(echo "$resp" | e2e_extract_token)
  if [ -z "$token" ]; then
    token=$(e2e_mint_workspace_token "$wsid")
  fi
  if [ -z "$token" ]; then
    fail "resolve claude-code workspace token" "no token returned"
    return 0
  fi

  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      validated "claude-code reply contains PONG"
    else
      validated "claude-code reply non-empty (first 80 chars: ${reply:0:80})"
    fi
    assert_activity_logged "claude-code" "$wsid" "$token"
  else
    fail "claude-code reply" "${reply:-<empty or error>}"
  fi
}

run_hermes() {
  echo ""
  echo "=== hermes happy path ==="
  if [ -z "${E2E_OPENAI_API_KEY:-}" ]; then
    skip "E2E_OPENAI_API_KEY not set (hermes needs an LLM provider key)"
    return 0
  fi
  local secrets
  secrets=$(python3 -c "
import json, os
k = os.environ['E2E_OPENAI_API_KEY']
print(json.dumps({
    'OPENAI_API_KEY': k,
    'OPENAI_BASE_URL': 'https://api.openai.com/v1',
    'MODEL_PROVIDER': 'openai:gpt-4o',
    # The HERMES_* fields below pin the provider deterministically
    # (see comment in test_staging_full_saas.sh:268-275 for why).
    'HERMES_INFERENCE_PROVIDER': 'custom',
    'HERMES_CUSTOM_BASE_URL': 'https://api.openai.com/v1',
    'HERMES_CUSTOM_API_KEY': k,
    'HERMES_CUSTOM_API_MODE': 'chat_completions',
}))
")
  local resp wsid
  resp=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
    -d "{\"name\":\"Priority E2E (hermes)\",\"runtime\":\"hermes\",\"tier\":1,\"model\":\"openai/gpt-4o\",\"secrets\":$secrets}")
  wsid=$(echo "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))') || true
  if [ -z "$wsid" ]; then
    fail "create hermes workspace" "$resp"
    return 0
  fi
  CREATED_WSIDS+=("$wsid")
  echo "  workspace=$wsid"

  # Hermes cold boot is the slow path: apt + uv + hermes-agent sidecar.
  # Up to 15 min on cold disk; usually 3-5 min when the runtime image is
  # already cached. Be generous so the test doesn't false-fail in CI.
  local final
  final=$(wait_for_status "$wsid" "online failed" 900) || true
  if [ "$final" != "online" ]; then
    fail "hermes workspace reaches online" "final status: $final"
    return 0
  fi
  pass "hermes workspace reaches online"

  local token
  token=$(echo "$resp" | e2e_extract_token)
  if [ -z "$token" ]; then
    token=$(e2e_mint_workspace_token "$wsid")
  fi
  if [ -z "$token" ]; then
    fail "resolve hermes workspace token" "no token returned"
    return 0
  fi

  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      validated "hermes reply contains PONG"
    else
      validated "hermes reply non-empty (first 80 chars: ${reply:0:80})"
    fi
    assert_activity_logged "hermes" "$wsid" "$token"
  else
    fail "hermes reply" "${reply:-<empty or error>}"
  fi
}

####################################################################
# Secondary runtimes — same provision/online/A2A loop, parametrized.
####################################################################
# Codex and OpenClaw use OpenAI as their LLM provider in this smoke and
# don't need the hermes-specific HERMES_* secret block. Skip if no key.
# claude-code + hermes stay first-class above because each has unique
# provisioning quirks (claude-code OAuth, hermes cold-boot tolerance);
# refactoring them into this generic loop would lose those guards.

run_openai_runtime() {
  local runtime="$1"
  local label="$2"
  echo ""
  echo "=== $label happy path ==="
  if [ -z "${E2E_OPENAI_API_KEY:-}" ]; then
    skip "E2E_OPENAI_API_KEY not set ($runtime needs an LLM provider key)"
    return 0
  fi
  local secrets
  secrets=$(python3 -c "
import json, os
k = os.environ['E2E_OPENAI_API_KEY']
print(json.dumps({
    'OPENAI_API_KEY': k,
    'OPENAI_BASE_URL': 'https://api.openai.com/v1',
    'MODEL_PROVIDER': 'openai:gpt-4o-mini',
}))
")
  local resp wsid
  resp=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
    -d "{\"name\":\"Priority E2E ($runtime)\",\"runtime\":\"$runtime\",\"tier\":1,\"model\":\"openai/gpt-4o-mini\",\"secrets\":$secrets}")
  wsid=$(echo "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))') || true
  if [ -z "$wsid" ]; then
    fail "create $runtime workspace" "$resp"
    return 0
  fi
  CREATED_WSIDS+=("$wsid")
  echo "  workspace=$wsid"

  local final
  final=$(wait_for_status "$wsid" "online failed" 240) || true
  if [ "$final" != "online" ]; then
    fail "$runtime workspace reaches online" "final status: $final"
    return 0
  fi
  pass "$runtime workspace reaches online"

  local token
  token=$(echo "$resp" | e2e_extract_token)
  if [ -z "$token" ]; then
    token=$(e2e_mint_workspace_token "$wsid")
  fi
  if [ -z "$token" ]; then
    fail "resolve $runtime workspace token" "no token returned"
    return 0
  fi

  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      validated "$runtime reply contains PONG"
    else
      validated "$runtime reply non-empty (first 80 chars: ${reply:0:80})"
    fi
    assert_activity_logged "$runtime" "$wsid" "$token"
  else
    fail "$runtime reply" "${reply:-<empty or error>}"
  fi
}

run_codex()      { run_openai_runtime "codex"      "codex"; }
run_openclaw()   { run_openai_runtime "openclaw"   "openclaw"; }

####################################################################
# MiniMax live arm — the CI-default REQUIRE-LIVE arm.
####################################################################
# Drives the claude-code runtime against MiniMax (BYOK) using the
# already-present Gitea secret MOLECULE_STAGING_MINIMAX_API_KEY,
# surfaced into the env as E2E_MINIMAX_API_KEY (same name + secret the
# staging-smoke / continuous-synth canaries use — see staging-smoke.yml
# and continuous-synth-e2e.yml). NO new credential is introduced.
#
# Why this is the arm that keeps the REQUIRED gate honest:
#   - claude-code's `minimax` provider (providers.yaml / registry_gen.go)
#     is third_party_anthropic_compat: it reads MINIMAX_API_KEY at boot
#     and routes ANTHROPIC_BASE_URL → api.minimax.io/anthropic. So the
#     ONLY tenant secret needed is {"MINIMAX_API_KEY": <key>} — exactly
#     the SECRETS_JSON branch test_staging_full_saas.sh uses.
#   - Model id is the NAMESPACED colon-form `minimax:MiniMax-M2.7`, the
#     registered BYOK arm for claude-code (registry_gen.go Runtimes
#     ["claude-code"]["minimax"]). Per core#2263 the BARE `MiniMax-M2`
#     id can 400 on a registry-skewed ws-server build; the namespaced
#     form resolves the way kimi's `moonshot/…` does, so it's the
#     robust choice for the gate.
run_minimax() {
  echo ""
  echo "=== minimax (claude-code BYOK) happy path ==="
  if [ -z "${E2E_MINIMAX_API_KEY:-}" ]; then
    skip "E2E_MINIMAX_API_KEY not set (MiniMax live arm needs the MiniMax key)"
    return 0
  fi
  local secrets
  secrets=$(python3 -c "
import json, os
# claude-code's minimax provider (third_party_anthropic_compat) reads
# MINIMAX_API_KEY and points ANTHROPIC_BASE_URL at api.minimax.io/anthropic
# at boot — so the ONLY tenant secret needed is the MiniMax key itself.
print(json.dumps({'MINIMAX_API_KEY': os.environ['E2E_MINIMAX_API_KEY']}))
")
  local resp wsid
  # Namespaced BYOK model id (core#2263): bare MiniMax-M2 can 400 on a
  # registry-skewed ws-server build; minimax:MiniMax-M2.7 is the
  # registered claude-code BYOK arm and resolves like kimi's moonshot/…
  resp=$(curl -s -X POST "$BASE/workspaces" -H "Content-Type: application/json" \
    -d "{\"name\":\"Priority E2E (minimax)\",\"runtime\":\"claude-code\",\"model\":\"minimax:MiniMax-M2.7\",\"tier\":1,\"secrets\":$secrets}")
  wsid=$(echo "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("id",""))') || true
  if [ -z "$wsid" ]; then
    fail "create minimax workspace" "$resp"
    return 0
  fi
  CREATED_WSIDS+=("$wsid")
  echo "  workspace=$wsid"

  # claude-code runtime image is already pulled; cold boot ~30-90s. The
  # first MiniMax cold-call can be slow but that's covered by send_test_prompt's
  # --max-time 180.
  local final
  final=$(wait_for_status "$wsid" "online failed" 240) || true
  if [ "$final" != "online" ]; then
    fail "minimax workspace reaches online" "final status: $final"
    return 0
  fi
  pass "minimax workspace reaches online"

  local token
  token=$(echo "$resp" | e2e_extract_token)
  if [ -z "$token" ]; then
    token=$(e2e_mint_workspace_token "$wsid")
  fi
  if [ -z "$token" ]; then
    fail "resolve minimax workspace token" "no token returned"
    return 0
  fi

  local reply
  if reply=$(send_test_prompt "$wsid" "$token"); then
    if echo "$reply" | grep -q "PONG"; then
      validated "minimax reply contains PONG"
    else
      validated "minimax reply non-empty (first 80 chars: ${reply:0:80})"
    fi
    assert_activity_logged "minimax" "$wsid" "$token"
  else
    fail "minimax reply" "${reply:-<empty or error>}"
  fi
}

WANT="${E2E_RUNTIMES:-claude-code codex hermes openclaw minimax}"
for r in $WANT; do
  case "$r" in
    claude-code) run_claude_code ;;
    codex)       run_codex ;;
    hermes)      run_hermes ;;
    openclaw)    run_openclaw ;;
    minimax)     run_minimax ;;
    all)         run_claude_code; run_codex; run_hermes; run_openclaw; run_minimax ;;
    *) echo "unknown runtime in E2E_RUNTIMES: $r" >&2; exit 2 ;;
  esac
done

echo ""
echo "=== Results: $PASS passed, $FAIL failed, $SKIP skipped, $VALIDATED runtime(s) validated end-to-end ==="

# Any real failure is always red.
if [ "$FAIL" -ne 0 ]; then
  exit 1
fi

# REQUIRE-LIVE gate (mirrors CP serving-e2e SERVING_E2E_REQUIRE_LIVE).
# A run where every runtime SKIPPED proves nothing. In enforced mode
# (CI sets E2E_REQUIRE_LIVE=1) that MUST be red so the required
# `E2E API Smoke Test` gate can't be false-green on an all-skip run.
REQUIRE_LIVE="${E2E_REQUIRE_LIVE:-0}"
if [ "$VALIDATED" -eq 0 ]; then
  if [ "$REQUIRE_LIVE" = "1" ] || [ "$REQUIRE_LIVE" = "true" ]; then
    echo "::error::E2E_REQUIRE_LIVE is set but ZERO runtimes were validated end-to-end." >&2
    echo "         Every runtime SKIPPED — no live secret was present, so this gate" >&2
    echo "         validated nothing. Wire at least one live arm via Gitea secrets" >&2
    echo "         (E2E_MINIMAX_API_KEY ← MOLECULE_STAGING_MINIMAX_API_KEY is the" >&2
    echo "         default CI arm; CLAUDE_CODE_OAUTH_TOKEN / E2E_OPENAI_API_KEY also" >&2
    echo "         work) so >=1 runtime actually provisions + replies. Failing RED" >&2
    echo "         instead of false-green." >&2
    exit 1
  fi
  # Dev convenience: no enforcement requested → loud skip, exit 0.
  echo "SKIPPED: no live secrets present and E2E_REQUIRE_LIVE is not set — validated" >&2
  echo "         zero runtimes. This is a dev-convenience pass; CI sets" >&2
  echo "         E2E_REQUIRE_LIVE=1 to make zero-validated a hard failure." >&2
  exit 0
fi

echo "OK: $VALIDATED runtime(s) validated end-to-end."
exit 0
