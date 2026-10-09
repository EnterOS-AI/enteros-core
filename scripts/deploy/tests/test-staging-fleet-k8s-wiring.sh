#!/usr/bin/env bash
# test-staging-fleet-k8s-wiring.sh — the staging lane's k8s arm, offline.
#
# WHAT THIS IS FOR
# ----------------
# The staging CD lane rolled NOTHING for eight days: `redeploy-fleet` passed no
# census source, so redeploy-staging-fleet.sh never armed its k8s arm, its
# anti-vacuity guard correctly refused, and rollback-pin reverted the pin on
# every run. This harness pins the three properties that fix has to preserve:
#
#   1. THE ANTI-VACUITY GUARD STILL REFUSES. With no census source and no docker
#      tenant, the roll must still exit non-zero. Everything else here would be
#      worthless if the way to "fix" the lane were to make an empty roll pass.
#   2. A GENUINELY FAILED ROLL STILL FAILS. The k8s arm's exit status is folded
#      into the docker roller's verbatim, so a docker arm that legitimately
#      matches zero containers cannot mask a failed k8s arm.
#   3. AN EXCLUSION MUST BE EARNED. fleet-drift-audit.sh reports drift by name
#      and refuses any excluded slug that is not measurably drifted — which is
#      what keeps EXCLUDE_SLUGS from becoming `--allow-empty` spelled
#      differently.
#
# It drives the REAL shipped scripts against a fake docker + fake kubectl + fake
# Infisical reader. Following test-managed-flags.sh and test-k8s-fleet-roller.sh:
# probing a helper in isolation while nothing calls it is itself a vacuous pass.
#
# THE FAKE DOCKER IS LOAD-BEARING, not convenience. test-k8s-fleet-roller.sh's
# docker-arm cases are wrapped in `if docker info`, so on a runner without a
# daemon the empty-fleet gate — property 1 above, the single most important
# assertion about this lane — silently does not run. Here it always runs.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DOCKER_ROLLER="$here/../redeploy-staging-fleet.sh"
K8S_ROLLER="$here/../redeploy-tenant-fleet-k8s.sh"
RESOLVER="$here/../resolve-k8s-fleet-census.sh"
AUDITOR="$here/../fleet-drift-audit.sh"

PASS=0 ; FAILED=0
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/state" "$tmp/work"
export K8RT="$tmp/state"

D_OLD="sha256:1111111111111111111111111111111111111111111111111111111111111111"
D_NEW="sha256:2222222222222222222222222222222222222222222222222222222222222222"
REPO="registry.enteros.invalid/molecule-ai/molecule-tenant"
ORG_A="aaaaaaaa-0000-4000-8000-0000000000aa"
ORG_G="99999999-0000-4000-8000-000000000099"

# ── fake docker: a daemon with ZERO staging tenants ──────────────────────────
# This is the measured staging shape (census 2026-08-12: 11 orgs, k8s=11,
# docker=0), and it is the shape in which a naive roller reports success.
cat > "$tmp/bin/docker" <<'FAKED'
#!/usr/bin/env bash
set -uo pipefail
case "${1:-}" in
  info) exit 0;;
  pull) exit 0;;
  image) [ "${2:-}" = "inspect" ] && exit 0; exit 1;;
  ps)  exit 0;;                      # no containers carry the tenant labels
  volume) [ "${2:-}" = "ls" ] && exit 0; exit 0;;
  inspect) exit 0;;
esac
exit 0
FAKED
chmod +x "$tmp/bin/docker"

# ── fake kubectl ─────────────────────────────────────────────────────────────
# Same fixture model as test-k8s-fleet-roller.sh: cluster state is a file, and
# `set image` promotes the "after" fixture over the current one, so "the rollout
# succeeded and nothing moved" is expressible with no cluster.
cat > "$tmp/bin/kubectl" <<'FAKEK'
#!/usr/bin/env bash
set -uo pipefail
ns="" ; args=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    -n) ns="$2"; shift 2;;
    --kubeconfig) shift 2;;
    *) args+=("$1"); shift;;
  esac
done
set -- "${args[@]:-}"
org_of_sel() { printf '%s' "${*}" | sed -n 's/.*molecule\.org-id=\([0-9a-f-]*\).*/\1/p'; }
case "${1:-}" in
  version) echo '{"clientVersion":{"gitVersion":"fake"}}'; exit 0;;
  config)  [ "${2:-}" = "current-context" ] && { echo "fake-ctx"; exit 0; }; exit 0;;
  exec)
    # Two distinct exec shapes: the CENSUS (psql into the CP db pod) and the
    # roller's /buildinfo probe (wget inside a tenant pod).
    if printf '%s' "$*" | grep -q 'psql'; then
      [ -f "$K8RT/census.fail" ] && { echo "permission denied for table org_substrates" >&2; exit 1; }
      cat "$K8RT/census.tsv" 2>/dev/null || true
      exit 0
    fi
    printf '{"git_sha":"%s"}\n' "${FAKE_BUILDINFO_SHA:-deadbee}"
    exit 0;;
  set)
    printf '%s\n' "$*" >> "$K8RT/set-image.log"
    [ -f "$K8RT/pods.after" ] && cp "$K8RT/pods.after" "$K8RT/pods"
    exit 0;;
  rollout) [ -f "$K8RT/rollout.fail" ] && exit 1; exit 0;;
  get) shift;;
  *) exit 0;;
esac
case "${1:-}" in
  deploy|deployment|deployments)
    shift; sel="$*"
    if printf '%s' "$sel" | grep -q -- '-A'; then
      org="$(org_of_sel "$sel")"
      awk -F'\t' -v o="$org" '$4==o {printf "%s\t%s\t%s\n",$1,$2,$3}' "$K8RT/deploys"
    else
      name="$1"
      awk -F'\t' -v n="$name" -v ns="$ns" '$1==ns && $2==n {printf "%s",$5}' "$K8RT/deploys"
    fi;;
  pods|pod)
    shift; sel="$*"; org="$(org_of_sel "$sel")"
    if printf '%s' "$sel" | grep -q 'imageID'; then
      awk -F'\t' -v o="$org" -v ns="$ns" '$1==ns && $3==o {print $4}' "$K8RT/pods"
    else
      awk -F'\t' -v o="$org" -v ns="$ns" '$1==ns && $3==o {print $2}' "$K8RT/pods"
    fi;;
esac
exit 0
FAKEK
chmod +x "$tmp/bin/kubectl"
export PATH="$tmp/bin:$PATH"
export KUBECTL="$tmp/bin/kubectl"

reset_state() {
  : > "$K8RT/set-image.log"; rm -f "$K8RT/rollout.fail" "$K8RT/pods.after" "$K8RT/census.fail"
  printf '%s\n' "ns-alpha	enteros-tenant	1	$ORG_A	$REPO@$D_OLD" > "$K8RT/deploys"
  printf '%s\n' "ns-alpha	enteros-tenant-abc	$ORG_A	$REPO@$D_OLD" > "$K8RT/pods"
  # alpha is a real, rollable tenant; ghost is control-plane/cluster DRIFT (the
  # CP places it on k8s, no Deployment carries its org-id).
  printf '%s\n' \
    "alpha	$ORG_A	k8s	ns-alpha" \
    "ghost	$ORG_G	k8s	" > "$K8RT/census.tsv"
}

say() { printf '\n== %s ==\n' "$*"; }
ok()   { echo "ok: $1"; PASS=$((PASS+1)); }
bad()  { echo "FAIL: $1"; printf '%s\n' "${2:-}" | sed 's/^/    /'; FAILED=$((FAILED+1)); }

# run_docker_roller <expect_rc> <needle|-> <label> -- <env assignments...> -- <args...>
drun() {
  local exp="$1" needle="$2" label="$3"; shift 3
  local out rc=0
  out="$("$@" 2>&1)" || rc=$?
  if [ "$rc" != "$exp" ]; then bad "$label — exit $rc, expected $exp" "$out"; return; fi
  if [ "$needle" != "-" ] && ! printf '%s' "$out" | grep -qF -- "$needle"; then
    bad "$label — output missing '$needle'" "$out"; return
  fi
  ok "$label (rc=$rc)"
}

# ─────────────────────────────────────────────────────────────────────────────
say "1. the anti-vacuity guard STILL refuses an unarmed roll"
# This is the mutation that matters most: the "fix" for this lane must NOT be to
# let a roll that touches nothing exit 0. With no SUBSTRATE_CENSUS_CMD and no
# docker tenant, the shipped script must still refuse.
reset_state
drun 1 "would roll" "no census source + 0 docker tenants => REFUSED" \
  env -u SUBSTRATE_CENSUS_CMD -u CP_DATABASE_URL TENANT_FLAGS="" \
      bash "$DOCKER_ROLLER" --tag staging-0badc0de
drun 1 "no k8s arm was" "the refusal NAMES the missing census wiring" \
  env -u SUBSTRATE_CENSUS_CMD -u CP_DATABASE_URL TENANT_FLAGS="" \
      bash "$DOCKER_ROLLER" --tag staging-0badc0de

say "2. an armed roll with a healthy k8s fleet succeeds, and re-pins repo-relative"
reset_state
printf '%s\n' "ns-alpha	enteros-tenant-abc	$ORG_A	$REPO@$D_NEW" > "$K8RT/pods.after"
drun 0 "k8s arm OK" "census wired + healthy fleet => rolled" \
  env TENANT_FLAGS="" KUBECTL="$tmp/bin/kubectl" \
      SUBSTRATE_CENSUS_CMD="cat $K8RT/census.tsv" \
      K8S_TENANT_DIGEST="$D_NEW" EXCLUDE_SLUGS="ghost" \
      CONVERGE_ATTEMPTS=1 CONVERGE_SLEEP=0 \
      bash "$DOCKER_ROLLER" --tag staging-0badc0de
if grep -qF "${REPO}@${D_NEW}" "$K8RT/set-image.log"; then
  ok "the k8s arm re-pinned <the workload's own repo>@<digest>, not this lane's registry host"
else
  bad "set image did not use the workload's existing repo" "$(cat "$K8RT/set-image.log")"
fi

say "3. a genuinely FAILED k8s roll still fails the whole roll"
reset_state
touch "$K8RT/rollout.fail"
drun 1 "k8s arm FAILED" "rollout that never completes => the roll FAILS" \
  env TENANT_FLAGS="" KUBECTL="$tmp/bin/kubectl" \
      SUBSTRATE_CENSUS_CMD="cat $K8RT/census.tsv" \
      K8S_TENANT_DIGEST="$D_NEW" EXCLUDE_SLUGS="ghost" \
      CONVERGE_ATTEMPTS=1 CONVERGE_SLEEP=0 \
      bash "$DOCKER_ROLLER" --tag staging-0badc0de
# ZERO docker tenants rolled successfully in that run. If the docker arm's
# "nothing to do" could mask the k8s arm, the assertion above would have been 0.

say "4. an EMPTY census is not an empty fleet"
reset_state
: > "$K8RT/census.tsv"
drun 1 "refusing to report success" "census returns zero rows => REFUSED" \
  env TENANT_FLAGS="" KUBECTL="$tmp/bin/kubectl" \
      SUBSTRATE_CENSUS_CMD="cat $K8RT/census.tsv" \
      K8S_TENANT_DIGEST="$D_NEW" \
      bash "$DOCKER_ROLLER" --tag staging-0badc0de

# ─────────────────────────────────────────────────────────────────────────────
say "5. fleet-drift-audit: drift is REPORTED, and an exclusion must be EARNED"
reset_state
arun() { # arun <expect_rc> <needle|-> <label> <exclude>
  local exp="$1" needle="$2" label="$3" exc="${4:-}"
  local out rc=0
  out="$(KUBECTL="$tmp/bin/kubectl" SUBSTRATE_CENSUS_CMD="cat $K8RT/census.tsv" \
         K8S_ROLLER="$K8S_ROLLER" CONVERGE_ATTEMPTS=1 CONVERGE_SLEEP=0 \
         bash "$AUDITOR" --digest "$D_NEW" --exclude "$exc" 2>&1)" || rc=$?
  if [ "$rc" != "$exp" ]; then bad "$label — exit $rc, expected $exp" "$out"; return; fi
  if [ "$needle" != "-" ] && ! printf '%s' "$out" | grep -qF -- "$needle"; then
    bad "$label — output missing '$needle'" "$out"; return
  fi
  ok "$label (rc=$rc)"
}
arun 0 "::warning::fleet drift: org ghost" "drift is named as a warning even when excluded" "ghost"
arun 0 "::warning::fleet drift: org ghost" "drift is named as a warning when NOT excluded" ""
arun 0 "WILL fail on it" "unexcluded drift is announced as a coming roll failure" ""
arun 1 "are NOT drifted: alpha" "excluding a HEALTHY tenant is REFUSED" "alpha"
arun 1 "are NOT drifted" "excluding a tenant that does not exist at all is REFUSED" "no-such-org"

say "5b. a drift parse that matches nothing must not read as a clean fleet"
# Point the auditor at a roller that fails WITHOUT emitting a report table. A
# naive parse finds no FAIL: rows, concludes "no drift", and would then accept
# ANY exclusion list. It must call that a parse fault instead.
cat > "$tmp/bin/silent-roller.sh" <<'SILENT'
#!/usr/bin/env bash
echo "some unrelated failure" >&2
exit 1
SILENT
chmod +x "$tmp/bin/silent-roller.sh"
out="$(K8S_ROLLER="$tmp/bin/silent-roller.sh" bash "$AUDITOR" --digest "$D_NEW" --exclude "alpha" 2>&1)" && rc=0 || rc=$?
if [ "${rc:-0}" = "1" ] && printf '%s' "$out" | grep -qF "PARSE fault"; then
  ok "roller refused + zero FAIL rows parsed => named a PARSE fault, not a clean fleet"
else
  bad "a silent roller failure was not reported as a parse fault (rc=${rc:-0})" "$out"
fi

# ─────────────────────────────────────────────────────────────────────────────
say "6. resolve-k8s-fleet-census: every failure is named, and none of them pass"
mk_reader() { # mk_reader <mode>
  cat > "$tmp/bin/reader.py" <<PYEOF
import sys
mode = "$1"
if mode == "missing":   sys.exit(10)
if mode == "broken":    sys.exit(1)
if mode == "empty":     print(""); sys.exit(0)
if mode == "notb64":    print("!!!not base64!!!"); sys.exit(0)
print("$(printf 'apiVersion: v1\nclusters: []\n' | base64 | tr -d '\n')")
PYEOF
}
PY="$(command -v python3 || command -v python)"
rrun() { # rrun <expect_rc> <needle> <label> <reader-mode>
  local exp="$1" needle="$2" label="$3" mode="$4"
  mk_reader "$mode"
  local out rc=0
  out="$(KUBECTL="$tmp/bin/kubectl" DEPLOY_PYTHON3="$PY" \
         INFISICAL_READER="$tmp/bin/reader.py" \
         CP_CENSUS_NAMESPACE=controlplane-staging CP_CENSUS_DB_POD=db-0 \
         CP_CENSUS_DB_USER=cps CP_CENSUS_DB_NAME=cp_staging \
         bash "$RESOLVER" --env-out "$tmp/work/out.env" --work-dir "$tmp/work" 2>&1)" || rc=$?
  if [ "$rc" != "$exp" ]; then bad "$label — exit $rc, expected $exp" "$out"; return; fi
  if [ "$needle" != "-" ] && ! printf '%s' "$out" | grep -qF -- "$needle"; then
    bad "$label — output missing '$needle'" "$out"; return
  fi
  ok "$label (rc=$rc)"
}
reset_state
# The `-e` DISCIPLINE CASE. A bare `V="$(reader)"` under `bash -e` dies ON THE
# ASSIGNMENT, above the diagnostics — which is precisely how the production k8s
# arm's first run reported nothing but `command not found`. Asserting the
# OPERATOR STEP sentence PRINTS is what proves the rc-captured form is still
# there; delete it and this case fails.
rrun 1 "OPERATOR STEP" "an unseeded K8S_FLEET_KUBECONFIG names the operator step" missing
rrun 1 "reader exit 1"  "a broken read is named as a READ failure, not a missing key" broken
rrun 1 "returned EMPTY" "a declared-but-empty secret is refused" empty
rrun 1 "not valid base64" "a raw (non-base64) kubeconfig is refused" notb64

reset_state
rrun 0 "census source ready" "happy path resolves a census" good
for k in KUBECONFIG SUBSTRATE_CENSUS_CMD FLEET_CENSUS_TSV; do
  if grep -q "^${k}=" "$tmp/work/out.env"; then ok "env-out publishes $k"
  else bad "env-out is missing $k" "$(cat "$tmp/work/out.env")"; fi
done

reset_state
: > "$K8RT/census.tsv"
rrun 1 "returned ZERO rows" "a census with no rows is a broken query, not an empty fleet" good
reset_state
printf '%s\n' "dockerorg	dddddddd-0000-4000-8000-0000000000dd	docker	mol-x" > "$K8RT/census.tsv"
rrun 1 "NONE on the k8s substrate" "a census with no k8s row refuses rather than rolling nothing" good
reset_state
touch "$K8RT/census.fail"
rrun 1 "census FAILED" "a census that errors names all four coordinates" good

# ─────────────────────────────────────────────────────────────────────────────
say "7. the lane never asserts an empty fleet"
# --allow-empty is the one flag that makes every guard above optional. It must
# not appear anywhere in the staging CD lane, in any spelling.
WF="$here/../../../.gitea/workflows/staging-tenant-cd.yml"
if [ -f "$WF" ]; then
  # Comment lines are allowed to NAME it — the reason it is not passed is worth
  # writing down. Any non-comment line carrying it is a hard failure.
  live="$(grep -- '--allow-empty' "$WF" | sed 's/^[[:space:]]*//' | grep -v '^#' || true)"
  if [ -n "$live" ]; then
    bad "staging-tenant-cd.yml passes --allow-empty on a non-comment line" "$live"
  else
    ok "staging-tenant-cd.yml never passes --allow-empty (only names it in comments)"
  fi
else
  bad "staging-tenant-cd.yml not found at $WF" ""
fi

echo
echo "passed=$PASS failed=$FAILED"
[ "$FAILED" -eq 0 ] || exit 1
echo "ALL OK"
