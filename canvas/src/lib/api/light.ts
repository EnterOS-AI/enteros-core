/** Enter OS Light REST client (docs/api-protocol/light-api.md). Every call
 *  goes through the shared `api` helper, so a failure throws its ApiError
 *  (status + raw body); `lightErrorSentence` in `@/lib/light` turns that into
 *  the sentence a user sees. */
import { api } from "@/lib/api";
import {
  parseLightInfo,
  parseLightRunsPage,
  parseLightState,
  type LightInfo,
  type LightRunsPage,
  type LightState,
} from "@/lib/light";
import type { ActivityEntry } from "@/types/activity";

const lightPath = (workspaceId: string) => `/workspaces/${workspaceId}/light`;

/** GET /workspaces/{id}/light. */
export async function fetchLight(workspaceId: string): Promise<LightInfo> {
  return parseLightInfo(await api.get<unknown>(lightPath(workspaceId)));
}

export interface LightRunsQuery {
  limit?: number;
  /** The previous page's next_before. */
  before?: string | null;
  /** Only this schedule's runs. */
  schedule?: string | null;
}

export const LIGHT_RUNS_PAGE_SIZE = 20;

/** GET /workspaces/{id}/light/runs, newest first. */
export async function fetchLightRuns(workspaceId: string, q: LightRunsQuery = {}): Promise<LightRunsPage> {
  const params = new URLSearchParams({ limit: String(q.limit ?? LIGHT_RUNS_PAGE_SIZE) });
  if (q.schedule) params.set("schedule", q.schedule);
  if (q.before) params.set("before", q.before);
  return parseLightRunsPage(await api.get<unknown>(`${lightPath(workspaceId)}/runs?${params.toString()}`));
}

/** What a management call answered: the state after it (null = not a Light
 *  agent), plus the call's own fields. */
export interface LightActionResult {
  light_state: LightState | null;
  /** enable/disable/park/open: "unchanged" when nothing was done. */
  status: string | null;
  /** enable/disable: started | woken | refresh_queued | none. */
  restart: string | null;
  /** open: the queued runs it cancelled. */
  cancelled: string[];
}

function parseAction(raw: unknown): LightActionResult {
  const o = (typeof raw === "object" && raw !== null ? raw : {}) as Record<string, unknown>;
  return {
    light_state: parseLightState(o.light_state),
    status: typeof o.status === "string" ? o.status : null,
    restart: typeof o.restart === "string" ? o.restart : null,
    cancelled: Array.isArray(o.cancelled) ? o.cancelled.filter((c): c is string => typeof c === "string") : [],
  };
}

export async function enableLight(workspaceId: string): Promise<LightActionResult> {
  return parseAction(await api.post<unknown>(`${lightPath(workspaceId)}/enable`));
}

export async function disableLight(workspaceId: string): Promise<LightActionResult> {
  return parseAction(await api.post<unknown>(`${lightPath(workspaceId)}/disable`));
}

export async function parkLight(workspaceId: string): Promise<LightActionResult> {
  return parseAction(await api.post<unknown>(`${lightPath(workspaceId)}/park`));
}

export async function openLight(workspaceId: string): Promise<LightActionResult> {
  return parseAction(await api.post<unknown>(`${lightPath(workspaceId)}/open`));
}

/** POST /workspaces/{id}/light/run: one run of a parked agent's schedule now
 *  (201 {"run_id"}). */
export async function runLightSchedule(workspaceId: string, schedule: string): Promise<string | null> {
  const r = (await api.post<unknown>(`${lightPath(workspaceId)}/run`, { schedule })) as Record<string, unknown> | null;
  return r && typeof r.run_id === "string" ? r.run_id : null;
}

/** The activity rows one run wrote: GET on the run's activity_path. */
export async function fetchRunActivity(activityPath: string): Promise<ActivityEntry[]> {
  const rows = await api.get<unknown>(activityPath);
  return Array.isArray(rows)
    ? rows.filter((r): r is ActivityEntry => typeof r === "object" && r !== null && typeof (r as ActivityEntry).id === "string")
    : [];
}
