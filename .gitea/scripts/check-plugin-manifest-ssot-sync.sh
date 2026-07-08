#!/usr/bin/env bash
# Retired compatibility stub.
#
# Core no longer vendors workspace-server/internal/plugins/contracts/
# plugin-manifest.schema.json. Runtime code reads the generated SDK asset from
# go.moleculesai.app/sdk/gen/go/molcontracts instead of embedding a local copy.
set -euo pipefail

echo "::error::check-plugin-manifest-ssot-sync.sh is retired; use SDK molcontracts.PluginManifestSchemaJSON instead."
exit 1
