"use client";

import { useState } from "react";
import { runLightSchedule } from "@/lib/api/light";
import { cronToHuman } from "@/lib/cron-human";
import { formatLightTime, lightErrorSentence, type LightSchedule, type LightState } from "@/lib/light";
import type { RunLightAction } from "./LightCard";

interface Props {
  workspaceId: string;
  state: LightState;
  schedules: LightSchedule[];
  busy: string | null;
  run: RunLightAction;
  /** A run was started: the runs list should refresh. */
  onRunStarted: () => void;
}

const RUN_BUTTON =
  "text-[11px] px-1.5 py-0.5 text-accent hover:bg-accent-strong/20 rounded transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-surface disabled:opacity-40 disabled:cursor-not-allowed shrink-0";

/**
 * The schedules of a sleeping Light agent, read-only, from GET /light (the
 * set its last park handed to the CronJobs). The agent itself is never asked
 * (no GET /schedules: it is asleep). "Run now" works only while it is parked.
 */
export function LightScheduleList({ workspaceId, state, schedules, busy, run, onRunStarted }: Props) {
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const runNow = async (name: string) => {
    setError(null);
    setNotice(null);
    const r = await run(`run:${name}`, () => runLightSchedule(workspaceId, name));
    if (!r) return;
    if (r.ok) {
      setNotice(
        `Started a run of ${name}${r.value ? ` (${r.value})` : ""}. It shows in the runs below once its pod starts.`,
      );
      onRunStarted();
    } else {
      setError(lightErrorSentence(r.error));
    }
  };

  return (
    <section aria-label="Schedules of the parked agent" data-testid="light-schedules">
      <div className="flex items-center justify-between px-3 py-2 border-b border-line/50">
        <span className="text-[10px] font-semibold text-ink-mid uppercase tracking-wider">Schedules</span>
        <span className="text-[10px] text-ink-mid">Open the agent to edit schedules.</span>
      </div>
      {error && (
        <div role="alert" aria-live="assertive" className="px-3 py-1.5 text-[10px] text-bad bg-red-900/20 border-b border-red-800/30">
          {error}
        </div>
      )}
      {notice && (
        <div role="status" aria-live="polite" className="px-3 py-1.5 text-[10px] text-good border-b border-line/30">
          {notice}
        </div>
      )}
      {schedules.length === 0 ? (
        <p className="px-3 py-3 text-[10px] text-ink-mid">The agent had no schedules at its last park.</p>
      ) : (
        <ul>
          {schedules.map((s) => {
            const tz = s.timezone || "UTC";
            const human = cronToHuman(s.cron, tz);
            return (
              <li key={s.name} className={`px-3 py-2 border-b border-line/30 ${s.enabled && !s.suspended ? "" : "opacity-70"}`}>
                <div className="flex items-start justify-between gap-2">
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center gap-1.5">
                      <span className="text-[10px] font-medium text-ink truncate">{s.name}</span>
                      <span className="text-[9px] px-1 rounded border border-line text-ink-mid">{s.enabled ? "on" : "off"}</span>
                      {s.suspended && (
                        <span className="text-[9px] px-1 rounded border border-red-800/40 text-bad">suspended</span>
                      )}
                    </div>
                    <div className="text-[9px] text-ink-mid mt-0.5 font-mono" title={s.cron}>
                      {human.includes(tz) ? human : `${human} (${tz})`}
                    </div>
                    <div className="text-[9px] text-ink-mid mt-0.5 truncate">
                      {s.prompt.slice(0, 80)}
                      {s.prompt.length > 80 ? "..." : ""}
                    </div>
                    {s.suspended && (
                      <div className="text-[9px] text-bad mt-0.5">
                        Suspended{s.suspended_reason ? `: ${s.suspended_reason}` : ""} ({s.consecutive_failures} failed run
                        {s.consecutive_failures === 1 ? "" : "s"} in a row)
                      </div>
                    )}
                    <div className="text-[9px] text-ink-mid mt-1">Next: {formatLightTime(s.next_run_at)}</div>
                  </div>
                  {state === "parked" && (
                    <button
                      type="button"
                      className={RUN_BUTTON}
                      onClick={() => void runNow(s.name)}
                      disabled={busy !== null}
                      aria-label={`Run schedule ${s.name} now`}
                    >
                      {busy === `run:${s.name}` ? "Starting…" : "Run now"}
                    </button>
                  )}
                </div>
              </li>
            );
          })}
        </ul>
      )}
    </section>
  );
}
