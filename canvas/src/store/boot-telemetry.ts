import type { BootStep, BootLogLine } from "@/store/canvas";

/**
 * boot-telemetry.ts — shared boot-step merge + watchdog-log derivation.
 *
 * Two ingestion paths feed the "Enter OS" boot screen and MUST agree on how
 * telemetry becomes UI state:
 *   1. Live BOOT_STEP WS events (canvas-events.ts) — one step at a time,
 *      validated through parseBootStepWire here.
 *   2. The `boot_steps` history replayed by GET /workspaces for provisioning
 *      rows (canvas-topology.ts) — the whole boot so far, after a page
 *      reload / WS reconnect / health-check rehydrate.
 *
 * Keeping the merge + log rules here means a rehydrate reconstructs exactly
 * the state the live path would have accumulated, so the watchdog log no
 * longer resets to "waiting for boot telemetry" mid-boot.
 *
 * TIMEKEEPING: `at` values mix clocks — server-stamped on replayed records,
 * client-stamped on live events — so absolute differences between arbitrary
 * lines are NOT trustworthy. Each line therefore carries an immutable `t`
 * (ms since the boot's first line), computed at append time as the previous
 * line's `t` plus the CLAMPED non-negative delta to the previous line's
 * `at`. A clock switch (replay → live after a reload) can distort at most
 * that single boundary delta; it can never make the column run backwards,
 * and dropping capped head lines never re-bases the numbers the user is
 * already reading.
 */

/** Wire shape of one replayed boot step (server events/boottrace.go
 *  BootStepRecord — the BOOT_STEP payload plus `at`, unix ms). */
export interface BootStepWire {
  step?: unknown;
  total?: unknown;
  key?: unknown;
  label?: unknown;
  status?: unknown;
  message?: unknown;
  at?: unknown;
}

/** Cap on retained watchdog log lines. A normal boot is ~20 lines; the cap
 *  only guards against a runaway runtime spamming distinct messages. Oldest
 *  lines drop first — the visible tail is what matters. */
export const MAX_BOOT_LOG_LINES = 300;

/** The watchdog line for a step: its message, else a status-derived label.
 *  An EMPTY message counts as absent — the runtime posts "" for plain
 *  status transitions (boot_event.go broadcasts the field verbatim), and
 *  `??` alone would render those as blank log lines. */
export function bootLineText(s: Pick<BootStep, "label" | "status" | "message">): string {
  if (s.message !== undefined && s.message.trim() !== "") return s.message;
  return s.status === "ok"
    ? `${s.label} — done`
    : s.status === "failed"
      ? `${s.label} — FAILED`
      : `${s.label}…`;
}

/** Validate one wire entry into a BootStep — the SINGLE acceptance rule for
 *  both ingestion paths (live WS handler and GET /workspaces replay), so a
 *  reload can never reconstruct different keycaps than the live session
 *  showed. Rejects rather than rendering a broken keycap. */
export function parseBootStepWire(w: BootStepWire): BootStep | null {
  const step = typeof w.step === "number" ? w.step : NaN;
  const total = typeof w.total === "number" ? w.total : NaN;
  const key = typeof w.key === "string" ? w.key : "";
  const label = typeof w.label === "string" ? w.label : "";
  const status =
    w.status === "running" || w.status === "ok" || w.status === "failed" ? w.status : null;
  if (!Number.isFinite(step) || step < 1 || !Number.isFinite(total) || total < step || !key || !label || !status) {
    return null;
  }
  const out: BootStep = { step, total, key, label, status };
  if (typeof w.message === "string") out.message = w.message;
  if (typeof w.at === "number") {
    out.at = w.at;
    // A wire-carried timestamp is the SERVER's clock (boottrace.go stamps
    // it); live WS events carry no `at` and get client-stamped by the
    // handler, which overrides this flag.
    out.serverClock = true;
  }
  return out;
}

/** Merge one step into the latest-per-step keycap list (in-place replace at
 *  the same index keeps keycap identity stable; new steps append). */
export function mergeBootStep(prev: BootStep[], incoming: BootStep): BootStep[] {
  const idx = prev.findIndex((s) => s.step === incoming.step);
  return idx >= 0 ? prev.map((s, i) => (i === idx ? incoming : s)) : [...prev, incoming];
}

/** True when the incoming step would repeat the step's last recorded line
 *  (same status AND same text). Build heartbeats change the message every
 *  tick and must each land as their own line; a re-broadcast identical
 *  update must not. */
function isRepeatOfLast(prevEntry: BootStep | undefined, incoming: BootStep): boolean {
  return (
    prevEntry !== undefined &&
    prevEntry.status === incoming.status &&
    bootLineText(prevEntry) === bootLineText(incoming)
  );
}

/** Max delta credited across a server↔client clock boundary. The real
 *  elapsed time between the last replayed line and the first live line is
 *  unknowable under skew — crediting at most one heartbeat-scale gap bounds
 *  the distortion in BOTH directions (a clock 10 minutes ahead must not
 *  inject +600s into the offset column). */
const CROSS_CLOCK_MAX_DELTA_MS = 5_000;

/** Build the next log line after `last` (or the first line when undefined).
 *  Owns the id sequencing and the clamped-delta `t` computation described in
 *  the module header. */
function nextLogLine(last: BootLogLine | undefined, incoming: BootStep): BootLogLine {
  const at = incoming.at ?? Date.now();
  const serverClock = incoming.serverClock === true;
  // Monotonic per-log sequence for the React key — derived from the last
  // line's id so it stays unique even after the cap starts dropping the
  // head (a length-based id would repeat once length pins at the cap).
  const seq = last ? Number(last.id.split(":")[0]) + 1 : 0;
  let delta = 0;
  if (last) {
    delta = Math.max(0, at - last.at);
    if ((last.serverClock === true) !== serverClock) {
      delta = Math.min(delta, CROSS_CLOCK_MAX_DELTA_MS);
    }
  }
  return {
    id: `${seq}:${incoming.step}-${incoming.status}`,
    at,
    t: last ? last.t + delta : 0,
    serverClock,
    status: incoming.status,
    text: bootLineText(incoming),
  };
}

/** Apply the retention cap, keeping the tail. Single definition so the live
 *  append path and the replay rebuild can never retain different logs. */
function capLog(lines: BootLogLine[]): BootLogLine[] {
  return lines.length > MAX_BOOT_LOG_LINES
    ? lines.slice(lines.length - MAX_BOOT_LOG_LINES)
    : lines;
}

/** Append the incoming step's log line (unless it repeats the step's last
 *  line), immutably — the live WS handler's single-event path. */
export function appendBootLog(
  prevLog: BootLogLine[],
  prevSteps: BootStep[],
  incoming: BootStep,
): BootLogLine[] {
  if (isRepeatOfLast(prevSteps.find((s) => s.step === incoming.step), incoming)) {
    return prevLog;
  }
  return capLog([...prevLog, nextLogLine(prevLog[prevLog.length - 1], incoming)]);
}

/** Rebuild {bootSteps, bootLog} from a replayed wire history — the state the
 *  live path would have accumulated had the client seen every event.
 *  Mutable accumulation (single pass, latest-per-step via map, cap applied
 *  once at the end): this runs inside buildNodesAndEdges on every hydrate,
 *  so the O(n²) immutable-append version was pure GC churn. */
export function bootStateFromWire(
  wire: BootStepWire[] | undefined,
): { bootSteps: BootStep[]; bootLog: BootLogLine[] } | null {
  if (!Array.isArray(wire) || wire.length === 0) return null;
  const bootSteps: BootStep[] = [];
  const byStep = new Map<number, number>(); // step index → position in bootSteps
  const bootLog: BootLogLine[] = [];
  for (const w of wire) {
    const step = parseBootStepWire(w);
    if (!step) continue;
    const pos = byStep.get(step.step);
    const prevEntry = pos !== undefined ? bootSteps[pos] : undefined;
    if (!isRepeatOfLast(prevEntry, step)) {
      bootLog.push(nextLogLine(bootLog[bootLog.length - 1], step));
    }
    if (pos !== undefined) {
      bootSteps[pos] = step;
    } else {
      byStep.set(step.step, bootSteps.length);
      bootSteps.push(step);
    }
  }
  if (bootSteps.length === 0) return null;
  return { bootSteps, bootLog: capLog(bootLog) };
}

/** Rank for step-status monotonicity: a step that reported ok/failed can
 *  never legitimately regress to running within one boot generation. */
function statusRank(s: BootStep["status"]): number {
  return s === "running" ? 0 : 1;
}

/** Merge a SAME-GENERATION server replay with locally-accumulated state.
 *  The replay is a snapshot ~1 round-trip old — live WS events consumed
 *  during the request are newer than it, so adopting it verbatim would
 *  regress keycaps and drop just-streamed log lines until the next
 *  rehydrate. Steps: latest-per-step with status monotonicity (local wins
 *  only when strictly more advanced). Log: replay lines first, then local
 *  lines the replay doesn't contain (matched by status+text), re-sequenced
 *  so React keys stay unique. Only call for a matching boot generation —
 *  cross-generation merging is exactly the stale-telemetry bug the
 *  generation marker exists to prevent. */
export function mergeBootTelemetry(
  replaySteps: BootStep[],
  replayLog: BootLogLine[],
  localSteps: BootStep[],
  localLog: BootLogLine[],
): { bootSteps: BootStep[]; bootLog: BootLogLine[] } {
  const bootSteps = [...replaySteps];
  const byStep = new Map<number, number>();
  bootSteps.forEach((s, i) => byStep.set(s.step, i));
  for (const ls of localSteps) {
    const i = byStep.get(ls.step);
    if (i === undefined) {
      byStep.set(ls.step, bootSteps.length);
      bootSteps.push(ls);
    } else if (statusRank(ls.status) > statusRank(bootSteps[i].status)) {
      bootSteps[i] = ls;
    }
  }

  const seen = new Set(replayLog.map((l) => `${l.status}|${l.text}`));
  const bootLog = [...replayLog];
  for (const ll of localLog) {
    const key = `${ll.status}|${ll.text}`;
    if (seen.has(key)) continue;
    seen.add(key);
    const last = bootLog[bootLog.length - 1];
    const seq = last ? Number(last.id.split(":")[0]) + 1 : 0;
    bootLog.push({ ...ll, id: `${seq}:${ll.status}` });
  }
  return { bootSteps, bootLog: capLog(bootLog) };
}
