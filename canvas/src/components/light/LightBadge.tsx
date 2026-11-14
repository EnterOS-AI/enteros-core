"use client";

import { useEffect, useState } from "react";
import { fetchLight } from "@/lib/api/light";
import { earliestNextRun, lightBadgeText, type LightSchedule, type LightState } from "@/lib/light";

/** Once the badge's "next" time has passed, read GET /light again so the
 *  badge moves on to the following run instead of showing a past time. */
const ROLL_OVER_GRACE_MS = 5_000;
/** setTimeout's ceiling (about 24.8 days). */
const MAX_TIMER_MS = 2 ** 31 - 1;

const STYLE: Record<LightState, string> = {
  open: "text-accent bg-accent/12 border-accent/35",
  parked: "text-ink-mid bg-surface-card border-line",
  parking: "text-warm bg-warm/10 border-warm/40",
  opening: "text-warm bg-warm/10 border-warm/40",
};

interface Props {
  workspaceId: string;
  lightState: LightState;
}

/**
 * The map node's Light badge: "Light · next <local time>" for a parked agent
 * (from the earliest next_run_at; "Light · parked" with none), else the
 * state's name. Rendered only for Light nodes; it reads GET /light only while
 * the node is parked (the one state whose text needs it), on mount and when
 * the node's light_state changes, not on every map re-hydrate.
 */
export function LightBadge({ workspaceId, lightState }: Props) {
  const [schedules, setSchedules] = useState<LightSchedule[] | null>(null);
  const [rollOver, setRollOver] = useState(0);
  const parked = lightState === "parked";

  useEffect(() => {
    if (!parked) {
      setSchedules(null);
      return;
    }
    let cancelled = false;
    fetchLight(workspaceId).then(
      (info) => {
        if (!cancelled) setSchedules(info.schedules);
      },
      () => {
        // The badge falls back to "Light · parked"; the Schedule tab shows errors.
      },
    );
    return () => {
      cancelled = true;
    };
  }, [workspaceId, parked, rollOver]);

  const next = parked && schedules ? earliestNextRun(schedules) : null;
  const nextAt = next?.next_run_at ?? null;
  useEffect(() => {
    if (!nextAt) return;
    const wait = Date.parse(nextAt) - Date.now() + ROLL_OVER_GRACE_MS;
    const t = setTimeout(() => setRollOver((n) => n + 1), Math.min(Math.max(wait, ROLL_OVER_GRACE_MS), MAX_TIMER_MS));
    return () => clearTimeout(t);
  }, [nextAt]);

  return (
    <span
      data-testid="light-badge"
      title={next ? `Enter OS Light: next run of ${next.name}` : "Enter OS Light: asleep between its scheduled runs when parked"}
      className={`max-w-[260px] truncate text-[10px] font-mono px-2 py-1 rounded-md border ${STYLE[lightState]}`}
    >
      {lightBadgeText(lightState, schedules)}
    </span>
  );
}
