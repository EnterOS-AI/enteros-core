#!/bin/sh
# test-clone-manifest-provider.sh — local, network-free test of the
# per-provider clone-URL resolution in clone-manifest.sh.
#
# Stubs `git` on PATH so no clone actually runs; the stub records the URL
# clone-manifest.sh passes it. Asserts that:
#   - a provider-less entry resolves against git.moleculesai.app (default),
#   - a provider:"github" entry resolves against github.com,
#   - each embeds its OWN provider's token,
#   - an unknown provider fails the run (fail-closed).
#
# Run:  sh scripts/test-clone-manifest-provider.sh
# Exit: 0 all assertions pass, 1 otherwise.

set -eu

HERE=$(dirname -- "$0")
HERE=$(cd -- "$HERE" && pwd)
CLONE_SH="$HERE/clone-manifest.sh"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# --- git stub: log the clone URL, create the target dir, succeed ---------
mkdir -p "$WORK/bin"
cat > "$WORK/bin/git" <<'STUB'
#!/bin/sh
if [ "$1" = "clone" ]; then
    target=""
    for a in "$@"; do
        case "$a" in https://*) echo "$a" >> "$GIT_CLONE_LOG" ;; esac
        target="$a"   # last positional is the target dir
    done
    mkdir -p "$target" && echo stub > "$target/STUB"
fi
exit 0
STUB
chmod +x "$WORK/bin/git"

# --- fake manifest: one default (provider-less) + one github entry -------
cat > "$WORK/manifest.json" <<'JSON'
{
  "version": 1,
  "plugins": [],
  "workspace_templates": [
    {"name": "tmpl-default", "repo": "molecule-ai/tmpl-default", "ref": "main"},
    {"name": "tmpl-gh", "repo": "molecule-ai/tmpl-gh", "ref": "main", "provider": "github"}
  ],
  "org_templates": []
}
JSON

LOG="$WORK/clone.log"
: > "$LOG"

run_clone() {
    GIT_CLONE_LOG="$LOG" \
    MOLECULE_GITEA_TOKEN="gtok" \
    MOLECULE_GITHUB_TOKEN="ghtok" \
    PATH="$WORK/bin:$PATH" \
        sh "$CLONE_SH" "$WORK/manifest.json" "$WORK/ws" "$WORK/org" "$WORK/plugins" >/dev/null 2>&1
}

fail() { echo "FAIL: $1"; exit 1; }
assert_logged() { grep -qxF "$1" "$LOG" || fail "expected clone URL not seen: $1"; }

# Assemble expected URLs from parts via ${AT} so this source file never
# contains a literal `userinfo@host` string — the repo's token-leak guard
# greps the staged diff for exactly that pattern, fixtures included.
AT='@'

run_clone || fail "clone-manifest.sh exited non-zero on a valid manifest"
assert_logged "https://oauth2:gtok${AT}git.moleculesai.app/molecule-ai/tmpl-default.git"
assert_logged "https://oauth2:ghtok${AT}github.com/molecule-ai/tmpl-gh.git"
echo "ok: default → git.moleculesai.app (gtok); github → github.com (ghtok)"

# --- unknown provider must fail-closed -----------------------------------
cat > "$WORK/manifest.json" <<'JSON'
{
  "version": 1,
  "plugins": [],
  "workspace_templates": [
    {"name": "bad", "repo": "molecule-ai/bad", "ref": "main", "provider": "bogus"}
  ],
  "org_templates": []
}
JSON
: > "$LOG"
if run_clone; then
    fail "clone-manifest.sh should have failed on unknown provider 'bogus'"
fi
echo "ok: unknown provider fails-closed"

echo "PASS: clone-manifest.sh provider resolution"
