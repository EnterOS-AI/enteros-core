#!/usr/bin/env bash
# Smoke-test for RFC#523 Layer 2 (task #146): the workspace/entrypoint.sh
# top-of-file forbidden-env guard.
#
# Strategy: source the prefix of entrypoint.sh that contains the guard
# (up through the closing `fi` of the guard block), in a sub-shell with
# the env we want to test. We rewrite the `exit 1` to a `return 1` so
# the guard signals failure via the sub-shell's exit code without
# killing the test harness.
#
# Why not docker-run the actual image: the test is unit-scope (does
# the guard logic correctly identify forbidden vs allowed env). Image
# integration is covered by the E2E provision test described in
# RFC#523 §"Acceptance criteria" Layer 2 (run on staging, not here).
#
# Pairs with: workspace_provision_forbidden_env_test.go (Layer 1
# Go-side unit tests).

set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENTRYPOINT="$HERE/../entrypoint.sh"

if [[ ! -f "$ENTRYPOINT" ]]; then
    echo "FAIL: entrypoint not found: $ENTRYPOINT" >&2
    exit 1
fi

# Extract just the guard block (from the first `if [ "${MOLECULE_TENANT_GUARD_DISABLE`
# through the matching `fi`) and rewrite `exit 1` to `return 1` so the
# guard can be invoked inside a function in a sub-shell.
GUARD_SNIPPET=$(awk '
    /^if \[ "\${MOLECULE_TENANT_GUARD_DISABLE/ { inblock=1 }
    inblock { print }
    inblock && /^fi$/ { exit }
' "$ENTRYPOINT" | sed 's/exit 1/return 1/')

if [[ -z "$GUARD_SNIPPET" ]]; then
    echo "FAIL: could not extract guard block from $ENTRYPOINT" >&2
    exit 1
fi

# Helper: run the guard with the env we set, capture exit code. The
# sub-shell starts with `env -i` semantics emulated by `unset` of every
# var the guard checks, so prior shell state doesn't contaminate.
run_guard() {
    # Pass extra-env assignments as args; e.g. run_guard GITEA_TOKEN=x.
    (
        set +e
        # Defensive unset of all keys the guard inspects, so the
        # caller's args are the ONLY positive cases.
        unset GITEA_TOKEN GITEA_PAT GITHUB_TOKEN GITHUB_PAT GH_TOKEN GITLAB_TOKEN GL_TOKEN BITBUCKET_TOKEN
        unset CP_ADMIN_API_TOKEN CP_ADMIN_TOKEN
        unset INFISICAL_OPERATOR_TOKEN INFISICAL_BOOTSTRAP_TOKEN
        unset RAILWAY_TOKEN RAILWAY_PERSONAL_API_TOKEN HETZNER_TOKEN HETZNER_API_TOKEN
        unset MOLECULE_OPERATOR_HOST MOLECULE_OPERATOR_SSH_KEY
        unset MOLECULE_TENANT_GUARD_DISABLE
        for kv in "$@"; do
            export "$kv"
        done
        guard_fn() {
            eval "$GUARD_SNIPPET"
        }
        guard_fn
        echo $?
    )
}

PASS=0
FAIL=0

assert_exit() {
    local label="$1"
    local want="$2"
    shift 2
    local got
    got=$(run_guard "$@" | tail -n 1)
    if [[ "$got" == "$want" ]]; then
        echo "PASS: $label"
        PASS=$((PASS + 1))
    else
        echo "FAIL: $label — want exit=$want got=$got (env: $*)" >&2
        FAIL=$((FAIL + 1))
    fi
}

# --- Case 1: clean env passes (exit 0) ---
assert_exit "clean_env_passes" 0

# --- Case 2: per-agent-scope vars pass (exit 0) ---
assert_exit "per_agent_vars_pass" 0 \
    GIT_HTTP_USERNAME=agent-dev-a \
    GIT_HTTP_PASSWORD=scoped-pat \
    ANTHROPIC_API_KEY=sk-keep \
    MOLECULE_AGENT_ROLE=agent-dev-a

# --- Case 3: forbidden exact-match keys fail (exit 1) ---
assert_exit "gitea_token_blocks"          1 GITEA_TOKEN=leak
assert_exit "github_token_blocks"         1 GITHUB_TOKEN=leak
assert_exit "cp_admin_api_token_blocks"   1 CP_ADMIN_API_TOKEN=leak
assert_exit "infisical_operator_blocks"   1 INFISICAL_OPERATOR_TOKEN=leak
assert_exit "railway_token_blocks"        1 RAILWAY_TOKEN=leak

# --- Case 4: MOLECULE_OPERATOR_ prefix family blocks ---
assert_exit "molecule_operator_host_blocks" 1 MOLECULE_OPERATOR_HOST=op.example.com
assert_exit "molecule_operator_ssh_blocks"  1 MOLECULE_OPERATOR_SSH_KEY=ssh-ed25519...

# --- Case 5: adjacent-but-allowed MOLECULE_* names pass ---
assert_exit "molecule_agent_role_passes" 0 MOLECULE_AGENT_ROLE=agent-dev-a
assert_exit "molecule_url_passes"        0 MOLECULE_URL=https://platform.example.com

# --- Case 6: MOLECULE_TENANT_GUARD_DISABLE=1 bypasses the guard ---
assert_exit "disable_flag_bypasses" 0 \
    MOLECULE_TENANT_GUARD_DISABLE=1 \
    GITEA_TOKEN=leak \
    CP_ADMIN_API_TOKEN=leak

echo
echo "=== L2 entrypoint guard: $PASS passed, $FAIL failed ==="
if [[ "$FAIL" -gt 0 ]]; then
    exit 1
fi
