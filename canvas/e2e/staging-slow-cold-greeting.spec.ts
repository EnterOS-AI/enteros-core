/**
 * staging-slow-cold-greeting.spec.ts — browser-level regression guard for the
 * concierge duplicate-reply on a SLOW cold first turn.
 * ============================================================================
 * Companion to the unit guard
 *   canvas/src/components/tabs/chat/hooks/__tests__/useChatHistory.slowColdGreetingRenderDup.test.tsx
 * which drives the REAL merge logic (mergeReconciledMessages / appendMessageDeduped)
 * with the exact server-ts-vs-client-ts 30s-gap scenario. This spec proves the
 * same invariant end-to-end in a real browser against live staging: on a SLOW
 * cold first turn the ONE reply the concierge produced must RENDER as EXACTLY
 * ONE bubble.
 *
 * Why a SLOW turn specifically (a fast-turn test cannot catch this): the render
 * doubling only appears when the persisted-history copy (server ingest-ts,
 * id "<rowid>:agent") and the live HTTP reply (client-ts, a random UUID) are
 * more than ~3s apart — appendMessageDeduped's 3s window then can't collapse
 * them, and (pre-fix) neither could the id-keyed reconcile merge, so BOTH
 * rendered. The current fix collapses the optimistic and authoritative copies
 * by stable content identity with count-based one-to-one matching, independent
 * of elapsed time. This spec still FORCES the two copies apart by holding the
 * /a2a HTTP reply ~30s, letting the ≤10s DB reconcile render the persisted copy
 * first, then asserting a single rendered reply AFTER the reconcile settles.
 *
 * ── THE TWO THINGS THIS SPEC GOT WRONG BEFORE (run 499907, both fixed here) ──
 *
 * 1. IT STOPPED ITS STOPWATCH ON THE WRONG EVENT. The first-turn poll waited for
 *    `agentBubbles(...).length >= 1`. On a cold open the STORED transcript is
 *    ALREADY rendered from chat history before "hi" is ever sent — so it clocked
 *    "time until history loaded" (1049ms), not "time until the reply arrived",
 *    and then declared the turn "not slow" and failed. The forced hold HAD in
 *    fact been applied. Fixed by BASELINING the bubbles before sending and
 *    polling for the count to grow PAST that baseline.
 *
 * 2. ITS MODEL OF THE WORLD WAS WRONG. The staging specs share ONE org and ONE
 *    long-lived concierge "My Chat" (staging-setup.ts globalSetup, workers:1,
 *    fullyParallel:false) and this spec sorts LAST, after staging-concierge-
 *    greeting.spec.ts has already sent three turns to the same concierge. So
 *    `agents.length === 1` over the whole transcript was asserting something
 *    false about the world, and the exact-content dedupe next to it could not
 *    see a semantic double-reply (two differently-worded answers to one "hi")
 *    at all — precisely the bug it existed to catch. Both are replaced by TURN
 *    ATTRIBUTION (agentRepliesForTurn): diff the transcript against the
 *    pre-send baseline and require the turn to have produced EXACTLY ONE reply,
 *    whatever its wording, on top of an intact prefix.
 *
 * Auth + harness model is identical to staging-concierge.spec.ts (shared global
 * setup provisions ONE fresh org; matched by the `staging-*.spec.ts` testMatch;
 * runs in the gated `Canvas tabs E2E` workflow). Reads the RENDERED DOM, never
 * the store — the bug is a render doubling, so the count must come from the
 * actual bubbles.
 */
import { test, expect, type Page, type BrowserContext } from "@playwright/test";
import { agentRepliesForTurn, type SimpleMessage } from "../src/lib/conciergeChatInvariants";
import { gotoWithNetworkChangeRetry } from "../test-utils/stagingNavigation";
import { installStagingWebSocketAuth } from "./support/stagingWebSocketAuth";

const STAGING = process.env.CANVAS_E2E_STAGING === "1";

// Fail-closed, not skip-green (mirrors staging-concierge.spec.ts): a staging run
// that was REQUESTED (CANVAS_E2E_STAGING=1) but has no tenant state is a
// provisioning failure. CANVAS_E2E_STAGING unset = staging not requested = skip.
test.skip(!STAGING, "CANVAS_E2E_STAGING not set — staging-only suite, not requested");

// This is an end-to-end render-dedup guard: it FORCES a slow cold first turn and
// asserts the ONE reply the concierge produces renders exactly once. That
// requires a LIVE agent to answer — which staging now provides: staging-setup.ts
// provisions the tenant with provider=molecules-server (injecting the CP LLM
// proxy env) and REQUIRES the agent to reach status===online before any spec
// runs. The old skip-when-offline path (the #2162 platform-proxy gap) is gone:
// the agent boots, so this browser belt runs for real against the live turn.

function tenantEnv() {
  const tenantURL = process.env.STAGING_TENANT_URL;
  const tenantToken = process.env.STAGING_TENANT_TOKEN;
  if (!tenantURL || !tenantToken) {
    throw new Error(
      "staging-setup.ts did not export STAGING_TENANT_URL / STAGING_TENANT_TOKEN. " +
        "CANVAS_E2E_STAGING=1 was set (staging WAS requested) but global setup " +
        "produced no tenant — a provisioning failure, NOT a reason to skip.",
    );
  }
  return { tenantURL, tenantToken };
}

/** Bearer on every request, stub /cp/auth/me, and turn stray 401s into empty
 *  JSON so a workspace-scoped 401 can't yank us to AuthKit. */
async function authenticate(
  context: BrowserContext,
  tenantURL: string,
  token: string,
) {
  await context.setExtraHTTPHeaders({ Authorization: `Bearer ${token}` });
  await installStagingWebSocketAuth(context, { token, tenantURL });
  await context.addInitScript(() => {
    window.localStorage.setItem(
      "molecule_cookie_consent",
      JSON.stringify({ decision: "rejected", decidedAt: new Date().toISOString(), version: 1 }),
    );
  });
  await context.route("**/cp/auth/me", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ user_id: "slowcold", org_id: "slowcold", email: "slowcold@test.local" }),
    }),
  );
  await context.route("**", async (route, request) => {
    if (request.resourceType() !== "fetch") return route.fallback();
    if (request.url().includes("/cp/auth/me")) return route.fallback();
    let resp;
    try {
      resp = await route.fetch();
    } catch {
      return route.fallback();
    }
    if (resp.status() !== 401) return route.fulfill({ response: resp });
    const last = new URL(request.url()).pathname.split("/").filter(Boolean).pop() || "";
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: /^[0-9a-f-]{8,}$/.test(last) ? "{}" : "[]",
    });
  });
}

/** Ordered rendered bubbles from the concierge My Chat panel (DOM, not store). */
async function readRenderedBubbles(page: Page): Promise<SimpleMessage[]> {
  return page.evaluate(() => {
    const panel = document.querySelector('[data-testid="chat-panel"]');
    if (!panel) return [] as { role: "user" | "agent"; content: string }[];
    const out: { role: "user" | "agent"; content: string }[] = [];
    panel.querySelectorAll(".prose").forEach((p) => {
      const wrap = p.closest(".flex");
      const role = wrap && wrap.className.includes("justify-end") ? "user" : "agent";
      const text = (p as HTMLElement).innerText.trim();
      if (text) out.push({ role, content: text });
    });
    return out;
  });
}

const agentBubbles = (b: SimpleMessage[]) => b.filter((m) => m.role === "agent");

// Hold the live /a2a HTTP reply this long so the client-ts copy lands well past
// appendMessageDeduped's 3s window AND after a ≤10s DB reconcile has already
// rendered the persisted copy — i.e. force the exact slow-cold render race.
const FORCE_SLOW_HOLD_MS = 30_000;

// The DB reconcile that collapses the optimistic + persisted copies runs on a
// ≤10s cadence (see the useChatHistory reconcile). 12s is one full cycle +
// margin: a rendered agent-bubble set must hold steady this long before we trust
// it has settled.
const RECONCILE_CYCLE_MS = 12_000;

// The bug's repro condition: the persisted copy and the live copy of the SAME
// reply must land more than appendMessageDeduped's 3s window apart. Below this
// separation the race was never forced and a green result proves nothing.
const DEDUPE_WINDOW_MS = 3_000;

/**
 * Poll the REAL rendered DOM until the agent-bubble set has STOPPED changing.
 *
 * A fixed wall-clock settle would be a coupling bug: too short → a late
 * duplicate is MISSED (false green); dependent on backend latency either way.
 * So we poll until the set is STABLE across a full reconcile cycle AND every
 * deterministic late-delivery path has had its chance to land — `readyAt()`
 * reports the absolute timestamp after which no further copy can arrive (for
 * the post-send settle that is the moment the HELD live reply was actually
 * delivered, plus one reconcile cycle; a hold that has not fired yet returns
 * null and we keep waiting).
 *
 * Real-signal poll: returns the instant both conditions hold — it never waits
 * out the deadline on the happy path. The deadline is only a safety net so a
 * pathological hang fails loud instead of hanging the runner. A late duplicate
 * simply changes the signature, resets the stability timer, and gets caught by
 * the assertions on the stabilized set.
 */
async function waitForStableAgentBubbles(
  page: Page,
  opts: { stableForMs: number; readyAt: () => number | null; safetyNetMs: number },
): Promise<SimpleMessage[]> {
  const POLL_MS = 1500;
  const deadline = Date.now() + opts.safetyNetMs;
  let lastSig: string | null = null;
  let stableSince = Date.now();
  while (Date.now() < deadline) {
    const rendered = await readRenderedBubbles(page);
    const sig = JSON.stringify(agentBubbles(rendered).map((m) => m.content));
    if (sig !== lastSig) {
      lastSig = sig;
      stableSince = Date.now();
    }
    const readyAt = opts.readyAt();
    if (readyAt !== null && Date.now() >= readyAt && Date.now() - stableSince >= opts.stableForMs) {
      return rendered;
    }
    await page.waitForTimeout(POLL_MS);
  }
  throw new Error(
    `agent bubbles did not stabilize within ${opts.safetyNetMs}ms`,
  );
}

test("slow cold first turn: the ONE stored greeting RENDERS exactly once (no duplicate bubble)", async ({
  page,
  context,
}) => {
  // Safety-net envelope: history settle (≤120s) + first-turn poll (≤240s) +
  // stability settle (≤300s) + page/auth overhead. 15 min is headroom over the
  // happy path (~60-110s), never waited out on success.
  test.setTimeout(15 * 60 * 1000);
  const { tenantURL, tenantToken } = tenantEnv();
  await authenticate(context, tenantURL, tenantToken);

  page.on("console", (m) => {
    if (m.type() === "error") console.log(`[console-error] ${m.text()}`);
  });

  // The baseline below is only meaningful once the cold chat-history load has
  // actually COME BACK. Register this before navigation so a fast response
  // cannot race past the listener. We await it before starting the stability
  // window; a boolean response listener could flip after a DOM poll but before
  // that poll returned, accepting an empty pre-response baseline.
  const chatHistoryResponse = page.waitForResponse(
    (response) =>
      response.request().method() === "GET" &&
      /\/chat-history(?:\?|$)/.test(response.url()),
    { timeout: 120_000 },
  );

  // FORCE the slow cold turn deterministically: hold the concierge /a2a reply so
  // the live copy is delivered ~30s after the persisted copy — independent of
  // backend warmth (a warm cache would otherwise give a fast turn that hides the
  // bug). Registered AFTER authenticate's catch-all so it wins for /a2a (LIFO);
  // it performs the REAL request, then delays fulfilling the genuine response.
  //
  // `liveReplyAt` is the load-bearing signal: the absolute instant the held live
  // copy actually reached the client. It is what makes the repro condition
  // MEASURABLE (see separationMs) rather than inferred from a wall clock.
  let forcedHoldApplied = false;
  let serverRepliedAt: number | null = null;
  let liveReplyAt: number | null = null;
  await context.route("**/workspaces/*/a2a", async (route) => {
    let resp;
    try {
      // The outer 240s cold-turn budget is the safety net; Playwright defaults
      // route.fetch() to 30s, which would abandon and replay a healthy slow turn.
      resp = await route.fetch({ timeout: 0 });
    } catch {
      return route.fallback();
    }
    forcedHoldApplied = true;
    serverRepliedAt = Date.now();
    await new Promise((r) => setTimeout(r, FORCE_SLOW_HOLD_MS));
    await route.fulfill({ response: resp });
    liveReplyAt = Date.now();
  });

  await gotoWithNetworkChangeRetry(page, tenantURL, {
    waitUntil: "domcontentloaded",
  });
  await page.waitForSelector('[data-testid="nav-home"], [data-testid="hydration-error"]', { timeout: 60_000 });
  expect(await page.locator('[data-testid="hydration-error"]').count(), "canvas hydration failed").toBe(0);

  await page.getByTestId("nav-home").click();
  const chatPanel = page.getByTestId("chat-panel");
  await expect(chatPanel, "Home did not mount the concierge ChatTab").toBeVisible({ timeout: 30_000 });
  const myChat = chatPanel.locator("#chat-tab-my-chat");
  if (await myChat.count()) await myChat.click();

  const historyResponse = await chatHistoryResponse;
  expect(
    historyResponse.ok(),
    `chat-history failed with HTTP ${historyResponse.status()}`,
  ).toBeTruthy();

  const composer = page.locator('textarea[aria-label="Message to agent"]');
  await expect(composer, "chat composer not present").toBeVisible({ timeout: 30_000 });

  // The org was provisioned with an online concierge: a permanently-disabled
  // composer is a real failure, not a skip.
  const enableDeadline = Date.now() + 6 * 60 * 1000;
  while ((await composer.isDisabled()) && Date.now() < enableDeadline) {
    await page.waitForTimeout(3000);
  }
  expect(await composer.isDisabled(), "concierge composer never enabled (agent unreachable)").toBeFalsy();

  // ─── BASELINE: the cold-loaded transcript, BEFORE we say anything ──────────
  // "My Chat" is ONE long-lived conversation on a shared org, so on a cold open
  // this already renders the earlier staging specs' turns. Those bubbles are not
  // ours and must not be counted as replies to our "hi" (that is exactly what
  // false-RED'd run 499907). The response was awaited above; now require the
  // rendered set to STOP changing across a full reconcile cycle. A baseline
  // snapshotted mid-render would let a straggler history bubble land after the
  // send and masquerade as a second reply.
  const baseline = agentBubbles(
    await waitForStableAgentBubbles(page, {
      stableForMs: RECONCILE_CYCLE_MS, // spans a full reconcile cycle
      readyAt: () => 0, // the history response was awaited before this poll began
      safetyNetMs: 120_000,
    }),
  );
  console.log(`[slow-cold] baseline agent bubbles (pre-existing history) = ${baseline.length}`);

  // ─── Turn 1: the opening 'hi' — held to a SLOW cold turn ──────────────────
  await composer.fill("hi");
  const t0 = Date.now();
  await composer.press("Enter");

  // Poll until a bubble that is OURS renders — i.e. the agent count grows PAST
  // the baseline. Budget 240s (30s forced hold + a cold openclaw first turn).
  // Because the live /a2a reply is held for FORCE_SLOW_HOLD_MS, the first copy
  // to appear here can only be the PERSISTED one, arriving via the ≤10s DB
  // reconcile — which is the whole point: the two copies of one reply land far
  // apart, and the client must still render exactly one bubble.
  let firstCopyAt = -1;
  const reply1Deadline = Date.now() + 240_000;
  while (Date.now() < reply1Deadline) {
    if (agentBubbles(await readRenderedBubbles(page)).length > baseline.length) {
      firstCopyAt = Date.now();
      break;
    }
    await page.waitForTimeout(1000);
  }
  expect(firstCopyAt, "concierge never produced a NEW agent bubble within 240s").toBeGreaterThan(0);
  const firstTurnMs = firstCopyAt - t0;

  // Fail promptly (not after the 5-min settle) if the interception never fired.
  // The persisted copy can win a narrow race against route.fetch() completing,
  // so poll the real hold signal instead of snapshotting its boolean once.
  await expect
    .poll(() => forcedHoldApplied, {
      message:
        "the /a2a hold never fired — the client did not POST /workspaces/*/a2a, " +
        "so the slow-turn race was never forced and a green result would prove nothing",
      timeout: 30_000,
    })
    .toBe(true);

  // ─── Settle: wait out every late-delivery path, on a REAL signal ───────────
  // The last possible copy is the HELD live reply; nothing can arrive after it
  // plus one reconcile cycle. We wait for the ACTUAL delivery instant, not a
  // wall-clock guess (the agent's own think time is unbounded, so `t0 + hold`
  // would be an under-estimate on a slow turn and could settle before the live
  // copy even landed — a false green).
  const rendered = await waitForStableAgentBubbles(page, {
    stableForMs: RECONCILE_CYCLE_MS,
    readyAt: () => (liveReplyAt === null ? null : liveReplyAt + RECONCILE_CYCLE_MS),
    safetyNetMs: 5 * 60 * 1000,
  });

  // How far apart the two copies of this ONE reply actually landed. This — not
  // "how long the turn took" — is the bug's repro condition: they must straddle
  // appendMessageDeduped's 3s window, or the race under test never happened.
  const separationMs = liveReplyAt === null ? -1 : liveReplyAt - firstCopyAt;
  const slowTurnForced = forcedHoldApplied && separationMs > DEDUPE_WINDOW_MS;

  const turn = agentRepliesForTurn(baseline, rendered);
  console.log(
    "RESULT_JSON " +
      JSON.stringify({
        baselineAgentBubbles: baseline.length,
        firstTurnMs,
        separationMs,
        slowTurnForced,
        forcedHoldApplied,
        serverThinkMs: serverRepliedAt === null ? -1 : serverRepliedAt - t0,
        repliesThisTurn: turn.replies.length,
        prefixIntact: turn.prefixIntact,
        prefixDrift: turn.prefixDrift,
        renderedAgents: agentBubbles(rendered).map((m) => m.content.slice(0, 80)),
      }),
  );

  // ─── THE GUARDS ───────────────────────────────────────────────────────────
  // (1) The race was really forced: the persisted copy rendered, and the live
  //     copy arrived >3s later. If the client only ever rendered the live copy,
  //     separationMs collapses to ~0 and this fails — the fail arm is reachable.
  expect(
    slowTurnForced,
    `the two copies of the reply landed ${separationMs}ms apart — not the >${DEDUPE_WINDOW_MS}ms ` +
      `repro condition (forcedHoldApplied=${forcedHoldApplied}, firstTurnMs=${firstTurnMs}). ` +
      "The slow-cold race was NOT reproduced, so a pass here would be vacuous.",
  ).toBeTruthy();

  // (2) The pre-existing transcript survived intact — a copy of an OLD bubble
  //     re-inserted by the reconcile is a render-dup too.
  expect(
    turn.prefixIntact,
    `the pre-existing transcript changed under the reconcile: ${JSON.stringify(turn.prefixDrift)}`,
  ).toBeTruthy();

  // (3) THE POINT: one user turn ⇒ EXACTLY ONE rendered agent reply. This holds
  //     the line against BOTH shapes of the bug — the same reply rendered twice
  //     (render-dup), and the agent genuinely answering twice with different
  //     wording (which an exact-content dedupe cannot see at all).
  expect(
    turn.replies.length,
    `one "hi" produced ${turn.replies.length} rendered agent replies; expected exactly 1: ` +
      JSON.stringify(turn.replies.map((m) => m.content.slice(0, 80))),
  ).toBe(1);
});
