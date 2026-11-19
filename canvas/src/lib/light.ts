/** Enter OS Light — the canvas side of docs/api-protocol/light-api.md.
 *
 *  A Light agent sleeps between its scheduled runs: while it is `parked`,
 *  each schedule fires as a short-lived copy of the agent and nothing wakes
 *  the agent itself (chat, restart, pause and resume answer 409). This module
 *  holds the wire types, tolerant parsers for `GET /light` and
 *  `GET /light/runs`, the error-code → sentence table (branch on `code`,
 *  never on `error`), and the time and label helpers the map badge, the
 *  Schedule and Chat tabs and the context menu share.
 *
 *  Leaf module (no React, no store) like workspace-status.ts. */

export const LIGHT_STATES = ["open", "parking", "parked", "opening"] as const;
export type LightState = (typeof LIGHT_STATES)[number];

/** A wire `light_state` value, or null for an ordinary agent (or a value
 *  this canvas does not know). */
export function parseLightState(v: unknown): LightState | null {
  return typeof v === "string" && (LIGHT_STATES as readonly string[]).includes(v)
    ? (v as LightState)
    : null;
}

/** parking or parked: every implicit wake path (chat, restart, pause,
 *  resume) answers 409 LIGHT_AGENT_PARKED / LIGHT_AGENT_PARKING. */
export function lightBlocksWake(state: LightState | null | undefined): boolean {
  return state === "parking" || state === "parked";
}

/** A park or open in progress: poll fast until it settles. */
export function lightInTransition(state: LightState | null | undefined): boolean {
  return state === "parking" || state === "opening";
}

export interface LightSchedule {
  name: string;
  cron: string;
  timezone: string;
  prompt: string;
  enabled: boolean;
  suspended: boolean;
  suspended_reason: string | null;
  suspended_at: string | null;
  consecutive_failures: number;
  cronjob: string | null;
  /** The schedule's next fire time (RFC 3339 UTC), null when off,
   *  suspended or unparseable. */
  next_run_at: string | null;
}

export interface LightLastError {
  code: string;
  message: string;
  at: string | null;
}

/** GET /workspaces/{id}/light, the fields the canvas reads. */
export interface LightInfo {
  light_state: LightState | null;
  light_state_at: string | null;
  refresh_pending: boolean;
  last_error: LightLastError | null;
  schedules: LightSchedule[];
}

export interface LightRun {
  run_id: string;
  schedule: string;
  /** running, succeeded, failed, timed_out or cancelled. */
  status: string;
  /** report (the run pod reported it) or job (recorded from Kubernetes). */
  source: string;
  scheduled_for: string | null;
  started_at: string | null;
  finished_at: string | null;
  exit_code: number | null;
  output: string;
  error: string;
  created_at: string;
  activity_count: number;
  activity_path: string;
}

export interface LightRunsPage {
  runs: LightRun[];
  next_before: string | null;
}

type Obj = Record<string, unknown>;

function isObj(v: unknown): v is Obj {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}
function str(v: unknown, fallback = ""): string {
  return typeof v === "string" ? v : fallback;
}
function strOrNull(v: unknown): string | null {
  return typeof v === "string" && v !== "" ? v : null;
}
function num(v: unknown, fallback = 0): number {
  return typeof v === "number" && Number.isFinite(v) ? v : fallback;
}

function parseSchedule(v: unknown): LightSchedule | null {
  if (!isObj(v) || typeof v.name !== "string" || v.name === "") return null;
  return {
    name: v.name,
    cron: str(v.cron),
    timezone: str(v.timezone),
    prompt: str(v.prompt),
    enabled: v.enabled !== false,
    suspended: v.suspended === true,
    suspended_reason: strOrNull(v.suspended_reason),
    suspended_at: strOrNull(v.suspended_at),
    consecutive_failures: num(v.consecutive_failures),
    cronjob: strOrNull(v.cronjob),
    next_run_at: strOrNull(v.next_run_at),
  };
}

/** Parses a GET /light body; anything off-contract degrades to "nothing
 *  known" rather than throwing into a render. */
export function parseLightInfo(raw: unknown): LightInfo {
  if (!isObj(raw)) {
    return { light_state: null, light_state_at: null, refresh_pending: false, last_error: null, schedules: [] };
  }
  const le = raw.last_error;
  return {
    light_state: parseLightState(raw.light_state),
    light_state_at: strOrNull(raw.light_state_at),
    refresh_pending: raw.refresh_pending === true,
    last_error:
      isObj(le) && typeof le.code === "string"
        ? { code: le.code, message: str(le.message), at: strOrNull(le.at) }
        : null,
    schedules: Array.isArray(raw.schedules)
      ? raw.schedules.map(parseSchedule).filter((s): s is LightSchedule => s !== null)
      : [],
  };
}

function parseRun(v: unknown): LightRun | null {
  if (!isObj(v) || typeof v.run_id !== "string" || v.run_id === "") return null;
  return {
    run_id: v.run_id,
    schedule: str(v.schedule),
    status: str(v.status),
    source: str(v.source),
    scheduled_for: strOrNull(v.scheduled_for),
    started_at: strOrNull(v.started_at),
    finished_at: strOrNull(v.finished_at),
    exit_code: typeof v.exit_code === "number" ? v.exit_code : null,
    output: str(v.output),
    error: str(v.error),
    created_at: str(v.created_at),
    activity_count: num(v.activity_count),
    activity_path: str(v.activity_path),
  };
}

/** Parses a GET /light/runs body. */
export function parseLightRunsPage(raw: unknown): LightRunsPage {
  if (!isObj(raw)) return { runs: [], next_before: null };
  return {
    runs: Array.isArray(raw.runs)
      ? raw.runs.map(parseRun).filter((r): r is LightRun => r !== null)
      : [],
    next_before: strOrNull(raw.next_before),
  };
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

/** A failed Light call: the HTTP status, the body's `code` and extras, and
 *  the server's own sentence (`error`) or the thrown error's message. */
export interface LightApiError {
  status?: number;
  code?: string;
  message: string;
  body: Obj;
  retryAfter?: string | null;
}

/** Reads a thrown `lib/api.ts` ApiError: it keeps `status` and the raw
 *  response body (`bodyText`), from which the code and extras come. */
export function parseLightError(err: unknown): LightApiError {
  const e = (isObj(err) || err instanceof Error ? err : {}) as {
    status?: unknown;
    bodyText?: unknown;
    retryAfter?: unknown;
    message?: unknown;
  };
  let body: Obj = {};
  if (typeof e.bodyText === "string" && e.bodyText !== "") {
    try {
      const parsed: unknown = JSON.parse(e.bodyText);
      if (isObj(parsed)) body = parsed;
    } catch {
      // Not JSON (an HTML gateway page): no code, keep the error's message.
    }
  }
  const code = typeof body.code === "string" && body.code !== "" ? body.code : undefined;
  const serverMessage = typeof body.error === "string" && body.error !== "" ? body.error : undefined;
  const ownMessage =
    typeof e.message === "string" && e.message !== "" ? e.message : String(err);
  return {
    status: typeof e.status === "number" ? e.status : undefined,
    code,
    message: serverMessage ?? ownMessage,
    body,
    retryAfter: typeof e.retryAfter === "string" ? e.retryAfter : null,
  };
}

function invalidList(extras: Obj): string {
  const items = Array.isArray(extras.invalid) ? extras.invalid : [];
  return items
    .filter(isObj)
    .map((i) => {
      const where = [str(i.field), str(i.reason)].filter(Boolean).join(": ");
      const name = str(i.schedule);
      return name ? `${name} (${where})` : where;
    })
    .filter(Boolean)
    .join("; ");
}

/** One sentence per error code of the Light routes (light-api.md), with the
 *  named extras folded in. */
const CODE_SENTENCES: Record<string, (extras: Obj) => string> = {
  WORKSPACE_NOT_FOUND: () => "This agent no longer exists.",
  LIGHT_FAILED: () => "The step failed on the platform. It is safe to try again.",
  LIGHT_TENANT_NOT_FOUND: () =>
    "The platform has no Kubernetes namespace for this organization, so it cannot run Light here.",
  LIGHT_UNSUPPORTED: () =>
    "This organization cannot run Light agents: Light needs the platform's Kubernetes substrate.",
  LIGHT_BACKEND_UNAVAILABLE: () =>
    "The platform's control plane did not answer, so nothing changed. Try again in a few seconds.",
  LIGHT_TRANSITION_IN_PROGRESS: () =>
    "Another Light step is running on this agent. Try again in a few seconds.",
  LIGHT_SELF_FORBIDDEN: () =>
    "An agent cannot manage its own Light state. Sign in as the owner to do this.",
  LIGHT_AGENT_PAUSED: () => "The agent is paused. Resume it, then enable Light.",
  LIGHT_RUNTIME_UNSUPPORTED: (x) =>
    `Light runs hermes agents only; this agent runs ${str(x.runtime) || "another runtime"}.`,
  LIGHT_PLATFORM_AGENT: () => "The organization's platform agent cannot be a Light agent.",
  LIGHT_NOT_ENABLED: () => "This agent is not a Light agent.",
  LIGHT_AGENT_NOT_AWAKE: (x) => {
    const status = str(x.status);
    return `The agent is not online${status ? ` (it is ${status})` : ""}. Park it once it is online.`;
  },
  LIGHT_AGENT_NOT_READY: () =>
    "The agent is starting or restarting and does not answer yet, so nothing changed. Try again in a few seconds.",
  LIGHT_AGENT_BUSY: () => "The agent is working on a task. Park it once the task is done.",
  LIGHT_RESTART_REQUIRED: () =>
    "The agent is not running with Light switched on yet. Restart it (or enable Light again), then park it.",
  LIGHT_TOO_LARGE: () =>
    "The agent's schedules are too large to park (over 8 MiB, or over the platform's limit). Shorten or remove some, then park.",
  LIGHT_SCHEDULES_UNREADABLE: () =>
    "The agent answered with something that is not a list of schedules, so it was not parked. Check the agent's schedules, then park it.",
  LIGHT_INVALID: (x) => {
    const list = invalidList(x);
    return list
      ? `Some schedules cannot run as Light schedules: ${list}.`
      : "Some schedules cannot run as Light schedules.";
  },
  LIGHT_ENV_UNSUPPORTED: (x) =>
    `The agent's environment variable ${str(x.key) || "(unnamed)"} cannot be copied into its runs. Change or remove it, then park.`,
  LIGHT_RUN_ACTIVE: (x) =>
    `That schedule already has a run going${str(x.run_id) ? ` (${str(x.run_id)})` : ""}.`,
  LIGHT_RUN_EXISTS: (x) =>
    `That schedule already started a run this second${str(x.run_id) ? ` (${str(x.run_id)})` : ""}.`,
  LIGHT_AGENT_AWAKE: () => "The agent's pod is still stopping. Try again in a few seconds.",
  LIGHT_TENANT_SUSPENDED: () =>
    "The organization is suspended for billing, so Light runs cannot start.",
  LIGHT_SCHEDULE_NOT_FOUND: () =>
    "The parked agent has no schedule with that name. Reload the list and try again.",
  LIGHT_NOT_PARKED: () => "Run now works only while the agent is parked.",
  LIGHT_ENABLED: () =>
    "This is a Light agent: park it instead of hibernating it, and disable Light before switching its provider.",
  LIGHT_AGENT_PARKED: () => "This Light agent is parked. Open it to chat.",
  LIGHT_AGENT_PARKING: () => "This Light agent is being parked. Open it to chat.",
};

/** The sentence for a code, or `fallback` (the server's own message) for a
 *  code this canvas does not know. */
export function lightCodeSentence(code: string | undefined, extras: Obj, fallback: string): string {
  const f = code ? CODE_SENTENCES[code] : undefined;
  return f ? f(extras) : fallback;
}

/** A human sentence for any error a Light call threw. */
export function lightErrorSentence(err: unknown): string {
  const p = parseLightError(err);
  return lightCodeSentence(p.code, p.body, p.message);
}

/** `last_error` codes (light-api.md, GET /light). A code here can mean
 *  something else than the same code answered by a call: LIGHT_AGENT_BUSY in
 *  last_error is a park that was undone. */
const LAST_ERROR_SENTENCES: Record<string, string> = {
  LIGHT_AGENT_BUSY: "A park was undone because the agent was working. It is open.",
  LIGHT_PARK_INCOMPLETE:
    "Parking is not finished: stopping the agent or resuming its schedules failed. Core keeps retrying.",
  LIGHT_PARK_FAILED: "The agent failed while it was being parked, so it is open again.",
  LIGHT_OPEN_FAILED: "The agent failed to start while opening. It is open: restart it.",
  LIGHT_OPEN_INCOMPLETE:
    "An open paused the schedules and has not woken the agent yet. If the open was interrupted, core resumes the schedules within two minutes.",
  LIGHT_REFRESH_OPEN_FAILED:
    "A refresh could not bring the agent up, so it was parked again on its previous setup. Core retries the refresh in 10 minutes.",
  LIGHT_ACTION_FAILED:
    "A queued change failed while the agent was open for a refresh. Core retries it in 10 minutes.",
};

/** The banner sentence for GET /light's `last_error`: the doc's table, then
 *  the refusal codes of park and open, then the server's `message`. */
export function lightLastErrorSentence(le: LightLastError): string {
  return LAST_ERROR_SENTENCES[le.code] ?? lightCodeSentence(le.code, {}, le.message || le.code);
}

// ---------------------------------------------------------------------------
// Times and labels
// ---------------------------------------------------------------------------

/** How long a park must have held `parking` before open may take it back
 *  (light-api.md, "A stuck park"). */
export const LIGHT_STUCK_PARK_MS = 3 * 60 * 1000;

export function isStuckPark(
  info: Pick<LightInfo, "light_state" | "light_state_at">,
  now: number = Date.now(),
): boolean {
  if (info.light_state !== "parking" || !info.light_state_at) return false;
  const at = Date.parse(info.light_state_at);
  return Number.isFinite(at) && now - at >= LIGHT_STUCK_PARK_MS;
}

const NO_UPCOMING_RUN = "no upcoming run";

/** An RFC 3339 time in the viewer's local time zone, with the zone named
 *  ("Sat, Nov 14, 9:00 AM GMT+9"). Null or unparseable reads "no upcoming
 *  run" (the only nullable time callers show without their own guard). */
export function formatLightTime(iso: string | null | undefined, opts: { seconds?: boolean } = {}): string {
  if (!iso) return NO_UPCOMING_RUN;
  const t = new Date(iso);
  if (Number.isNaN(t.getTime())) return NO_UPCOMING_RUN;
  return new Intl.DateTimeFormat(undefined, {
    weekday: "short",
    month: "short",
    day: "numeric",
    hour: "numeric",
    minute: "2-digit",
    ...(opts.seconds ? { second: "2-digit" } : {}),
    timeZoneName: "short",
  }).format(t);
}

/** "3m 34s" between two RFC 3339 times; null when either is missing or the
 *  end precedes the start. */
export function formatLightDuration(start: string | null | undefined, end: string | null | undefined): string | null {
  if (!start || !end) return null;
  const ms = Date.parse(end) - Date.parse(start);
  if (!Number.isFinite(ms) || ms < 0) return null;
  const s = Math.round(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

/** The schedule with the earliest next_run_at, or null when none is due. */
export function earliestNextRun(schedules: readonly LightSchedule[]): LightSchedule | null {
  let best: LightSchedule | null = null;
  let bestAt = Infinity;
  for (const s of schedules) {
    if (!s.next_run_at) continue;
    const at = Date.parse(s.next_run_at);
    if (Number.isFinite(at) && at < bestAt) {
      best = s;
      bestAt = at;
    }
  }
  return best;
}

/** The map badge: "Light · next <local time>" for a parked agent with an
 *  upcoming run ("Light · parked" without one), else the state's name. */
export function lightBadgeText(state: LightState, schedules: readonly LightSchedule[] | null): string {
  if (state !== "parked") return `Light · ${state}`;
  const next = schedules ? earliestNextRun(schedules) : null;
  return next ? `Light · next ${formatLightTime(next.next_run_at)}` : "Light · parked";
}

export const PARKED_STATUS_LABEL = "Parked";
export const PARKED_STATUS_TOOLTIP = "Asleep between scheduled runs";

/** A parked Light agent whose pod is asleep (status hibernated): the status
 *  surfaces read "Parked" instead of the hibernated status. */
export function isParkedStatus(status: string, lightState: LightState | null | undefined): boolean {
  return lightState === "parked" && status === "hibernated";
}

/** The status a surface shows: a parked Light agent (whose pod is
 *  hibernated) reads "Parked" with a tooltip; anything else is unchanged, so
 *  a parked agent in an unexpected status (failed) stays visible as such. */
export function workspaceStatusLabel(
  status: string,
  lightState: LightState | null | undefined,
): { label: string; tooltip?: string } {
  if (isParkedStatus(status, lightState)) {
    return { label: PARKED_STATUS_LABEL, tooltip: PARKED_STATUS_TOOLTIP };
  }
  return { label: status };
}
