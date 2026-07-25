#!/usr/bin/env bash
# test-managed-flags.sh — the ROLLBACK property of TENANT_FLAGS.
#
# Staging tenant env is inherited BY COPY (swap_tenant reads the running
# container's Config.Env and re-applies it). So a flag set by hand is STICKY
# FOREVER: copied into every future redeploy with no way to unset it. A burn-in
# flag you cannot un-flip is not a burn-in flag, and a rollback that cannot roll
# the flag back is not a rollback.
#
# WHY THIS DRIVES THE WHOLE SCRIPT AND NOT apply_managed_flags IN ISOLATION:
#
# It used to `eval` the function out of the script and probe it directly. Review
# proved that vacuous: DELETE the one line that CALLS it — restoring the
# sticky-forever bug in full — and the test still printed ok and exited 0. It
# asserted that a function behaves, never that the deploy USES it.
#
# So we run the real script against a FAKE docker and assert on the one artifact
# that decides what the container actually comes up with: the --env-file handed to
# `docker run`. Delete the call site, inherit the env around the strip, reorder the
# steps — the env-file is wrong and this goes red.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/../redeploy-staging-fleet.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin"

# ---------------------------------------------------------------------------
# Fake docker. The inherited env is deliberately ADVERSARIAL: it carries
# neighbours that a sloppy strip would eat. A prefix-only (`^KEY`) or unanchored
# (`KEY`) grep passes a bland FOO/OTHER fixture, and would silently delete
# DATABASE_URL-class vars on a real tenant — a staging outage. They live here so
# the strip can never regress to those forms unnoticed.
# ---------------------------------------------------------------------------
cat > "$tmp/bin/docker" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  info|pull) exit 0 ;;
  image)  [ "${2:-}" = "inspect" ] && exit 0 ;;
  ps)     printf 'mol-tenant-canary\n'; exit 0 ;;
  volume)
    # Mixed-generation rtstate fixture (Enter OS rebrand, internal#1089): one
    # LEGACY mol-* volume, one enteros-* volume, one non-rtstate distractor.
    # LITERAL strings on purpose — never derived from the script's prefix list —
    # so a script that goes blind to either generation (or over-matches) is
    # caught by the count assertion in test 7 below.
    if [ "${2:-}" = "ls" ]; then
      printf 'mol-ws-rtstate-11111111\n'
      printf 'enteros-ws-rtstate-22222222\n'
      printf 'totally-unrelated-volume\n'
    fi
    exit 0 ;;
  port)   printf '127.0.0.1:39001\n'; exit 0 ;;
  rename|stop|rm|start) exit 0 ;;
  inspect)
    case "${4:-}" in
      *NetworkSettings.Networks*) printf 'molecule-net\n' ;;
      *RestartPolicy.Name*)      printf 'unless-stopped\n' ;;
      *Config.Env*)
        if [ "${FAKE_ENV_MODE:-adversarial}" = "onlykey" ]; then
          # The envfile must contain NOTHING BUT the managed key (PATH is filtered by
          # the script), so `grep -v` prints zero lines and exits 1 — the fail-open
          # shape this mode exists to trigger.
          #
          # Engagement is proved OUT OF BAND, via this marker. It cannot be proved
          # with an extra env var: any additional line would give `grep -v` something
          # to print, exit 0, and suppress the very bug we are trying to catch. A
          # first attempt used exactly such a canary and silently disarmed the test.
          [ -n "${FAKE_MODE_MARKER:-}" ] && : > "$FAKE_MODE_MARKER"
          printf 'PATH=/usr/bin\n'
          printf 'DELEGATION_LEDGER_WRITE=1\n'
        else
          printf 'PATH=/usr/bin\n'
          printf 'MOLECULE_ENV=staging\n'
          printf 'DATABASE_URL=postgres://x/y\n'
          printf 'DELEGATION_LEDGER_WRITE=1\n'
          printf 'DELEGATION_LEDGER_WRITE_EXTRA=keep-me\n'
          printf 'X_DELEGATION_LEDGER_WRITE=keep-me\n'
          printf 'NOTE=value mentioning DELEGATION_LEDGER_WRITE inline\n'
        fi
        ;;
      *Config.Labels*)     printf 'molecule.local-tenant=1\nmolecule.cp-env=staging\n' ;;
      *HostConfig.ExtraHosts*) ;;
      *Config.Image*)      printf 'registry/old:staging-old\n' ;;
      *) printf 'stub\n' ;;
    esac
    exit 0
    ;;
  run)
    prev=""
    for a in "$@"; do
      [ "$prev" = "--env-file" ] && cp "$a" "$FAKE_ENVFILE_OUT"
      prev="$a"
    done
    printf 'new-container-id\n'; exit 0
    ;;
esac
exit 0
FAKE
cat > "$tmp/bin/curl" <<'FAKECURL'
#!/usr/bin/env bash
echo '{"git_sha":"deadbee1234567890"}'
FAKECURL
chmod +x "$tmp/bin/docker" "$tmp/bin/curl"

# roll_with "<TENANT_FLAGS>" -> the env-file `docker run` was actually handed
roll_with() {
  local out="$tmp/envfile.out"
  : > "$out"
  PATH="$tmp/bin:$PATH" \
  FAKE_ENVFILE_OUT="$out" \
  FAKE_ENV_MODE="${FAKE_ENV_MODE:-adversarial}" \
  FAKE_MODE_MARKER="${FAKE_MODE_MARKER:-}" \
  TENANT_FLAGS="$1" \
  TENANT_IMAGE="registry.example/molecule-tenant" \
  STAGING_CANVAS_APP_CONTAINER="" \
  HEALTH_GATE_ATTEMPTS=1 HEALTH_GATE_SLEEP_SECS=0 \
    bash "$script" --tag "staging-deadbee" >/dev/null 2>&1 || true
  cat "$out"
}

# 1. ROLLBACK — an inherited flag must NOT survive an empty TENANT_FLAGS. This is
#    the property the whole mechanism exists for, and it is asserted on what
#    DOCKER GOT, so deleting the apply_managed_flags call site turns it red.
env_out="$(roll_with "")"
[ -n "$env_out" ] || fail "fake docker recorded no --env-file — the harness proves nothing"
grep -q '^DELEGATION_LEDGER_WRITE=' <<<"$env_out" \
  && fail "an inherited managed flag reached docker despite an empty TENANT_FLAGS — it is sticky"
grep -q '^MOLECULE_ENV=staging$' <<<"$env_out" || fail "unmanaged env destroyed (MOLECULE_ENV)"
grep -q '^DATABASE_URL='         <<<"$env_out" || fail "unmanaged env destroyed (DATABASE_URL) — that is an outage"

# 1b. The strip must be EXACT. These three survive a correct `^KEY=` strip and are
#     eaten by `^KEY` or an unanchored match.
grep -q '^DELEGATION_LEDGER_WRITE_EXTRA=keep-me$' <<<"$env_out" \
  || fail "over-broad strip ate a key that merely SHARES A PREFIX with a managed key"
grep -q '^X_DELEGATION_LEDGER_WRITE=keep-me$' <<<"$env_out" \
  || fail "over-broad strip ate a key that merely CONTAINS a managed key"
grep -q '^NOTE=' <<<"$env_out" \
  || fail "over-broad strip ate a var whose VALUE mentions a managed key"

# 2. FLIP — declared flags reach docker.
env_out="$(roll_with "DELEGATION_LEDGER_WRITE=1 DELEGATION_RESULT_INBOX_PUSH=1")"
grep -q '^DELEGATION_LEDGER_WRITE=1$'      <<<"$env_out" || fail "declared flag never reached docker"
grep -q '^DELEGATION_RESULT_INBOX_PUSH=1$' <<<"$env_out" || fail "declared flag never reached docker"

# 3. IDEMPOTENT — the inherited copy and the re-applied one must not BOTH land.
#    docker takes the LAST value, so a stale duplicate silently wins on a later
#    partial edit.
[ "$(grep -c '^DELEGATION_LEDGER_WRITE=' <<<"$env_out")" = "1" ] \
  || fail "a managed key reached docker TWICE (the inherited copy was not stripped)"

# 4. REFUSE an undeclared key — it would never be stripped, i.e. exactly the
#    one-way door this exists to prevent. It must fail CLOSED: abort the roll
#    BEFORE any container is touched, rather than quietly build a sticky flag.
out="$(PATH="$tmp/bin:$PATH" FAKE_ENVFILE_OUT="$tmp/none.out" \
        TENANT_FLAGS="SOME_OTHER_FLAG=1" \
        TENANT_IMAGE="registry.example/molecule-tenant" \
        STAGING_CANVAS_APP_CONTAINER="" \
        HEALTH_GATE_ATTEMPTS=1 HEALTH_GATE_SLEEP_SECS=0 \
        bash "$script" --tag staging-deadbee 2>&1 || true)"
grep -q 'not in MANAGED_FLAG_KEYS' <<<"$out" \
  || fail "accepted a key not in MANAGED_FLAG_KEYS (it would be permanently sticky)"

# 5. FAIL CLOSED when the strip prints nothing. `grep -v` exits 1 when it emits no
#    lines — which is what happens when the inherited env contains nothing but the
#    managed key. Written as `grep -v ... > tmp && mv`, the mv is SKIPPED, the stale
#    flag survives, and the function still returns 0: the strip silently no-ops in
#    the one shape where it matters most. Driven end-to-end, so it is the env-file
#    docker receives that proves it.
marker="$tmp/onlykey.engaged"; rm -f "$marker"
env_out="$(FAKE_MODE_MARKER="$marker" FAKE_ENV_MODE=onlykey roll_with "")"
[ -f "$marker" ] \
  || fail "the onlykey harness never engaged — the correct output here is an EMPTY env-file, so without this marker the assertion below passes on empty and a typo'd FAKE_ENV_MODE would silently turn test 5 into a duplicate of test 1"
grep -q '^DELEGATION_LEDGER_WRITE=' <<<"$env_out" \
  && fail "strip no-oped when the inherited env was ONLY the managed key (grep -v exit 1 swallowed) — the flag reached docker"

# 6. EVERY CALL SITE MUST PASS TENANT_FLAGS.
#    The script strips every managed key and re-applies only what TENANT_FLAGS
#    says, so a call site that omits it silently turns the managed flags OFF across
#    the fleet. Shipped that way this is a flag ERASER, not a flag switch: an
#    unrelated merge to main would end an in-flight burn-in with no log line.
#    Guard it, or the next call site quietly re-introduces it.
# 6a. UNSET TENANT_FLAGS MUST ABORT THE ROLL.
#     This is the PRIMARY defence, and the reason it lives in the script rather
#     than in a lint: a call site that forgets the var does not inherit "the old
#     behaviour", it silently strips the managed flags off every tenant. The static
#     workflow guard below cannot close this on its own — a file may invoke the
#     script twice and mention TENANT_FLAGS once, which is exactly how the first
#     version of this test passed while a call site was unwired.
out="$(env -u TENANT_FLAGS PATH="$tmp/bin:$PATH" \
        TENANT_IMAGE="registry.example/molecule-tenant" \
        STAGING_CANVAS_APP_CONTAINER="" \
        bash "$script" --tag staging-deadbee 2>&1 || true)"
grep -q 'TENANT_FLAGS is not set' <<<"$out" \
  || fail "the script rolled the fleet with TENANT_FLAGS UNSET — it would strip every managed flag silently"

#    (The per-STEP wiring lint lives in .gitea/scripts/tests/test_managed_flags_wiring.py.
#    It has to parse the YAML: staging-tenant-cd.yml rolls the fleet from TWO steps
#    — the forward roll and the rollback — so a file-level grep here stays green
#    while one of them is silently unwired. That mutation was demonstrated. This
#    script keeps the behavioural assertions; the wiring assertions are next door.)

# 7. DUAL-PREFIX rtstate recognition (Enter OS rebrand, internal#1089).
#    The session-preservation guard must see BOTH brand generations of rtstate
#    volume names. The fake `docker volume ls` above lists exactly one LEGACY
#    mol-ws-rtstate-* volume, one enteros-ws-rtstate-* volume and one unrelated
#    volume, so the ONLY correct count is 2:
#      * a mol-only matcher counts 1  -> the flip would blind the guard to the
#        whole new-prefix fleet (silent, hollow session-preservation pass);
#      * an enteros-only matcher counts 1 -> blind to every EXISTING session;
#      * an over-broad matcher counts 3 -> guards volumes it must not own.
out="$(PATH="$tmp/bin:$PATH" \
        TENANT_FLAGS="" \
        TENANT_IMAGE="registry.example/molecule-tenant" \
        STAGING_CANVAS_APP_CONTAINER="" \
        HEALTH_GATE_ATTEMPTS=1 HEALTH_GATE_SLEEP_SECS=0 \
        bash "$script" --tag staging-deadbee 2>&1 || true)"
grep -Fq 'session (rtstate) volumes present before roll: 2' <<<"$out" \
  || fail "rtstate snapshot did not count exactly the 2 branded volumes (mol-* + enteros-*) — a single-prefix or over-broad matcher regressed the guard"
grep -Fq 'session-preservation OK: all 2 rtstate volume(s) intact after roll' <<<"$out" \
  || fail "session-preservation verdict did not cover both brand generations of rtstate volumes"

echo "ok: managed tenant flags are reversible, exact, idempotent, fail-closed, and wired at every call site; rtstate guard sees both brand prefixes"
