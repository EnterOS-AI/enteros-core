#!/bin/sh
# test-clone-manifest-tolerant.sh — local, network-free test of the
# strict/best-effort behavior in clone-manifest.sh.
#
# Stubs `git` so a clone fails for a repo listed in $PRIVATE_REPOS when the
# URL is anonymous (no oauth2: userinfo) and for any repo in $HARD_FAIL_REPOS
# (fails even with a token) — mirroring real Gitea. The SKIP decision in
# clone-manifest.sh is driven by the manifest's `"private": true` flag, NOT by
# which repo the stub fails, so these tests prove the safety boundary:
#
#   A. no token  → public clone, MARKED-private skip, exit 0
#   B. token set → every repo clones, exit 0
#   C. token set + genuine failure → exit 1 (strict)
#   E. no token + a PUBLIC (unmarked) repo hard-fails → exit 1
#        (the key negative case: best-effort must NOT swallow public failures)
#   D. no token + EVERY entry marked private → exit 0 + empty-palette warning
#
# Run:  sh scripts/test-clone-manifest-tolerant.sh
# Exit: 0 all pass, 1 otherwise.

set -eu

HERE=$(dirname -- "$0")
HERE=$(cd -- "$HERE" && pwd)
CLONE_SH="$HERE/clone-manifest.sh"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"

# git stub. Fails clone when:
#   - repo basename is in $HARD_FAIL_REPOS (fails even with a token), OR
#   - repo basename is in $PRIVATE_REPOS and the URL is anonymous.
cat > "$WORK/bin/git" <<'STUB'
#!/bin/sh
[ "$1" = "clone" ] || exit 0
url=""; target=""
for a in "$@"; do
    case "$a" in https://*) url="$a" ;; esac
    target="$a"
done
repo=$(basename "$url" .git)
case " ${HARD_FAIL_REPOS:-} " in *" $repo "*) exit 1 ;; esac
authed=0; case "$url" in *oauth2:*) authed=1 ;; esac
case " ${PRIVATE_REPOS:-} " in
    *" $repo "*) [ "$authed" -eq 1 ] || exit 1 ;;
esac
mkdir -p "$target" && echo stub > "$target/STUB"
exit 0
STUB
chmod +x "$WORK/bin/git"

# Default manifest: 2 public + 1 MARKED-private workspace template.
write_manifest() { cat > "$WORK/manifest.json"; }
write_manifest <<'JSON'
{
  "version": 1, "plugins": [], "org_templates": [],
  "workspace_templates": [
    {"name": "pub-a",  "repo": "molecule-ai/pub-a",  "ref": "main"},
    {"name": "priv-x", "repo": "molecule-ai/priv-x", "ref": "main", "private": true},
    {"name": "pub-b",  "repo": "molecule-ai/pub-b",  "ref": "main"}
  ]
}
JSON

fail() { echo "FAIL: $1"; exit 1; }
# run <env opts/assignments...>  → exit code preserved, output in $OUT.
# env opts (-u) must precede NAME=VALUE assignments (BSD env), so "$@" goes first.
# clone-manifest.sh is run with `bash` (NOT sh): it uses `set -o pipefail`, which
# dash (the CI runner's /bin/sh) rejects — and production runs it via bash too.
run() {
    OUT=$(env "$@" PATH="$WORK/bin:$PATH" \
        bash "$CLONE_SH" "$WORK/manifest.json" "$WORK/ws" "$WORK/org" "$WORK/plugins" 2>&1)
    rc=$?
    printf '%s\n' "$OUT" > "$WORK/last.out"
    return $rc
}
reset() { rm -rf "$WORK/ws" "$WORK/org" "$WORK/plugins"; }

# --- A. no token → marked-private skipped, public cloned, exit 0 ----------
reset
if run -u MOLECULE_GITEA_TOKEN PRIVATE_REPOS="priv-x"; then :; else fail "A: tokenless run should exit 0 (got $?)"; fi
[ -f "$WORK/ws/pub-a/STUB" ] && [ -f "$WORK/ws/pub-b/STUB" ] || fail "A: public repos not cloned"
[ -d "$WORK/ws/priv-x" ] && fail "A: private repo should have been skipped"
echo "$OUT" | grep -q "skipping 'priv-x'" || fail "A: missing skip warning for priv-x"
echo "$OUT" | grep -q "2/3 cloned, 1 skipped" || fail "A: summary wrong: $(echo "$OUT" | tail -1)"
echo "ok A: tokenless → 2 public cloned, 1 marked-private skipped, exit 0"

# --- B. token set → all clone, exit 0 ------------------------------------
reset
if run MOLECULE_GITEA_TOKEN=tok PRIVATE_REPOS="priv-x"; then :; else fail "B: tokened run should exit 0"; fi
[ -f "$WORK/ws/priv-x/STUB" ] || fail "B: private repo should clone with token"
echo "$OUT" | grep -q "3/3 repos cloned successfully" || fail "B: summary wrong: $(echo "$OUT" | tail -1)"
echo "ok B: with token → all 3 cloned, exit 0"

# --- C. token set + genuine failure → exit 1 -----------------------------
reset
if run MOLECULE_GITEA_TOKEN=tok HARD_FAIL_REPOS=pub-b; then
    fail "C: a genuine clone failure with token set must exit 1"
fi
echo "$OUT" | grep -q "genuine failure" || fail "C: missing genuine-failure error"
echo "ok C: with token + real failure → exit 1 (strict preserved)"

# --- E. no token + PUBLIC repo hard-fails → exit 1 (the key boundary) -----
# pub-a clones, priv-x is skipped (marked private), pub-b fails and is NOT
# marked private → must abort. Proves best-effort does NOT swallow a genuine
# public failure (bad ref / deleted repo / outage), even after a success.
reset
if run -u MOLECULE_GITEA_TOKEN PRIVATE_REPOS="priv-x" HARD_FAIL_REPOS=pub-b; then
    fail "E: tokenless run must exit 1 when a PUBLIC repo fails to clone"
fi
echo "$OUT" | grep -q "PUBLIC repo 'pub-b'" || fail "E: missing public-failure error for pub-b"
echo "ok E: tokenless + public hard-fail → exit 1 (no fail-open on public repos)"

# --- D. no token + EVERY entry marked private → exit 0 + warning ----------
write_manifest <<'JSON'
{
  "version": 1, "plugins": [], "org_templates": [],
  "workspace_templates": [
    {"name": "priv-a", "repo": "molecule-ai/priv-a", "ref": "main", "private": true},
    {"name": "priv-b", "repo": "molecule-ai/priv-b", "ref": "main", "private": true}
  ]
}
JSON
reset
if run -u MOLECULE_GITEA_TOKEN PRIVATE_REPOS="priv-a priv-b"; then :; else fail "D: all-private tokenless run must exit 0 (got $?)"; fi
echo "$OUT" | grep -q "EMPTY template palette" || fail "D: missing empty-palette warning"
echo "ok D: tokenless + all-private → exit 0 with empty-palette warning"

echo "PASS: clone-manifest.sh tolerant tokenless bootstrap"
