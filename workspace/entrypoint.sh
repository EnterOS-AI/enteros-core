#!/bin/sh
# Drop privileges to the agent user before exec'ing molecule-runtime.
# claude-code refuses --dangerously-skip-permissions when running as
# root/sudo for safety. Without this entrypoint, every cron tick fails
# with `ProcessError: Command failed with exit code 1` and the agent
# logs `--dangerously-skip-permissions cannot be used with root/sudo
# privileges for security reasons`.
#
# Pattern matches the legacy monorepo workspace/entrypoint.sh:
# fix volume ownership as root, then re-exec via gosu as agent (uid 1000).

# --- RFC#523 Layer 2: tenant-workspace forbidden-env guard (task #146) ---
# Defense-in-depth. The provisioner (workspace-server) has a fail-closed
# abort at provision time (Layer 1, prepareProvisionContext), and the
# in-container env-build has a silent strip (forensic #145,
# provisioner.buildContainerEnv). This guard fires if either upstream
# layer is bypassed — e.g. someone runs this image standalone with
# `docker run -e GITEA_TOKEN=...`. Exit 1 with a clear message instead
# of running with an operator-scope credential in tenant scope.
#
# Key names are generic. The MOLECULE_OPERATOR_ prefix is the one
# molecule-AI-specific literal; this entrypoint lives inside the
# claude-code template that is internal-only (memory
# `feedback_open_source_templates_no_hardcoded_org_internals` — claude-
# code template is internal, separate-published templates must NOT carry
# org-specific literals). A fork can edit FORBIDDEN_KEYS /
# FORBIDDEN_PREFIXES for its own operator-scope names without touching
# the rest of the entrypoint.
#
# Skipped when MOLECULE_TENANT_GUARD_DISABLE=1 — for local-dev where the
# operator host IS the tenant host (e.g. running molecule-runtime on the
# operator box for debugging). NEVER set this in tenant containers.
if [ "${MOLECULE_TENANT_GUARD_DISABLE:-0}" != "1" ]; then
    FORBIDDEN_KEYS="GITEA_TOKEN GITEA_PAT GITHUB_TOKEN GITHUB_PAT GH_TOKEN GITLAB_TOKEN GL_TOKEN BITBUCKET_TOKEN CP_ADMIN_API_TOKEN CP_ADMIN_TOKEN INFISICAL_OPERATOR_TOKEN INFISICAL_BOOTSTRAP_TOKEN RAILWAY_TOKEN RAILWAY_PERSONAL_API_TOKEN HETZNER_TOKEN HETZNER_API_TOKEN"
    FORBIDDEN_PREFIXES="MOLECULE_OPERATOR_"
    FOUND=""
    for k in $FORBIDDEN_KEYS; do
        # eval is safe here — $k is from a static whitespace-separated
        # literal list above (no user input). POSIX sh has no
        # associative arrays, hence the indirect-expansion via eval to
        # test "is this var set" without caring about its value.
        eval "v=\${$k+set}"
        if [ "$v" = "set" ]; then
            FOUND="$FOUND $k"
        fi
    done
    for prefix in $FORBIDDEN_PREFIXES; do
        # env | awk is the portable POSIX way to enumerate by prefix.
        # busybox awk (alpine), gawk (debian), and BSD awk (macOS-test)
        # all support index(). Doesn't depend on bash arrays / [[ =~ ]].
        prefix_hits=$(env | awk -F= -v p="$prefix" 'index($1, p)==1 {print $1}')
        if [ -n "$prefix_hits" ]; then
            FOUND="$FOUND $prefix_hits"
        fi
    done
    if [ -n "$FOUND" ]; then
        echo "RFC#523 Layer 2: refusing to start tenant workspace — forbidden operator-scope env var(s) present:$FOUND" >&2
        echo "These vars are operator-fleet scope and must not reach tenant workspaces." >&2
        echo "Remove them from workspace_secrets / global_secrets / docker -e and retry." >&2
        echo "If running this image standalone for local dev with intentional operator scope, set MOLECULE_TENANT_GUARD_DISABLE=1." >&2
        exit 1
    fi
fi

if [ "$(id -u)" = "0" ]; then
    # Configs volume is created by Docker as root; agent needs write access
    # for plugin installs, memory writes, .auth_token rotation, etc.
    chown -R agent:agent /configs 2>/dev/null
    # Strip CRLF from hook scripts — Windows Docker Desktop copies host files
    # with CRLF line endings even when .gitattributes says eol=lf. The \r in
    # the shebang line makes python3 try to open 'script.py\r' → ENOENT →
    # claude-code swallows the hook error → "(no response generated)".
    # This is the permanent fix — runs at every container start.
    for f in /configs/.claude/hooks/*.sh /configs/.claude/hooks/*.py; do
        [ -f "$f" ] && sed -i 's/\r$//' "$f"
    done
    # /workspace handling — only chown when the contents are root-owned
    # (typical on Docker Desktop on Windows where host uid maps to 0).
    # On Linux Docker with matching uids the recursive chown is skipped
    # to keep startup fast.
    chown agent:agent /workspace 2>/dev/null || true
    if [ -d /workspace ]; then
        first_entry=$(find /workspace -mindepth 1 -maxdepth 1 -print -quit 2>/dev/null)
        if [ -n "$first_entry" ] && [ "$(stat -c '%u' "$first_entry" 2>/dev/null)" = "0" ]; then
            chown -R agent:agent /workspace 2>/dev/null
        fi
    fi
    # Claude Code session directory — mounted at /root/.claude/sessions by
    # the platform provisioner. Symlink it into agent's home so the SDK
    # finds it when running as agent. The provisioner's mount point is
    # hardcoded to /root/.claude/sessions; we don't want to change the
    # platform contract just for this template.
    mkdir -p /home/agent/.claude
    if [ -d /root/.claude/sessions ]; then
        chown -R agent:agent /root/.claude /home/agent/.claude 2>/dev/null
        ln -sfn /root/.claude/sessions /home/agent/.claude/sessions
    fi

    # --- Per-persona git identity (closes molecule-core#155) ---
    # Without this, every team commit lands with an empty author and Gitea
    # attributes the work to the founder PAT instead of the persona that
    # actually authored it. Same fingerprint that got us suspended on GitHub
    # 2026-05-06. GITEA_USER is injected by the provisioner from the
    # workspace_secrets table; bot.moleculesai.app is the agent-only domain
    # so commits are clearly distinguishable from human authors.
    if [ -n "${GITEA_USER:-}" ]; then
        git config --global user.name  "${GITEA_USER}"
        git config --global user.email "${GITEA_USER}@bot.moleculesai.app"
    fi

    # --- GitHub credential helper setup (issue #547 / #613) ---
    # Configure git to use the molecule credential helper for github.com.
    # This runs as root so the global gitconfig is written before we drop
    # to agent. The helper fetches fresh GitHub App installation tokens
    # from the platform API, with caching and env-var fallback.
    #
    # NOTE: post-suspension (2026-05-06), github.com/Molecule-AI is gone;
    # the helper's platform endpoint also 500s (internal#187). The helper
    # block is kept for legacy boxes that still have a working token chain;
    # post-suspension provisioner injects GITEA_TOKEN directly so this
    # path's failure is non-fatal. Full removal tracked under #171.
    if [ -x /app/scripts/molecule-git-token-helper.sh ]; then
        # Set credential helper for github.com only (not all hosts).
        # The '!' prefix tells git to run the command as a shell command.
        git config --global "credential.https://github.com.helper" \
            "!/app/scripts/molecule-git-token-helper.sh"
        # Disable other credential helpers for github.com to avoid conflicts.
        git config --global "credential.https://github.com.useHttpPath" true
    fi
    # Move gitconfig to agent's home so it takes effect after gosu —
    # done unconditionally so the per-persona identity survives the drop
    # even when the github.com helper block is skipped.
    if [ -f /root/.gitconfig ]; then
        cp /root/.gitconfig /home/agent/.gitconfig
        chown agent:agent /home/agent/.gitconfig
    fi
    # Create the token cache directory for the agent user.
    mkdir -p /home/agent/.molecule-token-cache
    chown agent:agent /home/agent/.molecule-token-cache
    chmod 700 /home/agent/.molecule-token-cache

    exec gosu agent "$0" "$@"
fi

# Now running as agent (uid 1000)

# --- Start background token refresh daemon (with respawn supervision) ---
# Keeps gh CLI and git credentials fresh across the 60-min token TTL.
# Wrapped in a respawn loop so a daemon crash doesn't silently leave the
# workspace stuck on an expired token. Runs in the background; entrypoint
# continues to exec molecule-runtime.
if [ -x /app/scripts/molecule-gh-token-refresh.sh ]; then
    nohup bash -c '
        while true; do
            /app/scripts/molecule-gh-token-refresh.sh
            rc=$?
            echo "[molecule-gh-token-refresh] daemon exited rc=$rc — respawning in 30s" >&2
            sleep 30
        done
    ' > /home/agent/.gh-token-refresh.log 2>&1 &
fi

# --- Initial gh auth setup ---
# If GITHUB_TOKEN or GH_TOKEN is set (injected at provision time),
# authenticate gh CLI with it so it works immediately (before the first
# background refresh fires). The background daemon will replace this
# with a fresh token within ~60s of boot.
if [ -n "${GITHUB_TOKEN:-}" ]; then
    echo "${GITHUB_TOKEN}" | gh auth login --hostname github.com --with-token 2>/dev/null || true
elif [ -n "${GH_TOKEN:-}" ]; then
    echo "${GH_TOKEN}" | gh auth login --hostname github.com --with-token 2>/dev/null || true
fi

exec molecule-runtime "$@"
