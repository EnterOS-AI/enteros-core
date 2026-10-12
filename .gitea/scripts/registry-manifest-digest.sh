#!/usr/bin/env bash
# Print the content digest of the exact manifest bytes a registry serves.
set -euo pipefail

if [ "$#" -ne 1 ] || [ -z "${1:-}" ]; then
  echo "usage: $0 <registry-image-ref>" >&2
  exit 2
fi

ref="$1"
script_dir="$(CDPATH='' cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
# Validate before Docker can consult its credential store or contact a host.
# registry-manifest-state.py owns the canonical registry/ref grammar.
python3 "$script_dir/registry-manifest-state.py" --validate-ref "$ref"
inspect_timeout="${REGISTRY_INSPECT_TIMEOUT_SECONDS:-180}"
manifest_max_bytes="${REGISTRY_MANIFEST_MAX_BYTES:-5242880}"
if ! printf '%s' "$inspect_timeout" | grep -Eq '^[1-9][0-9]*$'; then
  echo "::error::REGISTRY_INSPECT_TIMEOUT_SECONDS must be a positive integer" >&2
  exit 2
fi
if ! printf '%s' "$manifest_max_bytes" | grep -Eq '^[1-9][0-9]*$'; then
  echo "::error::REGISTRY_MANIFEST_MAX_BYTES must be a positive integer" >&2
  exit 2
fi
command -v timeout >/dev/null 2>&1 || {
  echo "::error::GNU timeout is required for bounded registry inspection" >&2
  exit 1
}
manifest="$(mktemp)"
trap 'rm -f "$manifest"' EXIT

set +e
timeout "$inspect_timeout" docker buildx imagetools inspect "$ref" --raw \
  | head -c "$((manifest_max_bytes + 1))" > "$manifest"
inspect_rc=$?
set -e
manifest_bytes="$(wc -c < "$manifest" | tr -d '[:space:]')"
if [ "$manifest_bytes" -gt "$manifest_max_bytes" ]; then
  echo "::error::registry manifest for $ref exceeded ${manifest_max_bytes}-byte response bound" >&2
  exit 1
fi
if [ "$inspect_rc" -ne 0 ]; then
  echo "::error::could not read registry manifest for $ref" >&2
  exit 1
fi
if [ ! -s "$manifest" ]; then
  echo "::error::empty registry manifest for $ref" >&2
  exit 1
fi

python3 - "$manifest" <<'PY'
import hashlib
import pathlib
import sys

body = pathlib.Path(sys.argv[1]).read_bytes()
print("sha256:" + hashlib.sha256(body).hexdigest())
PY
