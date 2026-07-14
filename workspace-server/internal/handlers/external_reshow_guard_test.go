package handlers

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The re-show path (GET .../external/connection) returns auth_token="", and
// BuildExternalConnectionPayload stamps tokenUnavailableMarker at every
// credential site. The pre-existing gate for that
// (TestBuildExternalConnectionPayload_BlankTokenStaysVisiblyIncomplete) only
// asserts the marker is PRESENT — i.e. that the snippet is "visibly
// non-runnable". That is true only of snippets which use the token INLINE: run
// a curl with a placeholder bearer and it 401s, nothing is lost.
//
// It is FALSE, and dangerously so, for every snippet that PERSISTS the token.
// `claude mcp add`, `openclaw mcp set`, the channel .env merge, the kimi env
// file, `hermes gateway --replace` all happily accept the marker as if it were
// a credential and REPLACE the operator's working one with a dead string. The
// token is displayed exactly once, so the real one is then unrecoverable: the
// only way back is a rotation.
//
// So each such snippet carries a refusal guard, and these tests pin it.

// credentialPersistVerbs — command shapes that write a credential to disk or
// into a client's config store, or that restart a live client against whatever
// credential it was just handed. Running any of them with the marker degrades a
// working setup; they are exactly the shapes that must sit behind a guard.
var credentialPersistVerbs = []string{
	"writeFileSync",     // channel .env merge (bun)
	"renameSync",        // ...and its atomic swap
	"claude mcp add",    // OVERWRITES the entry for this server name
	"openclaw mcp set",  // OVERWRITES ~/.openclaw/mcp/<name>.json
	"cat > ",            // kimi env file (heredoc)
	"tee ",              // any redirect-free write
	"gateway --replace", // hermes: kills the running gateway, restarts on the new token
}

// snippetPersistsCredential reports whether a rendered snippet contains a
// credential-persisting command on a line that actually EXECUTES — comment
// lines are prose (several snippets *describe* `claude mcp add` in their
// troubleshooting section).
func snippetPersistsCredential(snippet string) (string, bool) {
	for _, line := range strings.Split(snippet, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "//") {
			continue
		}
		for _, verb := range credentialPersistVerbs {
			if strings.Contains(line, verb) {
				return verb, true
			}
		}
	}
	return "", false
}

// TestReshowSnippets_RefuseToRunWithoutAToken is the invariant: on the re-show
// path, every snippet not explicitly exempted must carry a refusal guard.
//
// It greps for tokenGuardSentinel, NOT for tokenGuardNeedle: the marker
// "<ROTATE_TO_REVEAL_TOKEN>" itself CONTAINS the needle, so a needle-presence
// assertion would pass vacuously on a completely unguarded snippet — the same
// class of mistake (checking that a string exists rather than that it does
// anything) that let the original defect ship.
func TestReshowSnippets_RefuseToRunWithoutAToken(t *testing.T) {
	p := BuildExternalConnectionPayload("https://app.example.com", "ws-abc123", "My Agent", "")

	for key := range externalSnippetTemplates {
		snippet, _ := p[key].(string)
		if !strings.Contains(snippet, tokenUnavailableMarker) {
			t.Fatalf("%s: re-show render lost the marker — the premise of this whole gate", key)
		}

		if _, exempt := snippetsExemptFromTokenGuard[key]; exempt {
			// An exemption is the claim "a dead token here destroys nothing".
			// Hold it to that claim: it may not persist a credential.
			if verb, persists := snippetPersistsCredential(snippet); persists {
				t.Errorf("%s is listed in snippetsExemptFromTokenGuard but executes %q — that "+
					"REPLACES the operator's stored credential with %s. The exemption is a lie; "+
					"give the snippet a guard instead of exempting it.",
					key, verb, tokenUnavailableMarker)
			}
			continue
		}

		if !strings.Contains(snippet, tokenGuardSentinel) {
			t.Errorf("%s renders on the re-show path with NO refusal guard (expected the "+
				"sentinel %q). Pasting it would run every step with %s substituted for the "+
				"credential — and the token is shown ONCE, so a snippet that writes it "+
				"(claude mcp add / openclaw mcp set / the channel .env merge / the kimi env "+
				"file / hermes gateway --replace) destroys the operator's only copy. Prepend "+
				"tokenGuardShell and wrap the persisting commands in "+
				"[ \"$MOLECULE_TOKEN_OK\" = \"1\" ], or add the key to "+
				"snippetsExemptFromTokenGuard with the reason it cannot destroy anything.",
				key, tokenGuardSentinel, tokenUnavailableMarker)
		}
	}

	// The exemption list must not name snippets that no longer exist — a stale
	// key silently exempts nothing today and the WRONG thing after a rename.
	for key := range snippetsExemptFromTokenGuard {
		if _, ok := externalSnippetTemplates[key]; !ok {
			t.Errorf("snippetsExemptFromTokenGuard names %q, which is not in "+
				"externalSnippetTemplates — drop the stale exemption", key)
		}
	}
}

// TestTokenGuards_MatchTheMarkerActuallyStamped closes the drift between the
// guard's needle and the marker it is supposed to catch: change
// tokenUnavailableMarker without changing the guards and every guard becomes a
// no-op that still passes the sentinel check above.
func TestTokenGuards_MatchTheMarkerActuallyStamped(t *testing.T) {
	if !strings.Contains(tokenUnavailableMarker, tokenGuardNeedle) {
		t.Fatalf("tokenGuardNeedle %q does not appear in tokenUnavailableMarker %q — every "+
			"refusal guard matches on the needle, so the guards no longer fire on the marker "+
			"the server actually stamps. They are all dead code.",
			tokenGuardNeedle, tokenUnavailableMarker)
	}
	if !strings.Contains(tokenGuardShell, tokenGuardNeedle) || !strings.Contains(tokenGuardShell, tokenGuardSentinel) {
		t.Errorf("tokenGuardShell must both test for %q and emit %q", tokenGuardNeedle, tokenGuardSentinel)
	}
	if !strings.Contains(externalChannelTemplate, tokenGuardNeedle) || !strings.Contains(externalChannelTemplate, tokenGuardSentinel) {
		t.Errorf("the channel snippet's JS guard must both test for %q and emit %q", tokenGuardNeedle, tokenGuardSentinel)
	}
}

// TestShellSnippets_ParseUnderBash runs `bash -n` over every shell snippet.
//
// The refusal guards are `if [ "$MOLECULE_TOKEN_OK" = "1" ]; then … fi` blocks
// wrapped around commands deep inside long templates, and an unbalanced `fi` is
// invisible to every other test in this package: the payload still builds, the
// substring gates still pass, and the operator pastes a block that dies with a
// bash syntax error. Parse it.
func TestShellSnippets_ParseUnderBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH: %v", err)
	}
	// Both renders: the token is substituted INTO the guard's case statement, so
	// a quoting bug can be present on one path and not the other.
	for _, tc := range []struct{ name, token string }{
		{"reshow", ""},
		{"live", "wst_live_TESTTOKEN=="},
	} {
		p := BuildExternalConnectionPayload("https://app.example.com", "ws-abc123", "My Agent", tc.token)
		for key := range externalSnippetTemplates {
			if key == "python_snippet" {
				continue // python source, not shell
			}
			snippet, _ := p[key].(string)
			cmd := exec.Command(bash, "-n")
			cmd.Stdin = strings.NewReader(snippet)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("%s (%s render) is not valid bash — the operator's paste would die with a "+
					"syntax error:\n%s", key, tc.name, out)
			}
		}
	}
}

// channelMergeScript extracts the bun/node script the channel snippet pipes
// into `bun -e` so the test can EXECUTE it. Pulling it out of the rendered
// snippet (not a copy) is the point: a copy would drift, and a drifted copy is
// how #79 shipped.
func channelMergeScript(t *testing.T, snippet string) string {
	t.Helper()
	const open = "<<'JS'\n"
	const close = "\nJS\n"
	i := strings.Index(snippet, open)
	if i < 0 {
		t.Fatalf("channel snippet no longer embeds a <<'JS' heredoc — this test extracts the "+
			"merge script from it; re-point the extractor at the new shape rather than deleting "+
			"the test.\nsnippet:\n%s", snippet)
	}
	rest := snippet[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("channel snippet's <<'JS' heredoc is unterminated")
	}
	return rest[:j]
}

// runChannelMerge executes the extracted merge script under node with HOME
// pointed at a scratch dir, and returns the resulting .env contents.
func runChannelMerge(t *testing.T, home, entry string) (stderr string, exitCode int) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH: the channel merge script is a bun/node script and this test "+
			"EXECUTES it (a substring assertion cannot tell a merge from a clobber). Install node "+
			"to run it. (%v)", err)
	}

	p := BuildExternalConnectionPayload("https://app.example.com", "ws-abc123", "My Agent", "")
	script := channelMergeScript(t, p["claude_code_channel_snippet"].(string))

	cmd := exec.Command(node, "-e", script)
	cmd.Env = append(os.Environ(), "HOME="+home, "MOLECULE_WS_ENTRY="+entry)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	cmd.Stdout = &strings.Builder{}
	err = cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		exitCode = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running the channel merge script: %v (stderr: %s)", err, errBuf.String())
	}
	return errBuf.String(), exitCode
}

// envPath / seedEnv / readEntries — the on-disk shape the channel plugin reads.
func envPath(home string) string {
	return filepath.Join(home, ".claude", "channels", "molecule", ".env")
}

func seedEnv(t *testing.T, home string, entries []map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(envPath(home)), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath(home), []byte("MOLECULE_WORKSPACES_JSON="+string(b)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readEntries(t *testing.T, home string) []map[string]string {
	t.Helper()
	b, err := os.ReadFile(envPath(home))
	if err != nil {
		t.Fatalf("read %s: %v", envPath(home), err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		const key = "MOLECULE_WORKSPACES_JSON="
		if !strings.HasPrefix(line, key) {
			continue
		}
		var out []map[string]string
		if err := json.Unmarshal([]byte(line[len(key):]), &out); err != nil {
			t.Fatalf("MOLECULE_WORKSPACES_JSON is not a JSON array: %v (line: %s)", err, line)
		}
		return out
	}
	t.Fatalf("no MOLECULE_WORKSPACES_JSON line in %s:\n%s", envPath(home), b)
	return nil
}

// TestChannelMergeScript_RefusesTheMarkerAndLeavesDiskIntact is THE regression
// test for the defect: the operator has a working workspace connected, reopens
// the modal (re-show → auth_token="" → marker), and pastes the block again.
//
// Before the guard, the script filtered their live entry out of the array and
// pushed {"token":"<ROTATE_TO_REVEAL_TOKEN>"} in its place — destroying a
// shown-once credential. It must instead exit non-zero and touch nothing.
func TestChannelMergeScript_RefusesTheMarkerAndLeavesDiskIntact(t *testing.T) {
	home := t.TempDir()
	live := []map[string]string{{
		"id":           "ws-abc123",
		"token":        "wst_live_REAL_TOKEN",
		"platform_url": "https://app.example.com",
	}}
	seedEnv(t, home, live)
	before, err := os.ReadFile(envPath(home))
	if err != nil {
		t.Fatal(err)
	}

	// The re-show render: BuildExternalConnectionPayload stamps the marker into
	// MOLECULE_WS_ENTRY. Same entry the operator's paste would carry.
	entry := `{"id":"ws-abc123","token":"` + tokenUnavailableMarker + `","platform_url":"https://app.example.com"}`
	stderr, code := runChannelMerge(t, home, entry)

	if code == 0 {
		t.Errorf("the merge script ACCEPTED %s as a credential (exit 0). On the re-show path "+
			"that overwrites the operator's live, shown-once token with a dead string and the "+
			"only recovery is a rotation.", tokenUnavailableMarker)
	}
	if !strings.Contains(stderr, tokenGuardSentinel) {
		t.Errorf("refusal did not explain itself: stderr must contain %q, got %q", tokenGuardSentinel, stderr)
	}

	after, err := os.ReadFile(envPath(home))
	if err != nil {
		t.Fatalf("the refusing script DELETED the .env: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf(".env was modified by a refusing run.\nbefore: %s\nafter:  %s", before, after)
	}
	if got := readEntries(t, home); len(got) != 1 || got[0]["token"] != "wst_live_REAL_TOKEN" {
		t.Errorf("the operator's live token is gone: %+v", got)
	}
	if _, err := os.Stat(envPath(home) + ".tmp"); err == nil {
		t.Errorf("the refusing run left a .env.tmp behind")
	}
}

// TestChannelMergeScript_IsAppendSafeAndIdempotent executes the merge with a
// REAL token. It is the behavioural half of
// TestExternalChannelTemplate_ConfigWriteIsAppendSafe, which only greps for the
// substrings "arr.filter" / "arr.push" — that grep still passes if the filter
// predicate is INVERTED (dropping every OTHER workspace instead of this one),
// which is precisely the data-loss bug it exists to prevent.
func TestChannelMergeScript_IsAppendSafeAndIdempotent(t *testing.T) {
	home := t.TempDir()

	// An unrelated workspace the operator connected earlier, on another tenant.
	other := map[string]string{
		"id":           "ws-other",
		"token":        "wst_other_TOKEN",
		"platform_url": "https://other.example.com",
	}
	seedEnv(t, home, []map[string]string{other})

	entry := `{"id":"ws-abc123","token":"wst_first_TOKEN","platform_url":"https://app.example.com"}`
	if _, code := runChannelMerge(t, home, entry); code != 0 {
		t.Fatalf("merge with a real token exited %d, want 0", code)
	}

	got := readEntries(t, home)
	if len(got) != 2 {
		t.Fatalf("append-safety: want 2 entries (the pre-existing one + this workspace), got %d: %+v — "+
			"a snippet run for workspace B must not evict workspace A, whose token was shown once", len(got), got)
	}
	if !hasEntry(got, "ws-other", "wst_other_TOKEN") {
		t.Errorf("the PRE-EXISTING workspace was evicted: %+v — this is the inverted-filter "+
			"data-loss bug (arr.filter keeping only the matching entry instead of dropping it)", got)
	}
	if !hasEntry(got, "ws-abc123", "wst_first_TOKEN") {
		t.Errorf("this workspace was not added: %+v", got)
	}

	// Re-running for the SAME workspace after a rotate must REPLACE its entry in
	// place — not duplicate it (the plugin would then authenticate with whichever
	// it hit first, which is the stale one half the time).
	rotated := `{"id":"ws-abc123","token":"wst_rotated_TOKEN","platform_url":"https://app.example.com"}`
	if _, code := runChannelMerge(t, home, rotated); code != 0 {
		t.Fatalf("re-merge after rotate exited %d, want 0", code)
	}

	got = readEntries(t, home)
	if len(got) != 2 {
		t.Errorf("rotate must replace this workspace's entry in place, not duplicate it: %+v", got)
	}
	if !hasEntry(got, "ws-abc123", "wst_rotated_TOKEN") {
		t.Errorf("rotate did not refresh the token: %+v", got)
	}
	if hasEntry(got, "ws-abc123", "wst_first_TOKEN") {
		t.Errorf("the pre-rotate token is still on disk: %+v", got)
	}
	if !hasEntry(got, "ws-other", "wst_other_TOKEN") {
		t.Errorf("the unrelated workspace was evicted by the rotate re-run: %+v", got)
	}
}

func hasEntry(entries []map[string]string, id, token string) bool {
	for _, e := range entries {
		if e["id"] == id && e["token"] == token {
			return true
		}
	}
	return false
}
