"use client";

import { useEffect, useId, useMemo, useRef, useState } from "react";
import { usePolling } from "@/hooks/usePolling";
import { fetchLightRuns, fetchRunActivity, LIGHT_RUNS_PAGE_SIZE } from "@/lib/api/light";
import { formatLightDuration, formatLightTime, lightErrorSentence, type LightRun, type LightRunsPage } from "@/lib/light";
import type { ActivityEntry } from "@/types/activity";

/** The runs list refreshes its loaded rows this often while the agent is a
 *  Light agent (its runs come and finish on their own); an ordinary agent's
 *  history is read once. */
export const LIGHT_RUNS_POLL_MS = 10_000;

interface Props {
  workspaceId: string;
  isLight: boolean;
  /** Schedule names to offer in the filter besides those seen in runs. */
  scheduleNames: string[];
  /** Bumped by the tab to refresh now (after "Run now"). */
  refreshKey: number;
}

const STATUS_STYLE: Record<string, { label: string; className: string }> = {
  running: { label: "running", className: "text-accent border-accent/40" },
  succeeded: { label: "succeeded", className: "text-good border-good/40" },
  failed: { label: "failed", className: "text-bad border-red-800/40" },
  timed_out: { label: "timed out", className: "text-warm border-warm/40" },
  cancelled: { label: "cancelled", className: "text-ink-mid border-line" },
};

const LINK_BUTTON =
  "text-[10px] text-accent hover:underline rounded focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-surface disabled:opacity-40";

interface ActivityState {
  loading: boolean;
  rows: ActivityEntry[];
  error: string | null;
}

/**
 * A Light agent's runs, newest first (GET /light/runs): a schedule filter,
 * "Load more" through next_before, and per run its status, times, exit code,
 * error, output tail and the activity rows it wrote (listed inline: the
 * Activity tab has no run filter). Shown whenever the agent is a Light agent
 * or has runs; an ordinary agent without runs renders nothing.
 */
export function LightRunsList({ workspaceId, isLight, scheduleNames, refreshKey }: Props) {
  const filterId = useId();
  const [filter, setFilter] = useState("");
  const [runs, setRuns] = useState<LightRun[]>([]);
  const [nextBefore, setNextBefore] = useState<string | null>(null);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [loadingMore, setLoadingMore] = useState(false);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [activity, setActivity] = useState<Record<string, ActivityState>>({});
  const countRef = useRef(0);
  countRef.current = runs.length;
  const filterRef = useRef(filter);
  filterRef.current = filter;
  // Bumped by "Load more": a refresh read that started before it must not
  // replace the rows it appended. (A filter change needs no bump: it changes
  // the polling key, which drops the old loop's reads.)
  const generation = useRef(0);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const poll = usePolling<{ page: LightRunsPage; gen: number }>({
    key: `${workspaceId}|${filter}|${isLight}`,
    enabled: true,
    load: async () => {
      const gen = generation.current;
      // Re-read as many rows as are shown (capped by the server's 100), so a
      // refresh keeps the pages "Load more" added.
      const limit = Math.min(100, Math.max(LIGHT_RUNS_PAGE_SIZE, countRef.current));
      const page = await fetchLightRuns(workspaceId, { limit, schedule: filter || null });
      return { page, gen };
    },
    apply: ({ page, gen }) => {
      if (gen !== generation.current) return;
      setRuns(page.runs);
      setNextBefore(page.next_before);
      setLoaded(true);
      setError(null);
    },
    onError: (e) => {
      setLoaded(true);
      setError(lightErrorSentence(e));
    },
    nextDelay: () => (isLight ? LIGHT_RUNS_POLL_MS : null),
  });

  // A filter change starts over from the first page (the polling key changes
  // with it, so the first page is fetched at once).
  const changeFilter = (next: string) => {
    if (next === filter) return;
    setFilter(next);
    setRuns([]);
    setNextBefore(null);
    setLoaded(false);
    setError(null);
  };

  const firstKey = useRef(refreshKey);
  useEffect(() => {
    if (firstKey.current === refreshKey) return;
    firstKey.current = refreshKey;
    poll.refresh();
  }, [refreshKey, poll]);

  const loadMore = async () => {
    if (!nextBefore || loadingMore) return;
    setLoadingMore(true);
    const gen = ++generation.current;
    const forFilter = filter;
    try {
      const page = await fetchLightRuns(workspaceId, { before: nextBefore, schedule: forFilter || null });
      if (!alive.current || gen !== generation.current || forFilter !== filterRef.current) return;
      setRuns((prev) => {
        const seen = new Set(prev.map((r) => r.run_id));
        return [...prev, ...page.runs.filter((r) => !seen.has(r.run_id))];
      });
      setNextBefore(page.next_before);
      setError(null);
    } catch (e) {
      if (alive.current) setError(lightErrorSentence(e));
    } finally {
      if (alive.current) setLoadingMore(false);
    }
  };

  const toggle = (run: LightRun) => {
    const opening = !expanded.has(run.run_id);
    setExpanded((prev) => {
      const next = new Set(prev);
      if (opening) next.add(run.run_id);
      else next.delete(run.run_id);
      return next;
    });
    if (opening && run.activity_count > 0 && run.activity_path && !activity[run.run_id]) {
      setActivity((prev) => ({ ...prev, [run.run_id]: { loading: true, rows: [], error: null } }));
      fetchRunActivity(run.activity_path).then(
        (rows) => {
          if (alive.current) setActivity((prev) => ({ ...prev, [run.run_id]: { loading: false, rows, error: null } }));
        },
        (e: unknown) => {
          if (alive.current)
            setActivity((prev) => ({ ...prev, [run.run_id]: { loading: false, rows: [], error: lightErrorSentence(e) } }));
        },
      );
    }
  };

  const names = useMemo(
    () => Array.from(new Set([...scheduleNames, ...runs.map((r) => r.schedule)])).filter(Boolean).sort(),
    [scheduleNames, runs],
  );

  if (!isLight && filter === "" && (!loaded || runs.length === 0)) return null;

  return (
    <section aria-label="Light runs" data-testid="light-runs" className="border-t border-line/50">
      <div className="flex items-center justify-between gap-2 px-3 py-2 border-b border-line/50">
        <span className="text-[10px] font-semibold text-ink-mid uppercase tracking-wider">Runs</span>
        <div className="flex items-center gap-1">
          <label htmlFor={filterId} className="text-[10px] text-ink-mid">
            Schedule
          </label>
          <select
            id={filterId}
            value={filter}
            onChange={(e) => changeFilter(e.target.value)}
            className="text-[10px] bg-surface-card border border-line rounded px-1 py-0.5 text-ink focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent"
          >
            <option value="">All schedules</option>
            {names.map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </div>
      </div>
      {error && (
        <div role="alert" aria-live="assertive" className="px-3 py-1.5 text-[10px] text-bad bg-red-900/20 border-b border-red-800/30">
          {error}
        </div>
      )}
      {loaded && runs.length === 0 && !error && (
        <p className="px-3 py-3 text-[10px] text-ink-mid">
          {filter ? `No runs of ${filter} yet.` : "No runs yet. A run shows here once its pod starts."}
        </p>
      )}
      {!loaded && <p className="px-3 py-3 text-[10px] text-ink-mid">Loading runs…</p>}
      <ul>
        {runs.map((run) => {
          const style = STATUS_STYLE[run.status] ?? { label: run.status || "unknown", className: "text-ink-mid border-line" };
          const duration = formatLightDuration(run.started_at, run.finished_at);
          const isOpen = expanded.has(run.run_id);
          const detailsId = `${filterId}-${run.run_id}`;
          const act = activity[run.run_id];
          return (
            <li key={run.run_id} className="px-3 py-2 border-b border-line/30" data-testid={`light-run-${run.run_id}`}>
              <div className="flex flex-wrap items-center gap-1.5">
                <span className="text-[10px] font-medium text-ink">{run.schedule}</span>
                <span className={`text-[9px] px-1 rounded border ${style.className}`}>{style.label}</span>
                {run.source === "job" && (
                  <span className="text-[9px] text-ink-mid" title="The run never reported; core recorded it from its Kubernetes Job">
                    recorded from Kubernetes
                  </span>
                )}
              </div>
              <dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-2 gap-y-0.5 text-[9px] text-ink-mid">
                {run.scheduled_for && (
                  <>
                    <dt>Scheduled</dt>
                    <dd>{formatLightTime(run.scheduled_for)}</dd>
                  </>
                )}
                <dt>Started</dt>
                <dd>{run.started_at ? formatLightTime(run.started_at, { seconds: true }) : "not started"}</dd>
                <dt>Finished</dt>
                <dd>
                  {run.finished_at
                    ? formatLightTime(run.finished_at, { seconds: true })
                    : run.status === "running"
                      ? "still running"
                      : "not recorded"}
                </dd>
                {duration && (
                  <>
                    <dt>Duration</dt>
                    <dd>{duration}</dd>
                  </>
                )}
                {run.exit_code !== null && (
                  <>
                    <dt>Exit code</dt>
                    <dd>{run.exit_code}</dd>
                  </>
                )}
              </dl>
              {run.error && <p className="mt-1 text-[9px] text-bad break-words">Error: {run.error}</p>}
              <button
                type="button"
                className={`${LINK_BUTTON} mt-1`}
                aria-expanded={isOpen}
                aria-controls={detailsId}
                onClick={() => toggle(run)}
              >
                {isOpen ? "Hide" : "Show"} output and activity ({run.activity_count} activity row
                {run.activity_count === 1 ? "" : "s"})
              </button>
              {isOpen && (
                <div id={detailsId} className="mt-1 space-y-1">
                  {run.output ? (
                    <pre className="max-h-48 overflow-auto whitespace-pre-wrap break-words rounded bg-surface-sunken border border-line/40 p-1.5 text-[9px] text-ink">
                      {run.output}
                    </pre>
                  ) : (
                    <p className="text-[9px] text-ink-mid">No output recorded.</p>
                  )}
                  {run.activity_count === 0 ? (
                    <p className="text-[9px] text-ink-mid">The run wrote no activity.</p>
                  ) : !act || act.loading ? (
                    <p className="text-[9px] text-ink-mid">Loading activity…</p>
                  ) : act.error ? (
                    <p role="alert" className="text-[9px] text-bad">
                      {act.error}
                    </p>
                  ) : (
                    <ul aria-label={`Activity of run ${run.run_id}`} className="space-y-0.5">
                      {act.rows.map((row) => (
                        <li key={row.id} className="text-[9px] text-ink-mid">
                          <span className="font-mono">{formatLightTime(row.created_at, { seconds: true })}</span>{" "}
                          <span className="text-ink">{row.activity_type}</span>
                          {row.summary ? ` · ${row.summary}` : row.method ? ` · ${row.method}` : ""}
                          {row.status && row.status !== "ok" ? ` · ${row.status}` : ""}
                        </li>
                      ))}
                    </ul>
                  )}
                </div>
              )}
            </li>
          );
        })}
      </ul>
      {nextBefore && (
        <div className="px-3 py-2">
          <button type="button" className={LINK_BUTTON} onClick={() => void loadMore()} disabled={loadingMore}>
            {loadingMore ? "Loading…" : "Load more"}
          </button>
        </div>
      )}
    </section>
  );
}
