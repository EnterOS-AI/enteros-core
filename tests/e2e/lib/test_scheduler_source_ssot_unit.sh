#!/usr/bin/env bash
# Offline unit test for lib/native_plugin_source_lint.sh (no Docker, no network).
#
# Two jobs, and the second is the one that matters:
#   1. Prove the lint DETECTS — fixtures for a banned pin, an unknown pin, the
#      tolerated-debt pins, and a non-plugin gitea:// source that must not trip
#      it. A lint whose fail direction is never exercised is a green sticker.
#   2. Run it over the REAL tests/e2e tree and require zero violations. That is
#      the standing gate: reintroducing the scheduler pin that used to sit in
#      test_staging_full_saas.sh turns this test RED immediately, offline.
#
# NOTE FOR EDITORS: this file must never contain a complete pinned source
# literal, or job (2) would flag the test itself. Fixtures assemble their URLs
# from parts via mk_src() for exactly that reason.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
E2E_ROOT="$(cd "$HERE/.." && pwd)"
# shellcheck source=native_plugin_source_lint.sh
. "$HERE/native_plugin_source_lint.sh"

FAILED=0
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# Assemble a pinned source without ever writing one out in full here.
mk_src() { printf 'gitea://molecule-ai/%s%s%s' "$1" '#' "${2:-v9.9.9}"; }

stage() { # $1 = fixture file name, $2 = line content
  mkdir -p "$TMP/fix"
  printf '%s\n' "$2" > "$TMP/fix/$1"
}

count_violations() { native_plugin_source_violations "$@" | grep -c . || true; }

check() { # $1 desc, $2 expected, $3 actual
  if [ "$2" = "$3" ]; then
    echo "PASS [$2] $1"
  else
    echo "FAIL [want $2, got $3] $1"; FAILED=1
  fi
}

# --- the banned pin is caught ------------------------------------------------
rm -rf "$TMP/fix"
stage bad.sh "src=$(mk_src molecule-ai-plugin-scheduler v0.2.1)"
check "banned scheduler pin -> 1 violation" 1 "$(count_violations "$TMP/fix")"
native_plugin_source_violations "$TMP/fix" | grep -q 'BANNED' \
  || { echo "FAIL: banned pin not labelled BANNED"; FAILED=1; }

# A stale pin must be caught the same way — the drift is the point, not the
# version. (The literal removed from the harness was two minors behind.)
rm -rf "$TMP/fix"
stage stale.sh "src=$(mk_src molecule-ai-plugin-scheduler v0.1.0)"
check "STALE scheduler pin -> still 1 violation" 1 "$(count_violations "$TMP/fix")"

# --- an unknown plugin pin is caught (the class, not just the instance) ------
rm -rf "$TMP/fix"
stage new.sh "src=$(mk_src molecule-ai-plugin-something-new v1.2.3)"
check "unlisted plugin pin -> 1 violation" 1 "$(count_violations "$TMP/fix")"
native_plugin_source_violations "$TMP/fix" | grep -q 'NEW PIN' \
  || { echo "FAIL: unlisted pin not labelled NEW PIN"; FAILED=1; }

# --- tolerated debt does not trip it ----------------------------------------
rm -rf "$TMP/fix"
stage debt.sh "a=$(mk_src molecule-ai-plugin-schedule-self v0.1.2) b=$(mk_src molecule-ai-plugin-digest-mail v0.1.0)"
check "allowlisted debt pins -> 0 violations" 0 "$(count_violations "$TMP/fix")"

# --- non-plugin gitea:// sources are none of this lint's business ------------
rm -rf "$TMP/fix"
stage tmpl.sh 'src=gitea://molecule-ai/molecule-ai-workspace-template-seo-agent/agent-skills/seo-all#main'
check "workspace-template source -> 0 violations (not a native plugin)" 0 "$(count_violations "$TMP/fix")"

rm -rf "$TMP/fix"
stage unpinned.sh 'echo gitea://molecule-ai/molecule-ai-plugin-scheduler'
check "unpinned mention (no #ref) -> 0 violations" 0 "$(count_violations "$TMP/fix")"

# --- non-shell files are out of scope (same scope as CI's shellcheck pass) ---
rm -rf "$TMP/fix"
mkdir -p "$TMP/fix"
printf '%s\n' "# doc: $(mk_src molecule-ai-plugin-scheduler v0.2.0)" > "$TMP/fix/doc.py"
check "pin in a .py file -> 0 violations (*.sh scope)" 0 "$(count_violations "$TMP/fix")"

# --- no paths -> no output, no crash ----------------------------------------
check "no paths -> 0 violations" 0 "$(count_violations)"

# --- THE GATE: the real harness must carry no banned/unlisted pin ------------
REAL=$(native_plugin_source_violations "$E2E_ROOT")
if [ -n "$REAL" ]; then
  echo "FAIL: hardcoded native-plugin source pin(s) in $E2E_ROOT:"
  printf '%s\n' "$REAL" | sed 's/^/       /'
  FAILED=1
else
  echo "PASS [0] real tests/e2e tree carries no banned or unlisted native-plugin source pin"
fi

if [ "$FAILED" = "0" ]; then echo "native-plugin source SSOT lint: OK"; else echo "native-plugin source SSOT lint: FAILURES"; fi
exit $FAILED
