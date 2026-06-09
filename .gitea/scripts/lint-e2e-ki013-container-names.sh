#!/usr/bin/env bash
# Drift-prevention guard: SEV #2499 class (KI-013 container/volume naming).
#
# KI-013 removed 12-char UUID truncation from container/volume names.
# E2E scripts must use FULL workspace IDs (ws-${WSID}) when referencing
# containers and volumes. Any ${VAR:0:12} truncation in a ws-* context
# is a regression risk.
#
# Run: bash .gitea/scripts/lint-e2e-ki013-container-names.sh
set -euo pipefail

PAT=':0:12([^0-9]|$)'
ERR=0

for f in tests/e2e/*.sh; do
  # Allow :0:12 when it is NOT inside a ws-* container/volume reference.
  # The grep looks for ws- followed anywhere on the same line by ${*:0:12.
  MATCHES=$(grep -nE "$PAT" "$f" 2>/dev/null || true)
  if [ -n "$MATCHES" ]; then
    echo "::error::SEV-2499 drift guard: truncated workspace ID in container/volume name"
    echo "::error::file=$f"
    echo "$MATCHES" | while read -r line; do
      echo "::error::  $line"
    done
    ERR=1
  fi
done

if [ "$ERR" -ne 0 ]; then
  echo ""
  echo "FAIL: E2E scripts reference containers/volumes with 12-char truncated IDs."
  echo "      KI-013 requires FULL workspace IDs. Update the flagged lines."
  exit 1
fi

echo "PASS: No truncated workspace IDs in E2E container/volume references."
