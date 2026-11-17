#!/usr/bin/env bash
# Where a workspace's trigger-scheduler state lives, resolved INSIDE the
# container (prepend to a `docker exec … sh -c` body; it sets $TSD).
#
# The runtime (molecule_runtime/trigger_state.py resolve_trigger_state_dir)
# keeps the schedule grid, health heartbeat, run history and state on the
# provisioner-declared durable plugin-state root, <MOLECULE_PLUGIN_STATE_ROOT>/
# .trigger-state, when the control plane declares one, and on the legacy
# /configs/schedules otherwise. Control planes since the plugin-state contract
# (the ephemeral gate's CP_EPHEMERAL_REF 7d79240c included) declare the root
# (/home/agent/.molecule/plugin-state) on every local-docker workspace; older
# ones do not. The literal root is a fallback for a container whose exec env
# lacks the variable. Whichever directory the runtime created wins.
# shellcheck disable=SC2016  # expanded by the container's sh, not here
TRIGGER_STATE_DIR_SH='TSD=/configs/schedules; for _r in "${MOLECULE_PLUGIN_STATE_ROOT:-}" /home/agent/.molecule/plugin-state; do if [ -n "$_r" ] && [ -d "$_r/.trigger-state" ]; then TSD="$_r/.trigger-state"; break; fi; done; '
export TRIGGER_STATE_DIR_SH
