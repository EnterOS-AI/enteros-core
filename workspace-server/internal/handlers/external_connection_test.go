package handlers

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestExternalTemplates_NoMoleculeOrgIDPlaceholder pins the invariant
// that operator-facing connection snippets do NOT advertise a
// MOLECULE_ORG_ID env var.
//
// Why: MOLECULE_ORG_ID is consumed only by the workspace-server's
// TenantGuard middleware (server-side, set by control plane via
// user-data on tenant boxes). The molecule_runtime MCP subprocess
// that codex/openclaw/hermes-channel spawns authenticates the client
// using Origin + Bearer token + X-Workspace-ID — it never reads
// MOLECULE_ORG_ID. Including the placeholder leaves operators with a
// "<your org id>" they can't fill, and external agents (codex CLI in
// particular) flag it as an unresolved setup blocker.
//
// The universal_mcp snippet is the reference: it calls into the same
// molecule_runtime and intentionally omits MOLECULE_ORG_ID.
// Ranges over externalSnippetTemplates, not a hand-kept map: the old map
// silently omitted the channel and kimi templates, so a snippet could be
// added to the product and skip this lint entirely. Registry membership is
// the enrolment mechanism — add a snippet, inherit every gate.
func TestExternalTemplates_NoMoleculeOrgIDPlaceholder(t *testing.T) {
	for name, body := range externalSnippetTemplates {
		if strings.Contains(body, "MOLECULE_ORG_ID") {
			t.Errorf("%s contains MOLECULE_ORG_ID — operator-facing templates must not advertise this env var (TenantGuard reads it server-side from the tenant's own env, not the client)", name)
		}
		if strings.Contains(body, "<your org id>") {
			t.Errorf("%s contains \"<your org id>\" placeholder — operators have no value to substitute, drop the line", name)
		}
	}
}

// TestExternalMcpTemplates_UseMoleculeMcpWrapper pins the invariant
// that operator-facing snippets configuring an MCP server entry point
// use the “molecule-mcp“ console-script wrapper (mcp_cli.main),
// NOT the bare “a2a_mcp_server“ module.
//
// Why: a2a_mcp_server exposes the MCP tools but does NOT call
// /registry/register or run the 20s heartbeat thread. mcp_cli wraps
// it with both, which is what flips the canvas presence indicator
// from awaiting_agent (OFFLINE) to online and keeps it that way.
// Originally tracked by molecule-core#2957 — operator hit the
// silent-OFFLINE failure mode when the Codex tab pointed at the bare
// module.
//
// The hermes-channel template intentionally uses the bare module: it
// owns the platform plugin path and runs its own
// register_platform/heartbeat code in-process, so wrapping with
// mcp_cli would double-heartbeat. universalMcp / codex / openclaw
// must all use the wrapper.
func TestExternalMcpTemplates_UseMoleculeMcpWrapper(t *testing.T) {
	mustUseWrapper := map[string]string{
		"externalUniversalMcpTemplate": externalUniversalMcpTemplate,
		"externalCodexTemplate":        externalCodexTemplate,
		"externalOpenClawTemplate":     externalOpenClawTemplate,
	}
	for name, body := range mustUseWrapper {
		if !strings.Contains(body, "molecule-mcp") {
			t.Errorf("%s does not reference 'molecule-mcp' — operator-facing MCP snippets must point at the heartbeat-wrapping console script, not the bare a2a_mcp_server module (#2957)", name)
		}
		if strings.Contains(body, `"-m", "molecule_runtime.a2a_mcp_server"`) {
			t.Errorf("%s spawns 'python3 -m molecule_runtime.a2a_mcp_server' — that bypasses the standalone register/heartbeat wrapper, leaving the canvas showing the workspace OFFLINE (#2957). Use 'molecule-mcp' instead.", name)
		}
		if strings.Contains(body, `["-m", "molecule_runtime.a2a_mcp_server"]`) {
			t.Errorf("%s spawns 'python3 -m molecule_runtime.a2a_mcp_server' — that bypasses the standalone register/heartbeat wrapper, leaving the canvas showing the workspace OFFLINE (#2957). Use 'molecule-mcp' instead.", name)
		}
	}
}

// TestExternalTemplates_NoBrokenMoleculeAIGitHubURLs pins the invariant
// that operator-facing snippets never embed github.com URLs pointing at
// Molecule-AI repos.
//
// Why: the Molecule-AI GitHub org was suspended 2026-05-06 and the
// canonical SCM is now git.moleculesai.app. Any `pip install
// git+https://github.com/Molecule-AI/...` or marketplace-add Molecule-AI/
// URL emitted to an external operator hits a 404 / org-suspended page,
// breaking onboarding silently. RFC #229 P2-5.
//
// Third-party github URLs (gin, openai/codex, NousResearch/hermes-agent
// upstream issue trackers, npm @openai/codex) remain valid — only
// Molecule-AI/ paths are broken.
// Registry-driven for the same reason as the MOLECULE_ORG_ID lint above:
// the hand-kept map omitted the kimi template.
func TestExternalTemplates_NoBrokenMoleculeAIGitHubURLs(t *testing.T) {
	templates := externalSnippetTemplates
	// Substrings that imply the snippet is pointing an operator at the
	// suspended Molecule-AI GitHub org.
	bannedSubstrings := []string{
		"github.com/Molecule-AI/",
		"github.com/molecule-ai/",
		// Bare `Molecule-AI/<repo>` form used by `/plugin marketplace add`
		// resolves through GitHub by default — explicit Gitea URL is
		// required post-suspension.
		"marketplace add Molecule-AI/",
		"marketplace add molecule-ai/",
	}
	for name, body := range templates {
		for _, banned := range bannedSubstrings {
			if strings.Contains(body, banned) {
				t.Errorf("%s contains %q — Molecule-AI GitHub org is suspended; use git.moleculesai.app/molecule-ai/<repo> instead (RFC #229 P2-5)", name, banned)
			}
		}
	}
}

// TestExternalRuntimeTemplates_InstallPrivateWheelWithPublicDependencies pins
// the copy-paste install boundary for every snippet backed by
// molecules-workspace-runtime. The Gitea package registry does not proxy
// public PyPI, so using it as pip's sole index makes a clean install fail while
// resolving dependencies such as a2a-sdk. Download the pinned private wheel
// without dependencies, then install that local artifact with public PyPI as
// the only resolver index. This also avoids an unrestricted --extra-index-url
// and its dependency-confusion ambiguity for the private package name.
func TestExternalRuntimeTemplates_InstallPrivateWheelWithPublicDependencies(t *testing.T) {
	runtimeTemplates := map[string]string{
		"externalUniversalMcpTemplate":  externalUniversalMcpTemplate,
		"externalHermesChannelTemplate": externalHermesChannelTemplate,
		"externalCodexTemplate":         externalCodexTemplate,
		"externalOpenClawTemplate":      externalOpenClawTemplate,
		"externalKimiTemplate":          externalKimiTemplate,
	}

	for name, body := range runtimeTemplates {
		for _, required := range []string{
			"pip download --no-deps",
			"molecules-workspace-runtime==0.4.36",
			"pip install --index-url https://pypi.org/simple/",
			"molecules_workspace_runtime-*.whl",
		} {
			if !strings.Contains(body, required) {
				t.Errorf("%s safe pinned runtime install is missing %q", name, required)
			}
		}
		if strings.Contains(body, "pip install --index-url https://git.moleculesai.app/api/packages/molecule-ai/pypi/simple/ molecule-ai-workspace-runtime") ||
			strings.Contains(body, "pip install --index-url https://git.moleculesai.app/api/packages/molecule-ai/pypi/simple/ \"molecule-ai-workspace-runtime") {
			t.Errorf("%s uses the private registry as pip's sole dependency index; clean installs cannot resolve public runtime dependencies", name)
		}
	}

	bridgeInstalls := map[string]struct {
		body string
		want string
	}{
		"externalCodexTemplate": {
			body: externalCodexTemplate,
			want: "python3 -m pip install --no-deps 'git+https://git.moleculesai.app/molecule-ai/codex-channel-molecule.git@876e91c46e1ce240cdaf96a720a2864c23bf52a0'",
		},
		"externalHermesChannelTemplate": {
			body: externalHermesChannelTemplate,
			want: "python3 -m pip install --no-deps 'git+https://git.moleculesai.app/molecule-ai/hermes-channel-molecule.git@d9028c6690394390f3a2b8211a6f6cdc3681971c'",
		},
	}
	for name, tc := range bridgeInstalls {
		if !strings.Contains(tc.body, tc.want) {
			t.Errorf("%s must install its bridge from the verified pinned Gitea commit with --no-deps; want %q", name, tc.want)
		}
	}
	if strings.Contains(externalCodexTemplate, "pip install codex-channel-molecule") {
		t.Error("externalCodexTemplate advertises a nonexistent public codex-channel-molecule distribution")
	}
}

// TestExternalOpenClawTemplate_UsesOneProfileAndCurrentConfigInspection keeps
// every OpenClaw command on the same default profile. `--dev` switches the
// gateway to ~/.openclaw-dev, where it cannot see the MCP server that the
// preceding default-profile `openclaw mcp set` wrote. OpenClaw also stores MCP
// servers inside openclaw.json; there is no per-server mcp/<name>.json file for
// operators to inspect.
func TestExternalOpenClawTemplate_UsesOneProfileAndCurrentConfigInspection(t *testing.T) {
	snippet := BuildExternalConnectionPayload(
		"https://app.example.com", "ws-abc123", "My Agent", "wst_real_TOKEN",
	)["openclaw_snippet"].(string)

	for _, stale := range []string{
		"openclaw gateway --dev",
		"~/.openclaw/mcp/",
	} {
		if strings.Contains(snippet, stale) {
			t.Errorf("OpenClaw setup contains stale profile/config guidance %q", stale)
		}
	}

	for _, required := range []string{
		"nohup openclaw gateway --port 18789 --bind loopback",
		"openclaw mcp show molecule-my-agent --json",
		"~/.openclaw/openclaw.json",
	} {
		if !strings.Contains(snippet, required) {
			t.Errorf("OpenClaw setup is missing current default-profile guidance %q", required)
		}
	}
}

// TestExternalPythonTemplate_UsesPublishedSDKTokenAPI keeps the canvas-served
// Python example aligned with the SDK that operators actually install from the
// Gitea package registry. The published RemoteAgentClient does not accept
// auth_token in its constructor; an existing workspace token must be persisted
// with save_token before register sends its authenticated announcement.
func TestExternalPythonTemplate_UsesPublishedSDKTokenAPI(t *testing.T) {
	if strings.Contains(externalPythonTemplate, "git+https://git.moleculesai.app/") {
		t.Error("externalPythonTemplate installs SDK source from git+main; use the published molecule-ai-sdk package so the copy-paste path is reproducible")
	}
	if strings.Contains(externalPythonTemplate, "pip install molecule-ai-sdk --index-url") {
		t.Error("externalPythonTemplate uses the private index as the sole install index, which cannot resolve the SDK's public dependencies")
	}
	if !strings.Contains(externalPythonTemplate, "molecule-ai-sdk") ||
		!strings.Contains(externalPythonTemplate, "api/packages/molecule-ai/pypi/simple/") {
		t.Error("externalPythonTemplate must install molecule-ai-sdk from the Gitea PyPI registry")
	}
	for _, required := range []string{
		"pip download --no-deps",
		"molecule-ai-sdk==0.5.2",
		"pip install --index-url https://pypi.org/simple/",
		"molecule_ai_sdk-*.whl",
	} {
		if !strings.Contains(externalPythonTemplate, required) {
			t.Errorf("externalPythonTemplate safe pinned package install is missing %q", required)
		}
	}
	if strings.Contains(externalPythonTemplate, "pypi/simple/molecule-ai-workspace-runtime/") ||
		!strings.Contains(externalPythonTemplate, "pypi/simple/molecule-ai-sdk/") {
		t.Error("externalPythonTemplate help must link to the molecule-ai-sdk package index, not the workspace-runtime package")
	}
	if strings.Contains(externalPythonTemplate, "auth_token=AUTH_TOKEN") {
		t.Error("externalPythonTemplate passes auth_token to RemoteAgentClient, but the published SDK constructor has no such parameter")
	}
	if strings.Contains(externalPythonTemplate, "AGENT_URL") {
		t.Error("externalPythonTemplate help refers to AGENT_URL, but the runnable variable is INBOUND_URL")
	}
	for _, required := range []string{
		`LOCAL_HOST = "127.0.0.1"`,
		"LOCAL_PORT = 8080",
		"host=LOCAL_HOST",
		"port=LOCAL_PORT",
		"ngrok http 8080",
	} {
		if !strings.Contains(externalPythonTemplate, required) {
			t.Errorf("externalPythonTemplate stable tunnel target is missing %q", required)
		}
	}
	if strings.Contains(externalPythonTemplate, "run_heartbeat_loop_async") {
		t.Error("externalPythonTemplate calls run_heartbeat_loop_async, but molecule-ai-sdk 0.5.2 exposes the synchronous run_heartbeat_loop API")
	}
	if !strings.Contains(externalPythonTemplate, "client.run_heartbeat_loop()") {
		t.Error("externalPythonTemplate must call the published client's run_heartbeat_loop API")
	}

	saveAt := strings.Index(externalPythonTemplate, "client.save_token(AUTH_TOKEN)")
	attachAt := strings.Index(externalPythonTemplate, "client.attach_inbound_server(server)")
	secretGuardAt := strings.Index(externalPythonTemplate, "if not client.load_platform_inbound_secret():")
	refuseAt := strings.Index(externalPythonTemplate, "refusing to start unauthenticated A2A server")
	startAt := strings.Index(externalPythonTemplate, "server.start_in_background()")
	registerAt := strings.Index(externalPythonTemplate, "client.register()")
	if saveAt < 0 {
		t.Fatal("externalPythonTemplate must call client.save_token(AUTH_TOKEN)")
	}
	if registerAt < 0 {
		t.Fatal("externalPythonTemplate must call client.register()")
	}
	if attachAt < 0 {
		t.Fatal("externalPythonTemplate must attach A2AServer so register/heartbeat can feed it the platform inbound secret")
	}
	if startAt < 0 {
		t.Fatal("externalPythonTemplate must start A2AServer")
	}
	if secretGuardAt < 0 || refuseAt < 0 {
		t.Fatal("externalPythonTemplate must refuse to start A2AServer when register does not provide an inbound auth secret")
	}
	if saveAt > registerAt {
		t.Error("externalPythonTemplate must save the existing workspace token before client.register()")
	}
	if attachAt > startAt || attachAt > registerAt {
		t.Error("externalPythonTemplate must attach A2AServer before starting it and before register captures the inbound secret")
	}
	if registerAt > startAt {
		t.Error("externalPythonTemplate must register and capture the inbound secret before starting A2AServer, so inbound requests are fail-closed from the first request")
	}
	if secretGuardAt < registerAt || secretGuardAt > startAt || refuseAt > startAt {
		t.Error("externalPythonTemplate must check the captured inbound secret after register and refuse before A2AServer starts")
	}
}

// TestExternalPythonTemplate_HandlerConsumesCanonicalJSONRPCEnvelope executes
// the rendered handler against the A2A v0.3 wire shape. Substring checks alone
// missed both the nested params.message path and the kind discriminator.
func TestExternalPythonTemplate_HandlerConsumesCanonicalJSONRPCEnvelope(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required to execute the operator-facing Python snippet")
	}

	snippet := strings.Replace(
		externalPythonTemplate,
		"if __name__ == \"__main__\":\n    main()",
		"if False:\n    main()",
		1,
	)
	harness := `
import sys
import types

sdk = types.ModuleType("molecule_external_workspace")
sdk.RemoteAgentClient = object
sdk.A2AServer = object
sys.modules["molecule_external_workspace"] = sdk
` + snippet + `

request = {
    "jsonrpc": "2.0",
    "id": "req-1",
    "method": "message/send",
    "params": {
        "message": {
            "messageId": "msg-1",
            "role": "user",
            "parts": [
                {"kind": "text", "text": "hello "},
                {"kind": "file", "file": {"uri": "workspace:/brief.pdf"}},
                {"kind": "text", "text": "world"},
            ],
        },
    },
}
got = handle(request)
want = {"parts": [{"kind": "text", "text": "echo: hello world"}]}
if got != want:
    raise AssertionError(f"canonical A2A handler mismatch: got={got!r} want={want!r}")
`
	cmd := exec.Command(python, "-c", harness)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rendered Python handler failed canonical A2A semantic check: %v\n%s", err, out)
	}
}

// TestExternalPythonTemplate_RefusesServerWithoutInboundSecret executes the
// negative register path. Core may return 200 without an inbound secret when
// lazy healing fails; starting the SDK server in that state is legacy
// unauthenticated passthrough and must never happen in the generated example.
func TestExternalPythonTemplate_RefusesServerWithoutInboundSecret(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("python3 is required to execute the operator-facing Python snippet")
	}

	snippet := strings.Replace(
		externalPythonTemplate,
		"if __name__ == \"__main__\":\n    main()",
		"if False:\n    main()",
		1,
	)
	harness := `
import sys
import types

class FakeClient:
    def __init__(self, workspace_id, **kwargs):
        self.workspace_id = workspace_id
    def save_token(self, token):
        pass
    def attach_inbound_server(self, server):
        pass
    def register(self):
        return ""
    def load_platform_inbound_secret(self):
        return None
    def run_heartbeat_loop(self):
        raise AssertionError("heartbeat loop must not start without inbound auth")

class FakeServer:
    started = False
    def __init__(self, **kwargs):
        pass
    def start_in_background(self):
        FakeServer.started = True
    def stop(self):
        pass

sdk = types.ModuleType("molecule_external_workspace")
sdk.RemoteAgentClient = FakeClient
sdk.A2AServer = FakeServer
sys.modules["molecule_external_workspace"] = sdk
` + snippet + `

try:
    main()
except RuntimeError as exc:
    if "refusing to start unauthenticated A2A server" not in str(exc):
        raise
else:
    raise AssertionError("missing inbound secret did not fail closed")

if FakeServer.started:
    raise AssertionError("A2AServer started without an inbound auth secret")
`
	cmd := exec.Command(python, "-c", harness)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rendered Python server did not fail closed without inbound auth: %v\n%s", err, out)
	}
}

// TestExternalChannelTemplate_LaunchFlagShape pins the Claude Code channel
// snippet to the working launch invocation. The channel spec must be the
// VALUE of --dangerously-load-development-channels, NOT a separate
// --channels flag. The two-flag form (`--dangerously-load-development-channels
// --channels plugin:molecule@...`) errors with "entries must be tagged:
// --channels" on current Claude Code builds (2.1.143+) and silently no-ops
// on older ones — either way, new users hit a wall on first launch.
//
// Empirical: hit by a session walking through this exact snippet 2026-05-21;
// the broken form was copy-pasted from this template, ran, errored, and
// confused the operator into believing the plugin install was broken when
// the snippet itself was the bug.
func TestExternalChannelTemplate_LaunchFlagShape(t *testing.T) {
	// The broken two-flag form. If this string ever appears in the
	// snippet again, the same onboarding pothole returns.
	bannedFormBroken := "--dangerously-load-development-channels \\\n  --channels plugin:molecule@molecule-channel"
	if strings.Contains(externalChannelTemplate, bannedFormBroken) {
		t.Errorf("externalChannelTemplate contains the broken two-flag launch form. " +
			"Use --dangerously-load-development-channels plugin:molecule@molecule-channel (spec as value, not a separate --channels flag).")
	}

	// The single-flag form must be present.
	requiredFormGood := "--dangerously-load-development-channels plugin:molecule@molecule-channel"
	if !strings.Contains(externalChannelTemplate, requiredFormGood) {
		t.Errorf("externalChannelTemplate must contain %q so operators see the working launch invocation", requiredFormGood)
	}
}

// TestExternalChannelTemplate_CanonicalEnvShape pins the canvas-served
// .env example to the canonical SSOT shape (MOLECULE_WORKSPACES_JSON)
// rather than the legacy single-platform shape. The legacy form
// (MOLECULE_PLATFORM_URL + comma-separated IDs/TOKENS) is still accepted
// by the channel plugin's parseWorkspaceTargets but is single-tenant
// only — it silently fails to onboard users who want to watch multiple
// platforms (e.g. hongming + agents-team from the same plugin instance),
// which is the post-PR#15 expected use case.
func TestExternalChannelTemplate_CanonicalEnvShape(t *testing.T) {
	if !strings.Contains(externalChannelTemplate, "MOLECULE_WORKSPACES_JSON=") {
		t.Errorf("externalChannelTemplate must use MOLECULE_WORKSPACES_JSON as the canonical .env shape (the post-PR#15 SSOT)")
	}
	// The JSON example must contain the workspace_id + platform_url placeholders
	// so the canvas substitutes them at serve time.
	for _, ph := range []string{"{{WORKSPACE_ID}}", "{{PLATFORM_URL}}"} {
		if !strings.Contains(externalChannelTemplate, ph) {
			t.Errorf("externalChannelTemplate must contain placeholder %q so the canvas substitutes per-workspace values", ph)
		}
	}
}

// TestPollingTemplates_OptIntoPeerInfo pins the invariant that any template
// which calls /workspaces/:id/activity for inbound delivery requests the
// Layer 1 enrichment via ?include=peer_info. Without this opt-in, the
// platform returns bare activity rows and the operator's bridge / channel
// loses peer_name / peer_role / agent_card_url / attachments[] — they're
// available on the server but not delivered.
//
// Pre-Layer-1 platforms ignore unknown query params (HTTP spec: filters
// not understood are dropped), so this is back-compat across deploys.
//
// The Claude Code channel template doesn't include the poll URL in this
// snippet — its polling lives in the plugin's own server.ts (handled by
// molecule-mcp-claude-channel PR#21). The Kimi template DOES include a
// poll loop in its kimi_bridge.py block, so the invariant applies there.
func TestPollingTemplates_OptIntoPeerInfo(t *testing.T) {
	pollingTemplates := map[string]string{
		"externalKimiTemplate": externalKimiTemplate,
	}
	for name, body := range pollingTemplates {
		// If the snippet polls /activity, it must opt into peer_info.
		// The detection is intentionally loose ("/activity" appears in
		// the script) — operators who customize the script keep the
		// invariant only if the include hint is in the template.
		if !strings.Contains(body, "/activity") {
			t.Errorf("%s no longer polls /activity — review whether this test still applies", name)
			continue
		}
		if !strings.Contains(body, `"include": "peer_info"`) && !strings.Contains(body, "include=peer_info") {
			t.Errorf("%s polls /activity without ?include=peer_info — operators lose Layer 1 enrichment "+
				"(peer_name / peer_role / agent_card_url / attachments[]). Add the param to the poll URL.", name)
		}
	}
}

// TestBuildExternalConnectionPayload_NoUnsubstitutedPlaceholders is the
// merge-blocking gate for #79: the CTO was handed a snippet whose token
// field was the literal string "<paste auth_token from create response>".
//
// Class-level, not instance-level: it renders EVERY snippet in
// externalSnippetTemplates and asserts (a) no "<paste" survives, (b) no
// "{{" survives, (c) the real token is actually present. (c) is the
// load-bearing arm — (a) alone would pass if a template simply dropped
// its token site.
func TestBuildExternalConnectionPayload_NoUnsubstitutedPlaceholders(t *testing.T) {
	const tok = "wst_live_TESTTOKEN==" // trailing '=' pins the cut -d= -f2- fix
	p := BuildExternalConnectionPayload("https://app.example.com/", "ws-abc123", "My Agent", tok)
	for key := range externalSnippetTemplates {
		v, _ := p[key].(string)
		if v == "" {
			t.Fatalf("%s: empty / missing from payload", key)
		}
		if strings.Contains(v, "<paste") {
			t.Errorf("%s ships an unsubstituted human placeholder (\"<paste …>\") — "+
				"the operator cannot run this snippet. Every credential site must be {{AUTH_TOKEN}} "+
				"and stamped server-side; the canvas does no fix-up (#79).", key)
		}
		if strings.Contains(v, "{{") {
			t.Errorf("%s ships an unsubstituted {{PLACEHOLDER}} — add it to stamp()", key)
		}
		if !strings.Contains(v, tok) {
			t.Errorf("%s does not contain the auth_token — it must be copy-paste-runnable "+
				"with zero manual edits", key)
		}
	}
}

// TestBuildExternalConnectionPayload_BlankTokenStaysVisiblyIncomplete pins
// the re-show path (external_rotate.go GetExternalConnection passes ""):
// the snippet must stay visibly non-runnable. Stamping the EMPTY string
// there yields a snippet that looks complete and 401s with no clue why.
func TestBuildExternalConnectionPayload_BlankTokenStaysVisiblyIncomplete(t *testing.T) {
	p := BuildExternalConnectionPayload("https://app.example.com", "ws-abc123", "My Agent", "")
	for key := range externalSnippetTemplates {
		v, _ := p[key].(string)
		if !strings.Contains(v, tokenUnavailableMarker) {
			t.Errorf("%s: blank-token re-show path must leave %q at the credential site, "+
				"not an empty string", key, tokenUnavailableMarker)
		}
		if strings.Contains(v, "{{") {
			t.Errorf("%s still contains an unsubstituted {{PLACEHOLDER}}", key)
		}
	}
}

// credentialSiteRes match every shape in which a snippet ASSIGNS the
// workspace credential. Submatch 1 is the assigned value.
//
// The assignment forms are line-anchored: an unanchored match also fires on
// a grep needle (kimi documents a one-off reply command containing
// `grep '^MOLECULE_WORKSPACE_TOKEN=' …/env | cut -d= -f2-`), which reads the
// credential rather than assigning it. The JSON form is not anchored — the
// channel snippet embeds it mid-line inside MOLECULE_WS_ENTRY.
var credentialSiteRes = []*regexp.Regexp{
	regexp.MustCompile(`^[ \t]*(?:export[ \t]+)?MOLECULE_WORKSPACE_TOKENS?[ \t]*=[ \t]*"?([^"\n\\]*)`),
	regexp.MustCompile(`^[ \t]*(?:export[ \t]+)?WORKSPACE_AUTH_TOKEN[ \t]*=[ \t]*"?([^"\n\\]*)`),
	regexp.MustCompile(`^[ \t]*(?:export[ \t]+)?WORKSPACE_TOKEN[ \t]*=[ \t]*"?([^"\n\\]*)`),
	regexp.MustCompile(`^[ \t]*(?:export[ \t]+)?AUTH_TOKEN[ \t]*=[ \t]*"?([^"\n\\]*)`),
	regexp.MustCompile(`"token"[ \t]*:[ \t]*"([^"]*)"`),
}

// credentialSites returns every value a rendered snippet assigns to a
// credential slot. Comment lines are skipped: they document how to READ the
// stored token (a grep/cut recipe), they do not assign one.
func credentialSites(snippet string) []string {
	var out []string
	for _, line := range strings.Split(snippet, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, re := range credentialSiteRes {
			for _, m := range re.FindAllStringSubmatch(line, -1) {
				got := strings.TrimSpace(m[1])
				// $VAR / ${VAR} / os.environ[…] is an indirection — the
				// credential was assigned at its real site elsewhere.
				if got == "" || strings.HasPrefix(got, "$") || strings.Contains(got, "environ") {
					continue
				}
				out = append(out, got)
			}
		}
	}
	return out
}

// TestBuildExternalConnectionPayload_EveryCredentialSiteIsStamped closes the
// PARTIAL-STAMP escape.
//
// The placeholder gate above is a PRESENCE check: strings.Contains(snippet,
// token). A snippet that stamps {{AUTH_TOKEN}} at one site and hard-codes a
// literal at a SECOND site satisfies it and ships broken. That is not
// hypothetical — externalCodexTemplate has two credential sites (the TOML
// config block and the bridge-daemon launch line), and the pre-#79 canvas
// fix-up repaired only the first. The daemon then started with a placeholder
// bearer, 401'd on every long-poll, and failed SILENTLY because it is
// nohup'd to a log nobody reads: outbound worked, inbound never woke the
// agent. Presence is not completeness — assert EVERY site.
func TestBuildExternalConnectionPayload_EveryCredentialSiteIsStamped(t *testing.T) {
	const tok = "wst_live_TESTTOKEN=="
	p := BuildExternalConnectionPayload("https://app.example.com", "ws-abc123", "My Agent", tok)
	for key := range externalSnippetTemplates {
		v, _ := p[key].(string)
		sites := credentialSites(v)
		if len(sites) == 0 {
			t.Errorf("%s: no credential site found — either the snippet dropped its token "+
				"(then it cannot authenticate) or credentialSiteRes needs the new assignment "+
				"shape added, which would leave this gate BLIND to it", key)
		}
		for _, got := range sites {
			if got != tok {
				t.Errorf("%s: credential site carries %q, want the stamped auth_token %q — "+
					"every credential assignment must be {{AUTH_TOKEN}}, not a literal. A snippet "+
					"that stamps one site and hard-codes another passes the presence check and "+
					"ships a half-working agent (#79).", key, got, tok)
			}
		}
	}
}

// TestExternalChannelTemplate_ConfigWriteIsAppendSafe closes the DATA-LOSS
// defect, which otherwise has NO regression coverage: reverting the write to
// `cat > ~/.claude/channels/molecule/.env <<'EOF'` keeps every other test in
// this package green (the canonical-shape test only asserts the substring
// "MOLECULE_WORKSPACES_JSON=", which the clobbering form also contains).
//
// MOLECULE_WORKSPACES_JSON is a JSON ARRAY holding every workspace the
// operator has connected, and auth_tokens are shown exactly once. A
// truncating redirect silently destroys the entries already on disk and the
// lost tokens are unrecoverable — they require a rotation on each clobbered
// workspace. The snippet must MERGE.
func TestExternalChannelTemplate_ConfigWriteIsAppendSafe(t *testing.T) {
	// Truncating writes into the shared, multi-workspace channel config.
	for _, banned := range []string{
		"cat > ~/.claude/channels/molecule/.env",
		"cat >~/.claude/channels/molecule/.env",
		"cat > $HOME/.claude/channels/molecule/.env",
		"> ~/.claude/channels/molecule/.env <<",
		"tee ~/.claude/channels/molecule/.env",
	} {
		if strings.Contains(externalChannelTemplate, banned) {
			t.Errorf("externalChannelTemplate contains %q — a TRUNCATING write into the "+
				"multi-workspace channel .env. An operator who already connected workspace A "+
				"and runs this snippet for workspace B destroys A's entry, and A's auth_token "+
				"is shown only once, so recovery needs a credential rotation. The snippet must "+
				"MERGE this workspace into the existing MOLECULE_WORKSPACES_JSON array (#79).", banned)
		}
	}

	// ...and it must actually merge: read the existing array, drop only this
	// workspace's own prior entry (rotate-in-place, no duplicate), append.
	for _, required := range []string{
		"MOLECULE_WORKSPACES_JSON=",
		"existsSync", // reads whatever is already on disk
		"JSON.parse", // parses the existing array
		"arr.filter", // idempotent: replaces this workspace's own entry
		"arr.push",   // ...and appends rather than overwriting
		"renameSync", // atomic write, no truncated .env on a crash
	} {
		if !strings.Contains(externalChannelTemplate, required) {
			t.Errorf("externalChannelTemplate lost %q — the config write must read the existing "+
				"MOLECULE_WORKSPACES_JSON array, replace only this workspace's own entry, append, "+
				"and rename atomically. Without it the merge is not idempotent or not crash-safe (#79).", required)
		}
	}
}

// externalConnectionGoldenPath is the canvas test fixture that the modal's
// vitest suite consumes. It is REAL server output, not a hand-copy: the
// hand-copied fixture is exactly why #79 shipped green (its snippet strings
// were derived from the client's own search needles, so the fail arm was
// unreachable by construction).
const externalConnectionGoldenPath = "../../../canvas/src/components/__tests__/__fixtures__/external-connection.golden.json"

// goldenConnectionPayload is the single fixture input. Keep in sync with
// nothing — this IS the source. The canvas fixture is generated from it.
func goldenConnectionPayload() ([]byte, error) {
	p := BuildExternalConnectionPayload("https://app.example.com", "ws-123", "My Agent", "secret-auth-token-abc")
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// TestWriteExternalConnectionGolden keeps the canvas fixture honest.
//
// Default: VERIFIES the on-disk golden equals live BuildExternalConnectionPayload
// output — so a template edit that changes the payload reds here instead of
// silently orphaning the canvas fixture.
// With MOLECULE_UPDATE_GOLDEN=1: REGENERATES it. CI runs the regenerate form
// and then `git diff --exit-code` on the fixture path (see
// .gitea/workflows/e2e-external-connect-snippet.yml), so a stale fixture can
// never be merged.
func TestWriteExternalConnectionGolden(t *testing.T) {
	want, err := goldenConnectionPayload()
	if err != nil {
		t.Fatalf("marshal golden payload: %v", err)
	}

	if os.Getenv("MOLECULE_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(externalConnectionGoldenPath), 0o755); err != nil {
			t.Fatalf("mkdir golden dir: %v", err)
		}
		if err := os.WriteFile(externalConnectionGoldenPath, want, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("regenerated %s", externalConnectionGoldenPath)
		return
	}

	got, err := os.ReadFile(externalConnectionGoldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v — regenerate with "+
			"MOLECULE_UPDATE_GOLDEN=1 go test ./internal/handlers/ -run TestWriteExternalConnectionGolden",
			externalConnectionGoldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("canvas golden fixture is STALE (%s). The snippets the modal is tested "+
			"against no longer match what the server actually returns. Regenerate with:\n"+
			"  MOLECULE_UPDATE_GOLDEN=1 go test ./internal/handlers/ -run TestWriteExternalConnectionGolden",
			externalConnectionGoldenPath)
	}
}
