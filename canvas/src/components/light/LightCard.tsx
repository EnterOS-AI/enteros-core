"use client";

import { useId, useState } from "react";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import {
  disableLight,
  enableLight,
  openLight,
  parkLight,
  type LightActionResult,
} from "@/lib/api/light";
import {
  earliestNextRun,
  formatLightTime,
  isStuckPark,
  lightErrorSentence,
  lightLastErrorSentence,
  type LightInfo,
  type LightState,
} from "@/lib/light";
import { WORKSPACE_KIND } from "@/lib/workspace-kind";
import type { WorkspaceNodeData } from "@/store/canvas";

/** Runs one Light request at a time (see useSingleFlight); null when another
 *  request was already in flight and this one was not sent. */
export type RunLightAction = <T>(
  name: string,
  call: () => Promise<T>,
) => Promise<{ ok: true; value: T } | { ok: false; error: unknown } | null>;

interface Props {
  workspaceId: string;
  data: WorkspaceNodeData;
  /** The state to render: GET /light's answer once loaded, else the node's. */
  state: LightState | null;
  info: LightInfo | null;
  /** Why GET /light failed, if it did. */
  loadError: string | null;
  /** The Light request in flight anywhere in the tab, if any. */
  busy: string | null;
  run: RunLightAction;
  /** A management call answered this state. */
  onState: (state: LightState | null) => void;
}

const BUTTON =
  "text-[11px] px-2 py-0.5 rounded transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-surface disabled:opacity-40 disabled:cursor-not-allowed";
const PRIMARY = `${BUTTON} bg-accent-strong/20 text-accent hover:bg-accent-strong/30`;
const DANGER = `${BUTTON} text-bad hover:bg-red-600/20`;

function enableNotice(r: LightActionResult): string {
  if (r.status === "unchanged") return "This agent already is a Light agent.";
  switch (r.restart) {
    case "started":
      return "Light is on. The agent restarts to apply it.";
    case "woken":
      return "Light is on. The agent is waking up to apply it.";
    case "refresh_queued":
      return "Light settings restored. The agent opens briefly to pick them up.";
    default:
      return "Light is on.";
  }
}

function disableNotice(r: LightActionResult): string {
  if (r.status === "unchanged") return "This agent is not a Light agent.";
  switch (r.restart) {
    case "started":
      return "Light is off. The agent restarts as an ordinary agent.";
    case "woken":
      return "Light is off. The agent is waking up as an ordinary agent.";
    default:
      return "Light is off.";
  }
}

function openNotice(r: LightActionResult): string | null {
  if (r.status === "unchanged") return "The agent already is open.";
  const n = r.cancelled.length;
  if (n === 0) return null;
  return `Opening. ${n} queued run${n === 1 ? "" : "s"} that had not started ${n === 1 ? "was" : "were"} cancelled.`;
}

/**
 * The Light card at the top of the Schedule tab (docs/api-protocol/light-api.md):
 * enable for a hermes agent, park and disable while open, open and disable
 * while parked, the transition in between, the last background failure and a
 * queued refresh. Every action shares the tab's one-request-at-a-time guard,
 * so a park or open can never be sent twice.
 */
export function LightCard({ workspaceId, data, state, info, loadError, busy, run, onState }: Props) {
  const headingId = useId();
  const [actionError, setActionError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [confirmDisable, setConfirmDisable] = useState(false);

  const act = async (name: string, call: () => Promise<LightActionResult>, toNotice: (r: LightActionResult) => string | null) => {
    setActionError(null);
    setNotice(null);
    const r = await run(name, call);
    if (!r) return;
    if (r.ok) {
      onState(r.value.light_state);
      setNotice(toNotice(r.value));
    } else {
      setActionError(lightErrorSentence(r.error));
    }
  };

  const enable = () => act("enable", () => enableLight(workspaceId), enableNotice);
  const park = () => act("park", () => parkLight(workspaceId), (r) => (r.status === "unchanged" ? "The agent already is parked." : null));
  const open = () => act("open", () => openLight(workspaceId), openNotice);
  const disable = () => {
    setConfirmDisable(false);
    void act("disable", () => disableLight(workspaceId), disableNotice);
  };

  const inFlight = busy !== null;
  const stuck = info ? isStuckPark(info) : false;
  const next = info ? earliestNextRun(info.schedules) : null;

  let body: React.ReactNode;
  let buttons: React.ReactNode = null;
  switch (state) {
    case null:
      if (data.kind === WORKSPACE_KIND.Platform) {
        body = "The organization's platform agent cannot be a Light agent.";
      } else if (data.runtime !== "hermes") {
        body = "Light needs a hermes agent.";
      } else {
        body = "Light parks this agent between its scheduled runs: each run starts a fresh copy of it, so nothing runs in between.";
        buttons = (
          <button type="button" className={PRIMARY} onClick={enable} disabled={inFlight}>
            {busy === "enable" ? "Enabling…" : "Enable Light"}
          </button>
        );
      }
      break;
    case "open":
      body = "Open. Scheduled runs are paused while the agent is open; it parks itself after 30 minutes without activity.";
      buttons = (
        <>
          <button type="button" className={PRIMARY} onClick={park} disabled={inFlight}>
            {busy === "park" ? "Parking…" : "Park now"}
          </button>
          <button type="button" className={DANGER} onClick={() => setConfirmDisable(true)} disabled={inFlight}>
            Disable Light
          </button>
        </>
      );
      break;
    case "parked":
      body = next
        ? `Parked. Next run ${formatLightTime(next.next_run_at)} (${next.name}).`
        : "Parked. No upcoming run.";
      buttons = (
        <>
          <button type="button" className={PRIMARY} onClick={open} disabled={inFlight}>
            {busy === "open" ? "Opening…" : "Open"}
          </button>
          <button type="button" className={DANGER} onClick={() => setConfirmDisable(true)} disabled={inFlight}>
            Disable Light
          </button>
        </>
      );
      break;
    case "parking":
      body = stuck
        ? "Parking has not finished for over three minutes. You can open the agent again, or disable Light."
        : "Parking: the schedules are handed to their CronJobs and the agent's pod is stopping.";
      buttons = (
        <>
          <button type="button" className={PRIMARY} onClick={open} disabled={inFlight || !stuck}>
            {busy === "open" ? "Opening…" : "Open"}
          </button>
          <button type="button" className={DANGER} onClick={() => setConfirmDisable(true)} disabled={inFlight || !stuck}>
            Disable Light
          </button>
        </>
      );
      break;
    case "opening":
      body = "Opening: the schedules are paused and the agent is waking up.";
      buttons = (
        <>
          <button type="button" className={PRIMARY} disabled>
            Park now
          </button>
          <button type="button" className={DANGER} disabled>
            Disable Light
          </button>
        </>
      );
      break;
  }

  const lastError = info?.last_error ?? null;
  const lastErrorSentence = lastError ? lightLastErrorSentence(lastError) : null;

  return (
    <section aria-labelledby={headingId} data-testid="light-card" className="px-3 py-2 border-b border-line/50 space-y-1.5">
      <div className="flex items-center justify-between gap-2">
        <h3 id={headingId} className="text-[10px] font-semibold text-ink-mid uppercase tracking-wider">
          Light
        </h3>
        {state && (
          <span className="text-[9px] font-mono uppercase tracking-wider text-ink-mid">{state}</span>
        )}
      </div>
      <p className="text-[10px] text-ink">{body}</p>
      {buttons && <div className="flex flex-wrap items-center gap-1.5">{buttons}</div>}
      {state && info?.refresh_pending && (
        <p className="text-[10px] text-ink-mid">The agent will open briefly to pick up a change, then park again.</p>
      )}
      {lastError && lastErrorSentence && (
        <div role="status" className="px-2 py-1.5 rounded border border-warm/40 bg-warm/10 text-[10px] text-warm">
          <p>{lastErrorSentence}</p>
          {lastError.message && lastError.message !== lastErrorSentence && (
            <p className="text-ink-mid mt-0.5">Core said: {lastError.message}</p>
          )}
        </div>
      )}
      {actionError && (
        <div role="alert" aria-live="assertive" className="text-[10px] text-bad">
          {actionError}
        </div>
      )}
      {notice && (
        <div role="status" aria-live="polite" className="text-[10px] text-good">
          {notice}
        </div>
      )}
      {state && loadError && (
        <p className="text-[10px] text-ink-mid">Could not refresh the Light state: {loadError}</p>
      )}
      <ConfirmDialog
        open={confirmDisable}
        title="Disable Light"
        message="Turn this agent back into an ordinary agent? Its CronJobs and any run still going are deleted, and the agent restarts (or wakes). The run history is kept."
        confirmLabel="Disable Light"
        confirmVariant="warning"
        onConfirm={disable}
        onCancel={() => setConfirmDisable(false)}
      />
    </section>
  );
}
