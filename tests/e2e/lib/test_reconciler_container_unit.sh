#!/usr/bin/env bash
# Offline regression test for the molecules-server reconciler kill-target lookup.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tests/e2e/lib/reconciler_container.sh
source "$ROOT/reconciler_container.sh"

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }

DOCKER_NAMES=""
DOCKER_RC=0
docker() {
  [ "${1:-}" = "ps" ] || return 2
  printf '%s' "$DOCKER_NAMES"
  return "$DOCKER_RC"
}

WS_ID="d32826e3-45e6-4f15-9abc-0123456789ab"

DOCKER_NAMES=$'concierge-e2e-rec\nmol-ws-e2e-rec-short-d32826e345e6\nmol-ws-unrelated-aaaaaaaaaaaa'
got=$(resolve_molecules_server_container "$WS_ID")
[ "$got" = "mol-ws-e2e-rec-short-d32826e345e6" ] \
  || fail "exact workspace suffix was not selected (got '$got')"
pass "selects the exact mol-ws workspace suffix"

DOCKER_NAMES=$'mol-ws-e2e-rec-short-d32826e345e6'
got=$(resolve_molecules_server_container "D32826E3-45E6-4F15-9ABC-0123456789AB")
[ "$got" = "mol-ws-e2e-rec-short-d32826e345e6" ] \
  || fail "uppercase UUID did not follow the provider's lowercase suffix contract (got '$got')"
pass "normalizes UUID case like the provider naming SSOT"

DOCKER_NAMES=$'not-mol-ws-e2e-d32826e345e6\nmol-ws-e2e-d32826e345e60\nmol-ws-e2e-aaaaaaaaaaaa'
got=$(resolve_molecules_server_container "$WS_ID")
[ -z "$got" ] || fail "accepted an unrelated or non-exact name ('$got')"
pass "rejects prefix and suffix lookalikes"

DOCKER_NAMES=$'mol-ws-first-d32826e345e6\nmol-ws-second-d32826e345e6'
got=$(resolve_molecules_server_container "$WS_ID")
[ "$got" = "mol-ws-first-d32826e345e6" ] \
  || fail "did not deterministically select the first exact match (got '$got')"
pass "selects one deterministic target when duplicate exact matches exist"

# These assignments run with errexit + pipefail enabled. A non-zero helper
# result would abort the test before the assertion, reproducing the live failure.
DOCKER_NAMES=$'mol-ws-e2e-aaaaaaaaaaaa'
got=$(resolve_molecules_server_container "$WS_ID")
[ -z "$got" ] || fail "no-match lookup returned '$got'"
pass "no match returns empty success under errexit + pipefail"

DOCKER_NAMES=""
DOCKER_RC=1
got=$(resolve_molecules_server_container "$WS_ID")
[ -z "$got" ] || fail "failed docker ps returned '$got'"
pass "transient docker ps failure returns empty success"

DOCKER_RC=0
DOCKER_NAMES=$'mol-ws-e2e-d32826e345e6'
got=$(resolve_molecules_server_container "not-a-workspace-uuid")
[ -z "$got" ] || fail "invalid workspace id resolved '$got'"
pass "invalid workspace id cannot select a container"

echo "All reconciler container resolution unit tests passed"
