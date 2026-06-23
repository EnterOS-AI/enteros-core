#!/bin/sh
# test-clone-manifest-tolerant.sh — local, network-free test of the
# tokenless graceful-degradation behavior in clone-manifest.sh.
#
# Stubs `git` so a "private" repo clone fails when the URL is anonymous
# (no oauth2: userinfo) and succeeds when a token is embedded — mirroring
# real Gitea. Asserts:
#   A. no token  → public repos clone, private repos SKIP, exit 0
#   B. token set → every repo clones, exit 0
#   C. token set + a repo that fails even authed → exit 1 (genuine failure)
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
#   - repo basename is in $PRIVATE_REPOS and the URL is anonymous, OR
#   - repo basename is in $HARD_FAIL_REPOS (fails even with a token).
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

# manifest: 2 public + 1 private workspace template
cat > "$WORK/manifest.json" <<'JSON'
{
  "version": 1, "plugins": [], "org_templates": [],
  "workspace_templates": [
    {"name": "pub-a",   "repo": "molecule-ai/pub-a",   "ref": "main"},
    {"name": "priv-x",  "repo": "molecule-ai/priv-x",  "ref": "main"},
    {"name": "pub-b",   "repo": "molecule-ai/pub-b",   "ref": "main"}
  ]
}
JSON

fail() { echo "FAIL: $1"; exit 1; }
run() {  # run <env-opts/assignments...>  (returns exit code, captures $OUT)
    # NOTE: env opts (-u) must precede NAME=VALUE assignments (BSD env),
    # so caller args go FIRST.
    OUT=$(env "$@" PATH="$WORK/bin:$PATH" PRIVATE_REPOS="priv-x" \
        sh "$CLONE_SH" "$WORK/manifest.json" "$WORK/ws" "$WORK/org" "$WORK/plugins" 2>&1)
    rc=$?
    printf '%s\n' "$OUT" > "$WORK/last.out"
    return $rc
}
reset() { rm -rf "$WORK/ws" "$WORK/org" "$WORK/plugins"; }

# --- A. no token → private skipped, public cloned, exit 0 ----------------
reset
if run -u MOLECULE_GITEA_TOKEN; then :; else fail "A: tokenless run should exit 0 (got non-zero)"; fi
[ -f "$WORK/ws/pub-a/STUB" ] && [ -f "$WORK/ws/pub-b/STUB" ] || fail "A: public repos not cloned"
[ -d "$WORK/ws/priv-x" ] && fail "A: private repo should have been skipped (dir present)"
echo "$OUT" | grep -q "skipping 'priv-x'" || fail "A: missing skip warning for priv-x"
echo "$OUT" | grep -q "2/3 cloned, 1 skipped" || fail "A: summary line wrong: $(echo "$OUT" | tail -1)"
echo "ok A: tokenless → 2 public cloned, 1 private skipped, exit 0"

# --- B. token set → all clone, exit 0 ------------------------------------
reset
if run MOLECULE_GITEA_TOKEN=tok; then :; else fail "B: tokened run should exit 0"; fi
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

# --- D. no token + ALL repos private → exit 0 with empty-palette warning -
# (every repo skipped; must NOT block bootstrap — setup.sh tolerates an
#  empty palette. Mark every repo private via PRIVATE_REPOS override.)
reset
OUT=$(env -u MOLECULE_GITEA_TOKEN PATH="$WORK/bin:$PATH" PRIVATE_REPOS="pub-a priv-x pub-b" \
    sh "$CLONE_SH" "$WORK/manifest.json" "$WORK/ws" "$WORK/org" "$WORK/plugins" 2>&1) \
    && rc=0 || rc=$?
[ "${rc:-0}" -eq 0 ] || fail "D: all-private tokenless run must exit 0 (got $rc)"
echo "$OUT" | grep -q "EMPTY template palette" || fail "D: missing empty-palette warning"
echo "ok D: tokenless + all-private → exit 0 with empty-palette warning"

echo "PASS: clone-manifest.sh tolerant tokenless bootstrap"
