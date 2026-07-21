#!/usr/bin/env bash
# Install the canonical private runtime wheel without exposing its package name
# to a mixed public/private resolver. Gitea does not proxy public dependencies,
# so resolve those only after the pinned private wheel is local.
set -euo pipefail

PYTHON_BIN="${PYTHON_BIN:-python3}"
RUNTIME_VERSION="0.4.36"
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
