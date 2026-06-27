#!/usr/bin/env bash
# SSOT-derived platform MCP management tool identifier.
#
# Reads MCPServerName and RequiredTool from
# workspace-server/internal/handlers/mcp_plugin_delivery_contract.go
# (the Go SSOT for the molecule-platform MCP plugin delivery contract).
# Exports:
#   PLATFORM_MCP_SERVER_NAME
#   PLATFORM_MCP_REQUIRED_TOOL
#   PLATFORM_MCP_REQUIRED_TOOL_ID  (mcp__<server>__<verb>)
#
# Also provides helper functions so PR-1's probe and PR-3's readiness probe
# use the SAME provision-workspace-callable check.

set -euo pipefail

# Path to this helper, relative to repo root.
__ssot_contract_go="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)/workspace-server/internal/handlers/mcp_plugin_delivery_contract.go"
if [ ! -f "$__ssot_contract_go" ]; then
  echo "provision_tool_ssot.sh: SSOT contract Go file not found at $__ssot_contract_go" >&2
  exit 1
fi

__extract_eq_last() {
  local field="$1"
  local line
  line=$(grep -F "eq(\"$field\"" "$__ssot_contract_go" || true)
  if [ -z "$line" ]; then
    echo "provision_tool_ssot.sh: could not find SSOT field $field in $__ssot_contract_go" >&2
    return 1
  fi
  echo "$line" | sed -E 's/.*, "([^"]+)"\)/\1/'
}

PLATFORM_MCP_SERVER_NAME=$(__extract_eq_last "mcp_server_name")
PLATFORM_MCP_REQUIRED_TOOL=$(__extract_eq_last "required_tool")

if [ -z "${PLATFORM_MCP_SERVER_NAME:-}" ] || [ -z "${PLATFORM_MCP_REQUIRED_TOOL:-}" ]; then
  echo "provision_tool_ssot.sh: failed to extract SSOT values" >&2
  exit 1
fi

PLATFORM_MCP_REQUIRED_TOOL_ID="mcp__${PLATFORM_MCP_SERVER_NAME}__${PLATFORM_MCP_REQUIRED_TOOL}"

export PLATFORM_MCP_SERVER_NAME
export PLATFORM_MCP_REQUIRED_TOOL
export PLATFORM_MCP_REQUIRED_TOOL_ID

# Echo the full dispatcher tool id (e.g. mcp__molecule-platform__provision_workspace).
required_provision_tool_id() {
  echo "$PLATFORM_MCP_REQUIRED_TOOL_ID"
}

# Echo the bare verb (e.g. provision_workspace).
required_provision_tool() {
  echo "$PLATFORM_MCP_REQUIRED_TOOL"
}

# loaded_mcp_tools_has_required <tools_json_array>
# Prints "yes" if the SSOT-required tool id is present, otherwise "no".
loaded_mcp_tools_has_required() {
  local tools_json="$1"
  python3 -c "
import json, sys
tools = json.load(sys.stdin)
print('yes' if '$PLATFORM_MCP_REQUIRED_TOOL_ID' in tools else 'no')
" <<<"$tools_json"
}
