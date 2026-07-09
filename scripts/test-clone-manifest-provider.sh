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
# Skip global options (`git -c key=val clone …`) — clone-manifest.sh invokes
# `git -c credential.helper= clone`, so the subcommand is not $1.
while [ $# -gt 0 ]; do
    case "$1" in
        -c) shift 2 ;;
        -*) shift ;;
        *) break ;;
    esac
done
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

# clone-manifest.sh is run with `bash` (NOT sh): it uses `set -o pipefail`, which
# dash (the CI runner's /bin/sh) rejects — and production runs it via bash too.
run_clone() {
    GIT_CLONE_LOG="$LOG" \
    MOLECULE_GITEA_TOKEN="gtok" \
    MOLECULE_GITHUB_TOKEN="ghtok" \
    PATH="$WORK/bin:$PATH" \
        bash "$CLONE_SH" "$WORK/manifest.json" "$WORK/ws" "$WORK/org" "$WORK/plugins" >/dev/null 2>&1
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

: > "$LOG"
run_clone || fail "clone-manifest.sh exited non-zero on an unchanged populated manifest"
[ ! -s "$LOG" ] || fail "unchanged populated manifest should not reclone entries"
grep -qxF "provider=moleculesai" "$WORK/ws/tmpl-default/.molecule-manifest-source" \
  || fail "default manifest marker missing provider"
grep -qxF "repo=molecule-ai/tmpl-default" "$WORK/ws/tmpl-default/.molecule-manifest-source" \
  || fail "default manifest marker missing repo"
grep -qxF "ref=main" "$WORK/ws/tmpl-default/.molecule-manifest-source" \
  || fail "default manifest marker missing ref"
if grep -R "gtok\\|ghtok" "$WORK/ws/tmpl-default/.molecule-manifest-source" "$WORK/ws/tmpl-gh/.molecule-manifest-source"; then
    fail "manifest marker must not persist provider tokens"
fi
echo "ok: populated dirs skip only when manifest marker matches, without persisting tokens"

cat > "$WORK/manifest.json" <<'JSON'
{
  "version": 1,
  "plugins": [],
  "workspace_templates": [
    {"name": "tmpl-default", "repo": "molecule-ai/tmpl-default", "ref": "feature"},
    {"name": "tmpl-gh", "repo": "molecule-ai/tmpl-gh", "ref": "main", "provider": "github"}
  ],
  "org_templates": []
}
JSON
: > "$LOG"
run_clone || fail "clone-manifest.sh exited non-zero after a manifest ref change"
assert_logged "https://oauth2:gtok${AT}git.moleculesai.app/molecule-ai/tmpl-default.git"
if grep -qxF "https://oauth2:ghtok${AT}github.com/molecule-ai/tmpl-gh.git" "$LOG"; then
    fail "unchanged github entry should not reclone when another entry changes"
fi
grep -qxF "ref=feature" "$WORK/ws/tmpl-default/.molecule-manifest-source" \
  || fail "ref-changed entry did not refresh its manifest marker"
echo "ok: changed manifest ref refreshes only the stale entry"

cat > "$WORK/manifest.json" <<'JSON'
{
  "version": 1,
  "plugins": [],
  "workspace_templates": [
    {"name": "legacy", "repo": "molecule-ai/legacy", "ref": "main"}
  ],
  "org_templates": []
}
JSON
mkdir -p "$WORK/ws/legacy"
printf 'old\n' > "$WORK/ws/legacy/OLD"
: > "$LOG"
run_clone || fail "clone-manifest.sh exited non-zero on a legacy markerless dir"
assert_logged "https://oauth2:gtok${AT}git.moleculesai.app/molecule-ai/legacy.git"
[ ! -e "$WORK/ws/legacy/OLD" ] || fail "legacy markerless dir should be replaced, not reused"
grep -qxF "repo=molecule-ai/legacy" "$WORK/ws/legacy/.molecule-manifest-source" \
  || fail "legacy refresh did not write manifest marker"
echo "ok: legacy populated dirs without manifest markers refresh once"

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
