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
# (org_substrates.substrate) and where each k8s org lives
# (org_substrates.instance_id, which for a k8s org IS its namespace), then finds
# the org's workload INSIDE that namespace by the labels the provisioner stamps:
#     molecule.component=tenant, molecule.managed-by=controlplane,
#     molecule.org-id=<uuid>, molecule.org-slug=<slug>
# Matching on molecule.org-id (the CP's own primary key) means the CP row and the
# cluster object are joined on identity, not on a name: a Deployment that does
# not carry this org's id is not this org's tenant, whatever namespace it is in.
# A tenant that is not in the namespace the control plane records is reported
# (FAIL:no-workload, naming the namespace searched) — never hunted for
# cluster-wide. See LEAST PRIVILEGE below.
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
#   TENANT_NAMESPACE_PREFIX  prefix for a census row whose instance_id is EMPTY
#                         (default: enteros — the SDK's branding.ResourcePrefix,
#                         from which the provisioner names every namespace)
#   EXCLUDE_SLUGS         same as --exclude, for CI env wiring
#
# SAFETY: --dry-run performs zero mutations. --exclude / EXCLUDE_SLUGS hard-skip
# named slugs before any mutation. This script only ever issues `kubectl set
# image` on a tenant Deployment; it never scales, drains, evicts, deletes or
# restarts anything, and never touches a PVC, namespace or StatefulSet.
#
# LEAST PRIVILEGE: every cluster call is scoped to ONE tenant namespace, so the
# credential needs, in each tenant namespace and nowhere else:
#     deployments.apps  get, list, watch, patch  (label lookup, current image,
#                                                 set image, rollout status)
#     pods              list                     (serving pods and their imageIDs)
#     pods/proxy        get                      (/buildinfo of each serving pod;
#                                                 only when EXPECTED_BUILD_SHA applies)
# No cluster-scoped grant and no pods/exec. `kubectl version` reads /version,
# which any authenticated identity may. The census is not part of this: the
# default one is psql, and a SUBSTRATE_CENSUS_CMD brings whatever it needs.
set -euo pipefail

log() { printf '>> [k8s-fleet] %s\n' "$*" >&2; }
err() { printf '::error::%s\n' "$*" >&2; }

TENANT_IMAGE="${TENANT_IMAGE:-registry.moleculesai.app/molecule-ai/molecule-tenant}"
KUBECTL="${KUBECTL:-kubectl}"
ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-300s}"
TENANT_CONTAINER="${TENANT_CONTAINER:-tenant}"
TENANT_NAMESPACE_PREFIX="${TENANT_NAMESPACE_PREFIX:-enteros}"
IMAGE="" ; TAG="" ; DIGEST="" ; DRY_RUN=0 ; ONLY="" ; ALLOW_EMPTY=0
EXCLUDE="${EXCLUDE_SLUGS:-}"

# The whole header comment, however long it grows (a fixed line range silently
# cut off Usage/Env as soon as a section was added above them).
usage() { sed -n '2,/^set -euo pipefail$/{/^#/p;}' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
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
# live_pods <ns> <selector>: pod name and imageID for each ready tenant container
# in a nonterminating Running pod. Reads .status.containerStatuses[].imageID — what the kubelet actually
# pulled and started — NOT .spec.template.spec.containers[].image, which is only
# what someone asked for. A `set image` that the cluster could not satisfy leaves
# .spec updated and the old bytes serving traffic; only imageID can tell them apart.
#
# Pods with a deletionTimestamp are EXCLUDED. Immediately after a successful
# rollout the superseded pod is still listed while it drains, and counting it made
# a perfectly good roll look like it had landed on two different digests at once.
# Excluding non-Running pods likewise keeps a Pending/Completed straggler from
# voting on what is currently serving.
live_pods() {
  local ns="$1" sel="$2"
  $KUBECTL -n "$ns" get pods -l "$sel" -o go-template='
{{- range .items -}}
  {{- $pod := .metadata.name -}}
  {{- if not .metadata.deletionTimestamp -}}
    {{- if eq .status.phase "Running" -}}
      {{- range .status.containerStatuses -}}
        {{- if and (eq .name "'"${TENANT_CONTAINER}"'") .ready -}}
          {{ $pod }}{{ "\t" }}{{ .imageID }}{{ "\n" }}
        {{- end -}}
      {{- end -}}
    {{- end -}}
  {{- end -}}
{{- end -}}' 2>/dev/null
}

# Keep pod identity and image identity together. Missing imageIDs remain visible
# so one healthy replica cannot hide another replica with unverifiable bytes.
pod_digests() {
  local pod image
  while IFS=$'\t' read -r pod image; do
    [ -n "$pod" ] || continue
    if [[ "$image" =~ (sha256:[0-9a-f]{64})$ ]]; then
      printf '%s\n' "${BASH_REMATCH[1]}"
    else
      printf '%s\n' '<missing-imageID>'
    fi
  done | sort -u | paste -sd',' -
}

# converged_pods <ns> <sel>: one serving-pod snapshot for digest AND build checks,
# allowing a bounded settle window for superseded pods to finish draining. If
# it never converges to a single digest the caller fails — a pod stranded on the
# old bytes past the window is a real "roll did not land", not a timing artefact.
CONVERGE_ATTEMPTS="${CONVERGE_ATTEMPTS:-20}"
CONVERGE_SLEEP="${CONVERGE_SLEEP:-3}"
converged_pods() {
  local ns="$1" sel="$2" d="" pods=""
  for _ in $(seq 1 "$CONVERGE_ATTEMPTS"); do
    pods="$(live_pods "$ns" "$sel")" || return 1
    d="$(pod_digests <<< "$pods")"
    case "$d" in
      "" ) ;;                       # no serving pod yet — keep waiting
      *,* ) ;;                      # still mixed — old pod draining
      * ) printf '%s' "$pods"; return 0;;
    esac
    sleep "$CONVERGE_SLEEP"
  done
  printf '%s' "$pods"; return 0
}

# buildinfo_sha <ns> <pod>: the git_sha the pod's own /buildinfo reports, read
# through the API server's pod proxy (GET on pods/proxy) — NOT `kubectl exec`.
# Reading a public build identifier must not need the power to run commands in
# every tenant pod. 8080 is the tenant container's `api` port, the Go server
# that serves /buildinfo and answers the pod's probes; the body is the same
# {"git_sha":"<sha>"} the in-pod wget used to read, parsed the same way.
#
# Any failure yields an EMPTY sha, which the caller fails as
# FAIL:buildinfo-mismatch; it can never pass. That includes the NETWORK half:
# the API server dials the pod itself, and a tenant namespace is default-deny,
# admitting ingress only from kube-system and same-namespace pods. Measured
# 2026-10-11 on the k3s fleet: tenants on the API server's own node answered,
# tenants on the worker nodes got a 502 from the proxy.
#
# </dev/null because the caller runs this inside `while read … <<< "$pods"`: a
# KUBECTL wrapper that reads stdin (ssh does) would swallow the remaining pod
# lines, and those replicas would never be probed at all.
buildinfo_sha() {
  local ns="$1" pod="$2" body
  body="$($KUBECTL get --raw "/api/v1/namespaces/${ns}/pods/${pod}:8080/proxy/buildinfo" </dev/null 2>/dev/null || true)"
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

# k8s_sanitize_org <slug>: the namespace segment the provisioner derives from a
# slug — molecule-controlplane's k8sSanitizeOrg (kubernetes_names.go), mirrored:
# lowercased, every run of characters outside [a-z0-9] collapsed to one '-',
# leading/trailing '-' trimmed, capped at 40 and re-trimmed, "org" if empty.
k8s_sanitize_org() {
  local s
  s="$(printf '%s' "$1" | LC_ALL=C tr '[:upper:]' '[:lower:]' \
        | LC_ALL=C sed -e 's/[^a-z0-9][^a-z0-9]*/-/g' -e 's/^-//' -e 's/-$//')"
  s="$(printf '%s' "${s:0:40}" | sed -e 's/-$//')"
  printf '%s' "${s:-org}"
}

# The namespaced lookup's stderr, so an API error can be told apart from "no
# such workload" (see below).
LOOKUP_ERR="$(mktemp)"
# `|| true`: under set -e a failing EXIT-trap command becomes the script's exit
# status, and cleanup must never turn a verified roll into a red one.
trap 'rm -f "$LOOKUP_ERR" || true' EXIT

for i in "${!SEL_SLUGS[@]}"; do
  slug="${SEL_SLUGS[$i]}" ; orgid="${SEL_ORGIDS[$i]}"
  sel="${TENANT_SELECTOR},molecule.org-id=${orgid}"

  # WHICH NAMESPACE: the control plane's record, never a search. For a k8s org
  # the census instance_id IS the namespace — the provisioner returns the
  # Namespace name as the InstanceID and the control plane stamps it into
  # org_substrates.instance_id (molecule-controlplane migration 068;
  # Kubernetes.resolveNamespace decodes it the same way). So a tenant recorded
  # under an older brand prefix (mol-<slug>) is looked for where it really is.
  # Only an EMPTY instance_id — a substrate flip blanks it until the next
  # provision stamps it — falls back to the control plane's own derivation,
  # K8sNamespaceName: <TENANT_NAMESPACE_PREFIX>-<sanitised slug>.
  inst="$(printf '%s' "${SEL_INSTIDS[$i]}" | tr -d '[:space:]')"
  if [ -n "$inst" ]; then
    ns="$inst"; ns_from="the namespace the control plane recorded as its census instance_id"
  else
    ns="${TENANT_NAMESPACE_PREFIX}-$(k8s_sanitize_org "$slug")"
    ns_from="derived as ${TENANT_NAMESPACE_PREFIX}-<slug> because the census instance_id is EMPTY"
  fi

  # Locate the workload by CP identity (org-id label) INSIDE that namespace.
  # Deliberately no cluster-wide fallback: the credential holds no cluster-wide
  # list, and "look everywhere" would quietly roll a tenant that is not where the
  # control plane believes it is.
  lrc=0
  loc="$($KUBECTL -n "$ns" get deploy -l "$sel" \
          -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.replicas}{"\n"}{end}' 2>"$LOOKUP_ERR")" || lrc=$?
  if [ "$lrc" -ne 0 ]; then
    # An API error is an ACCESS fault — most often no role in this namespace —
    # not evidence that the tenant is missing. Calling it no-workload would send
    # the operator after control-plane drift that does not exist.
    err "${slug}: could not list Deployments in namespace '${ns}', ${ns_from}: $(tr '\n' ' ' < "$LOOKUP_ERR" | cut -c1-400)"
    err "${slug}: that is a fault in this credential's access to '${ns}', not a statement about the tenant"
    record "$slug" "$ns" "-" "-" "FAIL:lookup-error"
    FAILED=$((FAILED+1)); continue
  fi
  nmatch="$(printf '%s' "$loc" | grep -c . || true)"

  if [ "$nmatch" -eq 0 ]; then
    # CP says this org is on k8s, but no tenant Deployment in its namespace
    # carries its id. This is real drift and must be loud: the CP's
    # org_instances.status column is known to read 'running' for orgs in
    # exactly this state.
    err "${slug}: control plane says substrate=k8s (instance_id='${SEL_INSTIDS[$i]}') but NO tenant Deployment in namespace '${ns}' carries molecule.org-id=${orgid}"
    err "${slug}: '${ns}' is ${ns_from}; no other namespace is searched"
    err "${slug}: control-plane state and cluster reality disagree — not silently skipping"
    record "$slug" "$ns" "-" "-" "FAIL:no-workload"
    FAILED=$((FAILED+1)); continue
  fi
  if [ "$nmatch" -gt 1 ]; then
    err "${slug}: ${nmatch} Deployments in namespace '${ns}' carry molecule.org-id=${orgid} — ambiguous, refusing to roll"
    record "$slug" "$ns" "-" "-" "FAIL:ambiguous"
    FAILED=$((FAILED+1)); continue
  fi

  dep="$(printf '%s' "$loc" | cut -f1)"
  reps="$(printf '%s' "$loc" | cut -f2)"
  [ -n "$reps" ] || reps=0

  before="$(live_pods "$ns" "$sel" | pod_digests)"
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
  if ! pods="$(converged_pods "$ns" "$sel")"; then
    err "${slug}: cannot read serving pods after rollout"
    record "$slug" "$ns" "$before" "-" "FAIL:pod-read"
    FAILED=$((FAILED+1)); continue
  fi
  after="$(pod_digests <<< "$pods")"
  npods="$(printf '%s\n' "$pods" | grep -c . || true)"

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
  if [ "$after" = '<missing-imageID>' ]; then
    err "${slug}: serving pods have no verifiable imageID"
    record "$slug" "$ns" "$before" "$after" "FAIL:missing-imageID"
    FAILED=$((FAILED+1)); continue
  fi
  if [ -n "$EXPECTED_BUILD_SHA" ]; then
    build_failed=0
    while IFS=$'\t' read -r pod _; do
      got="$(buildinfo_sha "$ns" "$pod")"
      if ! sha_matches "$got" "$EXPECTED_BUILD_SHA"; then
        err "${slug}: ${pod} /buildinfo git_sha='${got:-<empty>}' does not match expected ${EXPECTED_BUILD_SHA}"
        [ -n "$got" ] || err "${slug}: nothing came back through the pod proxy — this credential lacks get on pods/proxy in '${ns}', or the API server cannot reach ${pod}:8080 (see buildinfo_sha)"
        build_failed=1
      else
        log "  ${pod} /buildinfo git_sha=${got} matches ${EXPECTED_BUILD_SHA}"
      fi
    done <<< "$pods"
    if [ "$build_failed" -ne 0 ]; then
      record "$slug" "$ns" "$before" "$after" "FAIL:buildinfo-mismatch"
      FAILED=$((FAILED+1)); continue
    fi
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
