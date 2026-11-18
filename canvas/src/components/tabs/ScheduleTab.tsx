'use client';

import { useState, useEffect, useCallback, useId } from "react";
import { api } from "@/lib/api";
import { ConfirmDialog } from "@/components/ConfirmDialog";
import { cronToHuman } from "@/lib/cron-human";
import type { LightState } from "@/lib/light";
import { useCanvasStore, type WorkspaceNodeData } from "@/store/canvas";
import { LightCard } from "@/components/light/LightCard";
import { LightRunsList } from "@/components/light/LightRunsList";
import { LightScheduleList } from "@/components/light/LightScheduleList";
import { useLightInfo } from "@/components/light/useLightInfo";
import { useSingleFlight } from "@/components/light/useSingleFlight";

export interface Schedule {
  id: string;
  workspace_id: string;
  name: string;
  cron_expr: string;
  timezone: string;
  prompt: string;
  enabled: boolean;
  last_run_at: string | null;
  next_run_at: string | null;
  run_count: number;
  last_status: string;
  last_error: string;
  created_at: string;
}

interface Props {
  workspaceId: string;
  /** The workspace's node data. With it the tab adds the Enter OS Light card
   *  and runs list; without it (older hosts) it is the schedule list alone. */
  data?: WorkspaceNodeData;
}

function relativeTime(iso: string | null): string {
  if (!iso) return "never";
  const diff = Date.now() - new Date(iso).getTime();
  if (diff < 0) {
    const future = -diff;
    if (future < 60000) return `in ${Math.round(future / 1000)}s`;
    if (future < 3600000) return `in ${Math.round(future / 60000)}m`;
    if (future < 86400000) return `in ${Math.round(future / 3600000)}h`;
    return `in ${Math.round(future / 86400000)}d`;
  }
  if (diff < 60000) return `${Math.round(diff / 1000)}s ago`;
  if (diff < 3600000) return `${Math.round(diff / 60000)}m ago`;
  if (diff < 86400000) return `${Math.round(diff / 3600000)}h ago`;
  return `${Math.round(diff / 86400000)}d ago`;
}

export function ScheduleTab({ workspaceId, data }: Props) {
  // Enter OS Light (docs/api-protocol/light-api.md). The node's light_state
  // decides whether to poll GET /light; once polled, GET /light's answer is
  // the state the tab renders, and it corrects a node that missed an event.
  const nodeLightState = data?.lightState ?? null;
  const onLightState = useCallback(
    (state: LightState | null) => {
      const store = useCanvasStore.getState();
      const node = store.nodes.find((n) => n.id === workspaceId);
      if (node && (node.data.lightState ?? null) !== state) store.updateNodeData(workspaceId, { lightState: state });
    },
    [workspaceId],
  );
  const light = useLightInfo(workspaceId, nodeLightState, data !== undefined && nodeLightState !== null, onLightState);
  const lightState: LightState | null = data === undefined ? null : light.info ? light.info.light_state : nodeLightState;
  // Parked, parking or opening: the agent is asleep (or not up yet), so its
  // own schedule API is never called; the list comes from GET /light.
  const asleep = lightState === "parked" || lightState === "parking" || lightState === "opening";
  const { busy, run } = useSingleFlight();
  const [runsKey, setRunsKey] = useState(0);

  const [schedules, setSchedules] = useState<Schedule[]>([]);
  const [loading, setLoading] = useState(true);
  const [showForm, setShowForm] = useState(false);
  const [editId, setEditId] = useState<string | null>(null);
  const [formName, setFormName] = useState("");
  const [formCron, setFormCron] = useState("0 9 * * *");
  const [formTimezone, setFormTimezone] = useState("UTC");
  const [formPrompt, setFormPrompt] = useState("");
  const [formEnabled, setFormEnabled] = useState(true);
  const [error, setError] = useState("");
  const [pendingDelete, setPendingDelete] = useState<{ id: string; name: string } | null>(null);

  // Stable IDs for label↔input associations (WCAG 1.3.1)
  const cronId = useId();
  const timezoneId = useId();
  const promptId = useId();

  const fetchSchedules = useCallback(async () => {
    try {
      const data = await api.get<Schedule[]>(`/workspaces/${workspaceId}/schedules`);
      setSchedules(data);
      setError("");
    } catch (e: unknown) {
      setSchedules([]);
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoading(false);
    }
  }, [workspaceId]);

  useEffect(() => {
    if (asleep) return;
    fetchSchedules();
    const interval = setInterval(fetchSchedules, 10000);
    return () => clearInterval(interval);
  }, [fetchSchedules, asleep]);

  const resetForm = () => {
    setFormName("");
    setFormCron("0 9 * * *");
    setFormTimezone("UTC");
    setFormPrompt("");
    setFormEnabled(true);
    setEditId(null);
    setShowForm(false);
    setError("");
  };

  const handleSubmit = async () => {
    setError("");
    try {
      if (editId) {
        await api.patch(`/workspaces/${workspaceId}/schedules/${editId}`, {
          name: formName,
          cron_expr: formCron,
          timezone: formTimezone,
          prompt: formPrompt,
          enabled: formEnabled,
        });
      } else {
        await api.post(`/workspaces/${workspaceId}/schedules`, {
          name: formName,
          cron_expr: formCron,
          timezone: formTimezone,
          prompt: formPrompt,
          enabled: formEnabled,
        });
      }
      resetForm();
      fetchSchedules();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to save schedule");
    }
  };

  const confirmDelete = async () => {
    if (!pendingDelete) return;
    const { id } = pendingDelete;
    setPendingDelete(null);
    try {
      await api.del(`/workspaces/${workspaceId}/schedules/${id}`);
      fetchSchedules();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to delete schedule");
    }
  };

  const handleToggle = async (sched: Schedule) => {
    try {
      await api.patch(`/workspaces/${workspaceId}/schedules/${sched.id}`, {
        enabled: !sched.enabled,
      });
      fetchSchedules();
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : "Failed to toggle schedule");
    }
  };

  const handleEdit = (sched: Schedule) => {
    setFormName(sched.name);
    setFormCron(sched.cron_expr);
    setFormTimezone(sched.timezone);
    setFormPrompt(sched.prompt);
    setFormEnabled(sched.enabled);
    setEditId(sched.id);
    setShowForm(true);
  };

  const handleRunNow = async (sched: Schedule) => {
    // The agent's own run path wakes nothing, but a parked agent's runs go
    // through Light (its own list is not shown then); never post to it.
    if (asleep) return;
    try {
      const result = await api.post<{ prompt?: string; fired_by?: string }>(
        `/workspaces/${workspaceId}/schedules/${sched.id}/run`,
        {},
      );
      // Volume-authoritative workspaces: the trigger daemon already fired the
      // turn as a self-scheduler system turn (result.fired_by === "daemon").
      // Firing /a2a here too would double-fire — and as a role:user turn, which
      // is exactly the impersonation the trigger move fixes. Only the legacy
      // core-DB path hands a prompt back for the client to fire.
      if (result.fired_by !== "daemon" && result.prompt) {
        await api.post(`/workspaces/${workspaceId}/a2a`, {
          method: "message/send",
          params: {
            message: {
              role: "user",
              messageId: `manual-cron-${Date.now()}`,
              parts: [{ kind: "text", text: result.prompt }],
            },
          },
        });
      }
      fetchSchedules();
    } catch {
      setError("Failed to run schedule");
    }
  };

  const lightCard = data ? (
    <LightCard
      workspaceId={workspaceId}
      data={data}
      state={lightState}
      info={light.info}
      loadError={light.error}
      busy={busy}
      run={run}
      onState={light.applyState}
    />
  ) : null;
  // Shown for a Light agent, or an ordinary one with run history (an agent
  // that was a Light agent keeps its runs after disable).
  const runsList = data ? (
    <LightRunsList
      workspaceId={workspaceId}
      isLight={lightState !== null}
      scheduleNames={(light.info?.schedules ?? []).map((s) => s.name)}
      refreshKey={runsKey}
    />
  ) : null;

  if (asleep && lightState) {
    return (
      <div className="flex flex-col min-h-full">
        {lightCard}
        {light.info ? (
          <LightScheduleList
            workspaceId={workspaceId}
            state={lightState}
            schedules={light.info.schedules}
            busy={busy}
            run={run}
            onRunStarted={() => setRunsKey((k) => k + 1)}
          />
        ) : (
          <div className="p-4 text-[10px] text-ink-mid">Loading schedules...</div>
        )}
        {runsList}
      </div>
    );
  }

  if (loading) {
    return (
      <div className="flex flex-col min-h-full">
        {lightCard}
        <div className="p-4 text-[10px] text-ink-mid">Loading schedules...</div>
      </div>
    );
  }

  return (
    <div className="flex flex-col min-h-full">
      {lightCard}
      {/* Header */}
      <div className="flex items-center justify-between px-3 py-2 border-b border-line/50">
        <span className="text-[10px] font-semibold text-ink-mid uppercase tracking-wider">
          Schedules
        </span>
        <button
          onClick={() => { resetForm(); setShowForm(true); }}
          className="text-[11px] px-2 py-0.5 bg-accent-strong/20 text-accent rounded hover:bg-accent-strong/30 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-zinc-900"
        >
          + Add Schedule
        </button>
      </div>

      {/* Error banner — shown whether form is open or closed */}
      {error && !showForm && (
        <div className="px-3 py-1.5 text-[10px] text-bad bg-red-900/20 border-b border-red-800/30">
          {error}
        </div>
      )}

      {/* Create/Edit Form */}
      {showForm && (
        <div className="p-3 border-b border-line/50 bg-surface-sunken/50 space-y-2">
          <input
            type="text"
            aria-label="Schedule name"
            placeholder="Schedule name (e.g., Daily security scan)"
            value={formName}
            onChange={(e) => setFormName(e.target.value)}
            className="w-full text-[10px] bg-surface-card border border-line rounded px-2 py-1 text-ink placeholder:text-ink-mid"
          />
          <div className="flex gap-2">
            <div className="flex-1">
              <label htmlFor={cronId} className="text-[10px] text-ink-mid block mb-0.5">Cron Expression</label>
              <input
                id={cronId}
                type="text"
                value={formCron}
                onChange={(e) => setFormCron(e.target.value)}
                className="w-full text-[10px] bg-surface-card border border-line rounded px-2 py-1 text-ink font-mono"
              />
              <div className="text-[10px] text-ink-mid mt-0.5">
                {cronToHuman(formCron)}
              </div>
            </div>
            <div className="w-24">
              <label htmlFor={timezoneId} className="text-[10px] text-ink-mid block mb-0.5">Timezone</label>
              <select
                id={timezoneId}
                value={formTimezone}
                onChange={(e) => setFormTimezone(e.target.value)}
                className="w-full text-[10px] bg-surface-card border border-line rounded px-1 py-1 text-ink"
              >
                <option value="UTC">UTC</option>
                <option value="America/New_York">US Eastern</option>
                <option value="America/Chicago">US Central</option>
                <option value="America/Denver">US Mountain</option>
                <option value="America/Los_Angeles">US Pacific</option>
                <option value="Europe/London">London</option>
                <option value="Europe/Berlin">Berlin</option>
                <option value="Asia/Tokyo">Tokyo</option>
                <option value="Asia/Shanghai">Shanghai</option>
                <option value="Australia/Sydney">Sydney</option>
              </select>
            </div>
          </div>
          <div>
            <label htmlFor={promptId} className="text-[10px] text-ink-mid block mb-0.5">Prompt / Task</label>
            <textarea
              id={promptId}
              value={formPrompt}
              onChange={(e) => setFormPrompt(e.target.value)}
              placeholder="What should the agent do on this schedule?"
              rows={3}
              className="w-full text-[10px] bg-surface-card border border-line rounded px-2 py-1 text-ink placeholder:text-ink-mid resize-y"
            />
          </div>
          <div className="flex items-center gap-2">
            <label className="flex items-center gap-1.5 text-[10px] text-ink-mid cursor-pointer">
              <input
                type="checkbox"
                checked={formEnabled}
                onChange={(e) => setFormEnabled(e.target.checked)}
                className="rounded border-line"
              />
              Enabled
            </label>
          </div>
          {error && <div role="alert" aria-live="assertive" className="text-[10px] text-bad">{error}</div>}
          <div className="flex gap-2">
            <button
              type="button"
              onClick={handleSubmit}
              disabled={!formCron || !formPrompt}
              // Was bg-accent-strong hover:bg-accent — accent is the
              // LIGHTER variant, so this hovered lighter on white text
              // and dropped contrast below AA. Same trap fixed in
              // ConfirmDialog, ApprovalBanner.
              className="text-[11px] px-3 py-1 bg-accent text-white rounded hover:bg-accent-strong disabled:opacity-40 transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-surface"
            >
              {editId ? "Update" : "Create"}
            </button>
            <button
              type="button"
              onClick={resetForm}
              // Was hover:bg-surface-card on top of bg-surface-card —
              // silent no-op hover. Lift to surface-elevated.
              className="text-[11px] px-3 py-1 bg-surface-card text-ink-mid rounded hover:bg-surface-elevated hover:text-ink transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-surface"
            >
              Cancel
            </button>
          </div>
          <div className="text-[10px] text-ink-mid space-y-0.5">
            <div>Common patterns:</div>
            <div className="font-mono">{"0 9 * * *"} — Daily at 9:00 AM</div>
            <div className="font-mono">{"*/30 * * * *"} — Every 30 minutes</div>
            <div className="font-mono">{"0 */4 * * *"} — Every 4 hours</div>
            <div className="font-mono">{"0 9 * * 1-5"} — Weekdays at 9:00 AM</div>
          </div>
        </div>
      )}

      {/* Schedule List — the tab panel scrolls, so the runs list below
          follows the schedules instead of sitting under a second scroller. */}
      <div className="flex-1">
        {schedules.length === 0 && !showForm ? (
          <div className="p-6 text-center">
            <div className="text-2xl mb-2">⏲</div>
            <div className="text-[10px] text-ink-mid mb-1">No schedules yet</div>
            <div className="text-[9px] text-ink-mid">
              Add a schedule to run tasks automatically — daily scans, periodic reports, standup reminders.
            </div>
          </div>
        ) : (
          schedules.map((sched) => (
            <div
              key={sched.id}
              className={`px-3 py-2 border-b border-line/30 ${
                !sched.enabled ? "opacity-50" : ""
              }`}
            >
              <div className="flex items-start justify-between gap-2">
                <div className="flex-1 min-w-0">
                  <div className="flex items-center gap-1.5">
                    <button
                      onClick={() => handleToggle(sched)}
                      aria-label={
                        sched.last_status === "error"
                          ? "Last run failed — click to disable"
                          : sched.last_status === "ok"
                          ? "Last run OK — click to disable"
                          : "Never run — click to enable"
                      }
                      className={`w-2 h-2 rounded-full flex-shrink-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-zinc-900 ${
                        sched.last_status === "error"
                          ? "bg-red-400"
                          : sched.last_status === "ok"
                          ? "bg-emerald-400"
                          : "bg-surface-card"
                      }`}
                      title={sched.enabled ? "Click to disable" : "Click to enable"}
                    />
                    <span className="text-[10px] font-medium text-ink truncate">
                      {sched.name || "Unnamed schedule"}
                    </span>
                  </div>
                  <div className="text-[9px] text-ink-mid mt-0.5 font-mono">
                    {cronToHuman(sched.cron_expr)}
                    {sched.timezone !== "UTC" && (
                      <span className="text-ink-mid"> ({sched.timezone})</span>
                    )}
                  </div>
                  <div className="text-[9px] text-ink-mid mt-0.5 truncate">
                    {sched.prompt.slice(0, 80)}{sched.prompt.length > 80 ? "..." : ""}
                  </div>
                  <div className="flex items-center gap-3 mt-1 text-[8px] text-ink-mid">
                    <span>Last: {relativeTime(sched.last_run_at)}</span>
                    <span>Next: {relativeTime(sched.next_run_at)}</span>
                    <span>Runs: {sched.run_count}</span>
                  </div>
                  {sched.last_error && (
                    <div className="text-[8px] text-bad mt-0.5 truncate">
                      Error: {sched.last_error}
                    </div>
                  )}
                </div>
                <div className="flex items-center gap-1 flex-shrink-0">
                  <button
                    onClick={() => handleRunNow(sched)}
                    aria-label={`Run schedule ${sched.name} now`}
                    className="text-[11px] px-1.5 py-0.5 text-accent hover:bg-accent-strong/20 rounded transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-zinc-900"
                    title="Run now"
                  >
                    ▶
                  </button>
                  <button
                    onClick={() => handleEdit(sched)}
                    aria-label={`Edit schedule ${sched.name}`}
                    className="text-[11px] px-1.5 py-0.5 text-ink-mid hover:bg-surface-card rounded transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:ring-offset-zinc-900"
                    title="Edit"
                  >
                    ✎
                  </button>
                  <button
                    onClick={() => setPendingDelete({ id: sched.id, name: sched.name })}
                    aria-label={`Delete schedule ${sched.name}`}
                    className="text-[11px] px-1.5 py-0.5 text-bad hover:bg-red-600/20 rounded transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-red-400 focus-visible:ring-offset-1 focus-visible:ring-offset-zinc-900"
                    title="Delete"
                  >
                    ✕
                  </button>
                </div>
              </div>
            </div>
          ))
        )}
      </div>

      {runsList}

      <ConfirmDialog
        open={!!pendingDelete}
        title="Delete schedule"
        message={`Delete schedule "${pendingDelete?.name || "Unnamed"}"? This cannot be undone.`}
        confirmLabel="Delete"
        confirmVariant="danger"
        onConfirm={confirmDelete}
        onCancel={() => setPendingDelete(null)}
      />
    </div>
  );
}
