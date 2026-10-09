#!/usr/bin/env bash
# test-require-deploy-toolchain.sh — BOTH-DIRECTION tests for the deploy
# toolchain preflight and for the credential-capture shape it protects.
#
# The failure this replaces (run 644986) was an ABSENT interpreter, so the
# tempting "fix" is anything that fails loudly — and a preflight that fails on
# EVERY input would also "handle" a missing python3. So every property below is
# asserted in both directions: the guard FIRES when the tool/credential is
# missing AND the good path still resolves and still reports its real work.
#
#   1. present  => resolves to the ABSOLUTE path, exit 0, VAR=path on stdout
#   2. absent + no provisioning => exit 1, ::error:: NAMING the command
#   3. absent + provisioning available => installs, then resolves (exit 0)
#   4. absent + provisioning FAILS => exit 1, ::error:: naming command+package
#   5. present but UNRUNNABLE => exit 1 (existence is not capability)
#   6. package installs but does not provide the command => exit 1
#   7. ALL-OR-NOTHING stdout: one bad spec in a batch emits NO partial env
#   8. no specs at all => exit 2 (an empty toolchain must not report success)
#   9. THE `-e` SHAPE ITSELF: a bare `V="$(failing)"` dies ABOVE its own guard,
#      while the rc-captured form reaches the guard. This is the defect the
#      workflow change fixes, asserted directly under the runner's real shell
#      flags (`bash --noprofile --norc -e -o pipefail`).
#  10. exit code 10 (secret ABSENT) is distinguishable from any other reader
#      failure, and BOTH still fail closed.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/../require-deploy-toolchain.sh"
[ -f "$script" ] || { echo "FAIL: $script missing" >&2; exit 1; }

pass=0
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { pass=$((pass + 1)); echo "ok $pass - $*"; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/empty" "$tmp/stage"

# A stand-in "already installed" tool: resolves and runs.
cat > "$tmp/bin/goodtool" <<'GOOD'
#!/usr/bin/env bash
echo "goodtool 1.2.3"
GOOD
# Present on PATH but cannot run: the vacuous-`command -v` shape.
cat > "$tmp/bin/brokentool" <<'BROKEN'
#!/usr/bin/env bash
exit 3
BROKEN
chmod +x "$tmp/bin/goodtool" "$tmp/bin/brokentool"

# The binary a successful fake-apk "installs": staged, then copied onto PATH.
cat > "$tmp/stage/latertool" <<'LATER'
#!/usr/bin/env bash
echo "latertool 9.9"
LATER
chmod +x "$tmp/stage/latertool"

# ── Fake apk. FAKE_APK_MODE drives it; every invocation is logged so a test
#    can assert that provisioning was ATTEMPTED (or not) rather than infer it.
cat > "$tmp/bin/fakeapk" <<'APK'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${FAKE_APK_LOG:?}"
case "${FAKE_APK_MODE:-ok}" in
  ok)
    if [ "${1:-}" = "add" ]; then cp "${FAKE_APK_STAGE:?}/latertool" "${FAKE_APK_BIN:?}/latertool"; fi
    ;;
  addfail)   [ "${1:-}" = "add" ] && exit 1 ;;
  indexfail) [ "${1:-}" = "update" ] && exit 1 ;;
  noprovide) : ;;   # succeeds but installs nothing
esac
exit 0
APK
chmod +x "$tmp/bin/fakeapk"

run_tc() {
  # Always runs with a PATH that contains ONLY the fixture bin plus the real
  # coreutils the script itself needs; never the developer's ambient PATH.
  env PATH="$1:/usr/bin:/bin" \
      FAKE_APK_LOG="$tmp/apk.log" \
      FAKE_APK_MODE="${FAKE_APK_MODE:-ok}" \
      FAKE_APK_STAGE="$tmp/stage" \
      FAKE_APK_BIN="$2" \
      DEPLOY_TOOLCHAIN_APK="${DEPLOY_TOOLCHAIN_APK-}" \
      DEPLOY_TOOLCHAIN_NO_INSTALL="${DEPLOY_TOOLCHAIN_NO_INSTALL-0}" \
      RUNNER_NAME="fixture-runner" \
      bash "$script" "${@:3}"
}

# ── 1. GOOD PATH: present tool resolves to its absolute path ────────────────
out=""; rc=0
out="$(DEPLOY_TOOLCHAIN_NO_INSTALL=1 run_tc "$tmp/bin" "$tmp/bin" 'TOOL_A:goodtool:goodpkg')" || rc=$?
[ "$rc" -eq 0 ] || fail "1: present tool should exit 0, got $rc"
[ "$out" = "TOOL_A=$tmp/bin/goodtool" ] || fail "1: expected absolute-path env line, got '$out'"
ok "present tool resolves to an absolute path and exits 0"

# ── 2. GUARD FIRES: absent tool, provisioning refused ───────────────────────
err=""; rc=0
err="$(DEPLOY_TOOLCHAIN_NO_INSTALL=1 run_tc "$tmp/empty" "$tmp/empty" 'TOOL_A:goodtool:goodpkg' 2>&1 >/dev/null)" || rc=$?
[ "$rc" -ne 0 ] || fail "2: absent tool must NOT exit 0"
case "$err" in
  *"::error::"*"goodtool"*) ;;
  *) fail "2: diagnostic must be a ::error:: NAMING the command; got '$err'" ;;
esac
case "$err" in
  *"deploy-runner.Dockerfile"*) ;;
  *) fail "2: diagnostic must name the durable remedy; got '$err'" ;;
esac
ok "absent tool fails closed with a ::error:: naming the command and the remedy"

# ── 3. GOOD PATH: absent tool, provisioning succeeds ────────────────────────
: > "$tmp/apk.log"
rm -f "$tmp/empty/latertool"
out=""; rc=0
out="$(FAKE_APK_MODE=ok DEPLOY_TOOLCHAIN_APK="$tmp/bin/fakeapk" \
       run_tc "$tmp/empty:$tmp/bin" "$tmp/empty" 'TOOL_B:latertool:laterpkg')" || rc=$?
[ "$rc" -eq 0 ] || fail "3: provisioned tool should exit 0, got $rc"
[ "$out" = "TOOL_B=$tmp/empty/latertool" ] || fail "3: expected provisioned path, got '$out'"
grep -q '^add --no-cache laterpkg$' "$tmp/apk.log" \
  || fail "3: the package was never actually requested; log=$(cat "$tmp/apk.log")"
ok "absent tool is provisioned by package name and then resolves"

# ── 4. GUARD FIRES: provisioning attempted and failed ───────────────────────
: > "$tmp/apk.log"
rm -f "$tmp/empty/latertool"
err=""; rc=0
err="$(FAKE_APK_MODE=addfail DEPLOY_TOOLCHAIN_APK="$tmp/bin/fakeapk" \
       run_tc "$tmp/empty:$tmp/bin" "$tmp/empty" 'TOOL_B:latertool:laterpkg' 2>&1 >/dev/null)" || rc=$?
[ "$rc" -ne 0 ] || fail "4: failed install must NOT exit 0"
case "$err" in
  *"::error::"*"latertool"*"laterpkg"*) ;;
  *) fail "4: diagnostic must name both command and package; got '$err'" ;;
esac
ok "failed provisioning fails closed and names command + package"

# ── 4b. GUARD FIRES: the package index itself is unreachable ────────────────
: > "$tmp/apk.log"
rm -f "$tmp/empty/latertool"
err=""; rc=0
err="$(FAKE_APK_MODE=indexfail DEPLOY_TOOLCHAIN_APK="$tmp/bin/fakeapk" \
       run_tc "$tmp/empty:$tmp/bin" "$tmp/empty" 'TOOL_B:latertool:laterpkg' 2>&1 >/dev/null)" || rc=$?
[ "$rc" -ne 0 ] || fail "4b: unreachable index must NOT exit 0"
case "$err" in *"::error::"*"latertool"*) ;; *) fail "4b: got '$err'" ;; esac
ok "an unreachable package index fails closed, naming the command"

# ── 5. GUARD FIRES: present but unrunnable (existence != capability) ────────
err=""; rc=0
err="$(DEPLOY_TOOLCHAIN_NO_INSTALL=1 run_tc "$tmp/bin" "$tmp/bin" 'TOOL_C:brokentool:brokenpkg' 2>&1 >/dev/null)" || rc=$?
[ "$rc" -ne 0 ] || fail "5: an unrunnable tool must NOT be accepted"
case "$err" in *"::error::"*"brokentool"*) ;; *) fail "5: got '$err'" ;; esac
ok "a present-but-unrunnable tool is rejected, not counted as satisfied"

# ── 6. GUARD FIRES: install claims success but provides no command ──────────
: > "$tmp/apk.log"
rm -f "$tmp/empty/latertool"
err=""; rc=0
err="$(FAKE_APK_MODE=noprovide DEPLOY_TOOLCHAIN_APK="$tmp/bin/fakeapk" \
       run_tc "$tmp/empty:$tmp/bin" "$tmp/empty" 'TOOL_B:latertool:laterpkg' 2>&1 >/dev/null)" || rc=$?
[ "$rc" -ne 0 ] || fail "6: a package that does not provide the command must fail"
ok "a package that installs but provides nothing is rejected"

# ── 7. ALL-OR-NOTHING: a bad spec in a batch emits NO partial env ───────────
out=""; rc=0
out="$(DEPLOY_TOOLCHAIN_NO_INSTALL=1 run_tc "$tmp/bin" "$tmp/bin" \
        'TOOL_A:goodtool:goodpkg' 'TOOL_Z:missingtool:misspkg' 2>/dev/null)" || rc=$?
[ "$rc" -ne 0 ] || fail "7: batch with a missing tool must fail"
[ -z "$out" ] || fail "7: a failed batch must emit NO env lines, got '$out'"
ok "a partially-resolvable batch emits nothing (no half-armed \$GITHUB_ENV)"

# ── 8. An EMPTY toolchain must not report success ──────────────────────────
rc=0
DEPLOY_TOOLCHAIN_NO_INSTALL=1 run_tc "$tmp/bin" "$tmp/bin" >/dev/null 2>&1 || rc=$?
[ "$rc" -eq 2 ] || fail "8: no specs should exit 2, got $rc"
ok "resolving an EMPTY toolchain is an error, not a vacuous success"

# ── 9. THE SHELL SHAPE. Reproduce the runner's real step shell exactly:
#      `bash --noprofile --norc -e -o pipefail <script>`; the script's own
#      `set` line is NOT what decides this, the invocation is.
cat > "$tmp/reader-fail" <<'RDR'
#!/usr/bin/env bash
echo "::error::reader said no" >&2
exit "${READER_RC:-1}"
RDR
chmod +x "$tmp/reader-fail"

cat > "$tmp/bare.sh" <<'BARE'
V="$("$READER")"
if [ -z "$V" ]; then
  echo "GUARD-REACHED"
fi
echo "AFTER"
BARE
cat > "$tmp/guarded.sh" <<'GUARDED'
rc=0
V="$("$READER")" || rc=$?
if [ "$rc" -ne 0 ]; then
  echo "GUARD-REACHED rc=$rc"
  exit 1
fi
if [ -z "$V" ]; then
  echo "GUARD-REACHED empty"
  exit 1
fi
echo "AFTER"
GUARDED

rc=0
out="$(READER="$tmp/reader-fail" bash --noprofile --norc -e -o pipefail "$tmp/bare.sh" 2>/dev/null)" || rc=$?
[ "$rc" -ne 0 ] || fail "9: the bare form should still fail"
case "$out" in
  *GUARD-REACHED*) fail "9: the bare form was expected to die ABOVE its guard, but the guard ran" ;;
esac
ok "a bare \$( ) capture dies above its own guard under -e (the 644986 shape)"

rc=0
out="$(READER="$tmp/reader-fail" bash --noprofile --norc -e -o pipefail "$tmp/guarded.sh" 2>/dev/null)" || rc=$?
[ "$rc" -ne 0 ] || fail "9: the guarded form must still FAIL CLOSED, not pass"
case "$out" in
  *"GUARD-REACHED rc=1"*) ;;
  *) fail "9: the rc-captured form must reach its guard; got '$out'" ;;
esac
ok "the rc-captured form reaches its guard AND still fails closed"

# ── 9b. The guarded form's GOOD direction: a working reader still produces the
#       value and still runs the work below it. Without this, test 9 would pass
#       for a change that simply made every capture fail.
cat > "$tmp/reader-ok" <<'RDR'
#!/usr/bin/env bash
printf 'REAL-VALUE'
RDR
chmod +x "$tmp/reader-ok"
rc=0
out="$(READER="$tmp/reader-ok" bash --noprofile --norc -e -o pipefail "$tmp/guarded.sh" 2>/dev/null)" || rc=$?
[ "$rc" -eq 0 ] || fail "9b: a working reader must exit 0, got $rc"
[ "$out" = "AFTER" ] || fail "9b: the good path must run the work below the guard; got '$out'"
ok "the guarded form's GOOD path still runs the work below it"

# ── 10. rc=10 (secret ABSENT) is distinguishable, and both arms fail closed ─
cat > "$tmp/triage.sh" <<'TRIAGE'
rc=0
V="$("$READER")" || rc=$?
case "$rc" in
  0)  echo "READ-OK" ;;
  10) echo "NAMED-ABSENT"; exit 1 ;;
  *)  echo "NAMED-FAILURE rc=$rc"; exit 1 ;;
esac
TRIAGE
rc=0
out="$(READER_RC=10 READER="$tmp/reader-fail" bash --noprofile --norc -e -o pipefail "$tmp/triage.sh" 2>/dev/null)" || rc=$?
[ "$rc" -ne 0 ] || fail "10: an absent secret must fail closed"
[ "$out" = "NAMED-ABSENT" ] || fail "10: expected the absent-secret arm, got '$out'"
rc=0
out="$(READER_RC=1 READER="$tmp/reader-fail" bash --noprofile --norc -e -o pipefail "$tmp/triage.sh" 2>/dev/null)" || rc=$?
[ "$rc" -ne 0 ] || fail "10: a reader error must fail closed"
[ "$out" = "NAMED-FAILURE rc=1" ] || fail "10: expected the generic-failure arm, got '$out'"
ok "secret-ABSENT and reader-ERROR are named separately and BOTH fail closed"

echo "PASS: $pass assertions"
