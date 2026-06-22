#!/usr/bin/env bash
# prune_cf_e2e_dns.sh — prune stale ephemeral e2e DNS records from Cloudflare.
#
# Purpose:
#   Staging tenant provisioning repeatedly exhausts the Cloudflare DNS
#   record quota (error 81045) because e2e-smoke-* and e2e-tmpl-* test
#   records are not cleaned up after ephemeral test runs. This script
#   lists records in the zone, filters to clearly disposable test names,
#   and deletes records older than a configurable age threshold.
#
# Safety:
#   - DRY-RUN by default. It only prints what WOULD be deleted.
#   - Requires explicit --apply or PRUNE_APPLY=1 to actually delete.
#   - Skips anything that does NOT match the e2e-smoke-* or e2e-tmpl-*
#     prefixes (optionally anchored to the moleculesai.app zone).
#   - Aborts on any non-2xx Cloudflare API response (curl -f).
#   - Reports counts and exits non-zero on API or validation errors.
#
# Env vars required:
#   CF_API_TOKEN  — Cloudflare API token with Zone:DNS:Edit on the zone.
#   CF_ZONE_ID    — Cloudflare zone ID (e.g. for moleculesai.app).
#
# Optional env vars:
#   PRUNE_MIN_AGE_HOURS  — only delete records older than N hours (default: 24).
#   PRUNE_APPLY            — set to 1 to actually delete (default dry-run).
#
# Usage:
#   # Dry-run: print what would be pruned
#   CF_API_TOKEN=xxx CF_ZONE_ID=yyy ./scripts/ops/prune_cf_e2e_dns.sh
#
#   # Actually prune records older than 6 hours
#   CF_API_TOKEN=xxx CF_ZONE_ID=yyy PRUNE_MIN_AGE_HOURS=6 PRUNE_APPLY=1 \
#     ./scripts/ops/prune_cf_e2e_dns.sh --apply
#
# Long-term:
#   A scheduled post-run step in .gitea/workflows (e.g. after each E2E
#   staging-saas run) is the better permanent fix, so this quota blocker
#   does not recur. This script is the ready-to-run tool pending that
#   workflow change and a scoped CF token.

set -euo pipefail

APPLY=0
MIN_AGE_HOURS="${PRUNE_MIN_AGE_HOURS:-24}"
ZONE_DOMAIN="${PRUNE_ZONE_DOMAIN:-moleculesai.app}"

for arg in "$@"; do
  case "$arg" in
    --apply) APPLY=1 ;;
    --help|-h)
      grep '^#' "$0" | head -55 | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown arg: $arg (use --help)" >&2
      exit 1
      ;;
  esac
done

[ "${PRUNE_APPLY:-0}" = "1" ] && APPLY=1

need() {
  local var="$1"
  if [ -z "${!var:-}" ]; then
    echo "ERROR: $var is required" >&2
    exit 1
  fi
}
need CF_API_TOKEN
need CF_ZONE_ID

if ! [[ "$MIN_AGE_HOURS" =~ ^[0-9]+$ ]]; then
  echo "ERROR: PRUNE_MIN_AGE_HOURS must be a non-negative integer" >&2
  exit 1
fi

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

log "Pruning ephemeral e2e DNS records (min-age=${MIN_AGE_HOURS}h, apply=${APPLY})..."

# Validate CF token + zone reachability before any list/delete work.
PF_JSON=$(curl -sS -f -m 10 -H "Authorization: Bearer $CF_API_TOKEN" \
  "https://api.cloudflare.com/client/v4/user/tokens/verify" 2>/dev/null) || {
  log "ERROR: CF token verify failed (non-2xx or network error)."
  exit 1
}
if ! python3 -c "
import json, sys
p = json.loads(sys.stdin.read())
if not p.get('success'):
    print('ERROR: CF token verify returned success=false', file=sys.stderr)
    sys.exit(1)
status = (p.get('result') or {}).get('status', '?')
if status != 'active':
    print(f'ERROR: CF token not active (status={status})', file=sys.stderr)
    sys.exit(1)
" <<< "$PF_JSON"; then
  exit 1
fi
log "  CF token active ✓"

ZONE_JSON=$(curl -sS -f -m 10 -H "Authorization: Bearer $CF_API_TOKEN" \
  "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID" 2>/dev/null) || {
  log "ERROR: zone lookup failed (non-2xx or network error)."
  exit 1
}
if ! python3 -c "
import json, os, sys
p = json.loads(sys.stdin.read())
if not p.get('success'):
    print('ERROR: zone lookup returned success=false', file=sys.stderr)
    sys.exit(1)
res = p.get('result') or {}
if res.get('id') != os.environ['CF_ZONE_ID']:
    print('ERROR: zone id mismatch', file=sys.stderr)
    sys.exit(1)
" <<< "$ZONE_JSON"; then
  exit 1
fi
log "  zone $CF_ZONE_ID reachable ✓"

# Fetch all DNS records, paginated. Use a temp file for the raw list.
TMPDIR=$(mktemp -d -t cf-e2e-prune-XXXXXX)
LIST_FILE="$TMPDIR/records.json"
trap 'rm -rf "$TMPDIR"' EXIT

PAGE=1
while :; do
  PAGE_FILE="$TMPDIR/page-$(printf '%05d' "$PAGE").json"
  curl -sS -f -m 30 -H "Authorization: Bearer $CF_API_TOKEN" \
    "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records?per_page=100&page=$PAGE" \
    > "$PAGE_FILE" 2>/dev/null || {
    log "ERROR: DNS list page $PAGE failed (non-2xx or network error)."
    exit 1
  }
  if ! python3 -c "
import json, sys
try:
    p = json.load(open(sys.argv[1]))
except Exception as e:
    print(f'ERROR: non-JSON DNS list response: {e}', file=sys.stderr)
    sys.exit(1)
if not p.get('success'):
    print('ERROR: DNS list returned success=false', file=sys.stderr)
    sys.exit(1)
if not isinstance(p.get('result'), list):
    print('ERROR: DNS list result is not an array', file=sys.stderr)
    sys.exit(1)
" "$PAGE_FILE"; then
    exit 1
  fi
  RESULT_COUNT=$(python3 -c "import json; print(len(json.load(open(sys.argv[1]))['result']))" "$PAGE_FILE")
  if [ "$RESULT_COUNT" -eq 0 ]; then
    break
  fi
  PAGE=$((PAGE + 1))
  if [ "$PAGE" -gt 1000 ]; then
    log "WARNING: stopping pagination at page 1000 (100k records) — investigate if more."
    break
  fi
done

python3 - '
import glob, json, os, re, sys
from datetime import datetime, timezone, timedelta

min_age_hours = int(os.environ["MIN_AGE_HOURS"])
zone_domain = os.environ["ZONE_DOMAIN"]
cutoff = datetime.now(timezone.utc) - timedelta(hours=min_age_hours)

# Clearly ephemeral test prefixes only. Require the record to live under
# the configured zone domain so we never match a similarly-named record
# in a different zone.
EPHEMERAL_RE = re.compile(r"^(e2e-smoke|e2e-tmpl)[a-zA-Z0-9_-]*\." + re.escape(zone_domain) + r"$")

records = []
for f in sorted(glob.glob(os.path.join(sys.argv[1], "page-*.json"))):
    with open(f) as fh:
        records.extend(json.load(fh).get("result") or [])

def parse_iso(s):
    if not s:
        return None
    try:
        return datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return None

candidates = []
for r in records:
    name = r.get("name", "")
    rid = r.get("id", "")
    if not EPHEMERAL_RE.match(name):
        continue
    created = parse_iso(r.get("created_on"))
    if created is None:
        # If we cannot establish age, keep it for safety.
        continue
    if created >= cutoff:
        continue
    candidates.append({"id": rid, "name": name, "type": r.get("type", "?"), "created_on": r.get("created_on")})

print(json.dumps(candidates))
' "$TMPDIR" > "$LIST_FILE"

CANDIDATES_COUNT=$(python3 -c "import json; print(len(json.load(open(sys.argv[1]))))" "$LIST_FILE")
log "Found $CANDIDATES_COUNT candidate record(s) older than ${MIN_AGE_HOURS}h matching ephemeral prefixes."

if [ "$CANDIDATES_COUNT" -eq 0 ]; then
  log "Nothing to prune."
  exit 0
fi

if [ "$APPLY" -ne 1 ]; then
  log ""
  log "DRY RUN — the following $CANDIDATES_COUNT record(s) WOULD be deleted."
  log "Pass --apply or set PRUNE_APPLY=1 to actually delete."
  log ""
  python3 -c "
import json
for r in json.load(open(sys.argv[1])):
    print(f\"  {r['type']:6s}  {r['name']:<60s}  created={r['created_on']}\")
" "$LIST_FILE"
  exit 0
fi

# Apply deletes.
log ""
log "APPLY — deleting $CANDIDATES_COUNT record(s)..."
DELETED=0
FAILED=0
while IFS= read -r line; do
  rid=$(echo "$line" | python3 -c "import json,sys; print(json.loads(sys.stdin.read())['id'])")
  name=$(echo "$line" | python3 -c "import json,sys; print(json.loads(sys.stdin.read())['name'])")
  if curl -sS -f -m 15 -X DELETE \
      -H "Authorization: Bearer $CF_API_TOKEN" \
      "https://api.cloudflare.com/client/v4/zones/$CF_ZONE_ID/dns_records/$rid" \
      >/dev/null 2>&1; then
    log "  deleted: $name"
    DELETED=$((DELETED + 1))
  else
    log "  FAILED:  $name"
    FAILED=$((FAILED + 1))
  fi
done < <(python3 -c "import json,sys; print(json.dumps(json.load(sys.stdin)))" < "$LIST_FILE" | python3 -c "
import json, sys
for r in json.load(sys.stdin):
    print(json.dumps(r))
")

log ""
log "Done. deleted=$DELETED failed=$FAILED"
[ "$FAILED" -eq 0 ]
