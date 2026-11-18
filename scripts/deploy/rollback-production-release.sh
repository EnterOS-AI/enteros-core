#!/usr/bin/env bash
# Restore the pre-release production pin and roll its immutable digest.
# Called only after a failed apply/roll by promote-prod-tenant-pin.yml. The
# original release remains failed even when recovery succeeds.
set -euo pipefail

snapshot_dir="${1:?snapshot directory is required}"
: "${PROD_CP_BASE_URLS:?production endpoint table is required}"
: "${DEPLOY_PYTHON3:?deploy Python is required}"

previous="$("$DEPLOY_PYTHON3" - "$snapshot_dir" <<'PY'
import json
import os
from pathlib import Path
import re
import sys
sys.path.insert(0, '.gitea/scripts')
from prod_cp_endpoints import parse_endpoints
from prod_pin_plan import find_tenant_pin
rows = [row for row in parse_endpoints(os.environ['PROD_CP_BASE_URLS']) if row[2] == 'prod']
if len(rows) != 1:
    raise SystemExit('rollback requires exactly one production control plane for this fleet census')
label, base, env = rows[0]
pin = find_tenant_pin(json.loads((Path(sys.argv[1]) / f'pins-{label}.json').read_text())) or {}
digest, sha = pin.get('image_digest', ''), pin.get('git_sha', '')
if not re.fullmatch(r'sha256:[0-9a-f]{64}', digest) or not re.fullmatch(r'[0-9a-f]{7,40}', sha):
    raise SystemExit('previous production pin has no verifiable digest/build identity')
print(base, env, digest, sha, sep='\t')
PY
)"
IFS=$'\t' read -r base inf_env digest sha <<< "$previous"
[ "${2:-}" != "--validate-only" ] || exit 0
: "${TENANT_IMAGE_NAME:?tenant image repository is required}"

failed=0
# A pin write can restore the DB and still fail on the boot secret. Continue
# workload recovery independently; report either failure without hiding it.
if ! CP_BASE_URL="$base" INFISICAL_ENV="$inf_env" \
  bash scripts/deploy/advance-staging-tenant-pin.sh \
    --image "${TENANT_IMAGE_NAME}@${digest}" --git-sha "$sha"; then
  echo "::error::Production pin/boot-secret recovery failed; attempting workload recovery." >&2
  failed=1
fi
if ! EXPECTED_BUILD_SHA="$sha" bash scripts/deploy/redeploy-tenant-fleet-k8s.sh \
  --digest "$digest" --allow-empty; then
  echo "::error::Production workload recovery failed." >&2
  failed=1
fi
[ "$failed" = 0 ] || exit 1
echo "::warning::Production restored to the previous pin ${digest}; the attempted release failed."
