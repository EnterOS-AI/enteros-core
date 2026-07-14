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
	"save_token(",       // Python SDK: overwrites ~/.molecule/<workspace>/.auth_token
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

// TestShellReshowSnippets_SkipEveryExecutableStep executes each guarded shell
// snippet with a marker token, an empty HOME, and a PATH containing no external
// commands. A credential-only guard is insufficient: installers, mkdir, bridge
// file writes, and runtime starts must also be skipped when the dialog is
// re-shown without a token. With `set -eu`, reaching any executable command is
// an immediate failure; the empty HOME assertion catches built-in redirections.
func TestShellReshowSnippets_SkipEveryExecutableStep(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH: %v", err)
	}
	p := BuildExternalConnectionPayload("https://app.example.com", "ws-abc123", "My Agent", "")
	guarded := []string{
		"claude_code_channel_snippet",
		"universal_mcp_snippet",
		"hermes_channel_snippet",
		"codex_snippet",
		"openclaw_snippet",
		"kimi_snippet",
	}

	for _, key := range guarded {
		t.Run(key, func(t *testing.T) {
			home := t.TempDir()
			snippet := p[key].(string)
			cmd := exec.Command(bash, "-eu")
			cmd.Env = []string{"HOME=" + home, "PATH=/molecule-no-external-commands"}
			cmd.Stdin = strings.NewReader("set -o pipefail\n" + snippet)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("marker-stamped snippet reached an executable step instead of skipping the whole setup: %v\n%s", err, out)
			}
			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("marker-stamped snippet changed HOME; entries=%v", entries)
			}
		})
	}
}

// TestRuntimeInstallFailure_SkipsDependentSetup models an interactive paste
// where mktemp/pip fails. A bare failing command does not stop an interactive
// shell, so each template must explicitly suppress bridge installs, config
// writes, and agent starts, then return non-zero with actionable output.
func TestRuntimeInstallFailure_SkipsDependentSetup(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH: %v", err)
	}
	p := BuildExternalConnectionPayload("https://app.example.com", "ws-abc123", "My Agent", "wst_live_TOKEN")
	runtimeBacked := []string{
		"universal_mcp_snippet",
		"hermes_channel_snippet",
		"codex_snippet",
		"openclaw_snippet",
		"kimi_snippet",
	}

	for _, key := range runtimeBacked {
		t.Run(key, func(t *testing.T) {
			bin := t.TempDir()
			mktempStub := filepath.Join(bin, "mktemp")
			if err := os.WriteFile(mktempStub, []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(bash)
			cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + bin}
			cmd.Stdin = strings.NewReader(p[key].(string))
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("runtime install failure returned success; output:\n%s", out)
			}
			output := string(out)
			if !strings.Contains(output, "runtime install failed; config and agent startup were skipped") {
				t.Fatalf("failure was not actionable or did not prove dependent setup was skipped:\n%s", output)
			}
			if strings.Contains(output, "command not found") {
				t.Fatalf("a dependent command ran after runtime install failed:\n%s", output)
			}
		})
	}
}

// TestPythonSnippet_ReshowRefusesBeforeSavingToken executes the re-show render
// as a script. The SDK's save_token call overwrites the cached credential, so
// the visible marker must abort before that method is reached.
func TestPythonSnippet_ReshowRefusesBeforeSavingToken(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required to execute the operator-facing Python snippet")
	}
	p := BuildExternalConnectionPayload("https://app.example.com", "ws-abc123", "My Agent", "")
	snippet := p["python_snippet"].(string)

	const harness = `
import sys
import types

class FakeClient:
    saved = False
    def __init__(self, workspace_id, **kwargs):
        self.workspace_id = workspace_id
    def save_token(self, token):
        FakeClient.saved = True
    def attach_inbound_server(self, server):
        pass
    def register(self):
        pass
    def load_platform_inbound_secret(self):
        return "inbound-secret"
    def run_heartbeat_loop(self):
        return "stopped"

class FakeServer:
    def __init__(self, **kwargs):
        pass
    def start_in_background(self):
        pass
    def stop(self):
        pass

sdk = types.ModuleType("molecule_external_workspace")
sdk.RemoteAgentClient = FakeClient
sdk.A2AServer = FakeServer
sys.modules["molecule_external_workspace"] = sdk

try:
    exec(compile(sys.stdin.read(), "python_snippet", "exec"), {"__name__": "__main__"})
except RuntimeError as exc:
    if "molecule: this block has NO TOKEN" not in str(exc):
        raise
else:
    raise AssertionError("marker-stamped Python snippet did not refuse to run")

if FakeClient.saved:
    raise AssertionError("marker-stamped Python snippet overwrote the cached token")
`
	cmd := exec.Command(python, "-c", harness)
	cmd.Stdin = strings.NewReader(snippet)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("marker-stamped Python snippet did not refuse before save_token: %v\n%s", err, out)
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
	if !strings.Contains(externalPythonTemplate, tokenGuardNeedle) || !strings.Contains(externalPythonTemplate, tokenGuardSentinel) {
		t.Errorf("the Python snippet's guard must both test for %q and emit %q", tokenGuardNeedle, tokenGuardSentinel)
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

// TestRenderedRuntimeSnippets_StopAfterConfigurationFailure executes the
// operator-facing shell, with installs stubbed successful and a runtime
// configuration/start command stubbed failed. A later help block or
// install-status check must not turn that real failure back into exit 0, and
// no dependent agent/gateway command may run after its configuration failed.
func TestRenderedRuntimeSnippets_StopAfterConfigurationFailure(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not on PATH: %v", err)
	}

	stubDir := t.TempDir()
	const stub = `#!/usr/bin/env bash
name="${0##*/}"
printf '%s %s\n' "$name" "$*" >> "$CALL_LOG"
if [[ "$name $*" == *"$FAIL_NEEDLE"* ]]; then
  exit 42
fi
exit 0
`
	for _, name := range []string{"python3", "npm", "bun", "claude", "hermes", "openclaw", "codex", "codex-channel-molecule"} {
		if err := os.WriteFile(filepath.Join(stubDir, name), []byte(stub), 0o755); err != nil {
			t.Fatalf("write %s stub: %v", name, err)
		}
	}

	payload := BuildExternalConnectionPayload("https://app.example.com", "ws-abc123", "My Agent", "wst_live_TESTTOKEN")
	for _, tc := range []struct {
		name       string
		field      string
		failNeedle string
		mustNotRun []string
	}{
		{name: "Claude channel config", field: "claude_code_channel_snippet", failNeedle: "bun -e", mustNotRun: []string{"claude --dangerously-load-development-channels"}},
		{name: "universal MCP", field: "universal_mcp_snippet", failNeedle: "claude mcp add"},
		{name: "Hermes", field: "hermes_channel_snippet", failNeedle: "hermes gateway --replace"},
		{name: "OpenClaw", field: "openclaw_snippet", failNeedle: "openclaw mcp set", mustNotRun: []string{"openclaw gateway", "openclaw agent"}},
		{name: "OpenClaw agent", field: "openclaw_snippet", failNeedle: "openclaw agent"},
		{name: "Codex agent", field: "codex_snippet", failNeedle: "codex "},
		{name: "Kimi bridge", field: "kimi_snippet", failNeedle: "kimi_bridge.py"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "calls.log")
			snippet, ok := payload[tc.field].(string)
			if !ok {
				t.Fatalf("payload field %s is not a string", tc.field)
			}
			home := t.TempDir()
			if err := os.MkdirAll(filepath.Join(home, ".openclaw"), 0o700); err != nil {
				t.Fatalf("create OpenClaw config dir: %v", err)
			}
			cmd := exec.Command(bash, "-c", snippet)
			cmd.Env = append(os.Environ(),
				"PATH="+stubDir+":"+os.Getenv("PATH"),
				"HOME="+home,
				"CALL_LOG="+logPath,
				"FAIL_NEEDLE="+tc.failNeedle,
			)
			out, runErr := cmd.CombinedOutput()
			if runErr == nil {
				t.Errorf("rendered snippet exited 0 after %s failed; output:\n%s", tc.failNeedle, out)
			}
			calls, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read command log: %v", err)
			}
			if !strings.Contains(string(calls), tc.failNeedle) {
				t.Fatalf("failure command was not reached; calls:\n%s", calls)
			}
			for _, forbidden := range tc.mustNotRun {
				if strings.Contains(string(calls), forbidden) {
					t.Errorf("dependent command %q ran after configuration failed; calls:\n%s", forbidden, calls)
				}
			}
		})
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

// TestKimiSnippet_ShipsTheCurrentBridgeAndTellsTheTruth pins two defects that made
// the kimi snippet quietly lie to the operator.
//
//  1. STALE BRIDGE. The script was written with `[ -f … ] || cat > …`, so once an
//     operator had ANY kimi_bridge.py, no platform fix to it could ever reach them:
//     there was no copy of the current version on their disk to compare against, and
//     nothing told them a newer one existed. It now ships kimi_bridge.py.dist EVERY
//     run and installs it only when absent — edits survive AND upgrades are visible.
//
//  2. TAUTOLOGICAL MESSAGE. `[ -s …/kimi_bridge.py ] && echo "already exists — left
//     untouched"` sat AFTER the line that creates the file, so the file always
//     existed by the time the test ran. It printed "already exists — left untouched"
//     on the FIRST run too, about a file it had just written. A message that is true
//     no matter what happened conveys nothing about what happened.
func TestKimiSnippet_ShipsTheCurrentBridgeAndTellsTheTruth(t *testing.T) {
	p := BuildExternalConnectionPayload("https://app.example.com", "ws-abc123", "My Agent", "wst_live_TESTTOKEN")
	kimi, _ := p["kimi_snippet"].(string)

	if strings.Contains(kimi, "] || cat > ") {
		t.Error("the kimi bridge script is back to `[ -f … ] || cat >`: an operator who already " +
			"has a kimi_bridge.py can then NEVER receive a platform fix to it, and has no copy of " +
			"the current version to diff against")
	}
	if !strings.Contains(kimi, "kimi_bridge.py.dist") {
		t.Error("the kimi snippet no longer ships kimi_bridge.py.dist — the operator's edits are " +
			"preserved but upgrades become invisible again")
	}
	if !strings.Contains(kimi, "cmp -s") {
		t.Error("the kimi snippet must COMPARE the installed bridge against the shipped one; " +
			"without that it cannot tell the operator whether theirs is current")
	}

	// The message must distinguish the three real cases. If it cannot be false, it is
	// not information.
	for _, want := range []string{
		"installed kimi_bridge.py", // first run
		"is current — unchanged",   // re-run, unmodified
		"kept YOUR kimi_bridge.py", // re-run, operator edited it
	} {
		if !strings.Contains(kimi, want) {
			t.Errorf("the kimi snippet cannot report the %q case — the old message was true in "+
				"every case and therefore told the operator nothing", want)
		}
	}
	if strings.Contains(kimi, "already exists — left untouched") {
		t.Error("the tautological echo is back: it sits after the file is created, so it fires " +
			"even on a first run about a file it just wrote")
	}

	// Duplicate-bridge warning. The config dir was re-keyed from the workspace-NAME
	// slug to the WORKSPACE_ID, so an operator who set up before that change still has
	// a bridge running out of a directory this snippet never mentions. Re-running would
	// leave them with TWO bridges long-polling one inbox — every inbound message
	// processed twice, the user answered twice.
	if !strings.Contains(kimi, "pgrep -f") || !strings.Contains(kimi, "pkill -f kimi_bridge.py") {
		t.Error("the kimi snippet must detect an already-running bridge and tell the operator to " +
			"stop it: after the config-dir re-key, an orphaned bridge from the OLD name-slug " +
			"directory keeps polling, and a second bridge double-processes every inbound message")
	}
}

// TestKimiSnippet_CursorPollingIsChronological executes the Python bridge
// extracted from the rendered operator snippet. GET /activity returns a
// newest-first feed when since_secs is used without a cursor, but cursor reads
// are oldest-first. The bridge must normalize only the cold-start response and
// leave since_secs off steady-state cursor requests; otherwise it replies in
// reverse, persists the oldest id, replays newer rows, and can lose rows after
// a poll outage longer than the time window.
func TestKimiSnippet_CursorPollingIsChronological(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required to execute the rendered Kimi bridge")
	}

	p := BuildExternalConnectionPayload(
		"https://app.example.com",
		"ws-abc123",
		"My Agent",
		"wst_live_TESTTOKEN",
	)
	kimi, _ := p["kimi_snippet"].(string)
	const open = "<<'PYEOF'\n"
	const close = "\nPYEOF\n"
	start := strings.Index(kimi, open)
	if start < 0 {
		t.Fatal("rendered Kimi snippet no longer contains the bridge PYEOF heredoc")
	}
	rest := kimi[start+len(open):]
	end := strings.Index(rest, close)
	if end < 0 {
		t.Fatal("rendered Kimi bridge PYEOF heredoc is unterminated")
	}
	bridge := rest[:end]

	const harness = `
import sys
import types

httpx = types.ModuleType("httpx")
httpx.Client = object
sys.modules["httpx"] = httpx

namespace = {"__name__": "kimi_bridge_test"}
exec(compile(sys.stdin.read(), "kimi_bridge.py", "exec"), namespace)

class Response:
    def raise_for_status(self):
        pass
    def json(self):
        return [
            {"id": "newest"},
            {"id": "middle"},
            {"id": "oldest"},
        ]

class Client:
    def __init__(self):
        self.params = None
    def get(self, url, params, headers):
        self.params = params
        return Response()

client = Client()
cold = namespace["poll_inbound"](client, "https://app.example.com", "ws-abc123", "token", "")
if client.params.get("since_secs") != "30" or "since_id" in client.params:
    raise AssertionError(f"cold-start params are not bounded since_secs-only: {client.params!r}")
ordered = namespace["order_activity_items"](cold, "")
ids = [item["id"] for item in ordered]
if ids != ["oldest", "middle", "newest"]:
    raise AssertionError(f"cold-start order is not chronological: {ids!r}")
cursor = ""
for item in ordered:
    cursor = item["id"]
if cursor != "newest":
    raise AssertionError(f"cold-start persisted cursor would be {cursor!r}, want newest")

steady_rows = [
    {"id": "oldest"},
    {"id": "middle"},
    {"id": "newest"},
]
namespace["poll_inbound"](client, "https://app.example.com", "ws-abc123", "token", "middle")
if client.params.get("since_id") != "middle" or "since_secs" in client.params:
    raise AssertionError(f"steady-state params are not cursor-only: {client.params!r}")
steady = namespace["order_activity_items"](steady_rows, "middle")
if [item["id"] for item in steady] != ["oldest", "middle", "newest"]:
    raise AssertionError("cursor response order was changed")
`

	cmd := exec.Command(python, "-c", harness)
	cmd.Stdin = strings.NewReader(bridge)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rendered Kimi bridge violates cursor polling semantics: %v\n%s", err, out)
	}
}
