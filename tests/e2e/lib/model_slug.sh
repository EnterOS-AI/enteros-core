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
# PLATFORM-MANAGED path (E2E_LLM_PATH=platform) — the moonshot/kimi
# NOT_CONFIGURED regression (RFC#340 Fix A #2187):
#
#   The branches above all exercise BYOK: a tenant key (MINIMAX/ANTHROPIC/
#   OPENAI) is injected as a workspace secret and the model id resolves to that
#   vendor's *BYOK* provider entry. That path NEVER exercises the platform arm —
#   the exact arm that booted "moonshot/kimi-k2.6" into NOT_CONFIGURED in prod,
#   because the generated config.yaml lacked the derived `provider: platform`.
#
#   E2E_LLM_PATH=platform selects a platform-managed model id (slash-namespaced,
#   no tenant key — Molecule owns billing via the CP LLM proxy). The default is
#   "moonshot/kimi-k2.6", the headline incident combo. Override the specific
#   platform model with E2E_MODEL_SLUG. The provision branch in
#   test_staging_full_saas.sh sends NO secrets for this path (platform-managed
#   needs none), so the workspace must boot online purely on the proxy env the
#   control plane injects + the manifest-derived `provider: platform` that Fix A
#   stamps. That is the REAL boot-path assertion the deterministic unit test
#   (workspace_provision_platform_boot_test.go) cannot make.
#
# When E2E_MODEL_SLUG is set, it overrides this dispatch entirely — useful when
# an operator dispatches the workflow to test a specific slug (or a specific
# platform model id).
#
# Unit tested by tests/e2e/test_model_slug.sh — every branch must stay
# pinned because regressions silently mask as "Could not resolve
# authentication method" + the synth-E2E gate goes red without naming
# the slug-format mismatch.

# Default platform-managed model for the platform-boot regression path. The
# exact id that booted NOT_CONFIGURED in prod. Must stay a member of the
# claude-code `platform` arm in workspace-server/internal/providers/providers.yaml
# (the deterministic suite TestEnsureDefaultConfig_StampsProviderForEverySSOTPlatformModel
# enforces every member of that arm derives provider=platform). Resolved INSIDE
# pick_model_slug via ${E2E_DEFAULT_PLATFORM_MODEL:-...} so callers can override
# it (or unset it) without tripping `set -u`.
E2E_DEFAULT_PLATFORM_MODEL_FALLBACK="moonshot/kimi-k2.6"

# Usage: pick_model_slug <runtime>
#   stdout: the slug string
#   E2E_MODEL_SLUG (env): if set + non-empty, used as-is (operator override)
#   E2E_LLM_PATH=platform (env): select the platform-managed model id
#     (E2E_DEFAULT_PLATFORM_MODEL) instead of a BYOK slug. Takes precedence over
#     the per-key BYOK branches; E2E_MODEL_SLUG still wins over everything.
pick_model_slug() {
  local runtime="${1:-}"
  if [ -n "${E2E_MODEL_SLUG:-}" ]; then
    printf '%s' "$E2E_MODEL_SLUG"
    return 0
  fi
  # Platform-managed path: the slash-namespaced platform model, no tenant key.
  # Exercises the arm the moonshot/kimi NOT_CONFIGURED bug shipped on.
  if [ "${E2E_LLM_PATH:-}" = "platform" ]; then
    printf '%s' "${E2E_DEFAULT_PLATFORM_MODEL:-$E2E_DEFAULT_PLATFORM_MODEL_FALLBACK}"
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
