#!/usr/bin/env bash
# Offline anti-rot guard for the DELEGATION A2A POST's timeout budget and
# retry policy in tests/e2e/test_staging_full_saas.sh.
# No network, no curl, no tenant.
#
# WHY THIS EXISTS
#
# `message/send` is synchronous: the delegation POST's response carries the
# CHILD workspace's finished reply (the step reads result.parts[0].text), so the
# call waits out a full agent turn on a workspace that was just created — cold
# adapter start, TLS to the LLM endpoint, first prompt, first token.
#
# The PARENT leg routes through a2a_send_or_poll_queue and overrides the timeout
# to 90s for exactly that reason. The DELEGATION leg is raw curl, deliberately,
# because it authenticates as the PARENT WORKSPACE token rather than the tenant
# admin — and in being hand-rolled it silently inherited CURL_COMMON's generic
# --max-time 30. That took a REQUIRED gate red twice on 2026-07-30:
#
#   Delegation A2A POST failed after 1 attempt(s) (curl_rc=28, http=000)
#     core#4961 job 879714 @ 08:59:54   core#4958 job 880955 @ 11:08:49
#
# curl_rc=28 is "operation timed out"; http=000 means no response ever arrived.
# The retry loop around the call is scoped to cold-start HTTP statuses, so a
# transport timeout matched no arm and fell through after ONE attempt.
#
# It is fixed as a TIMEOUT, not a retry: rc=28 is maybe-processed (the child may
# be mid-turn), so re-POSTing risks double-delivering the delegation. That is the
# same rule lib/workspace_create_retry.sh encodes as "curl timeout 28 → no
# retry" for the non-idempotent create.
#
# HOW IT CHECKS (this is the whole point)
#
# The previous revision of this file grepped the harness for substrings. That is
# unfalsifiable here for two independent reasons:
#
#   * curl is LAST-FLAG-WINS, and `"${CURL_COMMON[@]}"` is an array expansion
#     that no grep can see through. `curl --max-time 90 "${CURL_COMMON[@]}"`
#     and `curl "${CURL_COMMON[@]}" --max-time 90` contain the SAME text and
#     have OPPOSITE effective budgets (30 vs 90). A text check cannot tell them
#     apart, so a reorder silently restores the 30s cap that reds the lane.
#   * "the retry does not key on the curl exit code" is a control-flow property.
#     A grep for `502|503|504` anywhere in a 3000-line harness matches the
#     workspace-create / bd_code / WAKE_CODE retries too, so it can never fail.
#
# So this guard does not read the harness as text. It EXTRACTS the delegation
# retry loop and EXECUTES it against a stub `curl` that records its own argv and
# returns a scripted (rc, http_code, body). Then it asserts on:
#
#   * the EFFECTIVE --max-time — resolved from the recorded argv with curl's own
#     last-flag-wins rule, so CURL_COMMON's contents and the override's POSITION
#     both count, and `${E2E_DELEG_A2A_TIMEOUT_SECS:-90}` is resolved by the
#     shell rather than by a sed; and
#   * the OBSERVED number of attempts under a scripted transport timeout
#     (rc=28 ⇒ must be exactly one) and under a scripted cold-start 503
#     (⇒ must still retry).
#
# Everything is scoped to the delegation loop specifically — the block is
# identified by the only a2a URL that targets $CHILD_ID — never to the file.
#
# NEGATIVE CONTROL (always on, not an env knob)
#
# Every run also probes two fixtures and requires the assertions to FAIL there:
#   * FIXTURE_REGRESSED — the pre-fix shape (no override, inherits 30s) WITH a
#     retry-on-rc=28 arm grafted in and the cold-start 5xx arm removed. All four
#     behavioural assertions must go red against it.
#   * FIXTURE_ABSENT — no delegation loop at all. Every assertion must go red,
#     i.e. the guard fails CLOSED when it cannot find what it is guarding.
# A guard you have not seen fail proves nothing, so it proves it on every run.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# DELEG_GUARD_TARGET exists so the mutation matrix in the PR body can point this
# guard at a mutated COPY of the harness. CI passes nothing and gets the real one.
SAAS="${DELEG_GUARD_TARGET:-$HERE/test_staging_full_saas.sh}"
PARENT_LEG_BUDGET=90   # a2a_send_or_poll_queue's --max-time for the parent leg
CURL_COMMON_BUDGET=30  # the generic cap the delegation leg must NOT inherit
MAX_ATTEMPTS=12        # the harness's cold-start retry ceiling

E2E_TMPDIR=$(mktemp -d -t delegto-XXXXXX)
trap 'rm -rf "$E2E_TMPDIR"' EXIT INT TERM

# Assertion ids, in report order. fail_remaining() marks any that a probe never
# reached, so an extraction failure reds the whole set instead of skipping it.
ASSERT_IDS="
deleg-loop-extractable
curl-common-present
delegation-curl-actually-invoked
effective-max-time-parses
effective-max-time-ge-parent-leg
effective-max-time-not-generic-30
curl-timeout-28-not-retried
cold-start-5xx-still-retried
"

# ── extraction ───────────────────────────────────────────────────────────────

# Print the delegation retry loop: from its `for`/`while` header through the
# `done` at the same indentation. Prints nothing if there is no such loop.
extract_deleg_loop() {
  awk '
    !inb && /^[[:space:]]*(for|while)[[:space:]].*DELEG_ATTEMPT/ {
      match($0, /^[[:space:]]*/); ind = substr($0, 1, RLENGTH)
      inb = 1; print; next
    }
    inb { print; if ($0 == ind "done") exit }
  ' "$1"
}

# ── the executable probe ─────────────────────────────────────────────────────

P_OUT=""
mark() { P_OUT="$P_OUT$1|$2"$'\n'; }
fail_remaining() {
  local id
  for id in $ASSERT_IDS; do
    case "$P_OUT" in *"$id|"*) ;; *) mark "$id" FAIL ;; esac
  done
}

# Number of stub-curl invocations recorded in an argv log. Always prints a
# single integer: a never-created log (curl never ran) counts as 0.
invocations() {
  local n
  n=$(grep -c '^--INVOCATION--$' "$1" 2>/dev/null)
  case "$n" in ''|*[!0-9]*) n=0 ;; esac
  printf '%s' "$n"
}

# The EFFECTIVE --max-time of the Nth recorded invocation, applying curl's
# last-flag-wins rule over the fully expanded argv. Empty if never specified.
effective_max_time() {
  awk -v want="$2" '
    $0 == "--INVOCATION--" { n++; next }
    n != want { next }
    take == 1 { v = $0; take = 0; next }
    $0 == "--max-time" || $0 == "-m" { take = 1; next }
    /^--max-time=/ { s = $0; sub(/^--max-time=/, "", s); v = s; next }
    END { print v }
  ' "$1"
}

# Run the extracted loop once with a scripted stub curl.
#   $1 runner script  $2 scratch dir  $3 curl rc  $4 http code  $5 body
run_loop() {
  ( STUB_DIR="$2" STUB_RC="$3" STUB_CODE="$4" STUB_BODY="$5" \
      bash "$1" >"$2/stdout" 2>&1 )
  return 0
}

# probe <harness-path> — prints "<assert-id>|PASS|FAIL" lines.
probe() {
  local target="$1" d loop runner cc n_timeout n_cold eff
  P_OUT=""
  d=$(mktemp -d "$E2E_TMPDIR/probe.XXXXXX") || { fail_remaining; printf '%s' "$P_OUT"; return 0; }
  loop="$d/loop.sh"
  runner="$d/runner.sh"

  # The delegation POST is identified by its URL shape: it is the only a2a call
  # that targets $CHILD_ID. Anything else in the harness is out of scope.
  extract_deleg_loop "$target" >"$loop" 2>/dev/null
  if ! grep -q 'workspaces/\$CHILD_ID/a2a' "$loop" || ! grep -q 'curl' "$loop"; then
    mark deleg-loop-extractable FAIL
    fail_remaining; printf '%s' "$P_OUT"; return 0
  fi
  mark deleg-loop-extractable PASS

  # Take CURL_COMMON from the harness itself so its contents (and any future
  # change to its own --max-time) are part of the effective budget under test.
  cc=$(grep -m1 '^CURL_COMMON=(' "$target")
  if [ -z "$cc" ]; then
    mark curl-common-present FAIL
    fail_remaining; printf '%s' "$P_OUT"; return 0
  fi
  mark curl-common-present PASS

  {
    printf '%s\n' 'set -uo pipefail'
    # Assert the CHECKED-IN default, not whatever the ambient env happens to say.
    printf '%s\n' 'unset E2E_DELEG_A2A_TIMEOUT_SECS'
    printf '%s\n' "$cc"
    cat <<'STUBS'
TENANT_ROUTE_HDRS=(-H "Host: stub.invalid")
TENANT_URL="https://stub.invalid"
CHILD_ID="child-stub"
PARENT_ID="parent-stub"
PARENT_WS_TOKEN="token-stub"
ORG_ID="org-stub"
DELEG_PAYLOAD='{"stub":true}'
DELEG_TMP="$STUB_DIR/body"
: >"$DELEG_TMP"
log() { :; }
sleep() { :; }                    # never actually wait out the backoff
sanitize_http_body() { cat; }
fail() { echo "HARNESS_FAIL: $*"; exit 9; }
# The whole point: record the fully expanded argv the harness really hands curl.
curl() {
  { printf '%s\n' '--INVOCATION--'; printf '%s\n' "$@"; } >>"$STUB_DIR/argv.log"
  printf '%s' "$STUB_BODY" >"$STUB_DIR/body"
  printf '%s' "$STUB_CODE"
  return "$STUB_RC"
}
STUBS
    cat "$loop"
  } >"$runner"

  # (1) transport timeout: curl exits 28 with no response. Maybe-processed, so
  #     the harness must give up after exactly one attempt.
  mkdir -p "$d/timeout"
  run_loop "$runner" "$d/timeout" 28 000 ""
  n_timeout=$(invocations "$d/timeout/argv.log")

  if [ "$n_timeout" -ge 1 ]; then
    mark delegation-curl-actually-invoked PASS
  else
    mark delegation-curl-actually-invoked FAIL
    fail_remaining; printf '%s' "$P_OUT"; return 0
  fi

  eff=$(effective_max_time "$d/timeout/argv.log" 1)
  case "$eff" in
    ''|*[!0-9]*) mark effective-max-time-parses FAIL
                 mark effective-max-time-ge-parent-leg FAIL
                 mark effective-max-time-not-generic-30 FAIL ;;
    *)           mark effective-max-time-parses PASS
                 if [ "$eff" -ge "$PARENT_LEG_BUDGET" ]; then
                   mark effective-max-time-ge-parent-leg PASS
                 else
                   mark effective-max-time-ge-parent-leg FAIL
                 fi
                 if [ "$eff" -ne "$CURL_COMMON_BUDGET" ]; then
                   mark effective-max-time-not-generic-30 PASS
                 else
                   mark effective-max-time-not-generic-30 FAIL
                 fi ;;
  esac
  mark observed-effective-max-time "${eff:-<none>}"

  if [ "$n_timeout" -eq 1 ]; then
    mark curl-timeout-28-not-retried PASS
  else
    mark curl-timeout-28-not-retried FAIL
  fi
  mark observed-attempts-on-rc28 "$n_timeout"

  # (2) cold-start 503 with a recognised body: the retry arm this leg DOES have
  #     must still fire, so the timeout fix cannot be "delete the retry".
  mkdir -p "$d/cold"
  run_loop "$runner" "$d/cold" 22 503 "Service Unavailable"
  n_cold=$(invocations "$d/cold/argv.log")
  if [ "$n_cold" -eq "$MAX_ATTEMPTS" ]; then
    mark cold-start-5xx-still-retried PASS
  else
    mark cold-start-5xx-still-retried FAIL
  fi
  mark observed-attempts-on-cold-503 "$n_cold"

  fail_remaining
  printf '%s' "$P_OUT"
}

lookup() {
  printf '%s\n' "$2" | awk -F'|' -v id="$1" '$1 == id { print $2; hit = 1 }
                                             END { if (!hit) print "MISSING" }'
}

# ── fixtures for the always-on negative control ──────────────────────────────

FIXTURE_REGRESSED="$E2E_TMPDIR/regressed_full_saas.sh"
cat >"$FIXTURE_REGRESSED" <<'REGRESSED'
CURL_COMMON=(-sS --fail-with-body --max-time 30)
  for DELEG_ATTEMPT in $(seq 1 12); do
    : >"$DELEG_TMP"
    set +e
    DELEG_CODE=$(curl "${CURL_COMMON[@]}" \
      -X POST "$TENANT_URL/workspaces/$CHILD_ID/a2a" \
      -H "Authorization: Bearer $PARENT_WS_TOKEN" \
      -d "$DELEG_PAYLOAD" \
      -o "$DELEG_TMP" \
      -w '%{http_code}' \
      2>/dev/null)
    DELEG_RC=$?
    set -e
    DELEG_CODE=${DELEG_CODE:-000}
    if [ "$DELEG_RC" = "0" ] && [ "$DELEG_CODE" -ge 200 ] && [ "$DELEG_CODE" -lt 300 ]; then
      break
    fi
    if [ "$DELEG_RC" = "28" ] && [ "$DELEG_ATTEMPT" -lt 12 ]; then
      sleep 10
      continue
    fi
    break
  done
REGRESSED

FIXTURE_ABSENT="$E2E_TMPDIR/absent_full_saas.sh"
cat >"$FIXTURE_ABSENT" <<'ABSENT'
# A harness with no delegation leg at all. The guard must fail CLOSED, not
# quietly report success on a call it never found.
echo "no delegation here"
ABSENT

# ── run ──────────────────────────────────────────────────────────────────────

rc=0
pass=0
fail=0

echo "── delegation A2A leg: executed against a stub curl ──"
echo "   target: $SAAS"
CLEAN=$(probe "$SAAS")
printf '   observed: effective --max-time=%s  attempts@rc28=%s  attempts@503=%s\n' \
  "$(lookup observed-effective-max-time "$CLEAN")" \
  "$(lookup observed-attempts-on-rc28 "$CLEAN")" \
  "$(lookup observed-attempts-on-cold-503 "$CLEAN")"
for id in $ASSERT_IDS; do
  r=$(lookup "$id" "$CLEAN")
  if [ "$r" = "PASS" ]; then
    echo "  PASS: $id"; pass=$((pass + 1))
  else
    echo "  FAIL: $id"; fail=$((fail + 1)); rc=1
  fi
done

echo "── negative control 1/2: regressed fixture (pre-fix budget + retry-on-rc=28) ──"
REGRESSED_OUT=$(probe "$FIXTURE_REGRESSED")
for id in effective-max-time-ge-parent-leg effective-max-time-not-generic-30 \
          curl-timeout-28-not-retried cold-start-5xx-still-retried; do
  r=$(lookup "$id" "$REGRESSED_OUT")
  if [ "$r" = "FAIL" ]; then
    echo "  OK (red as required): $id"
  else
    echo "  CONTROL BROKEN: $id reported $r against the regressed fixture"
    rc=1
  fi
done

echo "── negative control 2/2: absent fixture (guard must fail closed) ──"
ABSENT_OUT=$(probe "$FIXTURE_ABSENT")
for id in $ASSERT_IDS; do
  r=$(lookup "$id" "$ABSENT_OUT")
  if [ "$r" = "FAIL" ]; then
    echo "  OK (red as required): $id"
  else
    echo "  CONTROL BROKEN: $id reported $r against the absent fixture"
    rc=1
  fi
done

echo
echo "  passed=$pass failed=$fail"
if [ "$rc" -ne 0 ]; then
  echo "❌ delegation A2A timeout guard FAILED"
  exit 1
fi
echo "✅ delegation A2A timeout guard PASSED"
