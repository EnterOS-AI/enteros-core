#!/usr/bin/env bash
# reload-staging-controlplane.sh — provider-dispatched staging CP refresh hook.
#
# The tenant-image CI path must stay provider-agnostic; it should not install a
# vendor CLI or require vendor tokens. The normal staging tenant CD path
# promotes the molecule-tenant DB pin through the control-plane admin API, so
# fresh provisions do not need a repository-side provider CLI or CP restart.
#
# Usage:
#   reload-staging-controlplane.sh --tag staging-<sha>
#   reload-staging-controlplane.sh --image registry.../molecule-tenant:staging-<sha>
#
# Providers:
#   none|external|ci-on-merge  no-op; CP refresh is owned by another pipeline
set -euo pipefail

PROVIDER="${CONTROLPLANE_DEPLOY_PROVIDER:-${MOLECULE_CONTROLPLANE_DEPLOY_PROVIDER:-none}}"

case "$PROVIDER" in
  none|external|ci-on-merge)
    image="" tag="" dry_run=0
    while [ "$#" -gt 0 ]; do
      case "$1" in
        --image) image="$2"; shift 2;;
        --tag) tag="$2"; shift 2;;
        --dry-run) dry_run=1; shift;;
        -h|--help)
          sed -n '2,18p' "$0" | sed 's/^# \{0,1\}//'
          exit 0
          ;;
        *) echo "unknown arg: $1" >&2; exit 2;;
      esac
    done
    if [ -z "$image" ] && [ -n "$tag" ]; then
      image="${TENANT_IMAGE_NAME:-registry.moleculesai.app/molecule-ai/molecule-tenant}:$tag"
    fi
    [ -n "$image" ] || { echo "FATAL: one of --image / --tag is required" >&2; exit 2; }
    if [ "$dry_run" = "1" ]; then
      echo "DRY-RUN: CONTROLPLANE_DEPLOY_PROVIDER=$PROVIDER would leave staging CP refresh to the external provider pipeline for $image"
    else
      echo "CONTROLPLANE_DEPLOY_PROVIDER=$PROVIDER: staging CP refresh is external to this repo; no provider CLI invoked for $image"
    fi
    echo "TARGET_IMAGE=${image}"
    ;;
  *)
    echo "FATAL: unsupported CONTROLPLANE_DEPLOY_PROVIDER=$PROVIDER" >&2
    echo "supported providers: none, external, ci-on-merge" >&2
    exit 2
    ;;
esac
