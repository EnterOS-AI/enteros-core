#!/usr/bin/env bash
# Drift-prevention guard: SEV #2499 class (KI-013 container/volume naming).
#
# KI-013 removed 12-char UUID truncation from container/volume names.
# E2E scripts must use FULL workspace IDs when referencing containers
# and volumes. Any :0:12 substring-match truncation is a regression risk.
#
# Scans ALL .sh files under tests/e2e/ (including lib/ and subdirs).
# Run: bash .gitea/scripts/lint-e2e-ki013-container-names.sh
set -euo pipefail

PAT=':0:12([^0-9]|$)'
ERR=0

# Use find to recurse into tests/e2e subdirs (lib/, cron/, etc.)
while IFS= read -r -d '' f; do
  MATCHES=$(grep -nE "$PAT" "$f" 2>/dev/null || true)
  if [ -n "$MATCHES" ]; then
    echo "::error::SEV-2499 drift guard: truncated workspace ID (:0:12) in E2E script"
    echo "::error::file=$f"
    echo "$MATCHES" | while read -r line; do
      echo "::error::  $line"
    done
    ERR=1
  fi
done < <(find tests/e2e -type f -name '*.sh' -print0)

if [ "$ERR" -ne 0 ]; then
  echo ""
  echo "FAIL: E2E scripts use 12-char truncated IDs (:0:12)."
  echo "      KI-013 requires FULL workspace IDs. Update the flagged lines."
  exit 1
fi

echo "PASS: No truncated workspace IDs in E2E scripts."
