#!/bin/bash
# Full nuke + rebuild — one command to reset everything.
#
# What "everything" means:
#   1. The compose stack (containers + named volumes + network).
#   2. Dynamically-spawned ws-* workspace containers + their volumes.
#      These are NOT in docker-compose.yml — the provisioner creates them
#      at workspace-create time, so `compose down -v` leaves them behind.
#      Without this step, a fresh DB plus old ws-* containers = ghost
#      containers Canvas can't see, eating CPU + memory.
#   3. Repopulating the manifest-managed dirs (workspace-configs-templates/,
#      org-templates/, plugins/). These are .gitignored — fresh checkouts
#      and post-deletion runs leave them empty, which silently hides the
#      entire template palette in Canvas. clone-manifest.sh is idempotent,
#      so re-running with already-populated dirs is a fast no-op.
#
# Usage:
#   bash scripts/nuke-and-rebuild.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# Pin the compose project like dev-start.sh does: without this, a checkout in
# a differently-named directory creates a parallel <dir>-langfuse-1 that also
# claims the langfuse-web alias on the shared molecule-core-net.
COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-molecule-core}"
export COMPOSE_PROJECT_NAME
# Shared docker-teardown helpers (mol_purge_ws_objects) — same UUID-scoped,
# xargs-free purge scripts/dev-start.sh --fresh uses.
# shellcheck source=scripts/lib/docker-reset.sh
# shellcheck disable=SC1091
. "$ROOT/scripts/lib/docker-reset.sh"

echo "=== NUKE ==="
docker compose -f "$ROOT/docker-compose.yml" down -v 2>/dev/null || true
# Dynamically-spawned ws-<uuid> workspace containers + volumes (NOT in compose).
# Scoped to the workspace-UUID name shape — a bare `^ws-` prefix (the prior form)
# would also delete an unrelated project's ws-* object on the same host, and its
# `xargs -r` is a GNU-ism BSD/macOS xargs rejects.
mol_purge_ws_objects
docker network rm molecule-core-net 2>/dev/null || true
echo "  cleaned"

echo "=== POPULATE MANIFEST DIRS ==="
# Idempotent: clone-manifest.sh skips dirs whose manifest-source marker still
# matches manifest.json, so a re-nuke after templates are current is a fast
# no-op (a few stat calls) while stale/markerless checkouts self-refresh.
# Skip with a clear warning if jq is missing — installing it is a one-time
# step documented in the README quickstart.
if command -v jq >/dev/null 2>&1; then
  bash "$ROOT/scripts/clone-manifest.sh" \
    "$ROOT/manifest.json" \
    "$ROOT/workspace-configs-templates" \
    "$ROOT/org-templates" \
    "$ROOT/plugins" 2>&1 | tail -3
else
  echo "  WARNING: jq not installed — skipping template/plugin clone."
  echo "           Install (brew install jq) and rerun, or Canvas's template"
  echo "           palette will be empty and provisioning falls back to defaults."
fi

echo "=== REBUILD ==="
docker compose -f "$ROOT/docker-compose.yml" up -d --build
echo "  platform + canvas up"

echo "=== RESET COMPLETE ==="
echo "  Open http://127.0.0.1:3000 and complete the first-run flow."
echo "  Add tenant credentials through Canvas Settings."
echo "  Automation may use authenticated PUT /settings/secrets; do not write"
echo "  global_secrets rows directly."
