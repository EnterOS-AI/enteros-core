#!/usr/bin/env node
/**
 * Cross-repo manifest check — published @molecule-ai/mcp-server vs the
 * MCP-plugin delivery verb contract (core#3082).
 *
 * WHY THIS EXISTS
 * ───────────────
 * molecule-core's online/degraded gate marks a concierge healthy only when the
 * management MCP surfaces a known workspace-creation verb, derived from the
 * molecule-ai-sdk MCP-plugin delivery contract
 * (`mcp__<mcp_server_name>__<verb>` for verb ∈ required_tools ∪
 * transitional_tool_aliases). The verb is PRODUCED by a SEPARATE repo
 * (@molecule-ai/mcp-server) and DELIVERED as a PUBLISHED npm build. Nothing
 * stops a published build from skewing away from the contract — which is
 * exactly the staging incident: a stale published build exposed only
 * `provision_workspace` while core had hand-asserted `create_workspace`, so
 * every freshly provisioned concierge silently degraded.
 *
 * The producer-binding test in @molecule-ai/mcp-server catches a rename in
 * SOURCE. THIS check closes the remaining gap: it resolves the ACTUAL tool
 * manifest of the PUBLISHED build core would deliver and asserts it satisfies
 * the contract — so a stale/skewed published build fails core CI BEFORE any
 * tenant provisions against it.
 *
 * HOW IT RESOLVES THE MANIFEST
 * ────────────────────────────
 * The workflow installs @molecule-ai/mcp-server from the Gitea npm registry
 * (read:package auth) into a throwaway dir and passes that dir here. This
 * script loads the installed build's compiled `createServer()` in management
 * mode under a monkeypatched MCP SDK (McpServer.prototype.tool records names),
 * yielding the literal set of tools the published build registers — the same
 * set a live concierge would surface.
 *
 * PROVENANCE HAZARD — resolve the PUBLISHED package, never build from source.
 * The mcp-server's git `main` does NOT carry the management-mode split that the
 * PUBLISHED artifacts have (the verb a concierge actually runs lives only in the
 * published build). Building from `main` here would introspect a tool set the
 * fleet never runs, defeating the check. This script therefore deliberately
 * runs against an INSTALLED published tarball, not a source checkout.
 *
 * ASSERTIONS
 *   FAIL (exit 1) when:
 *     • the installed build can't be loaded / introspected, or
 *     • the management server name != contract.mcp_server_name, or
 *     • the published manifest contains NONE of the accepted verbs
 *       (required_tools ∪ transitional_tool_aliases) — the gate would
 *       fail-close every concierge built from this build (the staging bug).
 *   WARN (exit 0, ::warning) when:
 *     • the canonical verb(s) in required_tools are ABSENT and the gate is
 *       only satisfied by a transitional alias — an early signal that the
 *       published build is behind the canonical verb and that removing the
 *       alias (core task #87) WITHOUT first updating the published build would
 *       re-trigger the degrade. Surfaced loudly, not fatal, so the transitional
 *       window stays mergeable.
 *
 * Usage:
 *   node check-published-mcp-manifest.mjs \
 *     --contract-url <https://.../mcp-plugin-delivery.contract.json> \
 *     --install-dir <dir containing node_modules/@molecule-ai/mcp-server>
 *
 * For hermetic local tests, `--contract <path/to/mcp-plugin-delivery.contract.json>`
 * is still accepted. CI uses `--contract-url` so molecule-core no longer carries
 * a root-level JSON mirror.
 *
 * DEPENDS ON: molecule-core's verb-SSOT PR (feat/3082-mcp-verb-ssot-contract)
 * landing `required_tools` + `transitional_tool_aliases` in the contract. This
 * script reads those fields; without them it falls back to the legacy singular
 * `required_tool` (so it is safe to merge after the contract shape lands).
 */

import { readFileSync, writeFileSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { join, isAbsolute } from "node:path";

// Manifest resolution runs INSIDE the install tree (a child node process whose
// own file lives in <install>/node_modules), NOT from this script's location.
// Why: the installed mcp-server dist is ESM and imports
// "@modelcontextprotocol/sdk" by bare specifier; the SDK has NO "exports" map,
// so Node's ESM resolver and CommonJS require.resolve pick DIFFERENT files (the
// ESM build vs the CJS `main`). The package binds the ESM instance — so we must
// patch the ESM McpServer the package actually loads, and the only resolution
// context guaranteed to match the package's is one rooted in the same
// node_modules. Running the harness from inside the install tree makes every
// bare specifier resolve exactly as the package resolves it, independent of
// this script's location or Node's import.meta.resolve parent-arg behaviour
// (which is version-gated and unreliable for that purpose).

function arg(flag, fallback) {
  const i = process.argv.indexOf(flag);
  return i !== -1 && process.argv[i + 1] ? process.argv[i + 1] : fallback;
}

function fail(msg) {
  console.error(`::error::${msg}`);
  process.exit(1);
}
function warn(msg) {
  console.error(`::warning::${msg}`);
}

const defaultContractURL =
  "https://git.moleculesai.app/molecule-ai/molecule-ai-sdk/raw/branch/main/contracts/mcp/mcp-plugin-delivery.contract.json";
const contractPath = arg("--contract", "");
const contractURL = arg("--contract-url", contractPath ? "" : defaultContractURL);
const installDir = arg("--install-dir", process.cwd());

// ── Load the contract ─────────────────────────────────────────────────────
let contract;
try {
  if (contractPath && contractURL) {
    fail(`Pass only one of --contract or --contract-url, not both.`);
  }
  if (contractURL) {
    const res = await fetch(contractURL, { headers: { "User-Agent": "curl/8.4.0" } });
    if (!res.ok) {
      throw new Error(`HTTP ${res.status} ${res.statusText}`);
    }
    contract = await res.json();
  } else {
    contract = JSON.parse(readFileSync(contractPath, "utf8"));
  }
} catch (e) {
  const source = contractURL || contractPath;
  fail(`Cannot read contract from ${source}: ${e.message}`);
}

const serverName = contract.mcp_server_name;
if (!serverName) fail(`Contract is missing mcp_server_name.`);

// Prefer the plural verb-SSOT fields (post core#3082). Fall back to the legacy
// singular `required_tool` so this script is safe to land before/after the
// contract-shape PR.
let requiredTools = Array.isArray(contract.required_tools) ? contract.required_tools : [];
if (requiredTools.length === 0 && typeof contract.required_tool === "string") {
  requiredTools = [contract.required_tool];
}
const aliases = Array.isArray(contract.transitional_tool_aliases)
  ? contract.transitional_tool_aliases
  : [];
if (requiredTools.length === 0) {
  fail(
    `Contract declares no required workspace-creation verb (required_tools/required_tool both empty) — ` +
      `core would derive an empty accepted set and fail-close every concierge.`,
  );
}
const accepted = [...new Set([...requiredTools, ...aliases])];

// ── Resolve the published manifest via an in-tree child harness ────────────
const absInstall = isAbsolute(installDir) ? installDir : join(process.cwd(), installDir);

// The harness is written into <install>/node_modules so its own module URL is
// inside the install tree — every bare specifier then resolves exactly as the
// installed package resolves it. It patches the ESM McpServer.prototype.tool to
// record names, builds the management server, and prints one JSON line.
const harnessSrc = `
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
const recorded = [];
const origTool = McpServer.prototype.tool;
McpServer.prototype.tool = function (name, ...rest) {
  if (typeof name === "string") recorded.push(name);
  try { return origTool.apply(this, [name, ...rest]); } catch { return undefined; }
};
process.env.MOLECULE_MCP_MODE = "management";
// JEST_WORKER_ID suppresses the package's main() stdio auto-start.
process.env.JEST_WORKER_ID = process.env.JEST_WORKER_ID || "manifest-check";
try {
  const mod = await import("@molecule-ai/mcp-server");
  if (typeof mod.createServer !== "function") {
    console.log(JSON.stringify({ error: "createServer-not-exported" }));
    process.exit(0);
  }
  const srv = mod.createServer();
  const name = srv?.server?._serverInfo?.name ?? srv?.name ?? null;
  console.log(JSON.stringify({ name, tools: [...new Set(recorded)].sort() }));
} catch (e) {
  console.log(JSON.stringify({ error: String(e && e.message ? e.message : e) }));
}
`;

const harnessPath = join(absInstall, "node_modules", ".mcp-manifest-harness.mjs");
let result;
try {
  writeFileSync(harnessPath, harnessSrc, "utf8");
  const proc = spawnSync(process.execPath, [harnessPath], {
    cwd: absInstall,
    encoding: "utf8",
    timeout: 60_000,
  });
  if (proc.status !== 0 && !proc.stdout) {
    fail(`Manifest harness failed (exit ${proc.status}): ${proc.stderr || "(no output)"}`);
  }
  const line = (proc.stdout || "").trim().split("\n").filter(Boolean).pop() || "";
  try {
    result = JSON.parse(line);
  } catch {
    fail(`Manifest harness produced no parseable output. stdout="${proc.stdout}" stderr="${proc.stderr}"`);
  }
} finally {
  try {
    rmSync(harnessPath, { force: true });
  } catch {
    /* best-effort cleanup */
  }
}

if (result.error) {
  fail(`Could not introspect the published build's management manifest: ${result.error}`);
}
const builtName = result.name;
const tools = Array.isArray(result.tools) ? result.tools : [];
if (tools.length === 0) {
  fail(`Published build's management server registered ZERO tools — introspection is unreliable; refusing to pass.`);
}

// ── Assert ─────────────────────────────────────────────────────────────────
console.log(`Published @molecule-ai/mcp-server management server name: ${builtName}`);
console.log(`Published management tool manifest (${tools.length}): ${tools.join(", ")}`);
console.log(`Contract mcp_server_name: ${serverName}`);
console.log(`Contract required_tools: ${requiredTools.join(", ")}`);
console.log(`Contract transitional_tool_aliases: ${aliases.join(", ") || "(none)"}`);

if (builtName && builtName !== serverName) {
  fail(
    `Published management server registers under name "${builtName}" but the contract ` +
      `mcp_server_name is "${serverName}" — the gate derives mcp__${serverName}__<verb>, so ` +
      `NO derived id would match this build.`,
  );
}

const presentAccepted = accepted.filter((v) => tools.includes(v));
if (presentAccepted.length === 0) {
  fail(
    `Published build exposes NONE of the accepted workspace-creation verbs ` +
      `[${accepted.join(", ")}]. The online/degraded gate would fail-close EVERY ` +
      `concierge provisioned from this published build (the staging stale-build degrade). ` +
      `Publish a build that exposes at least one accepted verb before this can merge.`,
  );
}

const presentCanonical = requiredTools.filter((v) => tools.includes(v));
if (presentCanonical.length === 0) {
  warn(
    `Published build exposes only TRANSITIONAL alias verb(s) [${presentAccepted.join(", ")}] — ` +
      `the canonical required verb(s) [${requiredTools.join(", ")}] are ABSENT. The gate passes ` +
      `today only because the transitional alias is still accepted. Removing the alias ` +
      `(core task #87) WITHOUT first publishing a build that exposes the canonical verb would ` +
      `re-trigger the fleet degrade. Publish an updated mcp-server build before closing #87.`,
  );
}

console.log(
  `OK — published build satisfies the contract gate (accepted verbs present: ${presentAccepted.join(", ")}).`,
);
process.exit(0);
