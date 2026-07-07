/**
 * The setup scene's wire sequence (design SSOT §4) — ORDER IS LOAD-BEARING:
 *
 *   1. PUT /settings/secrets {key, value}       — key BEFORE create: the
 *      global-secrets auto-restart fan-out excludes kind='platform'
 *      (secrets.go:713-761), so a key written after the ensure-triggered
 *      provision never reaches the concierge without an explicit restart.
 *      Written first, the provision picks it up for free. PUT, not POST —
 *      the same wire shape lib/api/secrets.ts createSecret/updateSecret use.
 *      Skipped when the key is already configured and untouched.
 *   2. PATCH /workspaces/{rootId} {runtime}     — ONLY when the pick differs
 *      from the row's seeded runtime: the ensure upsert deliberately
 *      preserves an existing root's runtime (platform_agent.go:1461-1465),
 *      so runtime rides the standard runtime-change path.
 *   3. POST /admin/org/platform-agent/ensure    — {name: "Enter OS Agent",
 *      model, force:true}: repair the EXISTING root ('repaired' path),
 *      validate (runtime, model), write the MODEL workspace_secret, trigger
 *      the provision. Idempotent — Retry re-runs ensure alone.
 *
 * All calls ride the shared api module (platformAuthHeaders inside).
 */
import { api } from "@/lib/api";
import { PLATFORM_AGENT_NAME } from "./setup-scene-lib";

/** The api-module subset the sequence needs — injectable for tests. */
export type ConfigureClient = {
  put: (path: string, body?: unknown) => Promise<unknown>;
  patch: (path: string, body?: unknown) => Promise<unknown>;
  post: (path: string, body?: unknown) => Promise<unknown>;
};

export interface ConfigureInput {
  /** Platform root id; null = row missing (defensive ensure-created path). */
  rootId: string | null;
  /** The root row's runtime at gate time (PATCH fires only on change). */
  seededRuntime: string;
  /** The user's runtime pick. */
  runtime: string;
  /** The user's model pick (REQUIRED — no default by design). */
  model: string;
  /** Secret key NAME (the selected provider's auth_env[0]). */
  keyName: string;
  /** Secret value; ignored when skipKeyWrite. */
  keyValue: string;
  /** True when the key is already configured and untouched — skip step 1. */
  skipKeyWrite: boolean;
}

/**
 * Step 3 alone — also the Retry path (design SSOT §3 step 5: retry =
 * re-run ensure, idempotent + debounced by the caller).
 *
 * `runtime` is included ONLY for the defensive missing-root case (rootId
 * null): the ensure 'created' path stamps the payload runtime, and without
 * it a created root would ignore the user's pick. For an existing root the
 * field is deliberately omitted — the upsert preserves runtime and the pick
 * rides PATCH instead (§4).
 */
export async function runEnsure(
  model: string,
  runtimeForCreate: string | null,
  client: ConfigureClient = api,
): Promise<void> {
  await client.post("/admin/org/platform-agent/ensure", {
    name: PLATFORM_AGENT_NAME,
    force: true,
    // Omit an empty model rather than sending model:"" — absent means
    // "today's behavior" server-side (re-provision with the existing MODEL
    // secret; the resume-Retry path before any pick was made).
    ...(model !== "" ? { model } : {}),
    ...(runtimeForCreate !== null ? { runtime: runtimeForCreate } : {}),
  });
}

/** Run the full §4 sequence. Errors propagate to the caller (the scene maps
 *  them to §8 error states). */
export async function runConfigureSequence(
  input: ConfigureInput,
  client: ConfigureClient = api,
): Promise<void> {
  if (!input.skipKeyWrite) {
    await client.put("/settings/secrets", {
      key: input.keyName,
      value: input.keyValue,
    });
  }
  if (input.rootId !== null && input.runtime !== input.seededRuntime) {
    await client.patch(`/workspaces/${input.rootId}`, {
      runtime: input.runtime,
    });
  }
  await runEnsure(input.model, input.rootId === null ? input.runtime : null, client);
}
