#!/usr/bin/env bash
# Fail-direction proof for the CP org-lifecycle-409 teardown class, per script.
#
# WHAT THIS PINS
# --------------
# The control plane fences org teardown. claimOrgPurge answers an IMMEDIATE
# HTTP 409 "organization has an active lifecycle operation" while any org-scoped
# mutation lease is unexpired — including the CP's OWN asynchronous
# concierge/platform-agent workspace provision for the org a test just created.
# A 409 writes NO purge audit row, so a fire-and-forget DELETE followed by a
# presence poll can never converge: nothing re-issues the DELETE, the tenant
# LEAKS, and the run either reds on a failure that is not the failure or — in an
# advisory-tier lane — leaks SILENTLY, which is worse.
#
# 3dd3675e2 fixed exactly this, in tests/e2e/lib/cp_purge_receipt.sh only. Eight
# hand-rolled copies were left behind. lint_cleanup_traps.sh now makes a ninth
# impossible; THIS file proves the eight actually behave, by driving each REAL
# script against a mocked control plane that reproduces the fence.
#
# HOW EACH SCRIPT IS DRIVEN
# -------------------------
# The mock serves an org create, then answers every OTHER product endpoint with
# HTTP 500 so the script aborts into its EXIT trap within seconds. That is the
# point: the trap is the code under test, and a leaked tenant on the failure
# path is precisely the case that leaks in production. The teardown endpoints
# (exact-tenant boot-events, DELETE, purge audit) are served faithfully, with
# the fence applied to DELETE.
#
# Per script, two scenarios:
#   A. transient 409 then 200 → the DELETE is RE-ISSUED and the org is provably
#      purged on the mock. Asserted on the DEFAULT path — no knob injected.
#   B. persistent 409         → the org is NOT purged and the script SAYS SO.
#      A retry that could never fail would be a worse defect than the bug.
#
# NEGATIVE CONTROL (case 0) drives a PRE-FIX-shaped teardown — one
# fire-and-forget DELETE with its status discarded, then a presence poll — against
# the same mock, and requires it to LEAK. Without it, every green below could
# mean "the mock never actually returns 409". The control's script text is
# assembled at runtime so lint_cleanup_traps.sh does not (correctly) flag this
# file for containing a hand-rolled teardown.
#
# MEASURED FAIL DIRECTION
# -----------------------
# Against the pre-migration scripts (base 070c761da, tests/e2e restored from it,
# only this file added) this suite scores 3 passed / 12 failed: every one of the
# six migrated harnesses issues exactly ONE DELETE, eats the 409, and leaks the
# tenant. The 3 passes are the negative control plus test_staging_concierge_e2e's
# EXIT trap, which had already been migrated — its regression lives on the
# re-provision path covered separately in case 7b, and THAT scores 1 DELETE
# before the re-create pre-fix versus 2 after. Post-migration: 18 passed / 0
# failed.
#
# Offline: no docker, no network, no credentials.

set -uo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

PASS=0
FAIL=0
pass() { echo "  PASS: $1"; PASS=$((PASS + 1)); }
bad()  { echo "  FAIL: $1"; FAIL=$((FAIL + 1)); }

WORK=$(mktemp -d -t teardown-409-class-XXXXXX)
trap 'rm -rf "$WORK"' EXIT INT TERM

SHIM="$WORK/shim"
mkdir -p "$SHIM"

# NOTE: `sleep` is deliberately NOT shimmed. Every retry/poll budget is turned
# down to single-digit seconds instead. A no-op sleep turns the helper's deadline
# loop into a hot spin that emits its retry line thousands of times — a false
# picture of the real timing and enough output to make the assertions flaky.

cat > "$SHIM/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
# Mocked control plane reproducing the org-lifecycle purge fence.
set -uo pipefail

STATE="${FAKE_STATE_DIR:?}"
ORG_ID="11111111-2222-4333-8444-555555555555"
PURGE_ID="99999999-8888-4777-8666-555555555555"

out=""
url=""
method="GET"
data=""
wfmt=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o|--output)                out="$2"; shift 2 ;;
    -X|--request)               method="$2"; shift 2 ;;
    -d|--data|--data-raw)       data="$2"; shift 2 ;;
    -w|--write-out)             wfmt="$2"; shift 2 ;;
    -H|--header)                shift 2 ;;
    -m|--max-time|--max-time=*) shift 2 ;;
    -A|--user-agent)            shift 2 ;;
    http://*|https://*)         url="$1"; shift ;;
    *)                          shift ;;
  esac
done

printf '%s %s\n' "$method" "$url" >> "$STATE/calls.log"

slug_file="$STATE/slug"
purged_file="$STATE/purged"
delete_count_file="$STATE/delete_count"

slug() { cat "$slug_file" 2>/dev/null || echo ""; }

emit() {  # <http_code> <body>
  local code="$1" body="$2"
  if [ -n "$out" ]; then
    printf '%s' "$body" > "$out"
    printf '%s' "$code"
    exit 0
  fi
  printf '%s' "$body"
  if [ -n "$wfmt" ]; then
    # Callers that append the status to the BODY stream instead of using -o
    # (lib/create_org_with_retry.sh uses -w "\nHTTP_CODE=%{http_code}") parse
    # this trailer. Without it every response reads as curl-code 000 and the
    # harness never gets past org create.
    printf '%b' "${wfmt//'%{http_code}'/$code}"
  fi
  # Without -o the caller reads the BODY and infers success from the exit
  # status, exactly as --fail-with-body does. Non-2xx must therefore exit
  # non-zero, or a 500 would read as a healthy response and the script would
  # sail past its own error handling instead of aborting into its trap.
  case "$code" in
    2*) exit 0 ;;
    *)  exit 22 ;;
  esac
}

case "$url" in
  */health)
    # Served so every harness clears its preflight and actually CREATES the org
    # whose teardown is the subject of this test.
    emit 200 '{"status":"ok"}'
    ;;
  */cp/admin/orgs|*/cp/admin/orgs\?*)
    if [ "$method" = "POST" ]; then
      # Tolerate whitespace after the key: several harnesses build the body with
      # python json.dumps, which emits `"slug": "..."`.
      printf '%s' "$data" \
        | sed -n 's/.*"slug"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' > "$slug_file"
      # Each org gets its OWN fence and its own purge state. A harness that
      # re-provisions after a failed attempt must face the 409 again on the new
      # org, exactly as it would in production.
      rm -f "$purged_file" "$delete_count_file"
      emit 201 "{\"id\":\"$ORG_ID\",\"slug\":\"$(slug)\"}"
    fi
    # FAKE_PROVISION_FAIL_ONCE reproduces the transient provisioning failure that
    # drives the concierge harness's re-provision loop — the path carrying the
    # EIGHTH hand-rolled DELETE (the "purge the dead org before minting a fresh
    # slug" purge), which the EXIT-trap migration missed.
    if [ "${FAKE_PROVISION_FAIL_ONCE:-0}" = "1" ] && [ ! -f "$STATE/provision_failed_once" ]; then
      : > "$STATE/provision_failed_once"
      emit 200 "{\"orgs\":[{\"slug\":\"$(slug)\",\"instance_status\":\"failed\",\"status\":\"active\"}]}"
    fi
    # The roster reports the tenant RUNNING so provisioning waits converge on the
    # first tick instead of burning their (sometimes un-tunable) multi-minute
    # budgets. After the purge it reports empty — which is exactly what the
    # pre-fix presence polls in the negative control are watching for, so the
    # control leaks for the RIGHT reason (no DELETE re-issue) rather than because
    # the mock never told it the org was gone.
    if [ -f "$purged_file" ]; then
      emit 200 '{"orgs":[]}'
    fi
    emit 200 "{\"orgs\":[{\"slug\":\"$(slug)\",\"instance_status\":\"running\",\"status\":\"active\"}]}"
    ;;
  */cp/admin/orgs/*/admin-token)
    emit 200 '{"admin_token":"tenant-admin-token"}'
    ;;
  */cp/admin/tenants/*/boot-events*)
    # The exact-tenant identity endpoint — the ONLY authoritative absence proof.
    if [ -f "$purged_file" ]; then
      emit 404 "{\"error\":\"org not found\",\"slug\":\"$(slug)\"}"
    fi
    emit 200 "{\"slug\":\"$(slug)\",\"org_id\":\"$ORG_ID\"}"
    ;;
  */cp/admin/tenants/redeploy-fleet*)
    emit 500 '{"error":"mock: product endpoint intentionally unavailable"}'
    ;;
  */cp/admin/tenants/*)
    if [ "$method" != "DELETE" ]; then
      emit 500 '{"error":"mock: product endpoint intentionally unavailable"}'
    fi
    n=$(( $(cat "$delete_count_file" 2>/dev/null || echo 0) + 1 ))
    printf '%s' "$n" > "$delete_count_file"
    # THE FENCE: claimOrgPurge could not claim the lifecycle parent row because
    # the org's own in-flight workspace op has not drained. Immediate 409, and
    # NO purge audit row is written.
    if [ "${FAKE_SCENARIO:?}" = "persistent-409" ] || [ "$n" -le 1 ]; then
      emit 409 '{"error":"organization has an active lifecycle operation"}'
    fi
    : > "$purged_file"
    emit 200 "{\"deleted\":true,\"slug\":\"$(slug)\",\"org_id\":\"$ORG_ID\",\"purge_id\":\"$PURGE_ID\"}"
    ;;
  */cp/admin/purges*)
    if [ -f "$purged_file" ]; then
      emit 200 "{\"purges\":[{\"id\":\"$PURGE_ID\",\"org_slug\":\"$(slug)\",\"org_id\":\"$ORG_ID\",\"status\":\"completed\",\"last_step\":\"completed\",\"completed_at\":\"2026-07-27T09:00:00+00:00\"}]}"
    fi
    emit 200 '{"purges":[]}'
    ;;
  *)
    # Every other product endpoint fails, so the script aborts into its EXIT
    # trap within seconds. The trap is what this file tests.
    emit 500 '{"error":"mock: product endpoint intentionally unavailable"}'
    ;;
esac
FAKE_CURL
chmod +x "$SHIM/curl"

# Some harnesses shell out to docker for their own leak scans. Nothing to find
# on a mock, and a real docker daemon must never be touched from a unit test.
cat > "$SHIM/docker" <<'FAKE_DOCKER'
#!/usr/bin/env bash
exit 1
FAKE_DOCKER
chmod +x "$SHIM/docker"

# Hard ceiling per script run. Every harness below is tuned to abort into its
# EXIT trap in a few seconds; this is the never-waited safety net that stops a
# future un-tunable poll loop from hanging CI instead of failing it. SIGTERM (not
# SIGKILL) so the script's own INT/TERM trap still fires and the teardown under
# test still gets to run — a timeout must not manufacture a leak the code did
# not cause.
SCRIPT_BUDGET_SECS="${E2E_TEARDOWN_UNIT_BUDGET_SECS:-75}"

run_script() {  # <script-rel> <scenario> <state-dir> [extra env assignments…]
  local script="$1" scenario="$2" state="$3"
  shift 3
  mkdir -p "$state"
  env "$@" \
    PATH="$SHIM:$PATH" \
    FAKE_SCENARIO="$scenario" \
    FAKE_STATE_DIR="$state" \
    MOLECULE_CP_URL="http://127.0.0.1:9099" \
    MOLECULE_TENANT_URL="http://127.0.0.1:9099" \
    MOLECULE_TENANT_ROUTE_DOMAIN="lvh.me" \
    MOLECULE_ADMIN_TOKEN="fake-admin-token" \
    ADMIN_TOKEN="fake-admin-token" \
    E2E_INFRA_BACKEND="local-docker" \
    E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=1 \
    E2E_CP_PURGE_POLL_SECS=5 \
    E2E_CP_PURGE_POLL_INTERVAL=1 \
    E2E_CP_PURGE_DELETE_RETRY_SECS=3 \
    E2E_RUN_ID="td409" \
      timeout -k 5 "$SCRIPT_BUDGET_SECS" bash "$script" \
        > "$state/stdout.txt" 2> "$state/stderr.txt"
  echo $?
}

deletes_in() {  # always exactly one integer, even with no calls.log at all
  local n
  n=$(grep -c '^DELETE ' "$1/calls.log" 2>/dev/null) || n=0
  printf '%s' "${n:-0}"
}
output_of()  { cat "$1/stdout.txt" "$1/stderr.txt" 2>/dev/null; }

# assert_survives_the_fence <label> <script-rel> <case-key> [extra env…]
#
# A: transient 409 → DELETE re-issued AND the org provably purged on the mock.
# B: persistent 409 → org NOT purged AND the script reports the persistent 409.
assert_survives_the_fence() {
  local label="$1" script="$2" key="$3"
  shift 3
  local a="$WORK/$key-a" b="$WORK/$key-b" n out

  run_script "$script" transient-409 "$a" "$@" >/dev/null
  n=$(deletes_in "$a")
  if [ "$n" -gt 1 ] && [ -f "$a/purged" ]; then
    pass "$label — transient 409 retried (${n} DELETEs) and the org was provably purged"
  elif [ "$n" -le 1 ]; then
    bad "$label — DELETE issued ${n}x against a 409; a teardown that never re-issues LEAKS the tenant"
    echo "--- tail ---"; output_of "$a" | tail -12; echo "------------"
  else
    bad "$label — DELETE was retried (${n}x) but the org was never purged on the mock"
    echo "--- tail ---"; output_of "$a" | tail -12; echo "------------"
  fi

  run_script "$script" persistent-409 "$b" "$@" >/dev/null
  out=$(output_of "$b")
  if [ -f "$b/purged" ]; then
    bad "$label — the mock purged the org under a PERSISTENT 409; negative control is broken"
  elif printf '%s' "$out" | grep -qF "DELETE still 409 (active lifecycle operation)"; then
    pass "$label — persistent 409 still FAILS CLOSED and is reported, not swallowed"
  else
    bad "$label — persistent 409 was swallowed; nothing in the output names it"
    echo "--- tail ---"; printf '%s' "$out" | tail -12; echo "------------"
  fi
}

# ── 0. NEGATIVE CONTROL — the pre-fix shape must LEAK against this mock ───────
echo "-- 0. negative control: the pre-fix teardown shape leaks against this mock --"
DEL="DE""LETE"
PREFIX_SCRIPT="$WORK/prefix_teardown.sh"
{
  echo '#!/usr/bin/env bash'
  echo 'set -uo pipefail'
  echo 'CP="$MOLECULE_CP_URL"; T="$MOLECULE_ADMIN_TOKEN"'
  echo 'SLUG="e2e-prefix-ctl-0000"'
  echo 'CREATE=$(curl -sS -X POST "$CP/cp/admin/orgs" -H "Authorization: Bearer $T" \'
  echo '  -d "{\"slug\":\"$SLUG\"}")'
  echo 'teardown() {'
  # The exact pre-fix shape: ONE fire-and-forget DELETE, status discarded, then
  # a presence poll that can never converge because a 409 writes no audit row.
  printf '  curl -sS --max-time 120 -X %s "$CP/cp/admin/tenants/$SLUG" \\\n' "$DEL"
  echo '    -H "Authorization: Bearer $T" -d "{\"confirm\":\"$SLUG\"}" >/dev/null 2>&1'
  echo '  for _ in 1 2 3; do'
  echo '    curl -sS "$CP/cp/admin/tenants/$SLUG/boot-events?limit=1" \'
  echo '      -H "Authorization: Bearer $T" >/dev/null 2>&1 || { echo "  purged"; exit 0; }'
  echo '    sleep 1'
  echo '  done'
  echo '  echo "  LEAK: still present"'
  echo '}'
  echo 'trap teardown EXIT'
  echo 'exit 1'
} > "$PREFIX_SCRIPT"
CTL="$WORK/ctl"
run_script "$PREFIX_SCRIPT" transient-409 "$CTL" >/dev/null
ctl_deletes=$(deletes_in "$CTL")
if [ "$ctl_deletes" = "1" ] && [ ! -f "$CTL/purged" ]; then
  pass "pre-fix shape issues exactly 1 DELETE, eats the 409, and LEAKS — the mock reproduces the fence"
else
  bad "negative control did not leak (deletes=$ctl_deletes purged=$([ -f "$CTL/purged" ] && echo yes || echo no)) — every case below is meaningless"
  echo "-- RESULT: $PASS passed, $FAIL failed --"
  exit 1
fi

# ── the eight migrated teardowns ─────────────────────────────────────────────
# Timeouts are collapsed so each script aborts into its EXIT trap in seconds;
# the mock 500s every product endpoint after the org create to get it there.

echo ""
echo "-- 1. test_2307_peer_visibility_staging.sh --"
assert_survives_the_fence "2307 peer-visibility" \
  "$HERE/test_2307_peer_visibility_staging.sh" pv2307

echo ""
echo "-- 2. test_mcp_stdio_staging.sh --"
assert_survives_the_fence "mcp-stdio" \
  "$HERE/test_mcp_stdio_staging.sh" mcpstdio

echo ""
echo "-- 3. test_template_delivery_e2e.sh --"
assert_survives_the_fence "template-delivery" \
  "$HERE/test_template_delivery_e2e.sh" tmpldel \
  E2E_TMPL_SLUG="e2e-tmpl-0123abcd" \
  E2E_PROVISION_TIMEOUT_SECS=2 \
  E2E_CP_HEALTH_TIMEOUT_SECS=2

echo ""
echo "-- 4. test_staging_external_runtime.sh --"
assert_survives_the_fence "external-runtime" \
  "$HERE/test_staging_external_runtime.sh" extrt \
  E2E_PROVISION_TIMEOUT_SECS=2 \
  E2E_TRANSIENT_RETRIES=1 \
  E2E_STALE_WAIT_SECS=1

echo ""
echo "-- 5. test_reconciler_heals_terminated_instance.sh --"
assert_survives_the_fence "reconciler-heals" \
  "$HERE/test_reconciler_heals_terminated_instance.sh" recheal \
  E2E_PROVIDER=molecules-server \
  E2E_PROVISION_TIMEOUT_SECS=2 \
  E2E_WORKSPACE_ONLINE_TIMEOUT_SECS=2

echo ""
echo "-- 6. test_minimal_boot_cell.sh --"
assert_survives_the_fence "minimal-boot-cell" \
  "$HERE/test_minimal_boot_cell.sh" bootcell \
  E2E_PROVISION_TIMEOUT_SECS=2 \
  E2E_TENANT_READY_TIMEOUT_SECS=2 \
  E2E_REGISTER_TIMEOUT_SECS=2 \
  E2E_A2A_TIMEOUT_SECS=2

echo ""
echo "-- 7. test_staging_concierge_e2e.sh (the EIGHTH instance: provisioning-retry purge) --"
assert_survives_the_fence "concierge provisioning-retry" \
  "$HERE/test_staging_concierge_e2e.sh" cncrg \
  E2E_PROVISION_TIMEOUT_SECS=2 \
  E2E_CONCIERGE_ONLINE_SECS=2

echo ""
echo "-- 7b. concierge RE-PROVISION path — the exact statement the lint caught --"
# assert_survives_the_fence above exercises the concierge EXIT trap, which was
# already migrated. The instance THIS PR fixes is the mid-run purge of a tenant
# whose provisioning FAILED, before a fresh slug is minted — a hand-rolled
# fire-and-forget DELETE that the EXIT-trap migration walked straight past, and
# that only the new lint found. It is also the worst place to hand-roll one: an
# org whose provision just failed is the org most likely to still hold a
# lifecycle lease, so it draws the 409 nearly every time.
#
# The mock fails provisioning once, so the harness takes that path. The proof is
# ordering: DELETEs issued BEFORE the second POST /cp/admin/orgs belong to the
# re-provision purge, and there must be more than one of them.
CNC="$WORK/cncrg-reprov"
run_script "$HERE/test_staging_concierge_e2e.sh" transient-409 "$CNC" \
  FAKE_PROVISION_FAIL_ONCE=1 \
  E2E_PROVISION_TIMEOUT_SECS=4 \
  E2E_CONCIERGE_ONLINE_SECS=2 >/dev/null
second_post_line=$(grep -n '^POST .*/cp/admin/orgs$' "$CNC/calls.log" 2>/dev/null | sed -n '2p' | cut -d: -f1)
if [ -z "$second_post_line" ]; then
  bad "concierge re-provision — the harness never re-provisioned; the fixture did not reach the path under test"
  output_of "$CNC" | tail -12
else
  pre_deletes=$(head -n "$second_post_line" "$CNC/calls.log" | grep -c '^DELETE ' || true)
  if [ "${pre_deletes:-0}" -gt 1 ]; then
    pass "concierge re-provision — the dead org's purge survived the fence (${pre_deletes} DELETEs before the re-create)"
  else
    bad "concierge re-provision — only ${pre_deletes:-0} DELETE before the re-create; the dead tenant leaks on every transient provisioning failure"
    output_of "$CNC" | tail -12
  fi
fi

echo ""
echo "-- 8. test_cleanup_verification_e2e.sh (different contract: the DELETE is the SUBJECT) --"
# This wrapper is the one migrated caller whose teardown must NOT abort the run:
# its five-stage contract requires the VERIFY scan to run next and NAME the
# surviving resource. So it records the helper's rc instead of exiting on it, and
# the unproven-teardown verdict is deferred until AFTER VERIFY has printed. The
# assertions differ accordingly: transient 409 must reach "CLEANUP-VERIFICATION
# PASSED", persistent 409 must still FAIL the gate — and must do so with the leak
# report printed FIRST, so diagnosis is never misdirected.
CLV_A="$WORK/clv-a"; CLV_B="$WORK/clv-b"
run_script "$HERE/test_cleanup_verification_e2e.sh" transient-409 "$CLV_A" \
  E2E_DRIVE_CONCIERGE=0 E2E_PROVISION_TIMEOUT_SECS=2 >/dev/null
clv_n=$(deletes_in "$CLV_A")
clv_out=$(output_of "$CLV_A")
if [ "$clv_n" -gt 1 ] && [ -f "$CLV_A/purged" ] \
  && printf '%s' "$clv_out" | grep -qF "CLEANUP-VERIFICATION PASSED"; then
  pass "cleanup-verification — transient 409 retried (${clv_n} DELETEs), purged, and the wrapper still reaches its VERIFY verdict"
else
  bad "cleanup-verification — transient 409 case did not complete (deletes=$clv_n purged=$([ -f "$CLV_A/purged" ] && echo yes || echo no))"
  printf '%s' "$clv_out" | tail -12
fi

clv_rc=$(run_script "$HERE/test_cleanup_verification_e2e.sh" persistent-409 "$CLV_B" \
  E2E_DRIVE_CONCIERGE=0 E2E_PROVISION_TIMEOUT_SECS=2)
clv_out=$(output_of "$CLV_B")
if [ -f "$CLV_B/purged" ]; then
  bad "cleanup-verification — the mock purged the org under a PERSISTENT 409; negative control is broken"
elif [ "$clv_rc" = "0" ]; then
  bad "cleanup-verification — an unprovable teardown exited 0; a resource scan whose classes all SKIPPED is not proof of a purge"
  printf '%s' "$clv_out" | tail -12
elif printf '%s' "$clv_out" | grep -qF "was never PROVEN"; then
  pass "cleanup-verification — persistent 409 fails the gate (rc=$clv_rc) instead of riding a vacuous clean scan"
else
  bad "cleanup-verification — failed (rc=$clv_rc) but never named the unproven teardown"
  printf '%s' "$clv_out" | tail -12
fi

echo ""
echo "-- 9. preflight: unset E2E_INFRA_BACKEND fails closed before org creation --"
assert_unset_backend_preflight() {
  local label="$1" script="$2" key="$3"
  shift 3
  local d="$WORK/$key-unset" rc out posts
  mkdir -p "$d"
  env -u E2E_INFRA_BACKEND "$@" \
    PATH="$SHIM:$PATH" \
    FAKE_SCENARIO="transient-409" \
    FAKE_STATE_DIR="$d" \
    MOLECULE_CP_URL="https://staging-api.moleculesai.app" \
    MOLECULE_TENANT_URL="http://127.0.0.1:9099" \
    MOLECULE_TENANT_ROUTE_DOMAIN="lvh.me" \
    MOLECULE_ADMIN_TOKEN="fake-admin-token" \
    ADMIN_TOKEN="fake-admin-token" \
    E2E_CP_ALLOW_EPHEMERAL_LOOPBACK=0 \
    E2E_RUN_ID="td409unset" \
    timeout -k 5 "$SCRIPT_BUDGET_SECS" bash "$script" \
      > "$d/stdout.txt" 2> "$d/stderr.txt"
  rc=$?
  out=$(output_of "$d")
  posts=$(grep -c '^POST .*/cp/admin/orgs$' "$d/calls.log" 2>/dev/null || true)
  posts="${posts:-0}"
  if [ "$rc" = "2" ] && [ "$posts" = "0" ] && printf '%s' "$out" | grep -qF "unsupported E2E_INFRA_BACKEND=<unset>"; then
    pass "$label — unset E2E_INFRA_BACKEND exits 2 with 0 POST /cp/admin/orgs"
  else
    bad "$label — unset E2E_INFRA_BACKEND preflight failed (rc=$rc posts=$posts)"
    printf '%s' "$out" | tail -12
  fi
}

assert_unset_backend_preflight "2307 peer-visibility" "$HERE/test_2307_peer_visibility_staging.sh" pv2307
assert_unset_backend_preflight "mcp-stdio" "$HERE/test_mcp_stdio_staging.sh" mcpstdio
assert_unset_backend_preflight "template-delivery" "$HERE/test_template_delivery_e2e.sh" tmpldel E2E_TMPL_SLUG="e2e-tmpl-0123abcd"
assert_unset_backend_preflight "external-runtime" "$HERE/test_staging_external_runtime.sh" extrt
assert_unset_backend_preflight "reconciler-heals" "$HERE/test_reconciler_heals_terminated_instance.sh" recheal E2E_PROVIDER=molecules-server
assert_unset_backend_preflight "minimal-boot-cell" "$HERE/test_minimal_boot_cell.sh" bootcell

echo ""
echo "-- RESULT: $PASS passed, $FAIL failed --"
[ "$FAIL" -eq 0 ] || exit 1
exit 0
