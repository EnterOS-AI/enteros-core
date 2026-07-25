#!/usr/bin/env bash
# Install the canonical private runtime wheel without exposing its package name
# to a mixed public/private resolver. Gitea does not proxy public dependencies,
# so resolve those only after the pinned private wheel is local.
set -euo pipefail

PYTHON_BIN="${PYTHON_BIN:-python3}"
# IN-REPO SSOT for the molecules-workspace-runtime pin. Every other copy in this
# repository (the operator-facing external-connect snippets in
# workspace-server/internal/handlers/external_connection.go, and their tests)
# MUST derive from or be asserted against this line — see
# TestExternalRuntimeTemplates_PinMatchesInstallerSSOT. Three independent
# hard-coded copies is what let this pin rot.
#
# WHY THIS ROTS: the Gitea package-retention janitor keeps only the newest
# KEEP_UNPINNED (=5) unpinned versions of a package, and its pin-awareness
# guards are digest/container-shaped (runtime_image_pins, live containers,
# name-pin globs) — a pypi version referenced ONLY by a source constant like
# this one is invisible to it. So this pin is silently deleted from the private
# index once five newer runtime releases land, and every consumer breaks at
# once: CI (harness-replays / e2e-api install step) AND the live operator
# copy-paste snippet. That has now happened twice (0.4.29 -> pruned -> 0.4.36;
# 0.4.36 -> pruned -> 0.4.42 on 2026-07-25). The durable fix is upstream —
# either enrol molecule-core in the runtime repo's release propagation bot
# (scripts/propagate_runtime_version.py, which today filters to
# `*-workspace-template-*` repos and so skips this one) or teach the janitor
# source-pin awareness for pypi. Until one of those lands, this pin must be
# bumped by hand each time it nears the retention window.
RUNTIME_VERSION="0.4.42"
PRIVATE_INDEX="https://git.moleculesai.app/api/packages/molecule-ai/pypi/simple/"
PUBLIC_INDEX="https://pypi.org/simple/"

wheel_dir="$(mktemp -d)"
cleanup() {
  rm -rf "$wheel_dir"
}
trap cleanup EXIT

"$PYTHON_BIN" -m pip download --no-deps --dest "$wheel_dir" \
  --index-url "$PRIVATE_INDEX" \
  "molecules-workspace-runtime==${RUNTIME_VERSION}"

shopt -s nullglob
wheels=("$wheel_dir"/molecules_workspace_runtime-"$RUNTIME_VERSION"-*.whl)
shopt -u nullglob
if [ "${#wheels[@]}" -ne 1 ]; then
  echo "expected exactly one molecules-workspace-runtime ${RUNTIME_VERSION} wheel; found ${#wheels[@]}" >&2
  exit 1
fi

"$PYTHON_BIN" -m pip install --index-url "$PUBLIC_INDEX" "${wheels[0]}"
