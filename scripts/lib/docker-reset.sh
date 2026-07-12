# shellcheck shell=sh
# docker-reset.sh — shared docker-teardown helpers for the local-dev reset
# scripts (dev-start.sh --fresh, nuke-and-rebuild.sh). SOURCE this, don't execute.
#
# POSIX sh so /bin/sh dev-start.sh can source it; bash (nuke-and-rebuild.sh,
# set -euo pipefail) sources it fine too. Every helper is set -u safe and never
# relies on `xargs -r` (a GNU-ism BSD/macOS xargs rejects with "illegal option").

# MOL_WS_UUID_RE scopes ws-* deletion to Molecule's own dynamically-spawned
# workspace objects — NOT a bare `ws-` prefix, which would also match an
# unrelated project's `ws-*` object on the same docker host and destroy its data
# (the class of bug this file exists to prevent in one place).
#
# It must match BOTH name shapes the provisioner emits, so neither leaks as a
# ghost after a reset:
#   - current full-UUID:  ws-<8hex>-<4hex>-…            (ContainerName / *VolumeName)
#   - pre-KI-013 legacy:  ws-<8hex>-<3hex>[-suffix]     (legacy*Name, id[:12])
# `ws-<8hex>-<3-or-4 hex><dash-or-end>` covers both. It still rejects the common
# unrelated names (ws-frontend-cache, ws-redis-data) — "frontend"/"redis" are not
# 8 hex — and even a hex-shaped `ws-deadbeef-cache` (the 3-4 hex group can't reach
# a dash/end through "cache").
MOL_WS_UUID_RE='^ws-[0-9a-f]{8}-[0-9a-f]{3,4}(-|$)'

# mol_rm_by_filter LABEL LIST_CMD ERE_PATTERN REMOVE_CMD
# Delete the docker objects a `docker … --format {{…}}` + `grep -E` pipeline
# selects. Only invokes the remover when the selection is non-empty (the reason
# `xargs -r` existed); prints the count and WARNs per failed item so a partial
# wipe is never silently reported as a clean slate.
mol_rm_by_filter() {
    _label="$1"
    _sel=$(eval "$2" 2>/dev/null | grep -E "$3" 2>/dev/null || true)
    [ -n "$_sel" ] || return 0
    echo "    reset: removing $(printf '%s\n' "$_sel" | grep -c .) $_label"
    printf '%s\n' "$_sel" | while IFS= read -r _item; do
        [ -n "$_item" ] || continue
        eval "$4 \"$_item\"" >/dev/null 2>&1 \
            || echo "    WARN: failed to remove $_label: $_item" >&2
    done
}

# mol_purge_ws_objects — force-remove Molecule's dynamically-spawned ws-<uuid>
# workspace containers + volumes. These are NOT in docker-compose, so a
# `compose down -v` leaves them behind (ghost containers Canvas can't see).
# UUID-scoped via MOL_WS_UUID_RE, so it is safe to run on a shared docker host.
mol_purge_ws_objects() {
    mol_rm_by_filter "workspace containers" "docker ps -a --format '{{.Names}}'" "$MOL_WS_UUID_RE" "docker rm -f"
    mol_rm_by_filter "workspace volumes"    "docker volume ls --format '{{.Name}}'" "$MOL_WS_UUID_RE" "docker volume rm"
}
