package handlers

// external_connection.go — copy-paste connection payload for external-like
// workspaces. A real token is shown on create or credential rotation; the
// read-only re-show path deliberately returns non-runnable placeholders.
//
// The canvas UI surfaces these in a single modal so the operator can
// hand the block to whoever runs their external agent without having
// to piece together workspace_id + platform_url + auth_token + API
// shape from the docs. curl snippet has zero dependencies; Python
// snippet pairs with molecule-ai-sdk's A2AServer + RemoteAgentClient.
//
// BuildExternalConnectionPayload (below) is the single source of truth
// for the payload shape — used by Create (#workspace.go), Rotate
// (#external_rotate.go), and the read-only "show instructions again"
// endpoint. Adding a snippet means adding it here once; the three
// callers pick it up automatically.

import (
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// authTokenPlaceholder is the ONLY way a credential enters a snippet.
// Every template must contain it at every token site; the modal does no
// credential fix-up (it used to, via hand-copied string.replace() needles
// that silently drifted from these templates — see #79).
const authTokenPlaceholder = "{{AUTH_TOKEN}}"

// tokenUnavailableMarker is stamped in place of the token on the read-only
// re-show path (GET .../external/connection returns auth_token=""). It must
// stay visibly non-runnable: stamping "" there yields a snippet that LOOKS
// complete and 401s with no clue why.
//
// BUT "visibly non-runnable" is NOT enough on its own, and believing it was is
// how the bug below shipped. The marker is only self-evidently useless for
// snippets that use the token INLINE (a curl bearer, an exported env var): you
// run it, it 401s, nothing is lost. For the snippets that PERSIST the token —
// the Claude Code channel .env, the Kimi env file, `claude mcp add`,
// `openclaw mcp set` — the marker is just a string, so the script runs happily
// and OVERWRITES the operator's working credential with a dead one. The token is
// shown ONCE, so the real one is then unrecoverable: they must rotate.
//
// Concretely, before the guard below: an operator with a working channel reopens
// the modal (GET → auth_token="" → marker) and re-pastes the block; the merge
// script filters out their real {id, platform_url} entry and pushes
// {"token":"<ROTATE_TO_REVEAL_TOKEN>"}. Their channel starts 401ing and the only
// copy of the token is gone.
//
// So every snippet that WRITES a credential must ALSO fail closed on the marker.
// See tokenGuard* below; the invariant is pinned by
// TestReshowSnippets_RefuseToRunWithoutAToken.
const tokenUnavailableMarker = "<ROTATE_TO_REVEAL_TOKEN>"

// tokenGuardNeedle is the substring every refusal guard matches the stamped
// token against. Kept separate from tokenUnavailableMarker's angle brackets so
// the guard survives a shell/JSON quoting change to the marker itself.
const tokenGuardNeedle = "ROTATE_TO_REVEAL_TOKEN"

// tokenGuardSentinel appears in EVERY refusal guard (the shell one below and
// the JS one inside the channel snippet). The invariant test greps for THIS,
// not for tokenGuardNeedle: the marker itself contains the needle, so a
// needle-presence check would pass vacuously on a completely unguarded snippet.
const tokenGuardSentinel = "molecule: this block has NO TOKEN"

// tokenGuardShell is prepended to every SHELL snippet that persists the
// credential. It must NOT `exit` — these blocks are pasted into an interactive
// shell, and exiting would close the operator's terminal. It sets
// MOLECULE_TOKEN_OK, and each persisting command is wrapped in a test of it, so
// a marker-stamped paste is a NO-OP that explains itself and touches nothing.
const tokenGuardShell = `# ── refuse to run without a real token ────────────────────────────────
# The token is shown ONCE. If you re-opened this dialog, the block below
# carries a placeholder instead of your token. Pasting it anyway would
# OVERWRITE your working credential with a dead one and you would have to
# rotate to recover. So: if the token is missing, every step below is
# skipped and nothing on disk is touched.
MOLECULE_TOKEN_OK=1
case "{{AUTH_TOKEN}}" in
  *ROTATE_TO_REVEAL_TOKEN*)
    MOLECULE_TOKEN_OK=0
    echo "molecule: this block has NO TOKEN (it was re-shown; the token is displayed only once)." >&2
    echo "molecule: click Rotate to mint a new one, then copy the block again." >&2
    echo "molecule: nothing was changed — your existing config is untouched." >&2
    ;;
esac

if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
`

// externalWorkspaceRuntimeInstall keeps every runtime-backed operator snippet
// on the same reproducible install path. The private Gitea registry does not
// proxy public PyPI dependencies, while --extra-index-url would let a public
// package shadow the private name. Download the pinned private wheel without
// dependencies, then let public PyPI resolve only that local wheel's deps.
const externalWorkspaceRuntimeInstall = `RUNTIME_INSTALL_STATUS=0
RUNTIME_DOWNLOAD="$(mktemp -d)" || RUNTIME_INSTALL_STATUS=$?
if [ "$RUNTIME_INSTALL_STATUS" -eq 0 ]; then
  python3 -m pip download --no-deps --dest "$RUNTIME_DOWNLOAD" \
    --index-url https://git.moleculesai.app/api/packages/molecule-ai/pypi/simple/ \
    molecules-workspace-runtime==0.4.36 &&
  python3 -m pip install --index-url https://pypi.org/simple/ \
    "$RUNTIME_DOWNLOAD"/molecules_workspace_runtime-*.whl
  RUNTIME_INSTALL_STATUS=$?
fi
if [ -n "$RUNTIME_DOWNLOAD" ]; then
  rm -rf "$RUNTIME_DOWNLOAD"
fi
if [ "$RUNTIME_INSTALL_STATUS" -ne 0 ]; then
  MOLECULE_TOKEN_OK=0
  echo "molecule: runtime install failed; config and agent startup were skipped." >&2
  echo "molecule: fix the pip error above, then paste this block again." >&2
fi
`

// externalSnippetTemplates — SSOT for the operator-facing snippets.
// Adding a snippet here auto-enrolls it in BuildExternalConnectionPayload
// AND in the placeholder-leak invariant tests. Do not build a snippet
// payload key any other way.
var externalSnippetTemplates = map[string]string{
	"curl_register_template":      externalCurlTemplate,
	"python_snippet":              externalPythonTemplate,
	"claude_code_channel_snippet": externalChannelTemplate,
	"universal_mcp_snippet":       externalUniversalMcpTemplate,
	"hermes_channel_snippet":      externalHermesChannelTemplate,
	"codex_snippet":               externalCodexTemplate,
	"openclaw_snippet":            externalOpenClawTemplate,
	"kimi_snippet":                externalKimiTemplate,
}

// snippetsWithoutEnvKeyContract — snippets whose credential site is NOT an env var,
// so the SDK credentials contract's env-key taxonomy does not apply to it.
//
// Exactly one qualifies: the python snippet's `AUTH_TOKEN = "…"` is a
// module-level constant persisted through client.save_token(AUTH_TOKEN). No
// process reads an env var by that name, so requiring it to be a
// contract-declared key would be cargo-culting the rule rather than applying it.
//
// The exemption's PREMISE is itself asserted (see
// TestSnippetCredentials_ConformToSDKCredentialsContract): an exempt snippet may not
// read the value out of the environment. The moment it does, it IS env-key-bearing
// and the exemption stops being true.
var snippetsWithoutEnvKeyContract = map[string]string{
	"python_snippet": "python source: the credential is a module-level constant passed to " +
		"client.save_token(...), never an env var a runtime reads",
}

// snippetsExemptFromTokenGuard — the ONLY snippets allowed to render without a
// refusal guard on the re-show path, each with the reason it is safe. Every
// other snippet must carry tokenGuardSentinel; that is enforced by
// TestReshowSnippets_RefuseToRunWithoutAToken, which fails on any new
// unlisted, unguarded snippet. Adding a key here is a deliberate, reviewable
// act — it is the assertion "running this with a dead token destroys nothing".
var snippetsExemptFromTokenGuard = map[string]string{
	"curl_register_template": "one-shot inline request: a dead token 401s, nothing is written or replaced",
}

// BuildExternalConnectionPayload assembles the gin.H payload that the
// canvas's ExternalConnectModal consumes. Pure data — caller owns DB
// reads (workspace_id, workspace_name) and token minting (auth_token).
//
// The auth token is stamped HERE, server-side, at every {{AUTH_TOKEN}}
// site of every template — the modal is a dumb renderer. It used to do
// the credential fix-up itself with string.replace() needles copied by
// hand from these templates; a template edit then silently orphaned the
// needle (no compile error, no test failure) and the operator was handed
// a snippet whose token field was the literal "<paste …>" string (#79).
//
// authToken may be empty for the read-only "show instructions again"
// path; the snippets then carry tokenUnavailableMarker at the credential
// site so they stay visibly non-runnable (an empty string there would
// look complete and 401 with no clue why).
//
// workspaceName feeds the per-workspace MCP server-name in the snippets
// that wire molecule-mcp into an external Claude Code (or other
// MCP-stdio) client. Without a unique server name a second
// `claude mcp add molecule` call REPLACES the first entry, collapsing
// multi-workspace use into a single per-session slot — see
// mcpServerNameForWorkspace below. May be empty (re-show / rotate paths
// that don't plumb the name); the helper falls back to the workspace
// ID's short prefix so the snippet is always unique.
func BuildExternalConnectionPayload(platformURL, workspaceID, workspaceName, authToken string) gin.H {
	pURL := strings.TrimSuffix(platformURL, "/")
	mcpName := mcpServerNameForWorkspace(workspaceID, workspaceName)
	// ReplaceAll, not first-match: the codex template carries the token at
	// TWO sites (the config.toml block and the bridge-daemon env prefix).
	// A first-match-only substitution left the second one unstamped.
	tokenValue := authToken
	if tokenValue == "" {
		tokenValue = tokenUnavailableMarker
	}
	stamp := func(tmpl string) string {
		return strings.ReplaceAll(
			strings.ReplaceAll(
				strings.ReplaceAll(
					strings.ReplaceAll(tmpl, "{{PLATFORM_URL}}", pURL),
					"{{WORKSPACE_ID}}", workspaceID,
				),
				"{{MCP_SERVER_NAME}}", mcpName,
			),
			authTokenPlaceholder, tokenValue,
		)
	}
	out := gin.H{
		"workspace_id":       workspaceID,
		"platform_url":       pURL,
		"auth_token":         authToken,
		"registry_endpoint":  pURL + "/registry/register",
		"heartbeat_endpoint": pURL + "/registry/heartbeat",
	}
	for key, tmpl := range externalSnippetTemplates {
		out[key] = stamp(tmpl)
	}
	return out
}

// externalPlatformURL returns the public URL at which this workspace-
// server instance is reachable by the operator's external agent. This
// is NOT necessarily the caller's Host header (which could be an
// internal CF-tunnel / host.docker.internal address in the local-docker
// published-port posture — a topology leak AND unreachable by a real
// external agent off the docker host).
//
// Resolution order, public-base-env first, request Host LAST:
//
//  1. EXTERNAL_PLATFORM_URL — the control plane injects this as the
//     tenant's PUBLIC tunnel URL (molecule-controlplane #1050,
//     tenantPublicURLEnvArgs → "https://<slug>.<public-base-domain>").
//     The authoritative customer-facing base when present.
//
//  2. PLATFORM_URL — defense-in-depth (#1050 follow-up). The CP sets
//     PLATFORM_URL to the SAME public tunnel URL on EVERY backend: the
//     EC2 user-data (ec2.go: `-e PLATFORM_URL=https://<slug>.<domain>`,
//     where EXTERNAL_PLATFORM_URL is NOT set at all) and the local-docker
//     provisioner (tenantPublicURLEnvArgs sets both). So if a deploy ever
//     ships a tenant WITHOUT EXTERNAL_PLATFORM_URL (older user-data, a
//     forgotten env, a new backend), PLATFORM_URL still yields the public
//     base — the snippet stays correct instead of silently falling
//     through to the internal request Host. Preferred over the Host
//     header for exactly the leak reason above.
//
//  3. Request scheme + Host — last resort, reached only when NEITHER
//     public-base env is set (pure local dev / a misconfigured deploy).
func externalPlatformURL(c *gin.Context) string {
	// Each env source is trusted ONLY if it passes isPublicExternalURL. core
	// does not guarantee either env is public (main.go defaults PLATFORM_URL to
	// an internal host), so a hostile/in-cluster value must fail the predicate
	// and fall through rather than be served verbatim as the customer base.
	if v := strings.TrimSpace(os.Getenv("EXTERNAL_PLATFORM_URL")); v != "" && isPublicExternalURL(v) {
		return v
	}
	// Defense-in-depth: the public PLATFORM_URL the CP also injects. Falls
	// here BEFORE the request Host so a missing EXTERNAL_PLATFORM_URL never
	// leaks an internal host into the customer-facing connect snippet — but
	// only when PLATFORM_URL is itself public (validated above).
	if v := strings.TrimSpace(os.Getenv("PLATFORM_URL")); v != "" && isPublicExternalURL(v) {
		return v
	}
	scheme := "https"
	if xf := c.Request.Header.Get("X-Forwarded-Proto"); xf != "" {
		scheme = xf
	} else if c.Request.TLS == nil {
		scheme = "http"
	}
	host := c.Request.Host
	if xh := c.Request.Header.Get("X-Forwarded-Host"); xh != "" {
		host = xh
	}
	return scheme + "://" + host
}

// mcpServerNameForWorkspace derives the unique MCP server name used in
// the Universal MCP snippet's `claude mcp add <name> -- ...` line.
//
// Why per-workspace, not a fixed "molecule": `claude mcp add` keys
// entries by name in ~/.claude.json, so re-running with the same name
// silently REPLACES the previous entry. A single external Claude Code
// session that connects to N molecule workspaces must therefore use N
// distinct server names — otherwise the second install collapses the
// first, and the user experiences "MCP is per-session". MCP itself
// supports many servers per session; the install-snippet name was the
// only thing standing in the way.
//
// Pattern: "molecule-<slug>" where slug comes from the workspace name
// (lowercased, non-alphanumeric → hyphen, collapsed, trimmed, <=24
// chars). Falls back to the workspace ID's first 8 chars when the name
// is empty or slugifies to nothing — both produce a deterministic,
// Claude-Code-name-safe (alphanumeric + hyphens, no spaces / dots /
// slashes) identifier that disambiguates per-workspace.
//
// Two workspaces with identical names still produce identical slugs by
// design — the user picked them to look the same. The
// `claude mcp add` step will overwrite the older one in that case;
// the workaround is to rename one, then re-run. Documented in the
// snippet header so users aren't surprised.
func mcpServerNameForWorkspace(workspaceID, workspaceName string) string {
	const fallbackIDPrefixLen = 8
	const maxSlugLen = 24
	slug := slugifyForMcpName(workspaceName, maxSlugLen)
	if slug == "" {
		id := strings.ReplaceAll(workspaceID, "-", "")
		if len(id) > fallbackIDPrefixLen {
			id = id[:fallbackIDPrefixLen]
		}
		slug = id
	}
	if slug == "" {
		// Defensive: empty workspaceID at this layer means the caller
		// is misusing the API; we still return a usable (non-colliding
		// in the common case) constant rather than producing "molecule-"
		// which Claude Code would reject.
		return "molecule"
	}
	return "molecule-" + slug
}

// slugifyForMcpName lowercases, replaces non-[a-z0-9] runs with a single
// '-', trims leading/trailing '-', and truncates to maxLen. Returns ""
// if nothing usable remains. Pure helper; no allocations beyond the
// builder.
func slugifyForMcpName(s string, maxLen int) string {
	var b strings.Builder
	b.Grow(len(s))
	lastHyphen := true // suppress leading hyphens
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			lastHyphen = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if len(out) > maxLen {
		out = strings.TrimRight(out[:maxLen], "-")
	}
	return out
}

// externalCurlTemplate — zero-dependency register snippet. Placeholders:
//   - {{PLATFORM_URL}}, {{WORKSPACE_ID}}   — filled server-side
//   - $MOLECULE_WORKSPACE_TOKEN                — env var, operator sets
//   - $AGENT_URL                           — env var, operator's public HTTPS endpoint
//
// SSRF filter rejects private IPs at register time, so AGENT_URL must
// resolve to a public host.
//
// Heartbeat loop is NOT included here — curl is fine for one-shot
// register; keeping the workspace alive wants a real loop, so point
// operators at the Python snippet for long-lived setups.
const externalCurlTemplate = `# Replace AGENT_URL with YOUR agent's public HTTPS endpoint, then run:
export MOLECULE_WORKSPACE_TOKEN="{{AUTH_TOKEN}}"
export AGENT_URL="https://your-agent.example.com"

# NOTE on the "Origin" header below: hosted SaaS tenants run behind an
# edge WAF that requires same-origin requests. Without "Origin", paths
# like /workspaces/* silently 404 (rewritten to the canvas Next.js).
# /registry/register is currently allowed without Origin, but setting
# it preemptively keeps your snippet working if the WAF rules expand.
curl -fsS -X POST "{{PLATFORM_URL}}/registry/register" \
  -H "Authorization: Bearer $MOLECULE_WORKSPACE_TOKEN" \
  -H "Origin: {{PLATFORM_URL}}" \
  -H "Content-Type: application/json" \
  -d '{
    "id": "{{WORKSPACE_ID}}",
    "url": "'"$AGENT_URL"'",
    "agent_card": {
      "name": "My External Agent",
      "description": "",
      "version": "0.1.0"
    }
  }'

# Need help?
#   Documentation: https://doc.moleculesai.app/docs/guides/external-agent-registration
#   Common errors:
#     • 401 / 403 on register — MOLECULE_WORKSPACE_TOKEN must be the value
#       shown at workspace create. Tokens are shown only once.
`

// externalChannelTemplate — Claude Code channel plugin install + .env. For
// operators whose external agent IS a Claude Code session (laptop or
// remote dev VM); routes the workspace's A2A traffic into the running
// Claude Code session as conversation turns via MCP. The plugin source
// lives at git.moleculesai.app/molecule-ai/molecule-mcp-claude-channel — polling
// based, no tunnel required (bounded since_secs cold start, then a persisted
// since_id cursor in steady state).
const externalChannelTemplate = tokenGuardShell + `# Claude Code channel — bridges this workspace's A2A traffic into your
# Claude Code session. No tunnel/public URL needed (polling-based).
#
# Prereq: Bun 1.3+ installed (channel plugins are Bun scripts).
#   bun --version    # must print a version (1.3.x or newer)
CHANNEL_SETUP_STATUS=0
#
# 1. Inside Claude Code, install the channel plugin. The plugin lives in
#    Molecule's own Gitea marketplace (not Anthropic's default), so a
#    one-time marketplace-add is needed before install:
#
#      /plugin marketplace add https://git.moleculesai.app/molecule-ai/molecule-mcp-claude-channel.git
#      /plugin install molecule@molecule-channel
#
#    Then /reload-plugins (or restart Claude Code) so the plugin is
#    registered.
#
# 2. Extend the per-host config file. The canonical SSOT shape is
#    MOLECULE_WORKSPACES_JSON — a JSON array of {id, token, platform_url}
#    objects. One plugin instance watches many workspaces across many
#    tenants, so this block MERGES this workspace into whatever array is
#    already on disk rather than overwriting it: run it once per workspace
#    and they all accumulate. Re-running it for the SAME workspace replaces
#    that one entry (a rotate refreshes the token in place, no duplicate).
#    Uses Bun (the prereq above) — no extra dependency.
mkdir -p ~/.claude/channels/molecule
MOLECULE_WS_ENTRY='{"id":"{{WORKSPACE_ID}}","token":"{{AUTH_TOKEN}}","platform_url":"{{PLATFORM_URL}}"}' \
bun -e "$(cat <<'JS'
const fs = require("fs"), os = require("os");
const p = os.homedir() + "/.claude/channels/molecule/.env";
const KEY = "MOLECULE_WORKSPACES_JSON=";
const entry = JSON.parse(process.env.MOLECULE_WS_ENTRY);
// REFUSE to write without a real token. The token is shown ONCE; on the
// re-show path the modal stamps a placeholder here. Writing it would REPLACE
// this workspace's live entry below (filter+push) with a dead credential, and
// the real one is then unrecoverable — rotate-only. Exiting here only ends
// this bun process, never the operator's shell. Nothing on disk is touched.
if (String(entry.token).includes("ROTATE_TO_REVEAL_TOKEN")) {
  console.error("molecule: this block has NO TOKEN (it was re-shown; the token is displayed only once).");
  console.error("molecule: click Rotate to mint a new one, then copy the block again.");
  console.error("molecule: nothing was written — your existing .env is untouched.");
  process.exit(1);
}
const lines = fs.existsSync(p) ? fs.readFileSync(p, "utf8").split("\n").filter((l) => l.trim()) : [];
const cur = lines.find((l) => l.startsWith(KEY));
let arr = [];
if (cur) {
  try {
    arr = JSON.parse(cur.slice(KEY.length));
  } catch (e) {
    console.error("molecule: existing MOLECULE_WORKSPACES_JSON is invalid; refusing to overwrite .env.");
    process.exit(1);
  }
  if (!Array.isArray(arr)) {
    console.error("molecule: existing MOLECULE_WORKSPACES_JSON is invalid; expected a JSON array and refused to overwrite .env.");
    process.exit(1);
  }
}
arr = arr.filter((w) => !(w && w.id === entry.id && w.platform_url === entry.platform_url));
arr.push(entry);
const out = lines.filter((l) => !l.startsWith(KEY)).concat([KEY + JSON.stringify(arr)]).join("\n") + "\n";
fs.writeFileSync(p + ".tmp", out, { mode: 0o600 });
fs.renameSync(p + ".tmp", p);
console.log("molecule: .env now holds " + arr.length + " workspace(s)");
JS
)"
CHANNEL_SETUP_STATUS=$?
if [ "$CHANNEL_SETUP_STATUS" -ne 0 ]; then
  MOLECULE_TOKEN_OK=0
  echo "molecule: Claude channel config update failed; Claude startup was skipped." >&2
fi
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
chmod 600 ~/.claude/channels/molecule/.env
CHANNEL_SETUP_STATUS=$?
if [ "$CHANNEL_SETUP_STATUS" -ne 0 ]; then
  MOLECULE_TOKEN_OK=0
  echo "molecule: Claude channel config permissions could not be secured; Claude startup was skipped." >&2
fi
fi

# (Legacy single-platform shape — MOLECULE_PLATFORM_URL + comma-separated
# MOLECULE_WORKSPACE_IDS + MOLECULE_WORKSPACE_TOKENS — is still supported
# for back-compat but does NOT work across multiple tenant URLs. Use
# MOLECULE_WORKSPACES_JSON above unless you have a specific reason.)

# 3. Launch Claude Code with the channel enabled. The channel spec is the
#    VALUE of --dangerously-load-development-channels — NOT a separate
#    --channels flag (that flag does not exist in current Claude Code;
#    passing it errors with "entries must be tagged: --channels").
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
claude --dangerously-load-development-channels plugin:molecule@molecule-channel
CHANNEL_SETUP_STATUS=$?
fi

# You should see on stderr:
#   molecule channel: connected — watching N workspace(s) across M platform(s)
#     targets: <platform_url>: <workspace_id>
#
# Inbound A2A messages now surface as conversation turns (synthetic
# <channel ...> tags). Claude's replies route back via the
# reply_to_workspace / send_message_to_user MCP tools.
#
# Multi-workspace note: when watching more than one workspace, every
# outbound tool call (send_message_to_user, reply_to_workspace,
# delegate_task, list_peers) MUST pass _as_workspace=<id> so the plugin
# knows which token to authenticate with. The host returns -32603 if you
# forget — the synthetic <channel> tag's "watching_as" attribute tells
# you which id to use.
#
# Common errors:
#   "plugin not installed"            → Step 1 didn't run; run /plugin
#                                       marketplace add + /plugin install
#                                       inside Claude Code, then /reload-plugins.
#   "entries must be tagged"          → You passed --channels separately.
#                                       Put plugin:molecule@molecule-channel
#                                       directly after
#                                       --dangerously-load-development-channels.
#   "not on approved channels allowlist" → Org policy gating. See "managed
#                                       settings" note below.
#   "config-missing"                  → ~/.claude/channels/molecule/.env
#                                       not readable; re-run Step 2 and check
#                                       chmod 600.
#
# Team/Enterprise plans: the channel allowlist is gated by org policy
# AND must be written to the local managed-settings.json file on disk
# (not the claude.ai web admin UI — there is no web toggle for this).
# Path per OS:
#   macOS:   /Library/Application Support/ClaudeCode/managed-settings.json
#   Linux:   /etc/claude-code/managed-settings.json
#   Windows: C:\ProgramData\ClaudeCode\managed-settings.json
# Set channelsEnabled: true and add
#   { "plugin": "molecule", "marketplace": "molecule-channel" }
# to allowedChannelPlugins. Restart Claude Code after writing the file.
# A user-level ~/.claude/settings.json does NOT work on Team/Enterprise
# — this is the single most common reason a freshly-installed plugin
# appears to do nothing.
#
# Pro/Max plans skip the channelsEnabled gate but still need the
# allowedChannelPlugins entry in the managed-settings file.

# Need help?
#   Documentation: https://doc.moleculesai.app/docs/guides/external-agent-registration
#   Full README:   https://git.moleculesai.app/molecule-ai/molecule-mcp-claude-channel
#   Common errors:
#     • "plugin not installed" — run /plugin marketplace add then
#       /plugin install lines above; /reload-plugins or restart.
#     • "entries must be tagged: --channels" — the launch flag form
#       changed; use --dangerously-load-development-channels plugin:molecule@molecule-channel
#       (channel spec is the VALUE, not a separate --channels flag).
#     • "not on the approved channels allowlist" — custom channels need
#       allowedChannelPlugins in /Library/Application Support/ClaudeCode/managed-settings.json
#       (macOS) / equivalent on Linux+Windows. NOT a web setting.
#     • "Inbound messages not arriving" — stderr should show
#       "molecule channel: connected — watching N workspace(s)";
#       verify ~/.claude/channels/molecule/.env shape is MOLECULE_WORKSPACES_JSON.
[ "$CHANNEL_SETUP_STATUS" -eq 0 ]
fi
`

// externalUniversalMcpTemplate — runtime-agnostic standalone path.
// Ships as the `molecule-mcp` console script in the
// molecules-workspace-runtime wheel published to the Gitea package registry.
// Any MCP-aware runtime (Claude Code, hermes, codex, third-party)
// registers it once and gets the same 8 universal tools that
// container-bound runtimes use today: delegate_task, list_peers,
// send_message_to_user, commit_memory, etc.
//
// Standalone: the binary itself handles register-on-startup +
// continuous heartbeats (daemon thread, 20s cadence). No separate
// SDK or channel process needed to keep the workspace online. The
// only thing it does NOT yet do is poll inbound A2A messages. Claude Code can
// use its channel tab; other runtimes need their own bridge. The Python SDK tab
// is an authenticated push-server example to build that bridge on, not a
// turnkey adapter into an arbitrary runtime session.
//
// Origin/WAF: handled automatically by platform_auth.auth_headers()
// in the wheel — operator doesn't need to configure anything.
const externalUniversalMcpTemplate = tokenGuardShell + `# Universal MCP — standalone register + heartbeat + outbound platform tools
# for any MCP-aware runtime (Claude Code, hermes, codex, etc.).
# Claude Code can pair this with its channel tab for inbound turns. For other
# runtimes, the Python SDK tab is an authenticated push-server starting point;
# connect its handler to your runtime's session API to build an inbound bridge.
#
# Multi-workspace: MCP supports many servers per Claude Code session.
# This snippet uses a workspace-specific server name ({{MCP_SERVER_NAME}})
# so installing for a second workspace ADDS another entry instead of
# overwriting the first — run the snippet from each workspace's modal
# in turn and ` + "`claude mcp list`" + ` will show all of them. If two
# workspaces have the same name, slugs collide and the second install
# overwrites the first; rename one workspace to disambiguate.

# Requires Python >= 3.11. On 3.10 or older pip says
# "Could not find a version that satisfies the requirement
# (from versions: none)" — the wheel's requires_python pin filters
# the only available artifact before pip even attempts install.
# Upgrade the interpreter (brew install python@3.12 / apt install
# python3.12 / etc.) or use a 3.11+ venv.

# 1. Install the workspace runtime wheel (once per machine — safe to
#    re-run; subsequent workspaces share the same wheel):
` + externalWorkspaceRuntimeInstall + `
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then

# 2. Wire molecule-mcp into your agent's MCP config. Claude Code:
#    NOTE the server name is workspace-specific ("{{MCP_SERVER_NAME}}") so
#    multiple molecule workspaces co-exist in one Claude Code session.
#    ` + "`claude mcp add`" + ` OVERWRITES the entry for this server name, so a
#    tokenless re-show paste would replace a working credential with a dead
#    one — hence the MOLECULE_TOKEN_OK guard at the top of this block.
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
claude mcp add {{MCP_SERVER_NAME}} -s user -- env \
  WORKSPACE_ID={{WORKSPACE_ID}} \
  PLATFORM_URL={{PLATFORM_URL}} \
  MOLECULE_WORKSPACE_TOKEN="{{AUTH_TOKEN}}" \
  molecule-mcp
RUNTIME_INSTALL_STATUS=$?
if [ "$RUNTIME_INSTALL_STATUS" -ne 0 ]; then
  MOLECULE_TOKEN_OK=0
  echo "molecule: Claude MCP configuration failed; no usable server entry was created." >&2
fi
fi

# molecule-mcp registers the workspace + heartbeats every 20s in a
# daemon thread, then runs the MCP stdio loop. Same env-var contract
# works with hermes-agent, codex, or any MCP stdio runtime. Tools
# exposed: delegate_task, delegate_task_async, check_task_status,
# list_peers, get_workspace_info, send_message_to_user,
# commit_memory, recall_memory.
#
# Origin/WAF handling is built into the wheel — no manual headers
# needed when calling tools through the MCP server.

# Need help?
#   Where to install: https://git.moleculesai.app/api/packages/molecule-ai/pypi/simple/molecules-workspace-runtime/
#   Documentation: https://doc.moleculesai.app/docs/guides/external-agent-registration
#   Common errors:
#     • "Tools not appearing in your agent" — run ` + "`claude mcp list`" + ` (or
#       your runtime's equivalent) and confirm the {{MCP_SERVER_NAME}} entry.
#       If missing, re-run the ` + "`claude mcp add`" + ` line above.
#     • "Connecting a second workspace overwrote the first" — re-check that
#       the server name in the line above is {{MCP_SERVER_NAME}} (not a bare
#       "molecule"); each workspace's modal generates a distinct name.
#     • "ConnectionRefused / DNS error on first call" — PLATFORM_URL must
#       include the scheme (https://) and have NO trailing slash. Verify
#       with: curl ${PLATFORM_URL}/healthz
fi
[ "$RUNTIME_INSTALL_STATUS" -eq 0 ]
fi
`

// externalPythonTemplate uses molecule-ai-sdk's RemoteAgentClient +
// A2AServer from the published Gitea package registry.
const externalPythonTemplate = `# Install the pinned private SDK wheel, then resolve only its public
# dependencies from PyPI. Using the private registry as pip's sole install
# index cannot resolve PyYAML/requests; an unrestricted extra-index would
# reintroduce dependency-confusion risk for the private package name.
# SDK_DOWNLOAD="$(mktemp -d)"
# python3 -m pip download --no-deps --dest "$SDK_DOWNLOAD" \
#   --index-url https://git.moleculesai.app/api/packages/molecule-ai/pypi/simple/ \
#   molecule-ai-sdk==0.5.2
# python3 -m pip install --index-url https://pypi.org/simple/ \
#   "$SDK_DOWNLOAD"/molecule_ai_sdk-*.whl
# rm -rf "$SDK_DOWNLOAD"
# (installs as molecule-ai-sdk, imports as molecule_external_workspace — the names intentionally differ)

from molecule_external_workspace import RemoteAgentClient, A2AServer

WORKSPACE_ID  = "{{WORKSPACE_ID}}"
PLATFORM_URL  = "{{PLATFORM_URL}}"
AUTH_TOKEN    = "{{AUTH_TOKEN}}"
INBOUND_URL   = "https://your-agent.example.com/a2a/inbound"  # your public HTTPS endpoint
LOCAL_HOST = "127.0.0.1"
LOCAL_PORT = 8080  # stable target for ngrok / Cloudflare Tunnel / reverse proxy

if "ROTATE_TO_REVEAL_TOKEN" in AUTH_TOKEN:
    raise RuntimeError("molecule: this block has NO TOKEN; rotate to reveal one before running it")

def handle(request: dict) -> dict:
    # A2A v0.3 JSON-RPC: text parts live under params.message.parts and use
    # "kind" as the discriminator (not the legacy "type" key).
    message = request.get("params", {}).get("message", {})
    parts = message.get("parts", [])
    text = "".join(p.get("text", "") for p in parts if p.get("kind") == "text")
    return {"parts": [{"kind": "text", "text": f"echo: {text}"}]}

def main():
    client = RemoteAgentClient(
        workspace_id=WORKSPACE_ID,
        platform_url=PLATFORM_URL,
    )
    client.save_token(AUTH_TOKEN)
    server = A2AServer(
        agent_id=client.workspace_id,
        inbound_url=INBOUND_URL,
        message_handler=handle,
        host=LOCAL_HOST,
        port=LOCAL_PORT,
    )
    client.attach_inbound_server(server)
    client.reported_url = INBOUND_URL
    client.register()
    if not client.load_platform_inbound_secret():
        raise RuntimeError("platform did not provide inbound auth; refusing to start unauthenticated A2A server")
    server.start_in_background()       # fail-closed from the first request
    try:
        client.run_heartbeat_loop()    # keeps the workspace online
    finally:
        server.stop()

if __name__ == "__main__":
    main()

# Need help?
#   Where to install: https://git.moleculesai.app/api/packages/molecule-ai/pypi/simple/molecule-ai-sdk/
#   Documentation: https://doc.moleculesai.app/docs/guides/external-agent-registration
#   Common errors:
#     • 401 from /heartbeat — AUTH_TOKEN expired or wrong workspace_id.
#       Use Canvas → External Connection → Rotate credentials to revoke the
#       old token and mint a replacement without recreating the workspace.
#     • INBOUND_URL not reachable from platform — map the public HTTPS URL
#       to the stable loopback listener at 127.0.0.1:8080. For example,
#       run "ngrok http 8080" and set INBOUND_URL to the HTTPS URL it prints,
#       or configure the equivalent Cloudflare Tunnel / reverse-proxy route.
`

// externalHermesChannelTemplate — install snippet for operators whose
// external agent IS a hermes-agent session. Routes the workspace's
// A2A traffic into the running hermes gateway as platform messages
// via the molecule-channel plugin.
//
// The plugin (molecule-ai/hermes-channel-molecule on Gitea) is a hermes
// platform adapter that:
//  1. Spawns “python -m molecule_runtime.mcp_cli“ as the standalone
//     stdio MCP subprocess. That entry point owns external registration,
//     heartbeat, inbox polling, and the MCP tool server.
//  2. Long-polls “wait_for_message“ through that subprocess.
//  3. Dispatches each inbound activity into the hermes gateway as a
//     MessageEvent — same code path Telegram/Discord use.
//  4. Outbound replies route via “send_message_to_user“ (canvas
//     user) or “delegate_task“ (peer agent) MCP tool calls.
//
// Result: hermes gets push parity with Claude Code and codex —
// canvas messages and peer A2A arrive as conversation turns mid-session,
// not just at the start of a new “hermes“ invocation.
//
// Plugin uses the upstream “register_platform“ API shipped by
// NousResearch/hermes-agent#17751 (merged 2026-04-30) and falls back
// to the legacy “register_platform_adapter“ shape on older forks —
// same wheel installs cleanly on stock or patched hermes-agent.
const externalHermesChannelTemplate = tokenGuardShell + `# Hermes channel — bridges this workspace's A2A traffic into your
# hermes-agent session. No tunnel/public URL needed (long-poll based,
# same shape as the Claude Code channel).
#
# The plugin registers the fixed Hermes platform name "molecule" and reads
# one workspace identity from the environment. One Hermes gateway therefore
# connects to one Molecule workspace. To connect another workspace, run a
# separate Hermes gateway with its own config directory and environment;
# inventing a workspace-specific platform key is silently ignored by Hermes.
#
# Prereq: a hermes-agent install on the target machine. Latest builds
# (post #17751) ship the platform-plugin API natively; older ones are
# also supported via the plugin's dual-mode fallback.
#
# 1. Install the runtime + plugin:
` + externalWorkspaceRuntimeInstall + `
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
python3 -m pip install --no-deps 'git+https://git.moleculesai.app/molecule-ai/hermes-channel-molecule.git@d9028c6690394390f3a2b8211a6f6cdc3681971c'
RUNTIME_INSTALL_STATUS=$?
if [ "$RUNTIME_INSTALL_STATUS" -ne 0 ]; then
  MOLECULE_TOKEN_OK=0
  echo "molecule: Hermes plugin install failed; config and gateway restart were skipped." >&2
fi

# 2. Export the workspace credentials. Guarded: ` + "`hermes gateway --replace`" + `
#    below REPLACES a running gateway, so exporting a placeholder token and
#    restarting would take a working channel offline with no way back to the
#    real token (it is shown once). These exports affect this shell; if Hermes
#    runs under a service manager, persist the same three values in that
#    service's environment before restarting it.
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
export MOLECULE_WORKSPACE_ID={{WORKSPACE_ID}}
export MOLECULE_PLATFORM_URL={{PLATFORM_URL}}
export MOLECULE_WORKSPACE_TOKEN="{{AUTH_TOKEN}}"
fi

# 3. Edit ~/.hermes/config.yaml. Current Hermes refuses non-bundled pip
#    plugins unless they are explicitly enabled, and both current and legacy
#    Hermes read platform configuration from top-level platforms (the similarly
#    named internal storage field is NOT a YAML key). Merge these entries into any
#    existing plugins.enabled list and top-level platforms map:
#
#      plugins:
#        enabled:
#          - molecule
#      platforms:
#        molecule:
#          enabled: true
#
#    Do not append duplicate plugins: or platforms: keys; YAML duplicate-key
#    collapse can silently discard the working configuration. On current
#    Hermes, ` + "`hermes plugins enable molecule`" + ` is the CLI equivalent
#    of adding molecule to plugins.enabled.

# 4. Restart the hermes gateway (skipped without a real token — see above):
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
hermes gateway --replace
RUNTIME_INSTALL_STATUS=$?
if [ "$RUNTIME_INSTALL_STATUS" -ne 0 ]; then
  MOLECULE_TOKEN_OK=0
  echo "molecule: Hermes gateway restart failed; inspect ~/.hermes/gateway.log before retrying." >&2
fi
fi

# Inbound canvas messages + peer A2A now arrive as MessageEvents —
# same dispatch path Telegram/Discord/Slack use. The agent replies via
# send_message_to_user / delegate_task MCP tool calls through the plugin's
# standalone molecule_runtime.mcp_cli subprocess, which also owns external
# registration, heartbeat, and inbox polling.
#
# Source + issue tracker:
# https://git.moleculesai.app/molecule-ai/hermes-channel-molecule

# Need help?
#   Documentation: https://doc.moleculesai.app/docs/guides/external-agent-registration
#   Common errors:
#     • Gateway start failure — tail ~/.hermes/gateway.log. Duplicate
#       top-level plugins: or platforms: keys can discard working YAML;
#       merge into the existing maps instead of appending another block.
#     • Plugin not discovered after install — pip show hermes-channel-molecule
#       to confirm install, then run ` + "`hermes plugins enable molecule`" + `
#       (current Hermes) or verify platforms.molecule is enabled.
fi
[ "$RUNTIME_INSTALL_STATUS" -eq 0 ]
fi
`

// externalCodexTemplate — for operators whose external agent is a
// codex CLI (@openai/codex) session. Wires the molecule_runtime A2A
// MCP server into codex's config.toml so the agent can call
// list_peers / delegate_task / send_message_to_user / commit_memory,
// AND surfaces the codex-channel-molecule bridge daemon for inbound
// push parity.
//
// Push parity:
//   - Outbound (codex calls platform tools) — works via the wired
//     MCP server (step 2 below).
//   - Inbound (canvas messages and peer-initiated tasks wake the
//     codex agent) — works via codex-channel-molecule (step 3),
//     which long-polls the platform inbox and runs `codex exec
//     --resume <session>` per inbound message. Each turn is a fresh
//     subprocess but per-thread session continuity is preserved on
//     disk so conversation context survives.
//
// Long-term: when openai/codex#17543 lands (codex MCP runtime routes
// inbound notifications/* into the active session as Op::UserInput),
// the bridge daemon becomes redundant — the wired MCP server in
// step 2 will deliver push natively. Until then, run both.
const externalCodexTemplate = tokenGuardShell + `# Codex external setup — outbound tools (MCP) + inbound push (bridge).
# For operators whose external agent is a codex CLI (@openai/codex)
# session.
#
# Multi-workspace: the TOML table name is workspace-specific
# ("{{MCP_SERVER_NAME}}") so two molecule workspaces can coexist in one
# ~/.codex/config.toml — TOML rejects duplicate
# [mcp_servers.<name>] tables, so re-using a bare "molecule" name for a
# second workspace would either break codex parsing or silently
# overwrite the first. Re-running this snippet for another workspace
# ADDS a sibling table instead.

# 1. Install the workspace runtime, codex CLI, and bridge daemon:
` + externalWorkspaceRuntimeInstall + `
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
npm install -g @openai/codex@latest && \
  python3 -m pip install --no-deps 'git+https://git.moleculesai.app/molecule-ai/codex-channel-molecule.git@876e91c46e1ce240cdaf96a720a2864c23bf52a0'
RUNTIME_INSTALL_STATUS=$?
if [ "$RUNTIME_INSTALL_STATUS" -ne 0 ]; then
  MOLECULE_TOKEN_OK=0
  echo "molecule: Codex or bridge install failed; config and agent startup were skipped." >&2
fi
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then

# 2. Wire the molecule MCP server into codex's config.toml — this is
#    the OUTBOUND path (codex calls list_peers / delegate_task /
#    send_message_to_user / commit_memory). The table name
#    ({{MCP_SERVER_NAME}}) is workspace-specific; re-running the
#    snippet for a DIFFERENT workspace appends a sibling table without
#    touching the first. Re-running for the SAME workspace produces
#    the same name, so replace the existing block instead of appending.

mkdir -p ~/.codex
# (then open ~/.codex/config.toml in your editor and paste:)
#
# [mcp_servers.{{MCP_SERVER_NAME}}]
# command = "molecule-mcp"
# args = []
# startup_timeout_sec = 30
#
# [mcp_servers.{{MCP_SERVER_NAME}}.env]
# WORKSPACE_ID = "{{WORKSPACE_ID}}"
# PLATFORM_URL = "{{PLATFORM_URL}}"
# MOLECULE_WORKSPACE_TOKEN = "{{AUTH_TOKEN}}"
#
# Use the "molecule-mcp" console-script wrapper (NOT
# "python3 -m molecule_runtime.a2a_mcp_server"). The wrapper is what
# keeps the workspace ALIVE on the canvas: it POSTs /registry/register
# at startup and runs a 20s heartbeat thread alongside the MCP stdio
# loop. The bare a2a_mcp_server module exposes tools but does NOT
# heartbeat — pointing codex at it leaves the canvas showing this
# workspace as awaiting_agent (OFFLINE) within 60-90s even while
# tools work.

# 3. Run the bridge daemon as a durable background process — this
#    is the INBOUND path. Long-polls the platform inbox and runs
#    "codex exec --resume <session>" per inbound canvas/peer message,
#    routes the assistant reply back via send_message_to_user /
#    delegate_task. Per-thread session continuity persisted to
#    ~/.codex-channel-molecule/sessions.json so conversation context
#    survives daemon restarts.
#
#    Same env-var contract as the MCP server above.
#
#    Without this daemon, codex still works for outbound calls but
#    canvas messages won't wake an idle session — codex's MCP runtime
#    doesn't yet route notifications/* into the chat loop (tracked
#    upstream at openai/codex#17543; when that lands, the bridge
#    becomes redundant).

#    Guarded: a placeholder token here would start a daemon that 401s in a
#    loop against the inbox — and if one is already running, you would be
#    left believing the new one works.
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
mkdir -p ~/.codex-channel-molecule
WORKSPACE_ID="{{WORKSPACE_ID}}" \
PLATFORM_URL="{{PLATFORM_URL}}" \
MOLECULE_WORKSPACE_TOKEN="{{AUTH_TOKEN}}" \
nohup codex-channel-molecule >> ~/.codex-channel-molecule/daemon.log 2>&1 &
disown
fi

# 4. Run codex itself for interactive use — molecule tools are
#    available to the agent, and the bridge wakes a non-interactive
#    codex turn for any inbound canvas/peer message:
codex
RUNTIME_INSTALL_STATUS=$?
if [ "$RUNTIME_INSTALL_STATUS" -ne 0 ]; then
  echo "molecule: Codex exited with an error; inspect its output before retrying." >&2
fi

# Need help?
#   Documentation: https://doc.moleculesai.app/docs/guides/external-agent-registration
#   Common errors:
#     • [mcp_servers.{{MCP_SERVER_NAME}}] not loaded — codex must be ≥ 0.57.
#       Check with ` + "`codex --version`" + `; upgrade via npm install -g @openai/codex@latest.
#     • TOML parse error after re-running setup for the SAME workspace —
#       TOML rejects duplicate [mcp_servers.<name>] tables. Open
#       ~/.codex/config.toml and remove the old block before pasting the
#       new one. (A second molecule workspace gets a DIFFERENT table
#       name, so coexisting workspaces don't conflict.)
#     • Canvas messages don't wake codex — step 3 (codex-channel-molecule
#       bridge daemon) is required for inbound push. Check
#       pgrep -f codex-channel-molecule and tail ~/.codex-channel-molecule/daemon.log.
fi
fi
[ "$RUNTIME_INSTALL_STATUS" -eq 0 ]
fi
`

// externalOpenClawTemplate — for operators whose external agent is an
// openclaw session. Wires the molecule MCP server via openclaw's
// `mcp set` config + starts the openclaw gateway on loopback.
//
// Like the codex tab, this is outbound-only. Full push parity on an
// external openclaw would need a sessions.steer bridge daemon (the
// equivalent of hermes-channel-molecule for openclaw). Tracked
// separately; outbound tools is the first cut.
// externalKimiTemplate — complete poll-based external setup for Kimi CLI.
// Includes register + heartbeat + inbound activity polling + reply via
// /notify. No public URL needed (NAT-safe). Operators paste once and run
// in a background terminal or via launchd.
//
// Two defects this template used to have, both of which lied to the operator:
//
//  1. The bridge script was written `[ -f … ] || cat > …`. Never clobbering the
//     operator's edits is right (the reply logic is a stub they are meant to
//     rewrite) — but the consequence was that once they had ANY kimi_bridge.py, a
//     platform FIX to that script could never reach them: no copy of the current
//     version existed on their disk, and nothing told them a newer one was out.
//     Now the current script is written to kimi_bridge.py.dist every run and
//     installed as kimi_bridge.py only when absent, so edits survive AND upgrades
//     are visible + diffable.
//
//  2. `[ -s …/kimi_bridge.py ] && echo "already exists — left untouched"` sat AFTER
//     the line that creates the file, so the file always existed by the time the
//     test ran. It printed "already exists — left untouched" on the FIRST run too,
//     about a file it had just written. A message that is true no matter what
//     happened tells you nothing about what happened. It now distinguishes the three
//     real cases (installed / current / kept-yours-and-here-is-the-diff).
//
// It also warns on an already-running bridge: the config dir was re-keyed from the
// workspace-NAME slug to the WORKSPACE_ID, so an operator who set up before that
// change still has a bridge polling out of a directory this snippet never mentions.
// Re-running would silently leave them with TWO bridges long-polling one inbox —
// every inbound message processed twice, the user answered twice.
//
// Pinned by TestKimiSnippet_ShipsTheCurrentBridgeAndTellsTheTruth.
const externalKimiTemplate = tokenGuardShell + `# Kimi CLI external setup — register + heartbeat + inbound poll + reply.
# For operators whose external agent is a Kimi CLI session.
# No public URL needed; runs behind NAT in poll mode.

# 1. Install the workspace runtime wheel (provides HTTP client):
` + externalWorkspaceRuntimeInstall + `
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then

# 2. Save credentials and the bridge script. The config dir is keyed by
#    WORKSPACE_ID (unique by construction) — NOT by the workspace-name
#    slug, which two identically-named workspaces share and would collide
#    on. The env file is rewritten every run (that is the credential-
#    refresh path after a rotate); the bridge script is handled in step 3, which
#    never clobbers your edits but DOES ship you the current version to diff.
#    The env file is REWRITTEN in place, so a tokenless re-show paste would
#    replace a working credential with a dead one — hence the guard.
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
mkdir -p ~/.molecule-ai/kimi-{{WORKSPACE_ID}}
chmod 700 ~/.molecule-ai/kimi-{{WORKSPACE_ID}}
cat > ~/.molecule-ai/kimi-{{WORKSPACE_ID}}/env <<'EOF'
WORKSPACE_ID={{WORKSPACE_ID}}
PLATFORM_URL={{PLATFORM_URL}}
MOLECULE_WORKSPACE_TOKEN={{AUTH_TOKEN}}
EOF
chmod 600 ~/.molecule-ai/kimi-{{WORKSPACE_ID}}/env
fi

# 3. The bridge script. Written to kimi_bridge.py.dist EVERY run, and installed as
#    kimi_bridge.py only if you don't already have one.
#
#    Why .dist: you are meant to EDIT kimi_bridge.py (the reply logic near the
#    bottom is a stub), so re-running must never clobber your edits. But the old
#    ` + "`[ -f ] || cat >`" + ` form meant the opposite failure: once you had ANY
#    kimi_bridge.py, a platform fix to this script could never reach you — there was
#    no version of it on your disk to compare against, and nothing told you a newer
#    one existed. Shipping the current one alongside yours gives you both: your edits
#    survive, and you can actually see what changed.
mkdir -p ~/.molecule-ai/kimi-{{WORKSPACE_ID}}
cat > ~/.molecule-ai/kimi-{{WORKSPACE_ID}}/kimi_bridge.py.dist <<'PYEOF'
#!/usr/bin/env python3
"""Kimi bridge — keeps workspace online and polls for canvas messages."""
import json, logging, time
from pathlib import Path
import httpx

ENV = Path.home() / ".molecule-ai" / "kimi-{{WORKSPACE_ID}}" / "env"
HEARTBEAT_INTERVAL = 20
POLL_INTERVAL = 5

def load_env():
    env = {}
    for line in ENV.read_text().splitlines():
        if "=" in line and not line.startswith("#"):
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip()
    return env

def hdrs(url, token):
    return {"Authorization": f"Bearer {token}", "Origin": url, "Content-Type": "application/json"}

def register(client, url, ws, tok):
    r = client.post(f"{url}/registry/register", json={
        "id": ws, "url": "", "agent_card": {"name": "mac-laptop-kimi", "skills": []},
        "delivery_mode": "poll",
    }, headers=hdrs(url, tok))
    r.raise_for_status()
    logging.info("registered %s", ws)

def heartbeat(client, url, ws, tok, start):
    r = client.post(f"{url}/registry/heartbeat", json={
        "workspace_id": ws, "error_rate": 0.0, "sample_error": "",
        "active_tasks": 0, "current_task": "", "uptime_seconds": int(time.time() - start),
    }, headers=hdrs(url, tok))
    r.raise_for_status()

def poll_inbound(client, url, ws, tok, since_id):
    # include=peer_info opts into Layer 1's row-level projection so each
    # polled activity carries peer_name, peer_role, agent_card_url, and
    # attachments[] inline (when source_id resolves to a peer / when the
    # message included a file). Pre-Layer-1 platforms ignore unknown query
    # params and return the bare row shape, so this is back-compat. Use
    # the extra fields in your reply logic — e.g. address the sender by
    # peer_name rather than UUID, or Read attached files via the workspace:
    # URIs in attachments[].
    params = {"limit": "50", "include": "peer_info"}
    if since_id:
        params["since_id"] = since_id
    else:
        params["since_secs"] = "30"
    r = client.get(f"{url}/workspaces/{ws}/activity", params=params, headers=hdrs(url, tok))
    # Core returns 410 when a durable cursor is invalid or its row has been
    # pruned. Reset to the bounded cold-start path instead of retrying the same
    # dead since_id forever.
    if r.status_code == 410:
        logging.warning("activity cursor %s expired; resetting to bounded cold-start", since_id)
        return [], ""
    r.raise_for_status()
    return r.json(), since_id

def order_activity_items(items, since_id):
    """Normalize /activity into chronological order without mutating it."""
    # Cursor reads are ASC. A no-cursor recent feed is DESC, so reverse only
    # the bounded cold-start page; the final processed id is then the newest.
    return list(items) if since_id else list(reversed(items))

def send_reply(client, url, ws, tok, text):
    r = client.post(f"{url}/workspaces/{ws}/notify", json={"message": text}, headers=hdrs(url, tok))
    r.raise_for_status()
    logging.info("reply sent: %s", text[:80])

def extract_user_text(item):
    """Pull the user message text from an activity log request_body."""
    try:
        body = item.get("request_body") or {}
        parts = body.get("params", {}).get("message", {}).get("parts", [])
        return " ".join(p.get("text", "") for p in parts if p.get("text"))
    except Exception:
        return ""

def main():
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    start = time.time()
    since_id = ""
    last_beat = 0
    while True:
        try:
            e = load_env()
            purl, ws, tok = e["PLATFORM_URL"], e["WORKSPACE_ID"], e["MOLECULE_WORKSPACE_TOKEN"]
            with httpx.Client(timeout=10.0) as c:
                # Heartbeat every HEARTBEAT_INTERVAL seconds
                if time.time() - last_beat >= HEARTBEAT_INTERVAL:
                    register(c, purl, ws, tok)
                    heartbeat(c, purl, ws, tok, start)
                    last_beat = time.time()

                # Poll for new canvas messages
                items, since_id = poll_inbound(c, purl, ws, tok, since_id)
                items = order_activity_items(items, since_id)
                for item in items:
                    since_id = item["id"]
                    src = item.get("source_id")
                    method = item.get("method") or ""
                    # Skip our own /notify replies and agent-originated traffic
                    if method == "notify" or src is not None:
                        continue
                    text = extract_user_text(item)
                    if text:
                        logging.info("INBOUND from canvas: %s", text)
                        # Replace the echo below with your own logic:
                        send_reply(c, purl, ws, tok, f"Echo: {text}")
            time.sleep(POLL_INTERVAL)
        except Exception as exc:
            logging.warning("loop failed: %s", exc)
            time.sleep(5)

if __name__ == "__main__":
    main()
PYEOF

# Install it if you have none; otherwise leave YOURS alone and say whether it is
# current.
KIMI_DIR=~/.molecule-ai/kimi-{{WORKSPACE_ID}}
if [ ! -f "$KIMI_DIR/kimi_bridge.py" ]; then
  cp "$KIMI_DIR/kimi_bridge.py.dist" "$KIMI_DIR/kimi_bridge.py"
  echo "molecule: installed kimi_bridge.py (edit the reply logic near the bottom)"
elif cmp -s "$KIMI_DIR/kimi_bridge.py" "$KIMI_DIR/kimi_bridge.py.dist"; then
  echo "molecule: kimi_bridge.py is current — unchanged"
else
  echo "molecule: kept YOUR kimi_bridge.py (it differs from the version this platform ships)." >&2
  echo "molecule: the current one is $KIMI_DIR/kimi_bridge.py.dist — diff it if you want the fixes:" >&2
  echo "molecule:   diff -u $KIMI_DIR/kimi_bridge.py $KIMI_DIR/kimi_bridge.py.dist" >&2
fi
chmod +x "$KIMI_DIR/kimi_bridge.py"

# One bridge per workspace. If a bridge is ALREADY polling this workspace, starting
# a second one double-processes every inbound message: both long-poll the same
# inbox, both wake kimi, and the user gets two replies.
#
# This bites hardest across the config-dir re-key. The dir used to be keyed by the
# workspace-NAME slug and is now keyed by WORKSPACE_ID (two same-named workspaces
# collided on the old scheme). An operator who set up before that change still has a
# bridge running out of the OLD directory — invisible to every path check here,
# because the path it runs from no longer exists in this snippet. Re-running would
# quietly leave them with two.
KIMI_RUNNING=$(pgrep -f 'kimi_bridge\.py' 2>/dev/null | tr '\n' ' ')
if [ -n "$KIMI_RUNNING" ]; then
  echo "molecule: a kimi bridge is ALREADY running (pid(s):$KIMI_RUNNING)." >&2
  echo "molecule: two bridges on one workspace double-process every inbound message." >&2
  echo "molecule: if that is an older bridge for THIS workspace (the config dir was" >&2
  echo "molecule: re-keyed from the name-slug to the workspace id), stop it first:" >&2
  echo "molecule:   pkill -f kimi_bridge.py" >&2
fi

# 3. Start the bridge (run in a persistent terminal or via launchd):
python3 ~/.molecule-ai/kimi-{{WORKSPACE_ID}}/kimi_bridge.py
RUNTIME_INSTALL_STATUS=$?
if [ "$RUNTIME_INSTALL_STATUS" -ne 0 ]; then
  echo "molecule: Kimi bridge exited with an error; inspect the output before retrying." >&2
fi

# What the script does:
#   • Registers the workspace in poll mode (no public URL needed)
#   • Heartbeats every 20s to keep STATUS = online on the canvas
#   • Polls /workspaces/:id/activity?include=peer_info every 5s — Layer 1
#     enrichment surfaces peer_name / peer_role / agent_card_url /
#     attachments[] inline on each polled row when applicable
#   • Echo-replies via POST /workspaces/:id/notify
#
# To change the reply logic, edit the send_reply() call inside the loop.
# Each polled item has top-level peer_name / peer_role / agent_card_url
# fields (peer_agent rows) and attachments[] (any kind) when Layer 1 is
# enabled on the platform — use them to disambiguate senders and to Read
# attached files via the workspace: URIs.
# To send a one-off reply from another terminal:
#   curl -fsS -X POST "{{PLATFORM_URL}}/workspaces/{{WORKSPACE_ID}}/notify" \
#     -H "Authorization: Bearer $(grep '^MOLECULE_WORKSPACE_TOKEN=' ~/.molecule-ai/kimi-{{WORKSPACE_ID}}/env | cut -d= -f2-)" \
#     -H "Content-Type: application/json" \
#     -d '{"message":"Hello from Kimi"}'
#
# For push-mode inbound A2A, use the Python SDK tab as an authenticated server
# starting point, then connect its handler to Kimi. It requires a public HTTPS
# endpoint (ngrok / Cloudflare Tunnel) and is not a turnkey Kimi bridge.
#
# Need help?
#   Documentation: https://doc.moleculesai.app/docs/guides/external-agent-registration
fi
[ "$RUNTIME_INSTALL_STATUS" -eq 0 ]
fi
`

const externalOpenClawTemplate = tokenGuardShell + `# OpenClaw MCP config — outbound tool path. For operators whose
# external agent is an openclaw session.
#
# This wires the molecule platform's A2A MCP server into openclaw's
# gateway so the agent can call list_peers / delegate_task /
# send_message_to_user / commit_memory. Inbound A2A push into a
# running openclaw run is not wired here yet — the platform-side
# openclaw template (template-openclaw) implements the full
# sessions.steer push path; an external setup would need the same
# bridge daemon the template uses. The Python SDK tab is an authenticated
# echo-server starting point for implementing that runtime-specific bridge;
# it does not steer an OpenClaw session by itself.
#
# Multi-workspace: each workspace registers under a workspace-specific
# MCP server name ("{{MCP_SERVER_NAME}}"). OpenClaw keys MCP servers by
# name under mcp.servers in ~/.openclaw/openclaw.json, so re-running with
# a bare "molecule" name would overwrite the prior workspace's entry.
# Re-run this snippet for another workspace to ADD a sibling entry
# instead.

# 1. Install the pinned workspace runtime wheel + openclaw CLI. The
#    canonical 0.4.36 package includes the "molecule-mcp" console script,
#    which keeps the workspace ALIVE on canvas (register-on-startup +
#    20s heartbeat).
` + externalWorkspaceRuntimeInstall + `
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
npm install -g openclaw@latest
RUNTIME_INSTALL_STATUS=$?
if [ "$RUNTIME_INSTALL_STATUS" -ne 0 ]; then
  MOLECULE_TOKEN_OK=0
  echo "molecule: OpenClaw install failed; config and agent startup were skipped." >&2
fi
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then

# 2. Onboard openclaw against your model provider (one-time setup).
#    --non-interactive needs an explicit --provider + --model so it
#    doesn't prompt; pick what matches your API key. Skip step 2 if
#    you've already onboarded on this host.
#
#    openclaw onboard --non-interactive \
#      --provider openai \
#      --model gpt-5

# 3. Wire the molecule MCP server. Workspace id, platform URL and the
# auth token are all stamped server-side — run the block as-is.
#
# Use the "molecule-mcp" console-script wrapper (NOT
# "python3 -m molecule_runtime.a2a_mcp_server"). The wrapper is what
# keeps the workspace ALIVE on the canvas: it POSTs /registry/register
# at startup and runs a 20s heartbeat thread alongside the MCP stdio
# loop. The bare a2a_mcp_server module exposes tools but does NOT
# heartbeat — pointing openclaw at it leaves the canvas showing this
# workspace as awaiting_agent (OFFLINE) within 60-90s even while
# tools work.
# ` + "`openclaw mcp set`" + ` OVERWRITES the entry for this server name, so a
# tokenless re-show paste would replace a working credential with a dead one —
# hence the MOLECULE_TOKEN_OK guard at the top of this block.
MOLECULE_WORKSPACE_TOKEN="{{AUTH_TOKEN}}"
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
openclaw mcp set {{MCP_SERVER_NAME}} "$(cat <<EOF
{
  "command": "molecule-mcp",
  "args": [],
  "env": {
    "WORKSPACE_ID": "{{WORKSPACE_ID}}",
    "PLATFORM_URL": "{{PLATFORM_URL}}",
    "MOLECULE_WORKSPACE_TOKEN": "$MOLECULE_WORKSPACE_TOKEN"
  }
}
EOF
)"
RUNTIME_INSTALL_STATUS=$?
if [ "$RUNTIME_INSTALL_STATUS" -ne 0 ]; then
  MOLECULE_TOKEN_OK=0
  echo "molecule: OpenClaw MCP configuration failed; gateway and agent startup were skipped." >&2
fi
fi

# 4. Start the openclaw gateway as a durable background process. Keep the
#    default profile used by mcp set/onboard/agent above: --dev switches to
#    ~/.openclaw-dev, where the configured Molecule MCP server is absent.
#    A bare '&' dies when the terminal closes; nohup + log file keeps
#    the gateway alive across logout. For systemd-managed hosts,
#    register a unit instead. This step and the agent turn are skipped if
#    the MCP configuration above failed.
if [ "$MOLECULE_TOKEN_OK" = "1" ]; then
nohup openclaw gateway --port 18789 --bind loopback \
  > ~/.openclaw/gateway.log 2>&1 &
disown

# 5. Run an agent turn — molecule tools are now available:
openclaw agent --message "list my peers"
RUNTIME_INSTALL_STATUS=$?
if [ "$RUNTIME_INSTALL_STATUS" -ne 0 ]; then
  echo "molecule: OpenClaw agent turn failed; inspect the gateway log before retrying." >&2
fi
fi

# Need help?
#   Documentation: https://doc.moleculesai.app/docs/guides/external-agent-registration
#   Common errors:
#     • Gateway not starting — tail ~/.openclaw/gateway.log. The loopback
#       bind requires :18789 to be free; check with ` + "`lsof -iTCP:18789`" + `.
#     • ` + "`openclaw mcp set`" + ` rejected — the heredoc generates JSON.
#       Inspect the stored entry with
#       ` + "`openclaw mcp show {{MCP_SERVER_NAME}} --json`" + ` (or list all
#       entries with ` + "`openclaw mcp list`" + `), then re-run mcp set after
#       correcting it. OpenClaw stores these entries under mcp.servers in
#       ~/.openclaw/openclaw.json.
fi
fi
[ "$RUNTIME_INSTALL_STATUS" -eq 0 ]
fi
`
