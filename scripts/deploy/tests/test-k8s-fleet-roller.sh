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
#   scope-ns       optional; the ONLY namespaces the credential holds a role in
#                  (explicit per-namespace RoleBindings). Absent: every namespace.
#                  `auth can-i` answers from it; every other call outside it is
#                  Forbidden.
#   nolist-ns      optional; namespaces whose role can patch but not list
#                  Deployments (a partial grant)
#   cani.fail      optional; `auth can-i` itself errors (API unreachable)
#   cani.reason    optional; `auth can-i` answers "no - <this reason>" (an RBAC
#                  evaluation error, i.e. a broken binding)
#   cani.stderr    optional; `auth can-i` writes this to stderr and then answers
#                  as usual (yes or no). kubectl's resource-resolution fallback
#                  looks like this.
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
has_role() { [ ! -f "$K8RT/scope-ns" ] || grep -qxF -- "$1" "$K8RT/scope-ns"; }
cannot_list() { [ -f "$K8RT/nolist-ns" ] && grep -qxF -- "$1" "$K8RT/nolist-ns"; }

# ── the least-privilege RBAC this fake enforces ──────────────────────────────
for a in "$@"; do
  case "$a" in
    -A|--all-namespaces) forbid "cannot list at the cluster scope (the roller's credential is namespaced only)";;
  esac
done
# `auth can-i` is a SelfSubjectAccessReview, which every identity may create, so
# it is answered BEFORE the role check, from the same scope model. Like kubectl:
# "yes" exits 0, "no" exits 1, and an error prints nothing on stdout.
if [ "${1:-}" = "auth" ] && [ "${2:-}" = "can-i" ]; then
  [ ! -f "$K8RT/cani.fail" ] || { echo "error: You must be logged in to the server (Unauthorized)" >&2; exit 1; }
  [ ! -f "$K8RT/cani.reason" ] || { printf 'no - %s\n' "$(cat "$K8RT/cani.reason")"; exit 1; }
  [ ! -f "$K8RT/cani.stderr" ] || cat "$K8RT/cani.stderr" >&2
  if has_role "$ns"; then echo yes; exit 0; fi
  echo no; exit 1
fi
[ -z "$ns" ] || has_role "$ns" || forbid "the credential holds no role in namespace \"$ns\""

case "${1:-}" in
  version) echo '{"clientVersion":{"gitVersion":"fake"}}'; exit 0;;
  exec) forbid "cannot create resource \"pods/exec\" in namespace \"$ns\" (tenant /buildinfo is read through pods/proxy)";;
  set)
    # set image deployment/<name> <container>=<ref>
    printf '%s\n' "$*" >> "$K8RT/set-image.log"
    [ ! -f "$K8RT/set-image.fail" ] || exit 1
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
    has_role "$pns" || forbid "cannot get resource \"pods/proxy\" in namespace \"$pns\""
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
      [ ! -f "$K8RT/discovery.fail" ] || forbid "workload discovery disabled by test fixture"
      ! cannot_list "$ns" || forbid "cannot list resource \"deployments\" in API group \"apps\" in the namespace \"$ns\""
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

# The roller's REQUIRED_TENANT_NAMESPACES default is the staging canary, which
# the scenarios below are not about. They run with it explicitly EMPTY (none
# required). The required-namespace cases further down unset it, to test the
# DEFAULT rather than just the parameter, or set it on purpose.
export REQUIRED_TENANT_NAMESPACES=""
# No declared scope unless a case declares one: production's shape, in which
# nothing is ever skipped.
unset ROLLER_SCOPE_NAMESPACES
CANARY_NS="enteros-staging-canary-k8s"
ORG_K="c0c0c0c0-0000-4000-8000-0000000000c0"

# row <field>... — one TAB-separated fixture line.
row() { local IFS=$'\t'; printf '%s\n' "$*"; }

reset_state() {
  : > "$K8RT/set-image.log"; : > "$K8RT/calls.log"; : > "$K8RT/buildinfo.log"; : > "$K8RT/exec.log"
  rm -f "$K8RT/rollout.fail" "$K8RT/pods.after" "$K8RT/proxy.fail" "$K8RT/discovery.fail" "$K8RT/set-image.fail" \
        "$K8RT/scope-ns" "$K8RT/nolist-ns" "$K8RT/cani.fail" "$K8RT/cani.reason" "$K8RT/cani.stderr"
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
  LAST_OUT="$out"
  if [ "$rc" != "$exp" ]; then
    echo "FAIL: $label — exit $rc, expected $exp"; echo "$out" | sed 's/^/    /'; FAILED=$((FAILED+1)); return
  fi
  if [ "$needle" != "-" ] && ! printf '%s' "$out" | grep -qF -- "$needle"; then
    echo "FAIL: $label — output missing '$needle'"; echo "$out" | sed 's/^/    /'; FAILED=$((FAILED+1)); return
  fi
  echo "ok: $label (rc=$rc)"; PASS=$((PASS+1))
}
LAST_OUT=""
# saw <label> <fixed string>: the LAST check's output contained this. A here-
# string, not `printf | grep -q`, which can SIGPIPE into a false "no match".
saw() {
  if grep -qF -- "$2" <<< "$LAST_OUT"; then echo "ok: $1"; PASS=$((PASS+1)); return; fi
  echo "FAIL: $1 — output missing '$2'"; FAILED=$((FAILED+1))
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

echo "== missing inventory is separate from release targets =="
reset_state
check "missing-only fleet cannot claim to have rolled a tenant" 1 "DRIFT:no-workload" \
      "$CENSUS_GHOST" --digest "$D_NEW"
check "production pin-only release counts missing tenants without claiming a roll" 0 "eligible=0 rolled=0 skipped=0 failed=0 missing=1" \
      "$CENSUS_GHOST" --digest "$D_NEW" --allow-empty
reset_state
printf '%b\n' "ns-alpha\tenteros-tenant-abc\t$ORG_A\t$REPO@$D_NEW" > "$K8RT/pods.after"
CENSUS_MIXED="printf 'alpha\t$ORG_A\tk8s\tns-alpha\nghost\t$ORG_C\tk8s\tns-ghost\n'"
check "missing inventory does not fail a successful release" 0 "rolled=1 skipped=0 failed=0 missing=1" \
      "$CENSUS_MIXED" --digest "$D_NEW"
reset_state
check "plan reports missing inventory and retains existing targets" 0 "eligible=1 rolled=0 skipped=0 failed=0 missing=1" \
      "$CENSUS_MIXED" --digest "$D_NEW" --dry-run
reset_state
touch "$K8RT/discovery.fail"
check "API failure is never classified as missing inventory" 1 "workload discovery failed" \
      "$CENSUS_MIXED" --digest "$D_NEW"
if [ -s "$K8RT/set-image.log" ]; then
  echo "FAIL: discovery failure mutated a workload"; FAILED=$((FAILED+1))
else
  echo "ok: discovery failure performs no mutation"; PASS=$((PASS+1))
fi
reset_state
touch "$K8RT/set-image.fail"
check "target lost after discovery is still a release failure" 1 "FAIL:set-image" \
      "$CENSUS_MIXED" --digest "$D_NEW"
reset_state
printf '%b\n' "ns-zero\tduplicate\t1\t$ORG_B\t$REPO@$D_OLD" >> "$K8RT/deploys"
check "ambiguous inventory fails before rolling any target" 1 "ambiguous" \
      "$CENSUS_AB" --digest "$D_NEW"
if [ -s "$K8RT/set-image.log" ]; then
  echo "FAIL: ambiguous discovery mutated a workload"; FAILED=$((FAILED+1))
else
  echo "ok: ambiguous discovery performs no mutation"; PASS=$((PASS+1))
fi

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
logged "scope was asked first, in that namespace, as 'auth can-i patch deployments'" \
  "ns=ns-alpha auth can-i patch deployments"

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
# A namespace IN scope (can-i patch: yes) whose lookup still fails — here a
# partial grant that can patch but not list — is an ACCESS fault. Reporting it
# as "no workload" would send the operator after control-plane drift that does
# not exist. (A namespace with no role at all is out of scope: see below.)
echo ns-alpha > "$K8RT/nolist-ns"
check "an in-scope namespace whose lookup is refused is FAIL:lookup-error, not drift" 1 "FAIL:lookup-error" \
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

# ── fixtures for scope and the required canary ──────────────────────────────
canary_fixture() { # the staging canary org: 1 replica serving the OLD digest
  row "$CANARY_NS" enteros-tenant 1 "$ORG_K" "$REPO@$D_OLD" >> "$K8RT/deploys"
  row "$CANARY_NS" enteros-tenant-k "$ORG_K" "$REPO@$D_OLD" >> "$K8RT/pods"
}
after_new() { # after_new <git_sha>: alpha and the canary both serve the NEW digest
  {
    row ns-alpha enteros-tenant-abc "$ORG_A" "$REPO@$D_NEW" "" "" "" "$1"
    row "$CANARY_NS" enteros-tenant-k "$ORG_K" "$REPO@$D_NEW" "" "" "" "$1"
  } > "$K8RT/pods.after"
}
CENSUS_AK="printf 'alpha\t$ORG_A\tk8s\tns-alpha\nstaging-canary-k8s\t$ORG_K\tk8s\t$CANARY_NS\n'"
CENSUS_K="printf 'staging-canary-k8s\t$ORG_K\tk8s\t$CANARY_NS\n'"
SHA_OK="0badc0de0123456789abcdef0123456789abcdef"
SHA_BAD="feedface0123456789abcdef0123456789abcdef"
# default_required <check args...>: one check with REQUIRED_TENANT_NAMESPACES
# UNSET, i.e. under the roller's own default.
default_required() { unset REQUIRED_TENANT_NAMESPACES; check "$@"; export REQUIRED_TENANT_NAMESPACES=""; }

# with_scope <ns-list> <check args...>: one check with ROLLER_SCOPE_NAMESPACES
# DECLARED as <ns-list> and REQUIRED_TENANT_NAMESPACES unset (the roller's own
# default), which is the staging lane's shape.
with_scope() {
  local s="$1"; shift
  export ROLLER_SCOPE_NAMESPACES="$s"; unset REQUIRED_TENANT_NAMESPACES
  check "$@"
  unset ROLLER_SCOPE_NAMESPACES; export REQUIRED_TENANT_NAMESPACES=""
}
# no_calls_into <label> <ns>: not one kubectl call went into <ns>.
no_calls_into() {
  local hits; hits="$(grep "^ns=$2 " "$K8RT/calls.log" || true)"
  if [ -z "$hits" ]; then echo "ok: $1"; PASS=$((PASS+1)); return; fi
  echo "FAIL: $1"; printf '%s\n' "$hits" | sed 's/^/    /'; FAILED=$((FAILED+1))
}

echo "== scope: skipping is OPT-IN, through a DECLARED ROLLER_SCOPE_NAMESPACES =="
# The staging lane: its credential holds a RoleBinding in the canary only
# (scope-ns), and the lane DECLARES that list.
reset_state; canary_fixture; after_new "$SHA_OK"
printf '%s\n' "$CANARY_NS" > "$K8RT/scope-ns"
with_scope "$CANARY_NS" "a tenant outside the DECLARED scope is skipped and reported, not failed" 0 \
  "skipped-out-of-scope" "$CENSUS_AK" --tag staging-0badc0de
saw "the SUMMARY counts it as out of scope, not as a failure" \
  "found=2 in_scope=1 out_of_scope=1 eligible=1 rolled=1 skipped=0 failed=0"
saw "the declared, required canary was rolled and verified" \
  "required namespace '$CANARY_NS' was rolled and every serving pod's /buildinfo verified"
no_calls_into "the skipped namespace was not touched at all, not even by auth can-i" ns-alpha
logged "the canary's /buildinfo was read through the pod proxy" \
  "get --raw /api/v1/namespaces/$CANARY_NS/pods/enteros-tenant-k:8080/proxy/buildinfo"

reset_state; canary_fixture; after_new "$SHA_OK"
: > "$K8RT/scope-ns"   # the declared namespace's RoleBinding is missing
with_scope "$CANARY_NS" "inside the declared scope, an auth can-i 'no' (missing RoleBinding) FAILS, never skips" 1 \
  "FAIL:scope-check" "$CENSUS_AK" --tag staging-0badc0de
saw "...and says the binding must exist" "is IN the declared ROLLER_SCOPE_NAMESPACES"

reset_state; canary_fixture; after_new "$SHA_OK"
with_scope "$CANARY_NS ns-alpha" "every DECLARED scope entry must be rolled and verified too" 1 \
  "required namespace 'ns-alpha' is not in this run" "$CENSUS_K" --tag staging-0badc0de

reset_state; canary_fixture
export ROLLER_SCOPE_NAMESPACES="ns-nowhere"
check "a declared scope that matches no tenant FAILS as vacuous" 1 \
  "NONE is inside this credential's scope" "$CENSUS_AK" --digest "$D_NEW"
saw "...every tenant reported skipped" "found=2 in_scope=0 out_of_scope=2"
export ROLLER_SCOPE_NAMESPACES=""
check "a DECLARED but EMPTY scope is refused up front" 2 "set but EMPTY" "$CENSUS_AK" --digest "$D_NEW"
unset ROLLER_SCOPE_NAMESPACES
with_scope "ns-alpha" "a required namespace outside the declared scope is refused up front" 2 \
  "is outside ROLLER_SCOPE_NAMESPACES" "$CENSUS_AK" --digest "$D_NEW"
if [ ! -s "$K8RT/set-image.log" ]; then echo "ok: ...and nothing was mutated"; PASS=$((PASS+1))
else echo "FAIL: a refused scope still issued set image"; FAILED=$((FAILED+1)); fi

echo "== with NO declared scope (production's shape), nothing is ever skipped =="
# The 2026-10-11 production census, replayed (redeploy-tenants-on-main run
# 770669, job 1086175; slugs anonymised): 9 k8s orgs, 4 with live namespaces and
# 5 whose namespaces were DELETED (one recorded with an empty instance_id, so
# its namespace comes from the slug fallback). The production credential's RBAC
# follows namespace existence: a generated RoleBinding in each LIVE tenant
# namespace. So `auth can-i patch` answers "no" for the 5, and reading that
# "no" as "out of scope" turned the frozen production plan green over real
# drift. It must stay red.
replay_fixture() {
  local i
  : > "$K8RT/deploys"; : > "$K8RT/pods"; : > "$K8RT/scope-ns"
  CENSUS_PROD="printf '"
  for i in 1 2 3 4; do
    row "enteros-live-$i" enteros-tenant 1 "1111111$i-0000-4000-8000-00000000000$i" "$REPO@$D_OLD" >> "$K8RT/deploys"
    row "enteros-live-$i" "enteros-tenant-$i" "1111111$i-0000-4000-8000-00000000000$i" "$REPO@$D_OLD" >> "$K8RT/pods"
    printf '%s\n' "enteros-live-$i" >> "$K8RT/scope-ns"
    CENSUS_PROD="${CENSUS_PROD}live-$i\\t1111111$i-0000-4000-8000-00000000000$i\\tk8s\\tenteros-live-$i\\n"
  done
  for i in 1 2 3 4; do
    CENSUS_PROD="${CENSUS_PROD}gone-$i\\t2222222$i-0000-4000-8000-00000000000$i\\tk8s\\tenteros-gone-$i\\n"
  done
  CENSUS_PROD="${CENSUS_PROD}gone-5\\t22222225-0000-4000-8000-000000000005\\tk8s\\t\\n'"
}
reset_state; replay_fixture
check "replay: the 5 orgs whose namespaces were deleted FAIL the production plan" 1 \
  "FAIL:scope-check" "$CENSUS_PROD" --digest "$D_NEW" --dry-run
saw "...all five are failures and none is skipped" \
  "found=9 in_scope=4 out_of_scope=0 eligible=4 rolled=0 skipped=0 failed=5"
n="$(grep -cE '^gone-[1-5] +enteros-gone-[1-5] +- +- +FAIL:scope-check$' <<< "$LAST_OUT" || true)"
if [ "$n" = "5" ] && ! grep -q 'skipped-out-of-scope' <<< "$LAST_OUT"; then
  echo "ok: ...each deleted namespace is a FAIL:scope-check row, including the slug-derived one"; PASS=$((PASS+1))
else
  echo "FAIL: expected 5 FAIL:scope-check rows and no skip, got ${n}"; FAILED=$((FAILED+1))
fi
reset_state; replay_fixture; rm -f "$K8RT/scope-ns"   # CONTROL: a cluster-wide grant
check "replay CONTROL: with a cluster-wide grant the same five become DRIFT:no-workload instead of scope-check" 0 \
  "DRIFT:no-workload" "$CENSUS_PROD" --digest "$D_NEW" --dry-run
saw "...all five are DRIFT:no-workload with missing=5 and eligible=4" \
  "found=9 in_scope=9 out_of_scope=0 eligible=4 rolled=0 skipped=0 failed=0 missing=5"

reset_state; canary_fixture; after_new "$SHA_OK"
printf '%s\n' ns-alpha > "$K8RT/scope-ns"   # a LIVE tenant whose binding is missing
check "a live tenant whose binding is missing FAILS a real roll" 1 \
  "FAIL:scope-check" "$CENSUS_AK" --tag staging-0badc0de
saw "...counted as a failure, not as out of scope" \
  "found=2 in_scope=1 out_of_scope=0 eligible=1 rolled=1 skipped=0 failed=1"

reset_state; canary_fixture
: > "$K8RT/scope-ns"   # bound nowhere
check "a run with NO tenant in scope FAILS as vacuous" 1 \
  "NONE is inside this credential's scope" "$CENSUS_AK" --digest "$D_NEW"
saw "...each tenant a scope-check failure, none skipped" \
  "found=2 in_scope=0 out_of_scope=0 eligible=0 rolled=0 skipped=0 failed=2"
if [ ! -s "$K8RT/set-image.log" ]; then echo "ok: ...and mutated nothing"; PASS=$((PASS+1))
else echo "FAIL: a vacuous run issued set image"; FAILED=$((FAILED+1)); fi
check "the vacuous-scope gate holds in --dry-run too" 1 \
  "NONE is inside this credential's scope" "$CENSUS_AK" --digest "$D_NEW" --dry-run
# --allow-empty switches off the "none eligible" gate, which leaves the scope
# gate as the ONLY thing between this run and a green roll of nothing.
check "--allow-empty does not waive the vacuous-scope gate" 1 \
  "NONE is inside this credential's scope" "$CENSUS_AK" --digest "$D_NEW" --allow-empty

echo "== anything but a clean 'yes' from auth can-i fails =="
reset_state; touch "$K8RT/cani.fail"
check "an 'auth can-i' ERROR fails closed as FAIL:scope-check" 1 \
  "FAIL:scope-check" "$CENSUS_A" --digest "$D_NEW"
reset_state
printf '%s' 'RBAC: role.rbac.authorization.k8s.io "tenant-fleet-roller" not found' > "$K8RT/cani.reason"
check "a 'no' carrying an RBAC evaluation error (a broken binding) fails closed too" 1 \
  "FAIL:scope-check" "$CENSUS_A" --digest "$D_NEW"
reset_state
printf '%s\n' "Warning: the server doesn't have a resource type \"deployments\"" > "$K8RT/cani.stderr"
check "auth can-i writing to stderr FAILS even when it answers 'yes'" 1 \
  "FAIL:scope-check" "$CENSUS_A" --digest "$D_NEW"
saw "...and the stderr is shown" "the server doesn't have a resource type"
: > "$K8RT/scope-ns"
check "...and a 'no' with stderr is a failure too, never a skip" 1 \
  "FAIL:scope-check" "$CENSUS_A" --digest "$D_NEW"

echo "== a REQUIRED namespace (default: the staging canary) must be rolled AND verified =="
reset_state; after_new "$SHA_OK"
default_required "the canary missing from the census FAILS the run" 1 \
  "required namespace '$CANARY_NS' is not in this run" "$CENSUS_A" --tag staging-0badc0de
saw "...even though every tenant it did find rolled cleanly" \
  "found=1 in_scope=1 out_of_scope=0 eligible=1 rolled=1 skipped=0 failed=0"

reset_state; canary_fixture
{
  row ns-alpha enteros-tenant-abc "$ORG_A" "$REPO@$D_NEW" "" "" "" "$SHA_OK"
  row "$CANARY_NS" enteros-tenant-k "$ORG_K" "$REPO@$D_NEW" "" "" "" "$SHA_BAD"
} > "$K8RT/pods.after"
default_required "the canary's /buildinfo reporting another git_sha FAILS the run" 1 \
  "required namespace '$CANARY_NS' was not rolled and verified" "$CENSUS_AK" --tag staging-0badc0de

reset_state; canary_fixture; after_new "$SHA_OK"; touch "$K8RT/proxy.fail"
default_required "the canary's pod proxy being unreachable FAILS the run (a KNOWN sha must be confirmed)" 1 \
  "required namespace '$CANARY_NS' was not rolled and verified" "$CENSUS_K" --tag staging-0badc0de

# NO git sha known (a revert to a pin recorded without one): the digest of every
# serving pod is the verification, and the run says so instead of failing a
# rollback that verifiably landed. This relaxes missing METADATA only: the two
# cases above, where a sha IS known, still fail.
reset_state; canary_fixture; after_new "$SHA_OK"
default_required "with NO git sha known, a canary verified by its imageID digest passes, with a warning" 0 \
  "::warning::required namespace '$CANARY_NS' was rolled and verified by the imageID digest" \
  "$CENSUS_AK" --digest "$D_NEW"
reset_state; canary_fixture
row ns-alpha enteros-tenant-abc "$ORG_A" "$REPO@$D_NEW" > "$K8RT/pods.after"
row "$CANARY_NS" enteros-tenant-k "$ORG_K" "$REPO@$D_OLD" >> "$K8RT/pods.after"
default_required "...but a canary still on the OLD digest FAILS it" 1 \
  "staging-canary-k8s is FAIL:digest-mismatch" "$CENSUS_AK" --digest "$D_NEW"

reset_state
row "$CANARY_NS" enteros-tenant 0 "$ORG_K" "$REPO@$D_OLD" >> "$K8RT/deploys"
row ns-alpha enteros-tenant-abc "$ORG_A" "$REPO@$D_NEW" "" "" "" "$SHA_OK" > "$K8RT/pods.after"
default_required "a canary scaled to zero FAILS the run" 1 \
  "staging-canary-k8s is SKIP:scaled-to-zero" "$CENSUS_AK" --tag staging-0badc0de

reset_state; canary_fixture; after_new "$SHA_OK"
printf '%s\n' ns-alpha > "$K8RT/scope-ns"   # the canary's RoleBinding is missing
default_required "a canary the credential cannot roll FAILS the run" 1 \
  "staging-canary-k8s is FAIL:scope-check" "$CENSUS_AK" --tag staging-0badc0de

reset_state
default_required "--allow-empty does not waive a required namespace" 1 \
  "claims contradict each other" "$CENSUS_DOCKER" --digest "$D_NEW" --allow-empty

reset_state; canary_fixture
default_required "--dry-run: a planned canary satisfies the requirement" 0 \
  "required namespace '$CANARY_NS' is planned" "$CENSUS_AK" --digest "$D_NEW" --dry-run
default_required "--dry-run: a canary missing from the census still FAILS" 1 \
  "required namespace '$CANARY_NS' is not in this run" "$CENSUS_A" --digest "$D_NEW" --dry-run
reset_state
row "$CANARY_NS" enteros-tenant 0 "$ORG_K" "$REPO@$D_OLD" >> "$K8RT/deploys"   # present, not plannable
default_required "--dry-run: a canary that is present but not planned (scaled to zero) FAILS" 1 \
  "staging-canary-k8s is SKIP:scaled-to-zero" "$CENSUS_AK" --digest "$D_NEW" --dry-run

reset_state; after_new "$SHA_OK"
export REQUIRED_TENANT_NAMESPACES="ns-alpha"
check "REQUIRED_TENANT_NAMESPACES can name any namespace; met when it is rolled and verified" 0 \
  "required namespace 'ns-alpha' was rolled" "$CENSUS_A" --tag staging-0badc0de
export REQUIRED_TENANT_NAMESPACES="ns-alpha,$CANARY_NS"
check "...and EVERY namespace in a comma-separated list must be met" 1 \
  "required namespace '$CANARY_NS' is not in this run" "$CENSUS_A" --tag staging-0badc0de
export REQUIRED_TENANT_NAMESPACES=""

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
