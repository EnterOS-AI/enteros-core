import type { NextConfig } from "next";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";

// Load NEXT_PUBLIC_* vars from the monorepo root .env so a fresh
// `pnpm dev` works without a per-developer canvas/.env.local. Next.js
// only auto-loads .env from the project root by default — but our
// canonical config (NEXT_PUBLIC_PLATFORM_URL, NEXT_PUBLIC_WS_URL,
// MOLECULE_ENV, etc.) lives at the monorepo root, gitignored, shared
// by the Go platform binary. Without this, the canvas falls back to
// `window.location` (`ws://localhost:3000/ws`) and the WS pill stays
// "Reconnecting" forever because Next.js dev doesn't serve /ws.
//
// Mirrors workspace-server/cmd/server/dotenv.go's monorepo-rooted .env
// loader. Both processes look for the SAME marker (`workspace-server/
// go.mod`) so a developer renaming or relocating the repo only has to
// update one heuristic. Production is unaffected: `output: "standalone"`
// bakes resolved env into the build, and the marker file isn't shipped.
loadMonorepoEnv();
// Build-time matched-pair guard for the local-development and ephemeral-E2E
// ADMIN_TOKEN / NEXT_PUBLIC_ADMIN_TOKEN compatibility path. When local Canvas
// and workspace-server run as separate processes, the public client value must
// match the server bearer gate or every workspace API call 401s silently.
// Production SaaS builds must leave NEXT_PUBLIC_ADMIN_TOKEN empty and use the
// verified control-plane session; a tenant admin secret must never be baked
// into public JavaScript.
//
// Pre-fix the matched-pair contract was descriptive only (a comment in
// .env): future devs/agents could re-misconfigure with one of the two
// unset and silently 401. Closes the post-PR-#174 self-review gap.
//
// Warn-only (not exit): local dev remains easy to diagnose, while an
// accidentally inherited server secret does not break a production image
// build. The diagnostic explicitly tells production builders to remove the
// values instead of publishing the server secret.
checkAdminTokenPair();

const nextConfig: NextConfig = {
  output: "standalone",
};

export default nextConfig;

function loadMonorepoEnv() {
  const root = findMonorepoRoot(__dirname);
  if (!root) return;
  const envPath = join(root, ".env");
  if (!existsSync(envPath)) return;
  const body = readFileSync(envPath, "utf8");
  let loaded = 0;
  let skipped = 0;
  for (const line of body.split(/\r?\n/)) {
    const kv = parseLine(line);
    if (!kv) continue;
    const [k, v] = kv;
    // Existing env wins. NOTE: an explicitly-set empty string
    // (`KEY=` exported from a parent shell, where Node represents it
    // as `""` not `undefined`) counts as "set" — we keep the empty
    // value rather than backfilling from the file. Matches Go's
    // os.LookupEnv check in workspace-server/cmd/server/dotenv.go so
    // both processes treat the same input identically. Operators who
    // want the file value to win must `unset KEY` in the launching
    // shell.
    if (process.env[k] !== undefined) {
      skipped++;
      continue;
    }
    process.env[k] = v;
    loaded++;
  }
  // eslint-disable-next-line no-console
  console.log(
    `[next.config] loaded ${loaded} vars from ${envPath} (${skipped} already set in env)`,
  );
}

// Boot-time matched-pair guard. Runs after .env has been loaded so the
// check sees the post-load state. The two env vars must be set or
// unset together; one-without-the-other is the silent-401 footgun.
//
// Treats empty string ("") as unset. An explicitly-empty `KEY=` in
// .env counts as set-to-empty in `process.env`, but for auth purposes
// an empty bearer token is equivalent to no token — so both
// `ADMIN_TOKEN=` and an unset ADMIN_TOKEN are equivalent relative to
// the matched-pair invariant.
//
// Returns void; side effect is the console.error warning. Kept as a
// separate function (exported) so a future test can reset env, call
// this, and assert on captured stderr.
export function checkAdminTokenPair(): void {
  const serverSet = !!process.env.ADMIN_TOKEN;
  const clientSet = !!process.env.NEXT_PUBLIC_ADMIN_TOKEN;
  if (serverSet === clientSet) return;
  // Distinct messages so the operator can tell which half is missing
  // — the fix is symmetric (set the other one) but the diagnostic
  // mentions which side is currently set so they don't have to grep.
  if (serverSet && !clientSet) {
    // eslint-disable-next-line no-console
    console.error(
      "[next.config] ADMIN_TOKEN is set but NEXT_PUBLIC_ADMIN_TOKEN is not — " +
        "for local dev, set the matching public value; for production, remove " +
        "ADMIN_TOKEN from the Canvas build environment (never publish it).",
    );
  } else {
    // eslint-disable-next-line no-console
    console.error(
      "[next.config] NEXT_PUBLIC_ADMIN_TOKEN is set but ADMIN_TOKEN is not — " +
        "for local dev, set the matching server value; for production, remove " +
        "NEXT_PUBLIC_ADMIN_TOKEN from the public Canvas bundle.",
    );
  }
}

function findMonorepoRoot(start: string): string | null {
  let dir = start;
  for (let i = 0; i < 6; i++) {
    if (existsSync(join(dir, "workspace-server", "go.mod"))) return dir;
    const parent = dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  return null;
}

// Mirror of workspace-server/cmd/server/dotenv.go's parseDotEnvLine
// — same rules so the two loaders agree on every line in the shared
// .env. If you change one parser, change the other.
function parseLine(raw: string): [string, string] | null {
  let line = raw.replace(/^﻿/, "").trim();
  if (line === "" || line.startsWith("#")) return null;
  // `export ` prefix uses a literal space — `export\tFOO=bar` with a
  // tab is intentionally rejected, matching the Go mirror in
  // workspace-server/cmd/server/dotenv.go. Shells emit the prefix
  // with a space; tabs would only appear in hand-mangled files.
  if (line.startsWith("export ")) line = line.slice("export ".length).trimStart();
  const eq = line.indexOf("=");
  if (eq <= 0) return null;
  const k = line.slice(0, eq).trim();
  let v = line.slice(eq + 1).replace(/^[ \t]+/, "");
  if (v.length >= 2 && (v[0] === '"' || v[0] === "'")) {
    const quote = v[0];
    const end = v.indexOf(quote, 1);
    if (end >= 0) return [k, v.slice(1, end)];
    // unterminated — fall through to bare-value handling
  }
  for (let i = 0; i < v.length; i++) {
    if (v[i] !== "#") continue;
    if (i === 0 || v[i - 1] === " " || v[i - 1] === "\t") {
      v = v.slice(0, i);
      break;
    }
  }
  return [k, v.trim()];
}
