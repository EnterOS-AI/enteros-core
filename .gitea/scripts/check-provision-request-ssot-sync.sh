#!/usr/bin/env bash
# Retired compatibility stub.
#
# Core no longer vendors workspace-server/internal/provisioner/
# provision_request.contract.json. The producer-side contract test compares the
# wire struct directly against generated SDK bindings.
set -euo pipefail

echo "::error::check-provision-request-ssot-sync.sh is retired; provision_request_contract_test.go now consumes SDK molcontracts directly."
exit 1
