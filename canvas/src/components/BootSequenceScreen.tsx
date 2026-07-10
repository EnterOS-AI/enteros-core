"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import type { Node } from "@xyflow/react";
import type { WorkspaceNodeData, BootStep } from "@/store/canvas";
import { WORKSPACE_STATUS } from "@/lib/workspace-status";

/**
 * "Enter OS" workspace boot-sequence screen.
 *
 * Replaces the opaque "provisioning" spinner with a watchdog-driven,
 * per-step boot animation: each boot step is a 3D keycap that presses-in
 * and glows amber while working, pops up lit green when done, and shows a
 * red ring + failure reason when it fails (so a stuck boot fails LOUDLY
 * instead of hanging). An overall progress bar + a live watchdog log tail
 * mirror the approved mockup. When the workspace goes `online`, the panel
 * fades out and the caller (WorkspacePanelTabs) swaps back to the normal
 * tab content.
 *
 * DATA-DRIVEN: the keycaps come from `node.data.bootSteps` (appended by the
 * BOOT_STEP WS handler in canvas-events.ts). If no BOOT_STEP events have
 * arrived, the screen degrades gracefully to a GENERIC INDETERMINATE boot —
 * the default ordered step set is rendered dim with a marquee "booting…"
 * so a runtime that doesn't yet emit boot events still shows a coherent
 * boot screen rather than a blank panel.
 *
 * The tactile keycap physics (press/pop/glow box-shadow stacks + keyframes)
 * live in styles/boot-sequence.css; this component owns layout + state and
 * uses the canvas semantic Tailwind tokens for everything else.
 */

/** Default ordered boot plan — the sensible fallback keycap set when the
 *  runtime hasn't emitted BOOT_STEP events yet (indeterminate boot) and the
 *  reference ordering the runtime emitter should follow. `key` is the short
 *  keycap legend; `label` the human name. */
const DEFAULT_BOOT_PLAN: { key: string; label: string }[] = [
  { key: "PWR", label: "Provision compute" },
  { key: "RT", label: "Start runtime" },
  { key: "A2A", label: "Wire transport" },
  { key: "PLG", label: "Install plugins" },
  { key: "ID", label: "Load identity" },
  { key: "MCP", label: "Connect management MCP" },
  { key: "TOOL", label: "Enumerate tools" },
  { key: "NET", label: "Go online" },
];

/** A single log line rendered in the watchdog terminal. */
interface WatchLine {
  id: string;
  /** monotonic seconds since the boot screen mounted, e.g. "2.4s". */
  t: string;
  /** running | ok | failed — drives the line color. */
  status: "running" | "ok" | "failed" | "info";
  text: string;
}

/** Per-keycap view model derived from the received steps (or the default
 *  plan when none have arrived). */
interface KeyView {
  key: string;
  label: string;
  /** idle | running | done | failed — drives the keycap class + progress. */
  state: "idle" | "running" | "done" | "failed";
}

interface Props {
  /** The provisioning workspace node whose boot sequence to render. */
  node: Node<WorkspaceNodeData>;
}

export function BootSequenceScreen({ node }: Props) {
  const data = node.data;
  const bootSteps = useMemo<BootStep[]>(
    () => (Array.isArray(data.bootSteps) ? data.bootSteps : []),
    [data.bootSteps],
  );
  // True once ANY BOOT_STEP has arrived — distinguishes the real,
  // step-driven boot from the indeterminate fallback.
  const haveEvents = bootSteps.length > 0;

  // ── Ordered keycap plan ────────────────────────────────────────────────
  // When events are flowing, use the received steps in their reported order
  // (deduped by `step`, latest status wins — the store already merges in
  // place, but we sort by step index defensively). `total` from the latest
  // event sizes the plan so trailing not-yet-started keys still render dim.
  const keys = useMemo<KeyView[]>(() => {
    if (!haveEvents) {
      return DEFAULT_BOOT_PLAN.map((p) => ({ ...p, state: "idle" as const }));
    }
    const sorted = [...bootSteps].sort((a, b) => a.step - b.step);
    const total = Math.max(...sorted.map((s) => s.total), sorted.length);
    const byIndex = new Map<number, BootStep>();
    for (const s of sorted) byIndex.set(s.step, s);
    const out: KeyView[] = [];
    for (let i = 1; i <= total; i++) {
      const s = byIndex.get(i);
      if (s) {
        out.push({
          key: s.key,
          label: s.label,
          state: s.status === "ok" ? "done" : s.status === "failed" ? "failed" : "running",
        });
      } else {
        // A step index we haven't heard about yet — render a dim placeholder
        // sized from the default plan when possible so the row width is
        // stable as steps stream in.
        const fallback = DEFAULT_BOOT_PLAN[i - 1];
        out.push({
          key: fallback?.key ?? String(i),
          label: fallback?.label ?? `Step ${i}`,
          state: "idle",
        });
      }
    }
    return out;
  }, [haveEvents, bootSteps]);

  const doneCount = keys.filter((k) => k.state === "done").length;
  const failedStep = keys.find((k) => k.state === "failed") ?? null;
  const total = keys.length || 1;
  const pct = Math.round((doneCount / total) * 100);
  const online = data.status === WORKSPACE_STATUS.Online;
  const failed = data.status === WORKSPACE_STATUS.Failed || failedStep !== null;

  // ── Watchdog log ───────────────────────────────────────────────────────
  // Derived append-only from the boot steps' messages. Each step's latest
  // message becomes one log line; we keep our own list keyed by step so the
  // running→ok transition replaces the running line rather than stacking a
  // duplicate. Falls back to a single "watchdog attached" line when there
  // are no events yet.
  const mountedAt = useRef<number>(Date.now());
  const seenRef = useRef<Map<number, string>>(new Map());
  const [log, setLog] = useState<WatchLine[]>([]);
  const logEndRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    if (!haveEvents) return;
    const seen = seenRef.current;
    const additions: WatchLine[] = [];
    for (const s of [...bootSteps].sort((a, b) => a.step - b.step)) {
      // Dedup key: step index + status so running and ok both log once.
      const sig = `${s.step}:${s.status}`;
      if (seen.get(s.step) === sig) continue;
      seen.set(s.step, sig);
      const secs = ((Date.now() - mountedAt.current) / 1000).toFixed(1) + "s";
      const text =
        s.message ??
        (s.status === "ok"
          ? `${s.label} — done`
          : s.status === "failed"
            ? `${s.label} — FAILED`
            : `${s.label}…`);
      additions.push({
        id: `${s.step}-${s.status}-${additions.length}`,
        t: secs,
        status: s.status,
        text,
      });
    }
    if (additions.length > 0) setLog((prev) => [...prev, ...additions]);
  }, [haveEvents, bootSteps]);

  // Auto-scroll the log to the newest line.
  useEffect(() => {
    logEndRef.current?.scrollIntoView({ block: "nearest" });
  }, [log]);

  // ── ENTER OS strike → fade ─────────────────────────────────────────────
  // The panel visually "enters the OS" when the workspace goes online. We
  // don't own the tab swap (WorkspacePanelTabs re-renders the tabs once
  // status !== provisioning); we just play the strike + fade so the
  // handoff reads as a single motion. `hit` arms the fade-out class.
  const [hit, setHit] = useState(false);
  useEffect(() => {
    if (online) {
      const id = window.setTimeout(() => setHit(true), 400);
      return () => window.clearTimeout(id);
    }
    setHit(false);
  }, [online]);

  const armed = online && !hit;

  const runtime = data.runtime || "runtime";
  const statusLabel = failed
    ? "Boot failed"
    : online
      ? "Ready"
      : "Booting";

  return (
    <div
      className={`flex flex-col h-full min-h-0 overflow-y-auto bg-surface p-4 transition-opacity duration-500 ${
        hit ? "opacity-0" : "opacity-100"
      }`}
      role="status"
      aria-live="polite"
      aria-label={`Workspace boot sequence — ${statusLabel}`}
      data-testid="boot-sequence-screen"
    >
      {/* Header */}
      <div className="flex items-center justify-between gap-3 mb-4">
        <div className="min-w-0">
          <div className="text-[10px] tracking-[0.2em] uppercase text-ink-soft">
            Provisioning workspace
          </div>
          <div className="text-[13px] text-ink truncate font-mono" title={node.id}>
            {data.name || node.id}
          </div>
        </div>
        <div className="flex items-center gap-2 text-[11px] text-ink-mid border border-line-soft rounded-full px-2.5 py-1 shrink-0">
          <span
            aria-hidden="true"
            className={`w-1.5 h-1.5 rounded-full ${
              failed ? "bg-bad" : online ? "bg-good" : "bg-warm motion-safe:animate-pulse"
            }`}
          />
          <span>{statusLabel}</span>
          <span className="text-ink-soft">
            · <span className="text-ink">{runtime}</span>
          </span>
        </div>
      </div>

      {/* Keycap deck */}
      <div className="grid grid-cols-4 gap-2.5 rounded-2xl p-3.5 mb-4 bg-surface-sunken border border-line-soft [box-shadow:inset_0_2px_14px_rgba(0,0,0,0.4)]">
        {keys.map((k, i) => {
          const fillWidth =
            k.state === "done" || k.state === "failed" ? "100%" : k.state === "running" ? "72%" : "0%";
          const fillColor =
            k.state === "done" ? "var(--color-good)" : k.state === "failed" ? "var(--color-bad)" : "var(--color-warm)";
          const codeColor =
            k.state === "done" ? "text-good" : k.state === "failed" ? "text-bad" : k.state === "running" ? "text-warm" : "text-ink-soft";
          const codeLabel =
            k.state === "done" ? "ok" : k.state === "failed" ? "error" : k.state === "running" ? "run" : "idle";
          return (
            <div
              key={`${k.key}-${i}`}
              className={`mol-boot-key px-2.5 pt-3.5 pb-7 text-center ${
                k.state === "running" ? "is-active" : k.state === "done" ? "is-done" : k.state === "failed" ? "is-failed" : ""
              }`}
              aria-label={`${k.label}: ${codeLabel}`}
            >
              <span className="block text-[15px] font-extrabold tracking-wide text-ink leading-none mb-1.5">
                {k.key}
              </span>
              <span className={`absolute top-2 right-2.5 text-[8px] tracking-widest uppercase ${codeColor}`}>
                {codeLabel}
              </span>
              <span className="block text-[9px] leading-tight text-ink-mid min-h-[24px]">
                {k.label}
              </span>
              <span className="absolute left-3 right-3 bottom-2.5 h-[3px] rounded bg-white/[0.06] overflow-hidden">
                <span
                  className="mol-boot-key-fill block h-full rounded"
                  style={{ width: fillWidth, background: fillColor, boxShadow: `0 0 8px ${fillColor}` }}
                />
              </span>
            </div>
          );
        })}
      </div>

      {/* Overall progress */}
      <div className="flex items-center gap-3 mb-4">
        <div className="flex-1 h-1.5 rounded bg-white/5 border border-line-soft overflow-hidden">
          <span
            className="block h-full transition-[width] duration-500 ease-out"
            style={{
              width: `${pct}%`,
              background: failed
                ? "linear-gradient(90deg, var(--color-bad), var(--color-bad))"
                : "linear-gradient(90deg, var(--color-accent-strong), var(--color-accent))",
              boxShadow: failed
                ? "0 0 14px color-mix(in srgb, var(--color-bad) 60%, transparent)"
                : "0 0 14px var(--color-accent)",
            }}
          />
        </div>
        <div className="text-[11px] text-ink-mid tabular-nums min-w-[64px] text-right">
          <span className="text-ink">{pct}</span>% · {doneCount}/{total}
        </div>
      </div>

      {/* Watchdog log */}
      <div className="rounded-xl border border-line-soft overflow-hidden mb-4 bg-surface-sunken">
        <div className="flex items-center gap-2 px-3 py-2 border-b border-line-soft text-[10px] tracking-[0.16em] uppercase text-ink-mid">
          <span
            aria-hidden="true"
            className="w-1.5 h-1.5 rounded-full bg-accent motion-safe:animate-pulse"
          />
          Watchdog · live boot telemetry
        </div>
        <ul className="m-0 px-3 py-2.5 h-[140px] overflow-y-auto text-[11.5px] leading-relaxed font-mono">
          {!haveEvents ? (
            <li className="flex gap-2 text-ink-mid mol-boot-logline">
              <span className="text-ink-soft tabular-nums shrink-0">0.0s</span>
              <span>
                watchdog attached · waiting for boot telemetry
                <span className="motion-safe:animate-pulse"> …</span>
              </span>
            </li>
          ) : (
            log.map((l) => (
              <li
                key={l.id}
                className={`flex gap-2 mol-boot-logline ${
                  l.status === "ok"
                    ? "text-ink"
                    : l.status === "failed"
                      ? "text-bad"
                      : l.status === "running"
                        ? "text-warm"
                        : "text-ink-mid"
                }`}
              >
                <span className="text-ink-soft tabular-nums shrink-0">{l.t}</span>
                <span className="whitespace-pre-wrap">{l.text}</span>
              </li>
            ))
          )}
          <div ref={logEndRef} />
        </ul>
      </div>

      {/* Failure banner — a stuck boot fails loud, with the reason inline. */}
      {failedStep && (
        <div
          role="alert"
          className="rounded-xl border border-bad/40 bg-bad/10 px-3.5 py-2.5 mb-4 text-[11.5px] text-bad leading-relaxed"
        >
          <span className="font-semibold">Boot failed at {failedStep.label}.</span>{" "}
          {bootSteps.find((s) => s.label === failedStep.label && s.status === "failed")?.message ??
            "The runtime reported this step could not complete."}
        </div>
      )}

      {/* ENTER OS key */}
      <button
        type="button"
        disabled={!armed}
        aria-label={armed ? "Enter OS — workspace ready" : failed ? "Boot failed" : "Enter OS — locked, boot in progress"}
        className={`mol-boot-enter w-full flex items-center justify-center gap-3.5 px-5 py-4 rounded-[14px] font-mono text-[16px] font-extrabold tracking-[0.14em] border ${
          armed
            ? "is-armed text-ink border-accent/60 cursor-pointer"
            : hit
              ? "is-hit text-ink border-accent/60"
              : "text-ink-soft border-line cursor-not-allowed"
        }`}
      >
        <span className="text-[18px] opacity-70" aria-hidden="true">⏎</span>
        <span>{failed ? "BOOT FAILED" : "ENTER OS"}</span>
        <span className="text-[11px] font-semibold tracking-[0.1em] uppercase text-ink-soft">
          {failed ? `halted at step ${(failedStep && keys.indexOf(failedStep) + 1) || "?"}/${total}` : armed ? "ready · entering…" : "locked · boot in progress"}
        </span>
      </button>
    </div>
  );
}
