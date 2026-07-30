#!/usr/bin/env bash

# Lint: the e2e harness must not carry hardcoded NATIVE-PLUGIN SOURCE pins.
#
# WHY THIS EXISTS. WHICH first-party plugins the platform delivers, and from
# WHICH pinned source, is owned by ONE contract: the SDK native-plugins registry
# (contracts/plugin/native-plugins.registry.json), consumed by core as the
# generated Go binding molcontracts through
# workspace-server/internal/handlers/plugin_registry.go. Every copy of one of
# those sources is a second source of truth that nothing keeps in step. The
# scheduler proved it: a fallback pin in test_staging_full_saas.sh sat two minor
# versions behind the registry, silently, for as long as anyone looked — and it
# was doubly invisible because injecting MOLECULE_DECLARED_PLUGINS through the
# `secrets` channel is overwritten at provision anyway, so the stale pin never
# even reached a container to fail visibly.
#
# Deleting that literal fixes today. This lint is what stops it coming back: a
# reintroduced pin is RED at unit-test time, offline, with no Docker and no run.
#
# THE ALLOWLIST IS DEBT, NOT DESIGN. The two tolerated names below are the
# remaining pins the harness still carries; they are listed so that a NEW pin is
# a hard failure while the known ones stay visible and countable. Shrinking this
# list to empty is the finish line, not a nice-to-have.

# Names that must NEVER appear as a pinned source in the harness: core already
# declares them on every provision from the registry, so a harness copy is
# redundant AND drift-prone.
NATIVE_PLUGIN_SOURCE_BANNED="${NATIVE_PLUGIN_SOURCE_BANNED:-molecule-ai-plugin-scheduler}"

# Tolerated for now (see "THE ALLOWLIST IS DEBT" above). Each still declares
# through a channel the harness owns, so removing the pin is a behaviour change
# that needs its own verified run — not a drive-by edit.
NATIVE_PLUGIN_SOURCE_ALLOWED_DEBT="${NATIVE_PLUGIN_SOURCE_ALLOWED_DEBT:-molecule-ai-plugin-schedule-self molecule-ai-plugin-digest-mail}"

# native_plugin_source_name <text>
# Extracts the plugin name from a pinned git-native source, or prints nothing.
native_plugin_source_name() {
  printf '%s\n' "${1:-}" |
    sed -nE 's%^.*gitea://[A-Za-z0-9._-]+/(molecule-ai-plugin-[A-Za-z0-9._-]+)#.*$%\1%p'
}

# _native_plugin_source_in_list <name> <space-separated list>
_native_plugin_source_in_list() {
  local needle="${1:-}" item
  for item in ${2:-}; do
    [ "$item" = "$needle" ] && return 0
  done
  return 1
}

# native_plugin_source_violations <path...>
# Prints one line per offending pin ("<file>:<line>: <name>: <reason>"). Prints
# nothing when clean, so callers gate on the output being empty. Only *.sh is
# scanned — the same scope CI's shellcheck pass uses.
native_plugin_source_violations() {
  [ "$#" -gt 0 ] || return 0
  local hit name
  grep -rnoHE --include='*.sh' \
    'gitea://[A-Za-z0-9._-]+/molecule-ai-plugin-[A-Za-z0-9._-]+#[A-Za-z0-9._/-]+' "$@" 2>/dev/null |
    while IFS= read -r hit; do
      [ -n "$hit" ] || continue
      name=$(native_plugin_source_name "$hit")
      [ -n "$name" ] || continue
      if _native_plugin_source_in_list "$name" "$NATIVE_PLUGIN_SOURCE_BANNED"; then
        printf '%s: %s: BANNED — core declares this plugin at provision from the SDK native-plugins registry; a harness pin is a duplicate SSOT that drifts\n' \
          "${hit%%:gitea://*}" "$name"
      elif ! _native_plugin_source_in_list "$name" "$NATIVE_PLUGIN_SOURCE_ALLOWED_DEBT"; then
        printf '%s: %s: NEW PIN — not on the tolerated-debt allowlist; derive it from the registry SSOT instead of hardcoding a version\n' \
          "${hit%%:gitea://*}" "$name"
      fi
    done
}
