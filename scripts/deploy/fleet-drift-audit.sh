#!/usr/bin/env bash
# fleet-drift-audit.sh — report control-plane/cluster DRIFT before a k8s fleet
# roll, and refuse an exclusion list that would hide a live tenant.
#
# THE PROBLEM THIS SOLVES
# -----------------------
# redeploy-tenant-fleet-k8s.sh classifies an org the control plane says is on the
# k8s substrate, but whose id no tenant Deployment carries, as
# `FAIL:no-workload`, counts it in FAILED, and exits non-zero. That is CORRECT:
# org_instances.status is known to read 'running' for orgs in exactly that state,
# and a roller keyed on a status column inherits the column's lies.
#
# It is also, on a control plane carrying drifted orgs, a permanently red deploy
# lane: the roll exits 1 even when every REAL tenant rolled and verified, the
# candidate pin is reverted, and the fleet never advances. Measured on staging
# 2026-08-12: found=11 eligible=7 failed=4, the 4 being drifted proof orgs with
# an empty instance_id and no workload — while the 7 real tenants were rollable.
#
# The roller already accepts `--exclude` / EXCLUDE_SLUGS for this. But an
# exclusion list is a loaded weapon: the same flag that lets a lane past known
# drift will just as happily hide a HEALTHY tenant from the roll, and nothing in
# the roller can tell those two uses apart — an excluded slug is dropped before
# selection, so it never appears in the report at all. "Set EXCLUDE_SLUGS until
# it goes green" is a fleet that silently stops being deployed to.
#
# So this script makes the exclusion EARNED rather than asserted:
#
#   1. It runs the shipped roller in --dry-run with NO exclusions, so the
#      classification comes from the roller itself and cannot drift from what the
#      real roll will do.
#   2. It REPORTS every drifted org by name — as ::warning:: and in the job
#      summary — on every run, excluded or not. Exclusion never makes drift
#      quiet; it only makes it non-blocking for THAT slug.
#   3. It FAILS if any excluded slug is not, right now, measurably drifted. An
#      exclusion that names a tenant with a live workload is refused, which is
#      the property that keeps this from degrading into --allow-empty by another
#      spelling.
#
# It deliberately does NOT fail on drift that is merely unexcluded. That drift is
# the roll's business: the roll will fail on it, loudly, which is the correct
# outcome and the one the operator must act on.
#
# Usage:
#   fleet-drift-audit.sh --digest sha256:<hex> [--exclude a,b]
#
# Env:
#   KUBECTL / KUBECONFIG / SUBSTRATE_CENSUS_CMD  as for the roller
#   K8S_ROLLER            override the roller path (tests)
#   GITHUB_STEP_SUMMARY   appended to when set
#
# SAFETY: --dry-run only. The roller performs zero mutations in that mode; this
# script issues no cluster command of its own.
set -euo pipefail

err() { printf '::error::%s\n' "$*" >&2; }
log() { printf '>> [drift] %s\n' "$*" >&2; }

DIGEST="" ; EXCLUDE="${EXCLUDE_SLUGS:-}"
while [ "$#" -gt 0 ]; do
  case "$1" in
    --digest)  DIGEST="$2"; shift 2;;
    --exclude) EXCLUDE="$2"; shift 2;;
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
# `|| rc=$?` rather than a bare invocation: steps run as
# `bash --noprofile --norc -e -o pipefail {0}`, and a --dry-run that finds drift
# exits 1 BY DESIGN. Under `-e` that would abort this script before a single
# diagnostic printed — the exact defect this repository has already shipped once
# (a coverage gate whose failure aborted above its own reporting).
#
# 2>&1 is load-bearing: the roller writes its ENTIRE human-readable output,
# including the report table this parses, to stderr.
rc=0
bash "$ROLLER" --dry-run --digest "$DIGEST" > "$PLAN" 2>&1 || rc=$?

if [ ! -s "$PLAN" ]; then
  err "the roller produced NO output at all in --dry-run (0 bytes captured). This audit cannot see what the plan was, which is a fault in THIS script's capture, not a statement about the fleet."
  exit 1
fi
cat "$PLAN" >&2

# The report table's last column is the outcome; a slug never contains
# whitespace, so field 1 is the slug. FAIL:* rows are the drift.
DRIFTED="$(awk '$NF ~ /^FAIL:/ { print $1 }' "$PLAN" | sort -u)"
NDRIFT="$(printf '%s' "$DRIFTED" | grep -c . || true)"

# ── The parse must not be able to fail silently ───────────────────────────────
# A pattern that matches nothing looks identical to "there is no drift". If the
# dry-run refused AND this parse found no FAIL row, the parse is what broke —
# say so, rather than reporting a clean fleet and letting an exclusion list be
# validated against an empty drift set (which would accept ANY exclusion).
if [ "$rc" -ne 0 ] && [ "${NDRIFT:-0}" -eq 0 ]; then
  err "the roller's --dry-run exited ${rc} but this audit parsed ZERO 'FAIL:' rows out of its report."
  err "That is a PARSE fault in this script (the roller's report format changed, or the run died before printing a table) — not evidence that the fleet is clean. Refusing to validate an exclusion list against a drift set this script could not read."
  exit 1
fi

if [ "${NDRIFT:-0}" -gt 0 ]; then
  log "control-plane/cluster DRIFT: ${NDRIFT} org(s) the control plane places on the k8s substrate cannot be resolved to a rollable tenant workload"
  while IFS= read -r s; do
    [ -n "$s" ] || continue
    printf '::warning::fleet drift: org %s is substrate=k8s in the control plane but has no rollable tenant Deployment carrying its org-id. Until it is deprovisioned or repaired it will FAIL every fleet roll that includes it.\n' "$s" >&2
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
in_csv() { case ",${2}," in *",${1},"*) return 0;; esac; return 1; }
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

# Drift that is NOT excluded is deliberately NOT a failure here: the roll itself
# will fail on it, which is the correct and loud outcome. Saying so once, here,
# means the operator reading a red roll already knows why.
REMAINING=""
while IFS= read -r s; do
  [ -n "$s" ] || continue
  in_csv "$s" "$EXCLUDE" || REMAINING="${REMAINING}${REMAINING:+,}${s}"
done <<< "$DRIFTED"
if [ -n "$REMAINING" ]; then
  log "NOTE: ${REMAINING} is drifted and NOT excluded — the roll that follows WILL fail on it. That is the intended behaviour; the fix is to deprovision or repair the org, or to add it to the exclusion repo var once it is confirmed unrollable."
fi
log "drift audit complete: drifted=${NDRIFT} excluded='${EXCLUDE:-<none>}'"
