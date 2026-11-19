#!/usr/bin/env bash
# Report tenant inventory drift independently of release health.
# Missing workloads are reported by the shared roller as DRIFT:no-workload.
# Existing deployments cannot be hidden through an exclusion list. --strict
# makes inventory drift fail an operational audit; release callers omit it.
# Usage: fleet-drift-audit.sh --digest sha256:<hex> [--exclude a,b] [--strict]
# Read-only; uses KUBECTL / KUBECONFIG / SUBSTRATE_CENSUS_CMD like the roller.
set -euo pipefail

err() { printf '::error::%s\n' "$*" >&2; }
log() { printf '>> [drift] %s\n' "$*" >&2; }

DIGEST="" ; EXCLUDE="${EXCLUDE_SLUGS:-}" ; STRICT=0
while [ "$#" -gt 0 ]; do
  case "$1" in
    --digest)  DIGEST="$2"; shift 2;;
    --exclude) EXCLUDE="$2"; shift 2;;
    --strict) STRICT=1; shift;;
    -h|--help) sed -n '2,45p' "$0" | sed 's/^# \{0,1\}//'; exit 0;;
    *) err "unknown arg: $1"; exit 2;;
  esac
done
case "$DIGEST" in
  sha256:*) ;;
  *) err "--digest must be a full 'sha256:<hex>' reference, got '${DIGEST}'"; exit 2;;
esac

ROLLER="${K8S_ROLLER:-$(dirname "$0")/redeploy-tenant-fleet-k8s.sh}"
if [ ! -f "$ROLLER" ]; then
  err "the k8s roller '$ROLLER' is missing — refusing to report a drift audit this script cannot perform."
  exit 1
fi

PLAN="$(mktemp)"
trap 'rm -f "$PLAN"' EXIT

# NO EXCLUSIONS HERE, ON PURPOSE. This pass exists to MEASURE drift; passing the
# exclusion list would remove from the measurement exactly the rows the
# measurement is meant to justify.
#
# Capture failures so the underlying Kubernetes or target error is printed
# before this audit exits. Missing inventory alone does not fail the dry run.
#
# 2>&1 is load-bearing: the roller writes its ENTIRE human-readable output,
# including the report table this parses, to stderr.
rc=0
bash "$ROLLER" --dry-run --digest "$DIGEST" --allow-empty > "$PLAN" 2>&1 || rc=$?

if [ ! -s "$PLAN" ]; then
  err "the roller produced NO output at all in --dry-run (0 bytes captured). This audit cannot see what the plan was, which is a fault in THIS script's capture, not a statement about the fleet."
  exit 1
fi
cat "$PLAN" >&2

# The report table's last column is the outcome; a slug never contains
# whitespace, so field 1 is the slug. Only DRIFT rows identify missing inventory.
DRIFTED="$(awk '$NF == "DRIFT:no-workload" { print $1 }' "$PLAN" | sort -u)"
NDRIFT="$(printf '%s' "$DRIFTED" | grep -c . || true)"

# ── The parse must not be able to fail silently ───────────────────────────────
# A pattern that matches nothing looks identical to "there is no drift". If the
# dry-run refused AND this parse found no FAIL row, the parse is what broke —
# say so, rather than reporting a clean fleet and letting an exclusion list be
# validated against an empty drift set (which would accept ANY exclusion).
if [ "$rc" -ne 0 ] && [ "${NDRIFT:-0}" -eq 0 ]; then
  err "the roller's --dry-run exited ${rc} but this audit parsed ZERO 'DRIFT:' rows out of its report."
  err "That is a PARSE fault in this script (the roller's report format changed, or the run died before printing a table) — not evidence that the fleet is clean. Refusing to validate an exclusion list against a drift set this script could not read."
  exit 1
fi

if [ "${NDRIFT:-0}" -gt 0 ]; then
  log "control-plane/cluster DRIFT: ${NDRIFT} org(s) the control plane places on the k8s substrate cannot be resolved to a rollable tenant workload"
  while IFS= read -r s; do
    [ -n "$s" ] || continue
    printf '::warning::fleet drift: org %s is substrate=k8s in the control plane but has no rollable tenant Deployment carrying its org-id. Reconcile this inventory separately; it is not a release target.\n' "$s" >&2
  done <<< "$DRIFTED"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    {
      echo "## Fleet drift audit"
      echo ""
      echo "${NDRIFT} org(s) on the k8s substrate have no rollable tenant workload:"
      echo ""
      while IFS= read -r s; do
        [ -n "$s" ] || continue
        echo "- \`${s}\`"
      done <<< "$DRIFTED"
      echo ""
      echo "Excluded from this roll: \`${EXCLUDE:-<none>}\`"
    } >> "$GITHUB_STEP_SUMMARY"
  fi
else
  log "no drift: every k8s-substrate org resolves to a rollable tenant workload"
fi

# ── An exclusion must be EARNED ──────────────────────────────────────────────
UNJUSTIFIED=""
IFS=',' read -r -a _ex <<< "${EXCLUDE:-}"
for s in "${_ex[@]:-}"; do
  s="$(printf '%s' "$s" | tr -d '[:space:]')"
  [ -n "$s" ] || continue
  if ! printf '%s\n' "$DRIFTED" | grep -qxF "$s"; then
    UNJUSTIFIED="${UNJUSTIFIED}${UNJUSTIFIED:+ }${s}"
  fi
done
if [ -n "$UNJUSTIFIED" ]; then
  err "the exclusion list names org(s) that are NOT drifted: ${UNJUSTIFIED}"
  err "An exclusion is only ever a way to get past a tenant the cluster genuinely cannot roll. Excluding a tenant that HAS a live workload removes it from the deploy silently and forever — the roller drops excluded slugs before selection, so it never appears in the report at all."
  err "Either remove it from the exclusion list, or fix the drift it was claiming to work around."
  exit 1
fi
[ -z "$EXCLUDE" ] || log "exclusion list '${EXCLUDE}' — every entry is measurably drifted right now"

# A strict inventory audit remains red until the inventory is reconciled.
# Deployments do not inherit that verdict: they verify their actual targets.
log "drift audit complete: drifted=${NDRIFT} excluded='${EXCLUDE:-<none>}'"
if [ "$rc" -ne 0 ]; then
  err "inventory discovery could not produce an executable plan (exit ${rc})"
  exit "$rc"
fi
if [ "$STRICT" = "1" ] && [ "${NDRIFT:-0}" -gt 0 ]; then
  err "inventory drift: ${NDRIFT} tenant record(s) have no workload"
  exit 1
fi
