#!/usr/bin/env bash
# resolve-k8s-fleet-census.sh — materialise the cluster credential and a PROVEN
# substrate census for the k8s arm of a tenant fleet roll.
#
# WHY THIS IS A SCRIPT AND NOT INLINE YAML
# ----------------------------------------
# The production lane (.gitea/workflows/redeploy-tenants-on-main.yml, job
# `redeploy-k8s`) does this inline because it has exactly ONE call site. The
# staging lane has TWO — the forward roll in `redeploy-fleet` and the fleet
# revert inside `rollback-pin` — and the second one CANNOT be a set of preceding
# job steps: if credential resolution ran as a step ahead of the revert, an
# absent/broken credential would fail the job BEFORE the pin was reverted, and
# leave staging pinned to a candidate that had already failed. The revert must
# put the pin back FIRST and only then resolve a cluster credential for the
# fleet re-roll.
#
# So the logic lives here, called from both places, rather than being hand-copied
# into a second YAML block that can drift. That is the same reason
# redeploy-staging-fleet.sh exists as one shared swap script instead of two.
#
# WHAT IT PROVES BEFORE IT RETURNS
#   * the toolchain was resolved by the caller into absolute, executable paths
#     (KUBECTL, DEPLOY_PYTHON3) — never an ambient PATH lookup, which on the
#     local-deploy runner has already produced `python3: command not found`
#     mid-step (prod run 644986);
#   * K8S_FLEET_KUBECONFIG exists in the Infisical SSOT, is valid base64, decodes
#     to something kubectl can read a current-context out of;
#   * kubectl can actually REACH the cluster with that credential;
#   * the census command runs, returns rows, and at least one of them is on the
#     k8s substrate.
#
# The last point is the important one. The roller
# (redeploy-tenant-fleet-k8s.sh) has its own zero-row refusal, but a census
# broken HERE surfaces there as "the fleet is empty", which reads as a fact about
# staging rather than a fact about this wiring. This script fails first, and says
# which of the four coordinates (namespace / pod / user / database) it was using.
#
# It DOES NOT resolve the target digest. On this lane the digest is an OUTPUT of
# advance-pin (`new_digest` / `old_digest`) — the exact digest this run promoted
# through the CP admin API — so re-reading runtime_image_pins over psql would add
# a round trip AND a race with any concurrent promote, for a value we already
# hold. The production lane queries the table only because its k8s arm has no
# advance-pin upstream to inherit from.
#
# Usage:
#   resolve-k8s-fleet-census.sh --env-out <file> [--work-dir <dir>]
#
# On success writes KEY=value lines to <file> (and nothing to stdout):
#   KUBECONFIG            path to the materialised kubeconfig (mode 0600)
#   SUBSTRATE_CENSUS_CMD  path to an executable census command
#   FLEET_CENSUS_TSV      path to the census output already captured
# The caller sources it (`set -a; . <file>; set +a`) or appends it to
# $GITHUB_ENV *after* validating the capture.
#
# Env (no-hardcoding; every one is overridable so a cutover needs no code change):
#   KUBECTL             absolute path to kubectl        (REQUIRED, from the toolchain preflight)
#   DEPLOY_PYTHON3      absolute path to python3        (REQUIRED, from the toolchain preflight)
#   CP_CENSUS_NAMESPACE namespace holding the CP database pod
#   CP_CENSUS_DB_POD    the CP database pod
#   CP_CENSUS_DB_USER   psql role
#   CP_CENSUS_DB_NAME   database name
#   INFISICAL_SECRET_PATH   Infisical folder holding K8S_FLEET_KUBECONFIG (default /shared/controlplane)
#   INFISICAL_SECRET_ENV    Infisical environment slug (default: staging)
#   INFISICAL_CI_CLIENT_ID / INFISICAL_CI_CLIENT_SECRET / INFISICAL_PROJECT_ID
#
# SAFETY: read-only against the cluster and the control-plane database. It issues
# `kubectl config current-context`, `kubectl version`, and one `kubectl exec …
# psql -c SELECT`. It mutates nothing.
set -euo pipefail

err() { printf '::error::%s\n' "$*" >&2; }
log() { printf '>> [census] %s\n' "$*" >&2; }

ENV_OUT="" ; WORK_DIR=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --env-out)  ENV_OUT="$2"; shift 2;;
    --work-dir) WORK_DIR="$2"; shift 2;;
    -h|--help)  sed -n '2,60p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) err "unknown arg: $1"; exit 2;;
  esac
done
[ -n "$ENV_OUT" ] || { err "--env-out <file> is required"; exit 2; }
WORK_DIR="${WORK_DIR:-${RUNNER_TEMP:-/tmp}}"
mkdir -p "$WORK_DIR"

# ── The toolchain is a DECLARED dependency, so it is checked like one ─────────
# `command -v kubectl` is deliberately not what is checked here: on this platform
# that exact test has passed vacuously before, because the binary ships in images
# that hold no credentials. What the caller must hand over is an absolute,
# executable path that its own preflight resolved.
for v in KUBECTL DEPLOY_PYTHON3; do
  eval "p=\${$v:-}"
  if [ -z "$p" ] || [ ! -x "$p" ]; then
    err "$v is unset or not executable ('${p:-<unset>}') — the deploy toolchain preflight did not run or did not export it. Refusing to fall back to an ambient lookup: that is what produced 'python3: command not found' mid-step on this runner."
    exit 1
  fi
done

CP_CENSUS_NAMESPACE="${CP_CENSUS_NAMESPACE:-}"
CP_CENSUS_DB_POD="${CP_CENSUS_DB_POD:-}"
CP_CENSUS_DB_USER="${CP_CENSUS_DB_USER:-}"
CP_CENSUS_DB_NAME="${CP_CENSUS_DB_NAME:-}"
for v in CP_CENSUS_NAMESPACE CP_CENSUS_DB_POD CP_CENSUS_DB_USER CP_CENSUS_DB_NAME; do
  eval "p=\${$v:-}"
  if [ -z "$p" ]; then
    err "$v is empty — the census coordinates must be supplied by the caller. A defaulted-to-nothing coordinate would produce a kubectl exec against '' and surface as 'the fleet is empty'."
    exit 1
  fi
done

INFISICAL_SECRET_PATH="${INFISICAL_SECRET_PATH:-/shared/controlplane}"
INFISICAL_SECRET_ENV="${INFISICAL_SECRET_ENV:-staging}"
READER="${INFISICAL_READER:-.gitea/scripts/infisical-read-secret.py}"
if [ ! -f "$READER" ]; then
  err "the Infisical SSOT reader '$READER' is missing — refusing to invent a second way to read a cluster credential."
  exit 1
fi

# ── Read the credential ──────────────────────────────────────────────────────
# NOT a bare `V="$(reader)"`. Callers run steps as `bash --noprofile --norc -e -o
# pipefail`, and a simple assignment takes the exit status of its command
# substitution — so a bare capture KILLS the step the instant the reader fails,
# ABOVE the diagnostics written below it, which then never print. Capturing the
# status into `rc` puts the assignment in an OR-list, which `set -e` does not act
# on, so the value is JUDGED before it is used. `|| true` would be the wrong
# repair: it converts "the read failed" into "the read returned empty" and
# mis-names whichever fault actually happened.
#
# `optional` mode is passed so a MISSING key returns 10 instead of being
# flattened into the same exit 1 as an auth or transport error. Both still fail
# closed; this only decides WHICH sentence gets printed, and "the operator never
# seeded the secret" and "the read broke" want very different next actions.
rc=0
KUBECONFIG_B64="$("$DEPLOY_PYTHON3" "$READER" \
  K8S_FLEET_KUBECONFIG "$INFISICAL_SECRET_PATH" "$INFISICAL_SECRET_ENV" optional)" || rc=$?
case "$rc" in
  0) ;;
  10)
    err "K8S_FLEET_KUBECONFIG DOES NOT EXIST at ${INFISICAL_SECRET_PATH} (environment '${INFISICAL_SECRET_ENV}') in the Infisical SSOT. This lane's cluster credential was never seeded, so it can neither census nor roll the k8s tenant fleet."
    err "OPERATOR STEP: seed K8S_FLEET_KUBECONFIG as the BASE64 of a kubeconfig body. Base64 is not decoration — the SSOT reader rejects any value containing a line break, so a raw multi-line kubeconfig cannot be stored under this key at all."
    err "The credential must be able to (a) exec into ${CP_CENSUS_NAMESPACE}/${CP_CENSUS_DB_POD} and (b) get/patch tenant Deployments cluster-wide. The local-deploy pod's own ServiceAccount (ci:cp-deployer) has NEITHER."
    exit 1 ;;
  *)
    err "reading K8S_FLEET_KUBECONFIG from the Infisical SSOT FAILED (reader exit ${rc}); the reader's own ::error:: above says why (auth, transport, or a value it refuses such as one containing a line break)."
    err "Refusing to continue without a cluster credential rather than rolling zero tenants and calling it a deploy."
    exit 1 ;;
esac
if [ -z "$KUBECONFIG_B64" ]; then
  err "Infisical returned EMPTY for K8S_FLEET_KUBECONFIG (${INFISICAL_SECRET_PATH}, ${INFISICAL_SECRET_ENV})."
  err "A declared-but-empty secret produces exactly the vacuous pass this wiring exists to stop, so this fails rather than rolling zero tenants."
  exit 1
fi

# DECODE, THEN PROVE IT IS A KUBECONFIG BEFORE ANYTHING TRUSTS IT. Writing the
# raw value straight out would hand kubectl a base64 blob and turn a seeding
# mistake into an unreadable parse error three steps later. `wc -c > 0` is not
# evidence of a kubeconfig either — assert kubectl can PARSE it and name a
# current context.
KCFG="$WORK_DIR/fleet.kubeconfig"
umask 077
if ! printf '%s' "$KUBECONFIG_B64" | base64 -d > "$KCFG" 2>/dev/null; then
  err "K8S_FLEET_KUBECONFIG is not valid base64. Seed it as the base64 encoding of the kubeconfig body (the SSOT reader refuses a raw multi-line value)."
  exit 1
fi
ctx=""
if ! ctx="$("$KUBECTL" --kubeconfig "$KCFG" config current-context 2>/dev/null)" || [ -z "$ctx" ]; then
  err "the decoded K8S_FLEET_KUBECONFIG is not a usable kubeconfig — kubectl cannot read a current-context out of it ($(wc -c < "$KCFG") bytes decoded)."
  exit 1
fi
export KUBECONFIG="$KCFG"
log "kubeconfig materialized ($(wc -c < "$KCFG") bytes, context '${ctx}')"

if ! "$KUBECTL" version -o json >/dev/null 2>&1; then
  err "kubectl ('${KUBECTL}') cannot reach the cluster with the resolved credential. The binary existing is not the capability; this is the check that the binary-presence check never made."
  exit 1
fi

# ── The census command ───────────────────────────────────────────────────────
# Written as a FILE because SUBSTRATE_CENSUS_CMD is eval'd by the roller, and a
# multi-line SQL string through eval is how quoting bugs become "the fleet is
# empty". The coordinates are baked in at generation time so the roller cannot
# inherit a different namespace/pod than the one this script just proved.
CENSUS="$WORK_DIR/census.sh"
CENSUS_SQL="SELECT o.slug, o.id, s.substrate, s.instance_id FROM org_substrates s JOIN organizations o ON o.id = s.org_id ORDER BY o.slug;"
# Every interpolated value goes through `printf %q`, which emits a form the shell
# re-reads as EXACTLY the original string. A slug-shaped namespace needs no
# quoting today; relying on that is how an operator-supplied override with a
# space in it becomes a silently truncated psql invocation.
_TAB="$(printf '\t')"
{
  printf '#!/usr/bin/env bash\n'
  printf 'set -euo pipefail\n'
  # `:?` rather than a `:-kubectl` default ON PURPOSE — a default would silently
  # put the census back on ambient PATH lookup, which on this runner means
  # `kubectl: not found` INSIDE an eval'd command, surfacing as "the fleet is
  # empty" instead of "the census could not run".
  printf ': "${KUBECTL:?KUBECTL must be exported by the deploy toolchain preflight}"\n'
  printf ': "${KUBECONFIG:?KUBECONFIG must be exported by resolve-k8s-fleet-census.sh}"\n'
  printf 'exec "$KUBECTL" exec -i -n %q %q -- \\\n' "$CP_CENSUS_NAMESPACE" "$CP_CENSUS_DB_POD"
  printf '  psql -U %q -d %q -At -F %q -v ON_ERROR_STOP=1 \\\n' \
    "$CP_CENSUS_DB_USER" "$CP_CENSUS_DB_NAME" "$_TAB"
  printf '  -c %q\n' "$CENSUS_SQL"
} > "$CENSUS"
chmod +x "$CENSUS"

# ── Prove the census returns rows BEFORE handing it over ─────────────────────
# RUN IT ONCE AND COUNT THE FILE. Invoking it once per count would make several
# exec-into-Postgres round trips against a live control plane to answer two
# questions, and would let the numbers reported and the fleet rolled come from
# different reads.
TSV="$WORK_DIR/census.tsv"
crc=0
"$CENSUS" > "$TSV" 2>"$WORK_DIR/census.err" || crc=$?
if [ "$crc" -ne 0 ]; then
  err "the substrate census FAILED (exit ${crc}) using namespace='${CP_CENSUS_NAMESPACE}' pod='${CP_CENSUS_DB_POD}' user='${CP_CENSUS_DB_USER}' db='${CP_CENSUS_DB_NAME}'."
  err "That is a fault in THIS wiring or in this credential's RBAC, NOT a statement about how many tenants staging has."
  err "Common causes, in the order they have actually bitten this platform: the credential has no pods/exec in that namespace; the psql role is wrong (the staging role is NOT the same as production's); the pod name changed."
  sed -n '1,20p' "$WORK_DIR/census.err" >&2 || true
  exit 1
fi
rows="$(grep -c . "$TSV" || true)"
k8s_rows="$(awk -F'\t' '$3=="k8s"' "$TSV" | grep -c . || true)"
log "census: rows=${rows} k8s=${k8s_rows}"
if [ "${rows:-0}" -eq 0 ]; then
  err "the substrate census returned ZERO rows. A control plane that knows about no tenants on any substrate is a broken query or the wrong database, not an empty fleet."
  exit 1
fi
if [ "${k8s_rows:-0}" -eq 0 ]; then
  err "the census found ${rows} org(s) and NONE on the k8s substrate, so the k8s arm would roll nothing — the exact vacuous pass this wiring exists to remove."
  exit 1
fi

{
  printf 'KUBECONFIG=%s\n' "$KCFG"
  printf 'SUBSTRATE_CENSUS_CMD=%s\n' "$CENSUS"
  printf 'FLEET_CENSUS_TSV=%s\n' "$TSV"
} > "$ENV_OUT"
log "census source ready: ${CENSUS} (${rows} org(s), ${k8s_rows} on k8s)"
