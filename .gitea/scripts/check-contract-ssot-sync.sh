#!/usr/bin/env bash
# Retired compatibility stub.
#
# Core no longer vendors contracts/mcp-plugin-delivery.contract.json. Runtime code
# imports the generated SDK binding from go.moleculesai.app/sdk/gen/go/molcontracts,
# and cross-repo drift checks compare template/runtime participants directly
# against the molecule-ai-sdk SSOT.
set -euo pipefail

echo "::error::check-contract-ssot-sync.sh is retired; use SDK molcontracts and mcp-plugin-delivery-contract-drift instead."
exit 1
