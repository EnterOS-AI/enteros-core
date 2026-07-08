#!/usr/bin/env bash
# Retired compatibility stub.
#
# Core no longer vendors workspace-comms schema mirrors. The producer-side Go
# guard compares workspace-server models directly against generated SDK bindings
# in go.moleculesai.app/sdk/gen/go/molcontracts.
set -euo pipefail

echo "::error::check-workspace-comms-ssot-sync.sh is retired; workspace_comms_ssot_test.go now consumes SDK molcontracts directly."
exit 1
