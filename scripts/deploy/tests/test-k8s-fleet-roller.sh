#!/usr/bin/env bash
# test-k8s-fleet-roller.sh — the NON-VACUOUS property of the k8s fleet roll.
#
# The defect this guards against is not "the roller crashes". It is the roller
# REPORTING SUCCESS HAVING ROLLED NOTHING. That is what the local-docker roller
# did on a k8s-substrate control plane for as long as the substrate migration has
# existed: its `docker ps --filter label=molecule.local-tenant=1` matched zero
# containers, it logged "nothing to roll", and it exited 0 — so the fleet-roll
# step of production deploys was green while every tenant pod stayed on its
# provisioning-time image.
#
# So every case below asserts an EXIT CODE for a scenario in which a naive roller
# would happily exit 0, plus the classification the operator is shown. Following
# test-managed-flags.sh, this drives the REAL script against a FAKE kubectl rather
# than probing functions in isolation: a test that proves a helper behaves, while
# the script never calls it, is itself a vacuous pass.
#
# The fake cluster's pod state is a FILE, and `set image` swaps it for the
# "after" fixture. That is what makes the landed-vs-not distinction testable with
# no cluster: a fixture whose after-state keeps the OLD digest is precisely a
# deploy that returned success and moved nothing.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT="$here/../redeploy-tenant-fleet-k8s.sh"
DOCKER_SCRIPT="$here/../redeploy-staging-fleet.sh"

PASS=0 ; FAILED=0
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/state"
export K8RT="$tmp/state"

D_OLD="sha256:1111111111111111111111111111111111111111111111111111111111111111"
D_NEW="sha256:2222222222222222222222222222222222222222222222222222222222222222"
REPO="registry.example.invalid/molecule-ai/molecule-tenant"
ORG_A="aaaaaaaa-0000-4000-8000-0000000000aa"
ORG_B="bbbbbbbb-0000-4000-8000-0000000000bb"
ORG_C="cccccccc-0000-4000-8000-0000000000cc"

# ── fake kubectl ─────────────────────────────────────────────────────────────
cat > "$tmp/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
# Fixture-driven kubectl. State lives in $K8RT:
#   deploys      ns \t name \t replicas \t org-id \t image
#   pods         ns \t podname \t org-id \t imageID          (the CURRENT serving set)
#   pods.after   optional; `set image` promotes it over `pods`
set -uo pipefail
ns="" ; args=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    -n) ns="$2"; shift 2;;
    *) args+=("$1"); shift;;
  esac
done
set -- "${args[@]:-}"
org_of_sel() { printf '%s' "${*}" | sed -n 's/.*molecule\.org-id=\([0-9a-f-]*\).*/\1/p'; }

case "${1:-}" in
  version) echo '{"clientVersion":{"gitVersion":"fake"}}'; exit 0;;
  set)
    # set image deployment/<name> <container>=<ref>
    printf '%s\n' "$*" >> "$K8RT/set-image.log"
    [ -f "$K8RT/pods.after" ] && cp "$K8RT/pods.after" "$K8RT/pods"
    exit 0;;
  rollout)
    [ -f "$K8RT/rollout.fail" ] && exit 1
    exit 0;;
  get) shift;;
  *) exit 0;;
esac

case "${1:-}" in
  deploy|deployment|deployments)
    shift
    sel="$*"
    if printf '%s' "$sel" | grep -q -- '-A'; then
      org="$(org_of_sel "$sel")"
      awk -F'\t' -v o="$org" '$4==o {printf "%s\t%s\t%s\n",$1,$2,$3}' "$K8RT/deploys"
    else
      # -o go-template asking for the current image of one deployment
      name="$1"
      awk -F'\t' -v n="$name" -v ns="$ns" '$1==ns && $2==n {printf "%s",$5}' "$K8RT/deploys"
    fi
    ;;
  pods|pod)
    shift
    sel="$*"
    org="$(org_of_sel "$sel")"
    if printf '%s' "$sel" | grep -q 'imageID'; then
      awk -F'\t' -v o="$org" -v ns="$ns" '$1==ns && $3==o {print $4}' "$K8RT/pods"
    elif printf '%s' "$sel" | grep -q 'metadata.name'; then
      awk -F'\t' -v o="$org" -v ns="$ns" '$1==ns && $3==o {print $2}' "$K8RT/pods"
    else
      awk -F'\t' -v o="$org" -v ns="$ns" '$1==ns && $3==o {print $2}' "$K8RT/pods"
    fi
    ;;
esac
exit 0
FAKE
chmod +x "$tmp/bin/kubectl"
export PATH="$tmp/bin:$PATH"

reset_state() {
  : > "$K8RT/set-image.log"; rm -f "$K8RT/rollout.fail" "$K8RT/pods.after"
  # alpha: 1 replica, serving the OLD digest. zero: 0 replicas, no pods.
  printf '%s\n' \
    "ns-alpha	enteros-tenant	1	$ORG_A	$REPO@$D_OLD" \
    "ns-zero	enteros-tenant	0	$ORG_B	$REPO@$D_OLD" > "$K8RT/deploys"
  printf '%s\n' "ns-alpha	enteros-tenant-abc	$ORG_A	$REPO@$D_OLD" > "$K8RT/pods"
}

CENSUS_AB="printf 'alpha\t$ORG_A\tk8s\tns-alpha\nzero\t$ORG_B\tk8s\tns-zero\n'"
CENSUS_A="printf 'alpha\t$ORG_A\tk8s\tns-alpha\n'"
CENSUS_ZERO="printf 'zero\t$ORG_B\tk8s\tns-zero\n'"
CENSUS_GHOST="printf 'ghost\t$ORG_C\tk8s\tns-ghost\n'"
CENSUS_DOCKER="printf 'dockerorg\tdddddddd-0000-4000-8000-0000000000dd\tdocker\tmol-x\n'"

check() { # check <label> <expect_rc> <expect_substr|-> <census> <args...>
  local label="$1" exp="$2" needle="$3" cen="$4"; shift 4
  local out rc
  out="$(SUBSTRATE_CENSUS_CMD="$cen" CONVERGE_ATTEMPTS=1 CONVERGE_SLEEP=0 \
          bash "$SCRIPT" "$@" 2>&1)" && rc=0 || rc=$?
  if [ "$rc" != "$exp" ]; then
    echo "FAIL: $label — exit $rc, expected $exp"; echo "$out" | sed 's/^/    /'; FAILED=$((FAILED+1)); return
  fi
  if [ "$needle" != "-" ] && ! printf '%s' "$out" | grep -qF -- "$needle"; then
    echo "FAIL: $label — output missing '$needle'"; echo "$out" | sed 's/^/    /'; FAILED=$((FAILED+1)); return
  fi
  echo "ok: $label (rc=$rc)"; PASS=$((PASS+1))
}

echo "== the empty target set must never be a silent success =="
reset_state
check "found:0 refuses to report success" 1 "refusing to report success" \
      "$CENSUS_DOCKER" --digest "$D_NEW"
check "found:0 IS allowed when asserted with --allow-empty" 0 "empty fleet asserted by operator" \
      "$CENSUS_DOCKER" --digest "$D_NEW" --allow-empty
check "--only that matches nothing is still found:0, still fatal" 1 "refusing to report success" \
      "$CENSUS_AB" --digest "$D_NEW" --only no-such-tenant

echo "== a 0-replica Deployment is not a rolled tenant =="
reset_state
check "every target scaled to zero => nothing deployed => fatal" 1 "NONE were eligible to roll" \
      "$CENSUS_ZERO" --digest "$D_NEW"
reset_state
printf '%s\n' "ns-alpha	enteros-tenant-abc	$ORG_A	$REPO@$D_NEW" > "$K8RT/pods.after"
check "0-replica tenant is reported SKIP:scaled-to-zero, not ROLLED" 0 "SKIP:scaled-to-zero" \
      "$CENSUS_AB" --digest "$D_NEW"

echo "== control-plane/cluster drift is reported, not skipped =="
reset_state
check "org on k8s substrate with no matching workload is fatal" 1 "FAIL:no-workload" \
      "$CENSUS_GHOST" --digest "$D_NEW"

echo "== verification is against the pods, not the command's exit code =="
reset_state
printf '%s\n' "ns-alpha	enteros-tenant-abc	$ORG_A	$REPO@$D_OLD" > "$K8RT/pods.after"
check "set image + rollout succeed but pods still run the OLD digest => fatal" 1 "FAIL:digest-mismatch" \
      "$CENSUS_A" --digest "$D_NEW"
reset_state
printf '%s\n' "ns-alpha	enteros-tenant-abc	$ORG_A	$REPO@$D_NEW" > "$K8RT/pods.after"
check "pods on the new digest => ROLLED" 0 "ROLLED" "$CENSUS_A" --digest "$D_NEW"
# Fresh state: the previous check already promoted pods.after, so without this
# the "before" would be the NEW digest and the assertion would prove nothing.
reset_state
printf '%s\n' "ns-alpha	enteros-tenant-abc	$ORG_A	$REPO@$D_NEW" > "$K8RT/pods.after"
check "report carries the BEFORE digest, not just the after" 0 "${D_OLD:0:19}" "$CENSUS_A" --digest "$D_NEW"
reset_state
: > "$K8RT/pods.after"   # rollout "succeeds" into zero serving pods
check "no serving pod after the roll => fatal" 1 "FAIL:no-pod-after" "$CENSUS_A" --digest "$D_NEW"
reset_state
touch "$K8RT/rollout.fail"
check "rollout that never completes => fatal" 1 "FAIL:rollout-timeout" "$CENSUS_A" --digest "$D_NEW"

echo "== the roll must be verifiable at all =="
reset_state
check "a bare tag with no derivable identity is refused up front" 2 "liveness-only" \
      "$CENSUS_A" --tag latest
check "--tag <env>-<sha> is accepted (sha becomes the expected identity)" 0 "expected git_sha  = 0badc0de" \
      "$CENSUS_A" --tag "staging-0badc0de" --dry-run

echo "== --digest keeps each workload on the registry it already pulls from =="
reset_state
printf '%s\n' "ns-alpha	enteros-tenant-abc	$ORG_A	$REPO@$D_NEW" > "$K8RT/pods.after"
SUBSTRATE_CENSUS_CMD="$CENSUS_A" CONVERGE_ATTEMPTS=1 CONVERGE_SLEEP=0 \
  bash "$SCRIPT" --digest "$D_NEW" >/dev/null 2>&1 || true
if grep -qF "${REPO}@${D_NEW}" "$K8RT/set-image.log"; then
  echo "ok: set image used the workload's own repo (${REPO})"; PASS=$((PASS+1))
else
  echo "FAIL: set image did not re-pin against the workload's existing repo"; cat "$K8RT/set-image.log"; FAILED=$((FAILED+1))
fi

echo "== the docker roller must not silently pass on an empty fleet =="
dchk() { # dchk <label> <expect_rc> <needle> <extra args...>
  local label="$1" exp="$2" needle="$3"; shift 3
  local out rc
  out="$(bash "$DOCKER_SCRIPT" --dry-run --cp-env production --tag staging-deadbee "$@" 2>&1)" && rc=0 || rc=$?
  if [ "$rc" != "$exp" ]; then echo "FAIL: $label — exit $rc, expected $exp"; echo "$out" | sed 's/^/    /'; FAILED=$((FAILED+1)); return; fi
  if [ "$needle" != "-" ] && ! printf '%s' "$out" | grep -qF -- "$needle"; then
    echo "FAIL: $label — missing '$needle'"; echo "$out" | sed 's/^/    /'; FAILED=$((FAILED+1)); return; fi
  echo "ok: $label (rc=$rc)"; PASS=$((PASS+1))
}
if docker info >/dev/null 2>&1; then
  # Requires a reachable docker daemon (the script hard-fails without one, by
  # design). Where there is no daemon the k8s cases above still cover the gate.
  if [ "$(docker ps --filter 'label=molecule.local-tenant=1' --filter 'label=molecule.cp-env=production' -q | wc -l)" = "0" ]; then
    dchk "0 docker tenants + no k8s arm => fatal" 1 "vacuous pass"
    dchk "0 docker tenants + --allow-empty => permitted" 0 "-" --allow-empty
  else
    echo "skip: this host HAS production docker tenants — empty-fleet gate not exercisable here"
  fi
else
  echo "skip: no docker daemon — docker-arm gate not exercisable here"
fi

echo
echo "passed=$PASS failed=$FAILED"
[ "$FAILED" -eq 0 ] || exit 1
echo "ALL OK"
