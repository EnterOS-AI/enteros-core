#!/usr/bin/env bash
set -euo pipefail
# Anti-regression gate. Fails if any deleted SOP artifact reappears:
#   - #2403: SOP *tier* artifacts (sop-tier-check/refire).
#   - 2026-07-14: the whole SOP *review gate* (qa-review / security-review /
#     sop-checklist and their scripts), removed per CTO directive.

cd "$(dirname "$0")/../../.."

fail=0

# 1. Deleted workflow files must stay deleted
for f in .gitea/workflows/sop-tier-check.yml .gitea/workflows/sop-tier-refire.yml; do
  if [ -e "$f" ]; then
    echo "FAIL: $f was re-added (must stay deleted per #2403)" >&2
    fail=1
  fi
done

# 2. Deleted script files must stay deleted
for f in .gitea/scripts/sop-tier-check.sh .gitea/scripts/sop-tier-refire.sh; do
  if [ -e "$f" ]; then
    echo "FAIL: $f was re-added (must stay deleted per #2403)" >&2
    fail=1
  fi
done

# 3. No tier branching logic in gate_check.py
if grep -qE '_get_pr_tier|TIER_AGENTS' tools/gate-check-v3/gate_check.py; then
  echo "FAIL: tier branching reappeared in gate_check.py" >&2
  fail=1
fi

# 4. No _is_tier_low_pending_ok in merge queue
if grep -q '_is_tier_low_pending_ok' .gitea/scripts/gitea-merge-queue.py; then
  echo "FAIL: tier soft-fail reappeared in gitea-merge-queue.py" >&2
  fail=1
fi

# 5. No sop-tier-check context references in workflow YAML
if grep -rI --exclude-dir='__pycache__' 'sop-tier-check' .gitea/workflows/; then
  echo "FAIL: sop-tier-check context reappeared in workflows" >&2
  fail=1
fi

# 6. No SOP_TIER_CHECK_TOKEN references in workflow YAML or scripts
if grep -rI --exclude-dir='__pycache__' --exclude='test_no_tier_regression.sh' 'SOP_TIER_CHECK_TOKEN' .gitea/workflows/ .gitea/scripts/; then
  echo "FAIL: SOP_TIER_CHECK_TOKEN reference reappeared (use SOP_CHECKLIST_GATE_TOKEN)" >&2
  fail=1
fi

# 7. Deleted SOP review-gate workflows must stay deleted (2026-07-14).
for f in .gitea/workflows/qa-review.yml \
         .gitea/workflows/security-review.yml \
         .gitea/workflows/sop-checklist.yml \
         .gitea/workflows/review-check-tests.yml \
         .gitea/workflows/review-refire-comments.yml; do
  if [ -e "$f" ]; then
    echo "FAIL: $f was re-added (SOP review gate removed 2026-07-14)" >&2
    fail=1
  fi
done

# 8. Deleted SOP review-gate scripts/config must stay deleted (2026-07-14).
for f in .gitea/scripts/review-check.sh \
         .gitea/scripts/_review_check_filter.py \
         .gitea/scripts/sop-checklist.py \
         .gitea/scripts/review-refire-status.sh \
         .gitea/sop-checklist-config.yaml; do
  if [ -e "$f" ]; then
    echo "FAIL: $f was re-added (SOP review gate removed 2026-07-14)" >&2
    fail=1
  fi
done

# 9. The core cron scheduler must stay retired (scheduler-as-trigger-plugin RFC
#    P4, core#4399). ADR-005 guarantees core carries ZERO scheduler runtime code —
#    firing is owned by the per-workspace kind:trigger plugin. If the deleted
#    internal/scheduler package (the singleton cron loop + NativeSchedulerCheck)
#    reappears, that architectural invariant has regressed.
if [ -d workspace-server/internal/scheduler ]; then
  echo "FAIL: workspace-server/internal/scheduler/ was re-added — core must carry ZERO scheduler runtime code (retired in core#4399; see ADR-005 + rfc-scheduler-as-trigger-plugin.md P4). Firing is owned by the kind:trigger plugin." >&2
  fail=1
fi
if grep -rIn --include='*.go' --exclude='*_test.go' 'NativeSchedulerCheck' workspace-server/ >/dev/null 2>&1; then
  echo "FAIL: NativeSchedulerCheck reappeared — the core-vs-daemon double-fire gate was removed with the loop (core#4399); the fire path is 100% plugin-owned." >&2
  fail=1
fi

if [ "$fail" -eq 1 ]; then
  echo "TIER_REGRESSION_DETECTED" >&2
  exit 1
fi

echo "PASS: no tier regression detected"
