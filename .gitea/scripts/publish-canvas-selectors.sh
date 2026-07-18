#!/usr/bin/env bash
# Repair and prove the Canvas candidate selectors without touching :latest.
set -euo pipefail

: "${IMAGE_NAME:?IMAGE_NAME required}"
: "${CANDIDATE_TAG:?CANDIDATE_TAG required}"
: "${SHA_TAG:?SHA_TAG required}"
: "${EXPECTED_SHA:?EXPECTED_SHA required}"
: "${REG_USER:?REG_USER required}"
: "${REG_TOKEN:?REG_TOKEN required}"
: "${REGISTRY_PULL_TIMEOUT_SECONDS:?REGISTRY_PULL_TIMEOUT_SECONDS required}"
: "${GITHUB_OUTPUT:?GITHUB_OUTPUT required}"

if ! printf '%s' "$EXPECTED_SHA" | grep -Eq '^[0-9a-f]{40}$'; then
  echo "::error::EXPECTED_SHA must be a full lowercase commit SHA" >&2
  exit 2
fi
if [ "$CANDIDATE_TAG" != "staging-${EXPECTED_SHA}" ]; then
  echo "::error::candidate tag must be staging-<full-sha>" >&2
  exit 2
fi
if [ "$SHA_TAG" != "sha-${EXPECTED_SHA:0:7}" ]; then
  echo "::error::compatibility tag must be sha-<short-sha>" >&2
  exit 2
fi
if ! printf '%s' "$REGISTRY_PULL_TIMEOUT_SECONDS" | grep -Eq '^[1-9][0-9]*$'; then
  echo "::error::REGISTRY_PULL_TIMEOUT_SECONDS must be a positive integer" >&2
  exit 2
fi
command -v timeout >/dev/null 2>&1 || {
  echo "::error::GNU timeout is required for bounded registry operations" >&2
  exit 1
}

main_sha="$(git rev-parse refs/remotes/origin/main)"
if [ "$main_sha" != "$EXPECTED_SHA" ]; then
  git merge-base --is-ancestor "$EXPECTED_SHA" "$main_sha" || {
    echo "::error::release $EXPECTED_SHA is not an ancestor of main $main_sha" >&2
    exit 1
  }
  publisher_paths=(
    canvas
    .gitea/workflows/publish-canvas-image.yml
    .gitea/scripts/infisical-read-secret.py
    .gitea/scripts/private-gitea-download.py
    .gitea/scripts/publish-canvas-selectors.sh
    .gitea/scripts/registry-manifest-digest.sh
    .gitea/scripts/registry-manifest-state.py
  )
  # Any later commit that touched a publisher input supersedes this run, even
  # when a subsequent revert makes the endpoint trees byte-identical again.
  publisher_change="$(git rev-list --max-count=1 \
    "${EXPECTED_SHA}..${main_sha}" -- "${publisher_paths[@]}")"
  if [ -n "$publisher_change" ]; then
    echo "::error::Canvas release $EXPECTED_SHA is superseded by relevant main $main_sha" >&2
    exit 1
  fi
  echo "::notice::main advanced only through unrelated paths; $EXPECTED_SHA remains the latest Canvas publisher input"
fi

state_digest() {
  local ref="$1" digest rc
  set +e
  digest="$(python3 .gitea/scripts/registry-manifest-state.py "$ref")"
  rc=$?
  set -e
  case "$rc" in
    0) printf '%s\n' "$digest" ;;
    10) return 10 ;;
    *) echo "::error::could not prove registry state for $ref" >&2; return "$rc" ;;
  esac
}

verify_selector() {
  local ref="$1" expected_digest="$2" digest revision
  digest="$(state_digest "$ref")" || {
    echo "::error::selector $ref is absent after repair" >&2
    return 1
  }
  if [ "$digest" != "$expected_digest" ]; then
    echo "::error::selector $ref resolved to $digest, expected $expected_digest" >&2
    return 1
  fi
  timeout "$REGISTRY_PULL_TIMEOUT_SECONDS" docker pull "$ref" >/dev/null
  revision="$(docker image inspect "$ref" \
    --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}' \
    | tr -d '[:space:]')"
  if [ "$revision" != "$EXPECTED_SHA" ]; then
    echo "::error::selector $ref OCI revision ${revision:-<empty>} does not equal $EXPECTED_SHA" >&2
    return 1
  fi
  echo "::notice::verified selector $ref digest=$digest revision=$revision"
}

repair_selector() {
  local ref="$1" expected_digest="$2" current rc
  set +e
  current="$(state_digest "$ref")"
  rc=$?
  set -e
  if [ "$rc" -ne 0 ] && [ "$rc" -ne 10 ]; then
    return "$rc"
  fi
  if [ "$rc" -eq 10 ] || [ "$current" != "$expected_digest" ]; then
    timeout "$REGISTRY_PULL_TIMEOUT_SECONDS" docker buildx imagetools create \
      --prefer-index=false --tag "$ref" "${IMAGE_NAME}@${expected_digest}"
  fi
}

candidate="${IMAGE_NAME}:${CANDIDATE_TAG}"
candidate_digest="$(state_digest "$candidate")" || {
  echo "::error::write-once Canvas candidate $candidate is absent" >&2
  exit 1
}

# Prove the immutable source before mutating either moving selector.
verify_selector "$candidate" "$candidate_digest"

staging_latest="${IMAGE_NAME}:staging-latest"
compatibility="${IMAGE_NAME}:${SHA_TAG}"
repair_selector "$staging_latest" "$candidate_digest"
repair_selector "$compatibility" "$candidate_digest"

# Re-read every selector from the registry and verify both digest and revision.
verify_selector "$staging_latest" "$candidate_digest"
verify_selector "$compatibility" "$candidate_digest"

echo "candidate_digest=$candidate_digest" >> "$GITHUB_OUTPUT"
