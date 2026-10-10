#!/usr/bin/env bash
# redeploy-tenant-fleet-k8s.sh — roll the KUBERNETES tenant fleet onto a freshly
# promoted tenant image, and PROVE the roll landed on the running pods.
#
# WHY THIS EXISTS
# ---------------
# The historical fleet roller (scripts/deploy/redeploy-staging-fleet.sh) is
# local-docker ONLY: it enumerates with
#   docker ps --filter label=molecule.local-tenant=1 --filter label=molecule.cp-env=<env>
# On a control plane whose tenants have been migrated to the `k8s` substrate that
# filter matches ZERO containers, and the old script's empty-set branch logged
# "nothing to roll" and exited 0. So the documented step-4 fleet roll reported
# GREEN having rolled nothing, every time, while every tenant pod kept running
# whatever image it was provisioned with. Promoting a runtime image pin then only
# changed what FRESH provisions receive; running tenants never moved.
#
# That is a VACUOUS PASS — a guard that covers nothing and reports success. This
# script is built so it cannot produce one:
#
#   * found:0 is an ERROR unless --allow-empty is passed explicitly. "There were
#     no tenants" must be a deliberate operator claim, never an accident.
#   * eligible>0 but rolled:0 is an ERROR. A fleet where every workload turned
#     out to be un-rollable has not been deployed to.
#   * a Deployment scaled to 0 replicas is NOT counted as rolled. `kubectl
#     rollout status` returns success instantly for a 0-replica Deployment with
#     zero pods running the new image — the single easiest way to fake a green
#     fleet roll. Those are counted as `skipped` and reported by name.
#   * verification reads what the PODS ARE RUNNING (.status.containerStatuses[].imageID),
#     never the Deployment's .spec image. .spec is INTENT; imageID is the SSOT for
#     what is actually executing. A `kubectl set image` that the scheduler could
#     not satisfy leaves .spec updated and the old pod serving traffic.
#   * verification refuses to run "liveness-only". If neither a target digest nor
#     an EXPECTED_BUILD_SHA is resolvable, the script fails rather than asserting
#     something that cannot distinguish a landed roll from a no-op.
#
# ENUMERATION IS SUBSTRATE-DRIVEN, FROM THE CONTROL PLANE
# ------------------------------------------------------
# Targets are not guessed from namespace names and not taken from a status
# column. The census asks the control plane which orgs are on which substrate
# (org_substrates.substrate), then finds each k8s org's workload by the labels
# the provisioner stamps on it:
#     molecule.component=tenant, molecule.managed-by=controlplane,
#     molecule.org-id=<uuid>, molecule.org-slug=<slug>
# Matching on molecule.org-id (the CP's own primary key) means the CP row and the
# cluster object are joined on identity, not on a derived name — a namespace
# rename cannot silently empty the target set.
#
# Deliberately NOT keyed on org_instances.status. That column is known to drift:
# it reads 'running' for orgs that have no tenant pod at all. A roller keyed on a
# status column inherits the column's lies; a roller keyed on live workloads
# reports the drift instead (see the `no-workload` classification below).
#
# Usage:
#   redeploy-tenant-fleet-k8s.sh --digest sha256:<hex>  # PREFERRED for prod: re-pin every
#                                                       # tenant to <its own repo>@<digest>,
#                                                       # which is what runtime_image_pins stores
#   redeploy-tenant-fleet-k8s.sh --image <ref>          # roll onto an explicit ref
#   redeploy-tenant-fleet-k8s.sh --tag <tag>            # roll onto TENANT_IMAGE:<tag>
#   redeploy-tenant-fleet-k8s.sh --dry-run              # census + plan, ZERO mutations
#   redeploy-tenant-fleet-k8s.sh --only <substr>        # restrict to matching slug(s)
#   redeploy-tenant-fleet-k8s.sh --exclude a,b          # never touch these slugs
#   redeploy-tenant-fleet-k8s.sh --allow-empty          # assert "no k8s tenants expected"
#
# Env (no-hardcoding):
#   TENANT_IMAGE          registry.moleculesai.app/molecule-ai/molecule-tenant
#   KUBECTL               kubectl binary/wrapper (default: kubectl)
#   CP_DATABASE_URL       control-plane DSN used for the substrate census
#   SUBSTRATE_CENSUS_CMD  override the census entirely; must print TSV rows of
#                         "<slug>\t<org_id>\t<substrate>\t<instance_id>". Exists so
#                         the roller is testable without a control plane, and so a
#                         future CP admin endpoint can be swapped in without
#                         touching the roll/verify logic.
#   EXPECTED_BUILD_SHA    expected /buildinfo git_sha (auto-derived from a
#                         staging-<sha> tag when not set)
#   ROLLOUT_TIMEOUT       per-workload rollout wait (default 300s)
#   TENANT_CONTAINER      container name inside the pod (default: tenant)
#   EXCLUDE_SLUGS         same as --exclude, for CI env wiring
#
# SAFETY: --dry-run performs zero mutations. --exclude / EXCLUDE_SLUGS hard-skip
# named slugs before any mutation. This script only ever issues `kubectl set
# image` on a tenant Deployment; it never scales, drains, evicts, deletes or
# restarts anything, and never touches a PVC, namespace or StatefulSet.
set -euo pipefail

log() { printf '>> [k8s-fleet] %s\n' "$*" >&2; }
err() { printf '::error::%s\n' "$*" >&2; }

TENANT_IMAGE="${TENANT_IMAGE:-registry.moleculesai.app/molecule-ai/molecule-tenant}"
KUBECTL="${KUBECTL:-kubectl}"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-300s}"
TENANT_CONTAINER="${TENANT_CONTAINER:-tenant}"
IMAGE="" ; TAG="" ; DIGEST="" ; DRY_RUN=0 ; ONLY="" ; ALLOW_EMPTY=0
EXCLUDE="${EXCLUDE_SLUGS:-}"

usage() { sed -n '2,60p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    --image)       IMAGE="$2"; shift 2;;
    --digest)      DIGEST="$2"; shift 2;;
    --tag)         TAG="$2"; shift 2;;
    --only)        ONLY="$2"; shift 2;;
    --exclude)     EXCLUDE="$2"; shift 2;;
    --dry-run)     DRY_RUN=1; shift;;
    --allow-empty) ALLOW_EMPTY=1; shift;;
    -h|--help)     usage 0;;
    *) err "unknown arg: $1"; usage 2;;
  esac
done

# ── Target resolution ────────────────────────────────────────────────────────
# --digest is the preferred prod form and is REPO-RELATIVE ON PURPOSE.
# runtime_image_pins.image_digest stores a bare digest, and the registry a k8s
# tenant pulls from is NOT necessarily the one the local-docker fleet uses (the
# tenant pods on this platform pull from a different registry host than the
# docker roller's TENANT_IMAGE default). Baking a second registry constant into
# this script would be exactly the hardcoding that makes a fleet script wrong on
# the next environment. So with --digest each workload is re-pinned to
# <the repo it already pulls from>@<digest>: the bytes change, the registry the
# cluster is actually able to pull from does not.
if [ -n "$DIGEST" ]; then
  case "$DIGEST" in
    sha256:*) ;;
    *) err "--digest must be a full 'sha256:<hex>' reference, got '${DIGEST}'"; exit 2;;
  esac
  [ -z "$IMAGE" ] || { err "--digest and --image are mutually exclusive"; exit 2; }
elif [ -z "$IMAGE" ]; then
  [ -n "$TAG" ] || { err "one of --image <ref>, --digest sha256:<hex> or --tag <tag> is required"; exit 2; }
  IMAGE="${TENANT_IMAGE}:${TAG}"
fi

# ── Target identity: digest and/or expected git_sha ──────────────────────────
# TARGET_DIGEST is the strongest possible assertion — the exact bytes that must
# end up executing. Prefer it. EXPECTED_BUILD_SHA is the fallback identity for a
# tag ref. At least ONE must be resolvable, checked below, because verifying
# neither reduces the "did the roll land" question to "is something running",
# which is true both before and after a roll that did nothing.
TARGET_DIGEST="$DIGEST"
case "$IMAGE" in
  *@sha256:*) TARGET_DIGEST="sha256:${IMAGE##*@sha256:}";;
esac
EXPECTED_BUILD_SHA="${EXPECTED_BUILD_SHA:-}"
if [ -z "$EXPECTED_BUILD_SHA" ] && printf '%s' "${TAG:-}" | grep -Eq '^(staging|prod)-[0-9a-fA-F]{7,40}$'; then
  EXPECTED_BUILD_SHA="${TAG#*-}"
fi
if [ -z "$TARGET_DIGEST" ] && [ -z "$EXPECTED_BUILD_SHA" ]; then
  err "target image '${IMAGE}' carries no digest and no EXPECTED_BUILD_SHA could be derived."
  err "Verification would be liveness-only, which cannot tell a landed roll from a no-op."
  err "Pass a digest ref (…@sha256:…), or set EXPECTED_BUILD_SHA, or use --tag <env>-<sha>."
  exit 2
fi

TARGET_LABEL="${IMAGE:-<per-tenant repo>@${DIGEST}}"
log "target image      = ${TARGET_LABEL}"
[ -z "$TARGET_DIGEST" ]      || log "target digest     = ${TARGET_DIGEST}"
[ -z "$EXPECTED_BUILD_SHA" ] || log "expected git_sha  = ${EXPECTED_BUILD_SHA}"
log "dry_run=${DRY_RUN} allow_empty=${ALLOW_EMPTY}${ONLY:+ only='${ONLY}'}${EXCLUDE:+ exclude='${EXCLUDE}'}"

command -v "${KUBECTL%% *}" >/dev/null 2>&1 || { err "kubectl not found (KUBECTL='${KUBECTL}')"; exit 1; }
$KUBECTL version -o json >/dev/null 2>&1 || { err "kubectl cannot reach a cluster (check KUBECONFIG)"; exit 1; }

# ── Substrate census ─────────────────────────────────────────────────────────
# Emits TSV: <slug>\t<org_id>\t<substrate>\t<instance_id>
# There is no CP admin HTTP endpoint that exposes substrate today (the fleet
# roster GET /cp/admin/orgs joins org_instances but never org_substrates), so the
# default census reads the CP database. SUBSTRATE_CENSUS_CMD is the seam: when a
# substrate-listing endpoint lands, point this at it and nothing else changes.
census() {
  if [ -n "${SUBSTRATE_CENSUS_CMD:-}" ]; then
    log "census via SUBSTRATE_CENSUS_CMD"
    eval "$SUBSTRATE_CENSUS_CMD"
    return
  fi
  [ -n "${CP_DATABASE_URL:-}" ] || {
    err "no substrate census source: set CP_DATABASE_URL (control-plane DSN) or SUBSTRATE_CENSUS_CMD."
    err "Refusing to guess the fleet from namespace names — a roller that invents its own"
    err "target list cannot be trusted to have found all of them."
    return 1
  }
  local sql="SELECT o.slug, o.id, s.substrate, s.instance_id
             FROM org_substrates s JOIN organizations o ON o.id = s.org_id
             ORDER BY o.slug;"
  log "census via CP_DATABASE_URL (org_substrates)"
  if command -v psql >/dev/null 2>&1; then
    PGURL="$CP_DATABASE_URL" psql "$CP_DATABASE_URL" -At -F $'\t' -v ON_ERROR_STOP=1 -c "$sql"
  else
    docker run --rm -i -e PGURL="$CP_DATABASE_URL" "${PSQL_IMAGE:-postgres:16-alpine}" \
      sh -c "exec psql \"\$PGURL\" -At -F '$(printf '\t')' -v ON_ERROR_STOP=1 -c \"$sql\""
  fi
}

CENSUS_RAW="$(census)" || { err "substrate census FAILED — cannot enumerate the fleet"; exit 1; }

# Parse the census. A census that returns zero ROWS at all is itself suspicious:
# it means the control plane knows about no tenants on any substrate, which for a
# real environment is a broken query far more often than an empty fleet.
CENSUS_ROWS=0 ; K8S_SLUGS=() ; K8S_ORGIDS=() ; K8S_INSTIDS=() ; DOCKER_SLUGS=()
while IFS=$'\t' read -r c_slug c_orgid c_sub c_inst; do
  [ -n "${c_slug:-}" ] || continue
  CENSUS_ROWS=$((CENSUS_ROWS+1))
  case "$c_sub" in
    k8s)    K8S_SLUGS+=("$c_slug"); K8S_ORGIDS+=("$c_orgid"); K8S_INSTIDS+=("${c_inst:-}");;
    docker) DOCKER_SLUGS+=("$c_slug");;
    *)      err "census row for '${c_slug}' has unknown substrate '${c_sub}' — refusing to guess"; exit 1;;
  esac
done <<< "$CENSUS_RAW"

log "census: ${CENSUS_ROWS} org(s) — k8s=${#K8S_SLUGS[@]} docker=${#DOCKER_SLUGS[@]}"
if [ "${#DOCKER_SLUGS[@]}" -gt 0 ]; then
  log "docker-substrate orgs (NOT this script's job; roll them with redeploy-staging-fleet.sh): ${DOCKER_SLUGS[*]}"
fi

# ── Selection: --only / --exclude ────────────────────────────────────────────
in_csv() { case ",${2}," in *",${1},"*) return 0;; esac; return 1; }

SEL_SLUGS=() ; SEL_ORGIDS=() ; SEL_INSTIDS=() ; EXCLUDED=()
for i in "${!K8S_SLUGS[@]}"; do
  s="${K8S_SLUGS[$i]}"
  if [ -n "$ONLY" ] && ! printf '%s' "$s" | grep -qF -- "$ONLY"; then continue; fi
  if [ -n "$EXCLUDE" ] && in_csv "$s" "$EXCLUDE"; then EXCLUDED+=("$s"); continue; fi
  SEL_SLUGS+=("$s"); SEL_ORGIDS+=("${K8S_ORGIDS[$i]}"); SEL_INSTIDS+=("${K8S_INSTIDS[$i]}")
done
[ "${#EXCLUDED[@]}" -eq 0 ] || log "EXCLUDED by request (never touched): ${EXCLUDED[*]}"

FOUND="${#SEL_SLUGS[@]}"
log "found: ${FOUND} k8s tenant org(s) selected for roll"

# ── THE ANTI-VACUOUS GATE (part 1 of 2) ──────────────────────────────────────
# An empty target set is the exact defect this script replaces. It is only ever
# acceptable as an EXPLICIT operator assertion, never as a silent outcome.
if [ "$FOUND" -eq 0 ]; then
  if [ "$ALLOW_EMPTY" = "1" ]; then
    log "found: 0 k8s tenants — accepted because --allow-empty was passed explicitly"
    log "SUMMARY found=0 eligible=0 rolled=0 skipped=0 failed=0 (empty fleet asserted by operator)"
    exit 0
  fi
  err "found: 0 k8s tenant orgs to roll — refusing to report success."
  err "This is the vacuous-pass failure mode: a fleet roll that enumerates nothing and"
  err "exits 0 tells an operator the fleet is on the new image when nothing moved."
  err "If an empty k8s fleet is genuinely expected here, say so with --allow-empty."
  [ -z "$ONLY" ]    || err "  (a --only '${ONLY}' filter is active and may have excluded everything)"
  [ -z "$EXCLUDE" ] || err "  (an --exclude '${EXCLUDE}' filter is active and may have excluded everything)"
  exit 1
fi

# ── Per-tenant helpers ───────────────────────────────────────────────────────
# live_digests <ns> <selector>: the distinct digests the SERVING pods report, one
# per line. Reads .status.containerStatuses[].imageID — what the kubelet actually
# pulled and started — NOT .spec.template.spec.containers[].image, which is only
# what someone asked for. A `set image` that the cluster could not satisfy leaves
# .spec updated and the old bytes serving traffic; only imageID can tell them apart.
#
# Pods with a deletionTimestamp are EXCLUDED. Immediately after a successful
# rollout the superseded pod is still listed while it drains, and counting it made
# a perfectly good roll look like it had landed on two different digests at once.
# Excluding non-Running pods likewise keeps a Pending/Completed straggler from
# voting on what is currently serving.
live_digests() {
  local ns="$1" sel="$2"
  $KUBECTL -n "$ns" get pods -l "$sel" -o go-template='
{{- range .items -}}
  {{- if not .metadata.deletionTimestamp -}}
    {{- if eq .status.phase "Running" -}}
      {{- range .status.containerStatuses -}}
        {{- if eq .name "'"${TENANT_CONTAINER}"'" -}}{{ .imageID }}{{ "\n" }}{{- end -}}
      {{- end -}}
    {{- end -}}
  {{- end -}}
{{- end -}}' 2>/dev/null | sed -n 's/.*@\(sha256:[0-9a-f]\{64\}\).*/\1/p' | sort -u
}

serving_pods() {
  local ns="$1" sel="$2"
  $KUBECTL -n "$ns" get pods -l "$sel" -o go-template='
{{- range .items -}}
  {{- if not .metadata.deletionTimestamp -}}
    {{- if eq .status.phase "Running" -}}{{ .metadata.name }}{{ "\n" }}{{- end -}}
  {{- end -}}
{{- end -}}' 2>/dev/null | grep -c . || true
}

# converged_digests <ns> <sel>: live_digests, but allowing a bounded settle window
# for superseded pods to finish draining. Returns the comma-joined digest set. If
# it never converges to a single digest the caller fails — a pod stranded on the
# old bytes past the window is a real "roll did not land", not a timing artefact.
CONVERGE_ATTEMPTS="${CONVERGE_ATTEMPTS:-20}"
CONVERGE_SLEEP="${CONVERGE_SLEEP:-3}"
converged_digests() {
  local ns="$1" sel="$2" d=""
  for _ in $(seq 1 "$CONVERGE_ATTEMPTS"); do
    d="$(live_digests "$ns" "$sel" | paste -sd',' -)"
    case "$d" in
      "" ) ;;                       # no serving pod yet — keep waiting
      *,* ) ;;                      # still mixed — old pod draining
      * ) printf '%s' "$d"; return 0;;
    esac
    sleep "$CONVERGE_SLEEP"
  done
  printf '%s' "$d"; return 0
}

buildinfo_sha() {
  local ns="$1" pod="$2" body
  body="$($KUBECTL -n "$ns" exec "$pod" -c "$TENANT_CONTAINER" -- \
            wget -qO- http://localhost:8080/buildinfo 2>/dev/null || true)"
  printf '%s' "$body" | sed -n 's/.*"git_sha"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1
}

sha_matches() {
  local got="${1:-}" want="${2:-}"
  [ -n "$got" ] && [ -n "$want" ] || return 1
  case "$got" in "$want"*) return 0;; esac
  case "$want" in "$got"*) return 0;; esac
  return 1
}

ELIGIBLE=0 ; ROLLED=0 ; SKIPPED=0 ; FAILED=0
REPORT=()   # slug|namespace|before|after|outcome

record() { REPORT+=("$1|$2|$3|$4|$5"); }

TENANT_SELECTOR="molecule.component=tenant,molecule.managed-by=controlplane"

for i in "${!SEL_SLUGS[@]}"; do
  slug="${SEL_SLUGS[$i]}" ; orgid="${SEL_ORGIDS[$i]}"
  sel="${TENANT_SELECTOR},molecule.org-id=${orgid}"

  # Locate the workload by CP identity (org-id label), cluster-wide. No name
  # derivation: if the object is not labelled with this org's id, it is not this
  # org's tenant, whatever it is called.
  loc="$($KUBECTL get deploy -A -l "$sel" \
          -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.spec.replicas}{"\n"}{end}' 2>/dev/null || true)"
  nmatch="$(printf '%s' "$loc" | grep -c . || true)"

  if [ "$nmatch" -eq 0 ]; then
    # CP says this org is on k8s, but no tenant Deployment carries its id. This is
    # real drift and must be loud: the CP's org_instances.status column is known to
    # read 'running' for orgs in exactly this state.
    err "${slug}: control plane says substrate=k8s (instance_id='${SEL_INSTIDS[$i]}') but NO tenant Deployment carries molecule.org-id=${orgid}"
    err "${slug}: control-plane state and cluster reality disagree — not silently skipping"
    record "$slug" "-" "-" "-" "FAIL:no-workload"
    FAILED=$((FAILED+1)); continue
  fi
  if [ "$nmatch" -gt 1 ]; then
    err "${slug}: ${nmatch} Deployments carry molecule.org-id=${orgid} — ambiguous, refusing to roll"
    record "$slug" "-" "-" "-" "FAIL:ambiguous"
    FAILED=$((FAILED+1)); continue
  fi

  ns="$(printf '%s' "$loc" | cut -f1)"
  dep="$(printf '%s' "$loc" | cut -f2)"
  reps="$(printf '%s' "$loc" | cut -f3)"
  [ -n "$reps" ] || reps=0

  before="$(live_digests "$ns" "$sel" | paste -sd',' -)"
  [ -n "$before" ] || before="<no-running-pod>"

  # ── Vacuous-roll guard: a 0-replica Deployment ────────────────────────────
  # `kubectl set image` + `kubectl rollout status` both SUCCEED instantly against
  # a Deployment scaled to zero, with no pod anywhere running the new image. If
  # those counted as "rolled" the fleet report would be pure fiction. They are
  # counted separately and never as a roll. Scaling them up is NOT this script's
  # call — a tenant is at 0 replicas for a reason (an operator quiescing it, an
  # unrecoverable image) and starting it could be far worse than leaving it.
  if [ "$reps" = "0" ]; then
    log "${slug}: ${ns}/${dep} is scaled to 0 replicas — SKIPPED (a 0-replica rollout would report success with nothing running)"
    record "$slug" "$ns" "$before" "$before" "SKIP:scaled-to-zero"
    SKIPPED=$((SKIPPED+1)); continue
  fi

  ELIGIBLE=$((ELIGIBLE+1))

  # Per-tenant target ref. In --digest mode the repo is taken from what this
  # workload already pulls, so the roll cannot accidentally point a cluster at a
  # registry it has no credentials or route for.
  tgt="$IMAGE"
  if [ -z "$tgt" ]; then
    curref="$($KUBECTL -n "$ns" get deploy "$dep" -o go-template='
{{- range .spec.template.spec.containers -}}{{- if eq .name "'"${TENANT_CONTAINER}"'" -}}{{ .image }}{{- end -}}{{- end -}}' 2>/dev/null || true)"
    repo="${curref%%@*}"; repo="${repo%:*}"
    if [ -z "$repo" ]; then
      err "${slug}: cannot read the current image of container '${TENANT_CONTAINER}' in ${ns}/${dep} — cannot build a repo-relative digest ref"
      record "$slug" "$ns" "$before" "-" "FAIL:no-current-image"
      FAILED=$((FAILED+1)); continue
    fi
    tgt="${repo}@${DIGEST}"
  fi

  if [ "$DRY_RUN" = "1" ]; then
    log "DRY-RUN ${slug}: would roll ${ns}/${dep} (replicas=${reps}) ${before} -> ${tgt}"
    record "$slug" "$ns" "$before" "${TARGET_DIGEST:-<target>}" "DRYRUN:planned"
    continue
  fi

  log "== ${slug} (${ns}/${dep}, replicas=${reps}) =="
  log "  before (live): ${before}"
  log "  target       : ${tgt}"

  if ! $KUBECTL -n "$ns" set image "deployment/${dep}" "${TENANT_CONTAINER}=${tgt}" >/dev/null 2>&1; then
    err "${slug}: kubectl set image failed on ${ns}/${dep}"
    record "$slug" "$ns" "$before" "-" "FAIL:set-image"
    FAILED=$((FAILED+1)); continue
  fi
  if ! $KUBECTL -n "$ns" rollout status "deployment/${dep}" --timeout="$ROLLOUT_TIMEOUT" >/dev/null 2>&1; then
    err "${slug}: rollout did not complete within ${ROLLOUT_TIMEOUT} on ${ns}/${dep}"
    $KUBECTL -n "$ns" get pods -l "$sel" -o wide >&2 2>/dev/null || true
    record "$slug" "$ns" "$before" "-" "FAIL:rollout-timeout"
    FAILED=$((FAILED+1)); continue
  fi

  # ── VERIFY AGAINST THE PODS, NOT THE COMMAND ──────────────────────────────
  after="$(converged_digests "$ns" "$sel")"
  npods="$(serving_pods "$ns" "$sel")"

  if [ -z "$after" ] || [ "${npods:-0}" -eq 0 ]; then
    err "${slug}: rollout reported success but NO running pod reports an imageID — nothing is executing the new image"
    record "$slug" "$ns" "$before" "<none>" "FAIL:no-pod-after"
    FAILED=$((FAILED+1)); continue
  fi
  # More than one distinct digest live = the fleet is mid-flight or a pod is
  # stranded on the old bytes. Either way the roll has NOT uniformly landed.
  if printf '%s' "$after" | grep -q ','; then
    err "${slug}: serving pods still report MULTIPLE distinct digests $((CONVERGE_ATTEMPTS*CONVERGE_SLEEP))s after the roll (${after}) — a pod is stranded on the old bytes; roll did not uniformly land"
    record "$slug" "$ns" "$before" "$after" "FAIL:mixed-digests"
    FAILED=$((FAILED+1)); continue
  fi
  if [ -n "$TARGET_DIGEST" ] && [ "$after" != "$TARGET_DIGEST" ]; then
    err "${slug}: running pods report ${after} but the target was ${TARGET_DIGEST} — the roll did NOT land"
    record "$slug" "$ns" "$before" "$after" "FAIL:digest-mismatch"
    FAILED=$((FAILED+1)); continue
  fi
  if [ -n "$EXPECTED_BUILD_SHA" ]; then
    pod="$($KUBECTL -n "$ns" get pods -l "$sel" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    got="$(buildinfo_sha "$ns" "$pod")"
    if ! sha_matches "$got" "$EXPECTED_BUILD_SHA"; then
      err "${slug}: ${pod} /buildinfo git_sha='${got:-<empty>}' does not match expected ${EXPECTED_BUILD_SHA}"
      record "$slug" "$ns" "$before" "$after" "FAIL:buildinfo-mismatch"
      FAILED=$((FAILED+1)); continue
    fi
    log "  /buildinfo git_sha=${got} matches ${EXPECTED_BUILD_SHA}"
  fi

  log "  after  (live): ${after}  [${npods} pod(s)]"
  log "  OK ${slug} rolled and VERIFIED live"
  record "$slug" "$ns" "$before" "$after" "ROLLED"
  ROLLED=$((ROLLED+1))
done

# ── Before/after report ──────────────────────────────────────────────────────
# short(): abbreviate a digest for the table WITHOUT hiding that there were
# several. Truncating a comma-joined set to its first 19 characters would render
# "two different digests are live" as a single tidy digest — the report would
# conceal exactly the condition the verifier just failed on.
short() {
  local v="${1:-}" n
  case "$v" in
    *,*) n="$(printf '%s' "$v" | tr ',' '\n' | grep -c .)"
         printf '%s' "$(printf '%s' "${v%%,*}" | cut -c1-12)…+$((n-1))more";;
    sha256:*) printf '%s' "$v" | cut -c1-19;;
    *) printf '%s' "$v";;
  esac
}
{
  printf '\n%-24s %-28s %-21s %-21s %s\n' "TENANT" "NAMESPACE" "BEFORE(live)" "AFTER(live)" "OUTCOME"
  printf '%-24s %-28s %-21s %-21s %s\n' "------" "---------" "------------" "-----------" "-------"
  for r in "${REPORT[@]}"; do
    IFS='|' read -r rs rn rb ra ro <<< "$r"
    printf '%-24s %-28s %-21s %-21s %s\n' "$rs" "$rn" "$(short "$rb")" "$(short "$ra")" "$ro"
  done
  printf '\n'
} >&2

log "SUMMARY found=${FOUND} eligible=${ELIGIBLE} rolled=${ROLLED} skipped=${SKIPPED} failed=${FAILED} target=${TARGET_LABEL}"

if [ "$DRY_RUN" = "1" ]; then
  log "DRY-RUN complete — zero mutations performed"
  # A dry run still DISCOVERS. If discovery itself found drift (a CP org on the
  # k8s substrate with no workload carrying its id, or an ambiguous match), the
  # plan is not executable and saying so only in the table would let a --dry-run
  # gate go green over a fleet it could not actually roll.
  if [ "$FAILED" -gt 0 ]; then
    err "DRY-RUN found ${FAILED} tenant(s) that could not even be resolved to a rollable workload — the plan is not executable"
    exit 1
  fi
  if [ "$ELIGIBLE" -eq 0 ] && [ "$ALLOW_EMPTY" != "1" ]; then
    err "DRY-RUN: found=${FOUND} k8s tenant(s) but NONE are eligible to roll (skipped=${SKIPPED}) — a real run would deploy nothing"
    exit 1
  fi
  exit 0
fi

# ── THE ANTI-VACUOUS GATE (part 2 of 2) ──────────────────────────────────────
# Everything was in scope, nothing actually rolled. Distinct from found:0 and
# just as dishonest to report green: e.g. every tenant sat at 0 replicas, so the
# run "succeeded" while the running fleet is untouched.
if [ "$ELIGIBLE" -gt 0 ] && [ "$ROLLED" -eq 0 ]; then
  err "eligible=${ELIGIBLE} but rolled=0 — no workload was moved onto ${TARGET_LABEL}. Refusing to report success."
  exit 1
fi
if [ "$ELIGIBLE" -eq 0 ] && [ "$ALLOW_EMPTY" != "1" ]; then
  err "found=${FOUND} k8s tenant(s) but NONE were eligible to roll (skipped=${SKIPPED})."
  err "Every target was un-rollable, so this run deployed nothing. Pass --allow-empty only"
  err "if a fleet with no rollable tenant is genuinely the expected state here."
  exit 1
fi
if [ "$FAILED" -gt 0 ]; then
  err "k8s fleet roll had ${FAILED} failure(s) — see the report above"
  exit 1
fi

log "k8s fleet roll complete: ${ROLLED}/${FOUND} tenant(s) verified live on ${TARGET_DIGEST:-$TARGET_LABEL}"
