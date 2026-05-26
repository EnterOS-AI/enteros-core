#!/usr/bin/env bash
# Per-runtime model slug dispatch for E2E provisioning.
#
# Different runtimes parse the model slug differently (PR #2571 incident,
# 2026-05-03):
#
#   hermes      → "openai/gpt-4o"  (slash-form: derive-provider.sh splits
#                                    on the prefix to set
#                                    HERMES_INFERENCE_PROVIDER. Bare
#                                    "gpt-4o" falls through to Anthropic
#                                    default + 401, see PR #1714.)
#
#   claude-code → auth-aware:
#                  E2E_MINIMAX_API_KEY    → "MiniMax-M2"
#                  E2E_ANTHROPIC_API_KEY  → "claude-sonnet-4-6"
#                  otherwise              → "sonnet"
#
#                  claude-code provider routing is model-driven. The bare
#                  "sonnet" alias selects the OAuth provider, so it is only a
#                  good default when the canary is using Claude Code OAuth or
#                  intentionally exercising the missing-auth path. MiniMax and
#                  direct Anthropic API keys need model IDs that resolve to
#                  their provider entries, otherwise the workspace boots
#                  reachable but the first A2A call hits the wrong auth path.
#
# When E2E_MODEL_SLUG is set, it overrides this dispatch — useful when an
# operator dispatches the workflow to test a specific slug.
#
# Unit tested by tests/e2e/test_model_slug.sh — every branch must stay
# pinned because regressions silently mask as "Could not resolve
# authentication method" + the synth-E2E gate goes red without naming
# the slug-format mismatch.

# Usage: pick_model_slug <runtime>
#   stdout: the slug string
#   E2E_MODEL_SLUG (env): if set + non-empty, used as-is (operator override)
pick_model_slug() {
  local runtime="${1:-}"
  if [ -n "${E2E_MODEL_SLUG:-}" ]; then
    printf '%s' "$E2E_MODEL_SLUG"
    return 0
  fi
  case "$runtime" in
    hermes)      printf 'openai/gpt-4o' ;;
    claude-code)
      if [ -n "${E2E_MINIMAX_API_KEY:-}" ]; then
        printf 'MiniMax-M2'
      elif [ -n "${E2E_ANTHROPIC_API_KEY:-}" ]; then
        printf 'claude-sonnet-4-6'
      else
        printf 'sonnet'
      fi
      ;;
    *)           printf 'openai/gpt-4o' ;;  # safest fallback (matches hermes)
  esac
}
