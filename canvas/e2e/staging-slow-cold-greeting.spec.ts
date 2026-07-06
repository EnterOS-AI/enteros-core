/**
 * staging-slow-cold-greeting.spec.ts — browser-level regression guard for the
 * concierge duplicate-greeting RENDER-DUP on a SLOW cold first turn.
 * ============================================================================
 * Companion to the unit guard
 *   canvas/src/components/tabs/chat/hooks/__tests__/useChatHistory.slowColdGreetingRenderDup.test.tsx
 * which drives the REAL merge logic (mergeReconciledMessages / appendMessageDeduped)
 * with the exact server-ts-vs-client-ts 30s-gap scenario. This spec proves the
 * same invariant end-to-end in a real browser against live staging: on a SLOW
 * cold first turn the ONE stored greeting must RENDER as EXACTLY ONE bubble.
 *
 * Why a SLOW turn specifically (a fast-turn test cannot catch this): the render
 * doubling only appears when the persisted-history copy (server ingest-ts,
 * id "<rowid>:agent") and the live HTTP reply (client-ts, a random UUID) are
 * more than ~3s apart — appendMessageDeduped's 3s window then can't collapse
 * them, and (pre-fix) neither could the id-keyed reconcile merge, so BOTH
 * rendered. The fix (15032a31) collapses them in mergeReconciledMessages within
 * a 60s window. So this spec FORCES the first 'hi' to take >3s (~30s) by holding
 * the /a2a HTTP reply, letting the ≤10s DB reconcile bring the persisted copy
 * first, then asserting a single rendered greeting AFTER the reconcile settles.
 *
 * Auth + harness model is identical to staging-concierge.spec.ts (shared global
 * setup provisions ONE fresh org; matched by the `staging-*.spec.ts` testMatch;
 * runs in the gated `Canvas tabs E2E` workflow). Reads the RENDERED DOM, never
 * the store — the bug is a render doubling, so the count must come from the
 * actual bubbles.
 */
import { test, expect, type Page, type BrowserContext } from "@playwright/test";

const STAGING = process.env.CANVAS_E2E_STAGING === "1";

// Fail-closed, not skip-green (mirrors staging-concierge.spec.ts): a staging run
// that was REQUESTED (CANVAS_E2E_STAGING=1) but has no tenant state is a
// provisioning failure. CANVAS_E2E_STAGING unset = staging not requested = skip.
test.skip(!STAGING, "CANVAS_E2E_STAGING not set — staging-only suite, not requested");

// This is an end-to-end render-dedup guard: it FORCES a slow cold first turn and
// asserts the ONE greeting the concierge produces renders exactly once. That
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
async function authenticate(context: BrowserContext, token: string) {
  await context.setExtraHTTPHeaders({ Authorization: `Bearer ${token}` });
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
async function readRenderedBubbles(page: Page): Promise<{ role: "user" | "agent"; content: string }[]> {
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

const agentBubbles = (b: { role: string; content: string }[]) => b.filter((m) => m.role === "agent");

/** Identical-content agent bubbles rendered more than once = the render-dup. */
function duplicateAgentContents(b: { role: string; content: string }[]): string[] {
  const seen = new Map<string, number>();
  for (const m of agentBubbles(b)) seen.set(m.content, (seen.get(m.content) ?? 0) + 1);
  return [...seen.entries()].filter(([, n]) => n > 1).map(([c]) => c);
}

// Hold the live /a2a HTTP reply this long so the client-ts copy lands well past
// appendMessageDeduped's 3s window AND after a ≥10s DB reconcile has already
// rendered the persisted copy — i.e. force the exact slow-cold render race.
const FORCE_SLOW_HOLD_MS = 30_000;

// The DB reconcile that collapses the optimistic + persisted greeting copies
// runs on a ≤10s cadence (see the useChatHistory reconcile). 12s is one full
// cycle + margin: the rendered agent-bubble set must hold steady this long
// before we trust it has settled.
const RECONCILE_CYCLE_MS = 12_000;

/**
 * Deterministic replacement for the old blind `waitForTimeout(15_000)` settle.
 *
 * The bug is a LATE-arriving duplicate bubble, so a fixed wall-clock wait is a
 * coupling bug: too short → a late duplicate (or a not-yet-run reconcile) is
 * MISSED (false green); dependent on backend latency either way. Instead we
 * poll the REAL rendered DOM until the agent-bubble set is STABLE across a full
 * reconcile cycle AND every deterministic late-delivery path has had its chance
 * to land — specifically the forced /a2a hold, whose live copy is delivered
 * FORCE_SLOW_HOLD_MS after send (the last possible late delivery in this test).
 * Only then can no further copy appear, so a stable set is trustworthy.
 *
 * Real-signal poll: returns the instant both conditions hold — it never waits
 * out the deadline on the happy path. The deadline is only a ~7× safety net so
 * a pathological hang fails loud instead of hanging the runner. A late
 * duplicate simply changes the signature, resets the stability timer, and gets
 * caught by the assertions on the stabilized set below.
 */
async function waitForStableAgentBubbles(
  page: Page,
  sentAt: number,
): Promise<{ role: "user" | "agent"; content: string }[]> {
  const POLL_MS = 1500;
  const SAFETY_NET_MS = 5 * 60 * 1000; // ~7× the ~42s late-delivery envelope
  // No new copy can arrive after the forced hold delivers + one reconcile cycle.
  const lateDeliveryEnvelopeMs = FORCE_SLOW_HOLD_MS + RECONCILE_CYCLE_MS;
  const deadline = Date.now() + SAFETY_NET_MS;
  let lastSig: string | null = null;
  let stableSince = Date.now();
  while (Date.now() < deadline) {
    const rendered = await readRenderedBubbles(page);
    const sig = JSON.stringify(agentBubbles(rendered).map((m) => m.content));
    if (sig !== lastSig) {
      lastSig = sig;
      stableSince = Date.now();
    }
    const stableForMs = Date.now() - stableSince;
    const sinceSentMs = Date.now() - sentAt;
    if (stableForMs >= RECONCILE_CYCLE_MS && sinceSentMs >= lateDeliveryEnvelopeMs) {
      return rendered;
    }
    await page.waitForTimeout(POLL_MS);
  }
  // Safety net reached (pathological): return the current DOM and let the
  // assertions below judge it loudly — never silently pass.
  return readRenderedBubbles(page);
}

test("slow cold first turn: the ONE stored greeting RENDERS exactly once (no duplicate bubble)", async ({
  page,
  context,
}) => {
  // Safety-net envelope: first-turn poll (≤240s) + stability settle (≤300s) +
  // page/auth overhead. 15 min is headroom over the happy path (~45-90s), never
  // waited out on success.
  test.setTimeout(15 * 60 * 1000);
  const { tenantURL, tenantToken } = tenantEnv();
  await authenticate(context, tenantToken);

  page.on("console", (m) => {
    if (m.type() === "error") console.log(`[console-error] ${m.text()}`);
  });

  // FORCE the slow cold turn deterministically: hold the concierge /a2a reply so
  // the live copy is delivered ~30s after the persisted copy — independent of
  // backend warmth (a warm cache would otherwise give a fast turn that hides the
  // bug). Registered AFTER authenticate's catch-all so it wins for /a2a (LIFO);
  // it performs the REAL request, then delays fulfilling the genuine response.
  let forcedHoldApplied = false;
  await context.route("**/workspaces/*/a2a", async (route) => {
    let resp;
    try {
      resp = await route.fetch();
    } catch {
      return route.fallback();
    }
    forcedHoldApplied = true;
    await new Promise((r) => setTimeout(r, FORCE_SLOW_HOLD_MS));
    await route.fulfill({ response: resp });
  });

  await page.goto(tenantURL, { waitUntil: "domcontentloaded" });
  await page.waitForSelector('[data-testid="nav-home"], [data-testid="hydration-error"]', { timeout: 60_000 });
  expect(await page.locator('[data-testid="hydration-error"]').count(), "canvas hydration failed").toBe(0);

  await page.getByTestId("nav-home").click();
  const chatPanel = page.getByTestId("chat-panel");
  await expect(chatPanel, "Home did not mount the concierge ChatTab").toBeVisible({ timeout: 30_000 });
  const myChat = chatPanel.locator("#chat-tab-my-chat");
  if (await myChat.count()) await myChat.click();

  const composer = page.locator('textarea[aria-label="Message to agent"]');
  await expect(composer, "chat composer not present").toBeVisible({ timeout: 30_000 });

  // The org was provisioned with an online concierge: a permanently-disabled
  // composer is a real failure, not a skip.
  const enableDeadline = Date.now() + 6 * 60 * 1000;
  while ((await composer.isDisabled()) && Date.now() < enableDeadline) {
    await page.waitForTimeout(3000);
  }
  expect(await composer.isDisabled(), "concierge composer never enabled (agent unreachable)").toBeFalsy();

  // ─── Turn 1: the opening 'hi' — held to a SLOW cold turn ──────────────────
  await composer.fill("hi");
  const t0 = Date.now();
  await composer.press("Enter");

  // Poll until a greeting bubble RENDERS. Budget 240s (30s forced hold + a cold
  // openclaw first turn). Record the observed first-turn latency.
  let firstTurnMs = -1;
  const reply1Deadline = Date.now() + 240_000;
  while (Date.now() < reply1Deadline) {
    if (agentBubbles(await readRenderedBubbles(page)).length >= 1) {
      firstTurnMs = Date.now() - t0;
      break;
    }
    await page.waitForTimeout(1000);
  }
  expect(firstTurnMs, "concierge never produced a greeting bubble within 240s").toBeGreaterThan(0);
  const slowTurnForced = firstTurnMs > 3000;
  console.log(`[slow-cold] first-turn latency = ${firstTurnMs}ms (slowTurnForced=${slowTurnForced}, hold=${forcedHoldApplied})`);

  // Let EVERY late delivery path settle — but on a REAL signal, not a blind
  // wall-clock wait: poll the rendered DOM until the agent-bubble set is stable
  // across a full reconcile cycle AND past the forced-hold late-delivery
  // envelope (see waitForStableAgentBubbles). A late duplicate resets the
  // stability timer and is caught below instead of being missed by a fixed 15s.
  const rendered = await waitForStableAgentBubbles(page, t0);
  const agents = agentBubbles(rendered);
  const dups = duplicateAgentContents(rendered);
  console.log(
    "RESULT_JSON " +
      JSON.stringify({
        firstTurnMs,
        slowTurnForced,
        forcedHoldApplied,
        agentBubbleCount: agents.length,
        duplicateAgentContents: dups,
        renderedAgents: agents.map((m) => m.content.slice(0, 80)),
      }),
  );

  // THE GUARD: after the slow cold first turn + reconcile, exactly ONE greeting
  // bubble is rendered, and no agent content is duplicated in the DOM.
  expect(slowTurnForced, `first turn was not slow (${firstTurnMs}ms) — the >3s repro condition was not met`).toBeTruthy();
  expect(dups, `duplicate greeting bubble(s) rendered: ${JSON.stringify(dups)}`).toEqual([]);
  expect(agents.length, `expected EXACTLY ONE greeting bubble; got ${agents.length}`).toBe(1);
});
