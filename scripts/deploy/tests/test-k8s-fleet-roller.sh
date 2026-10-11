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
#
# The fake ALSO plays the roller's LEAST-PRIVILEGE credential. What that
# credential is not granted — a cluster-wide read (-A / --all-namespaces),
# `exec` into a pod, any call in a namespace it holds no role in — is refused the
# way the API server refuses it (non-zero exit, `Error from server (Forbidden)`),
# and every call is logged so the cases below can assert WHICH calls the roller
# made. A fake that answered `exec` and `-A` happily is exactly how a regression
# back to either would stay green.
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
ORG_E="eeeeeeee-0000-4000-8000-0000000000ee"

# ── fake kubectl ─────────────────────────────────────────────────────────────
cat > "$tmp/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
# Fixture-driven kubectl. State lives in $K8RT:
#   deploys        ns \t name \t replicas \t org-id \t image
#   pods           ns, name, org-id, imageID, deletionTimestamp, phase, ready, git_sha (TSV)
#                  Omitted phase/ready fields default to Running/true.
#   pods.after     optional; `set image` promotes it over `pods`
#   forbidden-ns   optional; namespaces the credential holds NO role in
#   proxy.fail     optional; the API server cannot reach the pod (pods/proxy 503)
#   calls.log      every invocation, one line: "ns=<ns> <args...>"
#   buildinfo.log  every pod whose /buildinfo was requested through pods/proxy
# A pod's /buildinfo reports its git_sha column, or FAKE_BUILDINFO_SHA (default
# deadbee) when that column is empty.
set -uo pipefail
ns="" ; args=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    -n) ns="$2"; shift 2;;
    *) args+=("$1"); shift;;
  esac
done
set -- "${args[@]:-}"
printf 'ns=%s %s\n' "$ns" "$*" >> "$K8RT/calls.log"
org_of_sel() { printf '%s' "${*}" | sed -n 's/.*molecule\.org-id=\([0-9a-f-]*\).*/\1/p'; }
forbid() { echo "Error from server (Forbidden): $*" >&2; exit 1; }
no_role_in() { [ -f "$K8RT/forbidden-ns" ] && grep -qxF -- "$1" "$K8RT/forbidden-ns"; }

# ── the least-privilege RBAC this fake enforces ──────────────────────────────
for a in "$@"; do
  case "$a" in
    -A|--all-namespaces) forbid "cannot list at the cluster scope (the roller's credential is namespaced only)";;
  esac
done
[ -z "$ns" ] || ! no_role_in "$ns" || forbid "the credential holds no role in namespace \"$ns\""

case "${1:-}" in
  version) echo '{"clientVersion":{"gitVersion":"fake"}}'; exit 0;;
  exec) forbid "cannot create resource \"pods/exec\" in namespace \"$ns\" (tenant /buildinfo is read through pods/proxy)";;
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
  --raw)
    # The pods/proxy subresource: /api/v1/namespaces/<ns>/pods/<pod>:<port>/proxy/<path>
    p="${2:-}"
    case "$p" in
      /api/v1/namespaces/*/pods/*/proxy/*) ;;
      *) echo "Error from server (NotFound): the server could not find the requested resource" >&2; exit 1;;
    esac
    rest="${p#/api/v1/namespaces/}"; pns="${rest%%/*}"; rest="${rest#*/pods/}"
    target="${rest%%/proxy/*}"; path="${rest#*/proxy/}"
    pod="${target%%:*}"; port=""; [ "$target" = "$pod" ] || port="${target#*:}"
    printf '%s\n' "$pod" >> "$K8RT/buildinfo.log"
    ! no_role_in "$pns" || forbid "cannot get resource \"pods/proxy\" in namespace \"$pns\""
    awk -F'\t' -v n="$pns" -v p="$pod" '$1==n && $2==p {f=1} END {exit !f}' "$K8RT/pods" \
      || { echo "Error from server (NotFound): pods \"$pod\" not found" >&2; exit 1; }
    [ ! -f "$K8RT/proxy.fail" ] \
      || { echo "Error from server (ServiceUnavailable): error trying to reach service: dial tcp ${pod}:${port}: connect: connection refused" >&2; exit 1; }
    # Only the tenant API port serves /buildinfo; anything else is a 404 from the pod.
    if [ "$port" != "8080" ] || [ "$path" != "buildinfo" ]; then
      echo "Error from server (NotFound): the server could not find the requested resource" >&2; exit 1
    fi
    sha="$(awk -F'\t' -v n="$pns" -v p="$pod" '$1==n && $2==p {print $8; exit}' "$K8RT/pods")"
    printf '{"git_sha":"%s"}\n' "${sha:-${FAKE_BUILDINFO_SHA:-deadbee}}"
    ;;
  deploy|deployment|deployments)
    shift
    if [ "${1:-}" = "-l" ]; then
      # namespaced list by label selector -> name \t replicas
      org="$(org_of_sel "${2:-}")"
      awk -F'\t' -v o="$org" -v ns="$ns" '$1==ns && $4==o {printf "%s\t%s\n",$2,$3}' "$K8RT/deploys"
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
    # Emulate the requested projection/filter, including the old unfiltered
    # items[0] query: a draining first pod must reproduce the regression.
    filtered=0
    if [[ "$sel" == *deletionTimestamp* && "$sel" == *Running* ]]; then filtered=1; fi
    ready=0
    [[ "$sel" != *.ready* ]] || ready=1
    rows="$(awk -F'\t' -v o="$org" -v ns="$ns" -v f="$filtered" -v r="$ready" '
      $1==ns && $3==o && (!f || ($5=="" && ($6=="" || $6=="Running"))) && (!r || $7!="false")' "$K8RT/pods")"
    if printf '%s' "$sel" | grep -q 'imageID'; then
      if [[ "$sel" == *metadata.name* ]]; then
        awk -F'\t' 'NF {printf "%s\t%s\n",$2,$4}' <<< "$rows"
      else
        awk -F'\t' 'NF {print $4}' <<< "$rows"
      fi
    elif [[ "$sel" == *'items[0]'* ]]; then
      awk -F'\t' 'NR==1 {print $2}' <<< "$rows"
    elif printf '%s' "$sel" | grep -q 'metadata.name'; then
      awk -F'\t' 'NF {print $2}' <<< "$rows"
    else
      awk -F'\t' -v o="$org" -v ns="$ns" '$1==ns && $3==o {print $2}' "$K8RT/pods"
    fi
    ;;
esac
exit 0
FAKE
chmod +x "$tmp/bin/kubectl"
export PATH="$tmp/bin:$PATH"

# row <field>... — one TAB-separated fixture line.
row() { local IFS=$'\t'; printf '%s\n' "$*"; }

reset_state() {
  : > "$K8RT/set-image.log"; : > "$K8RT/calls.log"; : > "$K8RT/buildinfo.log"
  rm -f "$K8RT/rollout.fail" "$K8RT/pods.after" "$K8RT/forbidden-ns" "$K8RT/proxy.fail"
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

echo "== digest and build checks use the same serving replicas =="
reset_state
printf '%s\n' \
  "ns-alpha	draining-old	$ORG_A	$REPO@$D_OLD	2026-10-11T00:00:00Z	Running	true	0badc0de" \
  "ns-alpha	pending-old	$ORG_A	$REPO@$D_OLD		Pending	false	0badc0de" \
  "ns-alpha	unready-new	$ORG_A	$REPO@$D_NEW		Running	false	cafe1234" \
  "ns-alpha	serving-a	$ORG_A	$REPO@$D_NEW		Running	true	cafe1234" \
  "ns-alpha	serving-b	$ORG_A	$REPO@$D_NEW		Running	true	cafe1234" > "$K8RT/pods.after"
EXPECTED_BUILD_SHA=cafe1234 check "rollback ignores a draining old pod listed first" 0 "ROLLED" "$CENSUS_A" --digest "$D_NEW"
if [ "$(sort "$K8RT/buildinfo.log")" = $'serving-a\nserving-b' ]; then
  echo "ok: buildinfo probed every serving replica and no stragglers"; PASS=$((PASS+1))
else
  echo "FAIL: buildinfo probed the wrong pod set"; cat "$K8RT/buildinfo.log"; FAILED=$((FAILED+1))
fi
reset_state
printf '%s\n' \
  "ns-alpha	serving-a	$ORG_A	$REPO@$D_NEW		Running	true	cafe1234" \
  "ns-alpha	serving-b	$ORG_A	$REPO@$D_NEW		Running	true	0badc0de" > "$K8RT/pods.after"
EXPECTED_BUILD_SHA=cafe1234 check "a second replica with stale buildinfo fails" 1 "FAIL:buildinfo-mismatch" "$CENSUS_A" --digest "$D_NEW"
reset_state
printf '%s\n' \
  "ns-alpha	serving-a	$ORG_A	$REPO@$D_NEW" \
  "ns-alpha	serving-b	$ORG_A	$REPO@$D_OLD" > "$K8RT/pods.after"
check "a serving old replica is not mistaken for a draining pod" 1 "FAIL:mixed-digests" "$CENSUS_A" --digest "$D_NEW"
reset_state
printf '%s\n' \
  "ns-alpha	serving-a	$ORG_A	$REPO@$D_NEW" \
  "ns-alpha	serving-b	$ORG_A	" > "$K8RT/pods.after"
check "a replica without an imageID cannot hide behind a healthy replica" 1 "FAIL:mixed-digests" "$CENSUS_A" --digest "$D_NEW"

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

# logged <label> <fixed string>: the roller made this exact kubectl call.
logged() {
  if grep -qF -- "$2" "$K8RT/calls.log"; then echo "ok: $1"; PASS=$((PASS+1)); return; fi
  echo "FAIL: $1 — no kubectl call contains '$2'"; sed 's/^/    /' "$K8RT/calls.log"; FAILED=$((FAILED+1))
}
# never_logged <label> <ERE>: the roller made NO kubectl call like this.
never_logged() {
  local hits
  hits="$(grep -E -- "$2" "$K8RT/calls.log" || true)"
  if [ -z "$hits" ]; then echo "ok: $1"; PASS=$((PASS+1)); return; fi
  echo "FAIL: $1"; printf '%s\n' "$hits" | sed 's/^/    /'; FAILED=$((FAILED+1))
}
NO_CLUSTER_WIDE='(^| )(-A|--all-namespaces)( |$)'
NO_EXEC='^ns=[^ ]* exec( |$)'

echo "== the tenant is looked up INSIDE its namespace, never cluster-wide =="
# The census instance_id IS the namespace: the k8s provisioner returns the
# Namespace name as the InstanceID and the control plane records it there.
reset_state
row ns-alpha enteros-tenant-abc "$ORG_A" "$REPO@$D_NEW" > "$K8RT/pods.after"
check "a tenant is found in the namespace its census row records" 0 "ROLLED" "$CENSUS_A" --digest "$D_NEW"
logged "the Deployment lookup is scoped to that namespace" \
  "ns=ns-alpha get deploy -l molecule.component=tenant,molecule.managed-by=controlplane,molecule.org-id=$ORG_A"
never_logged "no call reads cluster-wide (-A / --all-namespaces)" "$NO_CLUSTER_WIDE"
never_logged "no call execs into a pod" "$NO_EXEC"

reset_state
# alpha's Deployment carries alpha's org-id but sits somewhere the control plane
# does not record. A cluster-wide lookup would have found and rolled it; the
# namespaced one must say where it looked instead of hunting for it.
row ns-elsewhere enteros-tenant 1 "$ORG_A" "$REPO@$D_OLD" > "$K8RT/deploys"
check "a tenant outside its recorded namespace is FAIL:no-workload, naming where it looked" 1 \
  "NO tenant Deployment in namespace 'ns-alpha'" "$CENSUS_A" --digest "$D_NEW"
never_logged "not finding it there did not trigger a cluster-wide search" "$NO_CLUSTER_WIDE"
# CONTROL: the same fixture IS a findable tenant when the census points at it, so
# the FAIL above is the namespace scoping and not a fixture that never took.
check "control: pointed at ns-elsewhere, the same Deployment is found" 0 "DRYRUN:planned" \
  "printf 'alpha\t$ORG_A\tk8s\tns-elsewhere\n'" --digest "$D_NEW" --dry-run

reset_state
# An EMPTY instance_id (a substrate flip blanks it until the next provision
# stamps it) falls back to the control plane's own derivation, K8sNamespaceName:
# <prefix>-<slug>, sanitised the way the provisioner sanitises it ("--" collapses,
# a trailing "-" is trimmed).
row enteros-gamma-two enteros-tenant 1 "$ORG_E" "$REPO@$D_OLD" >> "$K8RT/deploys"
row enteros-gamma-two enteros-tenant-g "$ORG_E" "$REPO@$D_OLD" >> "$K8RT/pods"
row enteros-gamma-two enteros-tenant-g "$ORG_E" "$REPO@$D_NEW" > "$K8RT/pods.after"
check "an empty instance_id falls back to <prefix>-<sanitised slug>" 0 "ROLLED" \
  "printf 'gamma--two-\t$ORG_E\tk8s\t\n'" --digest "$D_NEW"
logged "the fallback namespace is enteros-gamma-two" "ns=enteros-gamma-two get deploy -l "

reset_state
row acme-gamma-two enteros-tenant 1 "$ORG_E" "$REPO@$D_OLD" >> "$K8RT/deploys"
row acme-gamma-two enteros-tenant-g "$ORG_E" "$REPO@$D_OLD" >> "$K8RT/pods"
row acme-gamma-two enteros-tenant-g "$ORG_E" "$REPO@$D_NEW" > "$K8RT/pods.after"
export TENANT_NAMESPACE_PREFIX=acme
check "TENANT_NAMESPACE_PREFIX drives the fallback (it mirrors the SDK brand prefix)" 0 "ROLLED" \
  "printf 'gamma--two-\t$ORG_E\tk8s\t\n'" --digest "$D_NEW"
unset TENANT_NAMESPACE_PREFIX

reset_state
# A tenant provisioned under the previous brand prefix lives in mol-<slug>, and
# its instance_id says so. A row that names its namespace is used as recorded,
# never re-derived.
row mol-delta enteros-tenant 1 "$ORG_E" "$REPO@$D_OLD" >> "$K8RT/deploys"
row mol-delta enteros-tenant-d "$ORG_E" "$REPO@$D_OLD" >> "$K8RT/pods"
row mol-delta enteros-tenant-d "$ORG_E" "$REPO@$D_NEW" > "$K8RT/pods.after"
check "a recorded mol-<slug> namespace is used as recorded" 0 "ROLLED" \
  "printf 'delta\t$ORG_E\tk8s\tmol-delta\n'" --digest "$D_NEW"
logged "the lookup went to mol-delta" "ns=mol-delta get deploy -l "

reset_state
# No role in the tenant's namespace (e.g. its RoleBinding was never created) is
# an ACCESS fault. Reporting it as "no workload" would send the operator after
# control-plane drift that does not exist.
echo ns-alpha > "$K8RT/forbidden-ns"
check "no role in the tenant namespace is FAIL:lookup-error, not drift" 1 "FAIL:lookup-error" \
  "$CENSUS_A" --digest "$D_NEW"
check "the lookup error carries the API server's own reason" 1 "Error from server (Forbidden)" \
  "$CENSUS_A" --digest "$D_NEW"

echo "== /buildinfo is read through the API server's pod proxy, never exec =="
# --tag <env>-<sha> carries no digest, so /buildinfo is the ONLY identity check:
# these cases prove its answer is actually used, in both directions.
reset_state
row ns-alpha enteros-tenant-abc "$ORG_A" "$REPO@$D_NEW" > "$K8RT/pods.after"
export FAKE_BUILDINFO_SHA="0badc0de0123456789abcdef0123456789abcdef"
check "/buildinfo reporting the expected git_sha => ROLLED" 0 \
  "/buildinfo git_sha=${FAKE_BUILDINFO_SHA} matches 0badc0de" "$CENSUS_A" --tag staging-0badc0de
logged "the probe is GET pods/proxy on the tenant API port" \
  "get --raw /api/v1/namespaces/ns-alpha/pods/enteros-tenant-abc:8080/proxy/buildinfo"
never_logged "no call execs into a pod" "$NO_EXEC"

reset_state
row ns-alpha enteros-tenant-abc "$ORG_A" "$REPO@$D_NEW" > "$K8RT/pods.after"
export FAKE_BUILDINFO_SHA="feedface0123456789abcdef0123456789abcdef"
check "/buildinfo reporting another git_sha => FAIL:buildinfo-mismatch" 1 "FAIL:buildinfo-mismatch" \
  "$CENSUS_A" --tag staging-0badc0de

reset_state
row ns-alpha enteros-tenant-abc "$ORG_A" "$REPO@$D_NEW" > "$K8RT/pods.after"
export FAKE_BUILDINFO_SHA="0badc0de0123456789abcdef0123456789abcdef"
touch "$K8RT/proxy.fail"
check "a pod proxy that cannot reach the pod reads as an EMPTY git_sha and fails closed" 1 \
  "git_sha='<empty>'" "$CENSUS_A" --tag staging-0badc0de
unset FAKE_BUILDINFO_SHA

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
