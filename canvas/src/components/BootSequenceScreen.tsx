"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import type { CSSProperties, ReactNode } from "react";
import type { Node } from "@xyflow/react";
import type { WorkspaceNodeData, BootStep, BootLogLine } from "@/store/canvas";
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
 * The tactile keycap physics (press/pop/glow keyframes) live in
 * styles/boot-sequence.css; this component owns layout + state.
 * VISUAL LANGUAGE: mirrors the approved "Enter OS — Workspace Boot
 * Sequence" mockup (artifact bbc372b1, the design SSOT): a CENTERED card
 * on a near-black page — never full-bleed — all-mono type, a 7+spacebar
 * keycap grid (the final "go online" step spans the row), violet accent /
 * amber running / green ok, and a grammar-highlighted watchdog log. All
 * colors come from the --eos-* tokens pinned under `.mol-boot`, NOT the
 * canvas app theme, so the screen is identical in light and dark themes.
 */

/** Default ordered boot plan — the sensible fallback keycap set when the
 *  runtime hasn't emitted BOOT_STEP events yet (indeterminate boot).
 *
 *  SSOT: this MUST byte-mirror the runtime's emit_boot_step call sites
 *  (molecule-ai-workspace-runtime molecule_runtime/main.py) in wall-clock
 *  order — same `key`, same `label`, same 1..8 index. The runtime is the
 *  canonical source (each step fires at the real boot phase); this array is
 *  the pre-events fallback + the per-index placeholder legend the streaming
 *  path falls back on (see `keys` useMemo below). A drift between the two
 *  means a placeholder keycap renders a DIFFERENT legend than the step that
 *  later lands on it, so keep them aligned. `key` is the short keycap legend;
 *  `label` the human name. */
const DEFAULT_BOOT_PLAN: { key: string; label: string }[] = [
  { key: "PLG", label: "Install plugins" },
  { key: "ID", label: "Load identity" },
  { key: "RT", label: "Start runtime" },
  { key: "MCP", label: "Management MCP" },
  { key: "TOOL", label: "Enumerate tools" },
  { key: "A2A", label: "Wire transport" },
  { key: "NET", label: "Register" },
  { key: "ONLINE", label: "Go online" },
];

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
  /**
   * Optional handler for the armed "ENTER OS" key. In the tab/shell mounts
   * (WorkspacePanelTabs / ConciergeShell) the caller swaps content itself once
   * the workspace reports online, so the key is a visual affordance with no
   * handler. In the self-host setup scene there is no such parent, so the
   * scene passes an explicit dismiss handler here. When omitted, the key is
   * rendered non-interactive (never clickable) so it never looks clickable
   * with nothing behind it. */
  onEnter?: () => void;
}

/** Grammar-highlight one watchdog line, mockup-style: state words render
 *  green-bold (ready / OK / connected / ONLINE / up / done), references
 *  render violet (`:8642` ports, `@pkg@1.2.3` specs). Pure text-in,
 *  spans-out — the line's textContent is unchanged, so tests and e2e
 *  substring matches keep working. */
function renderBootLogText(text: string): ReactNode[] {
  const parts = text.split(/(\bready\b|\bconnected\b|\bONLINE\b|\bOK\b|\bok\b|\bdone\b|\bup\b|:\d{2,5}\b|@[\w./@^~-]+)/);
  return parts.map((p, i) => {
    if (i % 2 === 0) return p;
    if (/^[:@]/.test(p)) {
      return (
        <span key={i} className="mol-boot-log-ref">
          {p}
        </span>
      );
    }
    return (
      <span key={i} className="mol-boot-log-kw">
        {p}
      </span>
    );
  });
}

export function BootSequenceScreen({ node, onEnter }: Props) {
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
    // Highest step that has reported anything. ONLY step 1 (the
    // provisioner's PWR, which deliberately never flips itself to ok — see
    // provisioner.go startWorkspace) is displayed as implicitly complete
    // once the runtime's own steps take over; runtime steps report their
    // own ok/failed, and force-completing them would misreport which step a
    // hung boot is actually stuck on (out-of-order/concurrent runtimes).
    const maxReported = sorted[sorted.length - 1]?.step ?? 0;
    const out: KeyView[] = [];
    for (let i = 1; i <= total; i++) {
      const s = byIndex.get(i);
      if (s) {
        out.push({
          key: s.key,
          label: s.label,
          state:
            s.status === "ok"
              ? "done"
              : s.status === "failed"
                ? "failed"
                : s.step === 1 && maxReported > 1
                  ? "done"
                  : "running",
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
  // Owned by the store (node.data.bootLog — appended by the BOOT_STEP WS
  // handler and rebuilt from the server's boot_steps replay on hydrate), so
  // remounts, reconnects and periodic rehydrates never reset it. Each line
  // carries its own immutable `t` offset (boot-telemetry.ts) — stable under
  // remounts, log-cap head drops, and server/client clock skew, unlike the
  // previous mount-relative clock that showed "11.5s" on a boot already
  // 3m40s in.
  const log = useMemo<BootLogLine[]>(
    () => (Array.isArray(data.bootLog) ? data.bootLog : []),
    [data.bootLog],
  );
  const logEndRef = useRef<HTMLDivElement | null>(null);

  // Auto-scroll the log to the newest line. `scrollIntoView` is optional-called
  // because it is not implemented in every environment (jsdom, some embedded
  // webviews) — a missing method must not crash the boot screen.
  useEffect(() => {
    logEndRef.current?.scrollIntoView?.({ block: "nearest" });
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

  // Visually "armed" (lit) the moment the workspace is online, before the
  // strike/fade — the tab/shell mounts rely on this window for the ENTER OS
  // motion. Interactivity is a separate axis: the key is only clickable when
  // an onEnter handler exists (the setup scene). In the tab/shell mounts the
  // caller swaps content itself, so the key stays a non-interactive affordance
  // rather than a dead button that merely looks clickable.
  const armed = online && !hit;
  const interactive = armed && onEnter !== undefined;

  const runtime = data.runtime || "runtime";
  const statusLabel = failed
    ? "Boot failed"
    : online
      ? "Ready"
      : "Booting";

  // Indeterminate boot — no telemetry yet, but the machine IS working.
  // Animate anyway (staggered keycap shimmer + a sweeping progress bar) so
  // the pre-telemetry screen never reads as frozen.
  const indeterminate = !haveEvents && !failed && !online;

  return (
    <div
      className={`mol-boot flex h-full min-h-0 overflow-y-auto p-4 sm:p-6 transition-opacity duration-500 ${
        hit ? "opacity-0" : "opacity-100"
      }`}
      role="status"
      aria-live="polite"
      aria-label={`Workspace boot sequence — ${statusLabel}`}
      data-testid="boot-sequence-screen"
    >
      {/* Centered card — the mockup never stretches full-bleed. */}
      <div className="mol-boot-card m-auto p-5 sm:p-7">
        {/* Header — return-key tile, eyebrow + workspace name, status pill */}
        <div className="flex items-center justify-between gap-3 mb-5">
          <div className="flex items-center gap-3.5 min-w-0">
            <span
              aria-hidden="true"
              className="mol-boot-icon flex items-center justify-center w-9 h-9 shrink-0 text-white text-[17px]"
            >
              ⏎
            </span>
            <div className="min-w-0">
              <div className="text-[10px] font-medium tracking-[0.22em] uppercase text-[var(--eos-fg-dim)]">
                Provisioning workspace
              </div>
              <div className="text-[13px] text-[var(--eos-fg)] truncate mt-0.5" title={node.id}>
                {data.name || node.id}
              </div>
            </div>
          </div>
          <div className="flex items-center gap-2 text-[11px] text-[var(--eos-fg-muted)] border border-[var(--eos-border)] rounded-full px-3 py-1.5 shrink-0 bg-black/20">
            <span
              aria-hidden="true"
              className={`w-1.5 h-1.5 rounded-full ${failed || online ? "" : "motion-safe:animate-pulse"}`}
              style={{
                background: failed
                  ? "var(--eos-bad)"
                  : online
                    ? "var(--eos-good)"
                    : "var(--eos-warm)",
              }}
            />
            <span>{statusLabel}</span>
            <span className="text-[var(--eos-fg-dim)]">
              · runtime <span className="text-[var(--eos-fg)] font-semibold">{runtime}</span>
            </span>
          </div>
        </div>

        {/* Keycap deck — auto-fit columns; the final "go online" step spans
            the row like the mockup's wide NET spacebar. */}
        <div
          className="grid gap-2.5 rounded-xl p-3.5 mb-4 bg-[var(--eos-deck)] border border-[var(--eos-border)] [box-shadow:inset_0_2px_14px_rgba(0,0,0,0.45)]"
          style={{ gridTemplateColumns: "repeat(auto-fit, minmax(96px, 1fr))" }}
        >
          {keys.map((k, i) => {
            const isWide = i === keys.length - 1 && keys.length >= 4;
            const fillWidth = k.state === "idle" ? "0%" : "100%";
            const codeColor =
              k.state === "done"
                ? "text-[var(--eos-good)]"
                : k.state === "failed"
                  ? "text-[var(--eos-bad)]"
                  : k.state === "running"
                    ? "text-[var(--eos-warm)]"
                    : "text-[var(--eos-fg-dim)]";
            const codeLabel =
              k.state === "done" ? "ok" : k.state === "failed" ? "error" : k.state === "running" ? "run" : "idle";
            return (
              <div
                key={`${k.key}-${i}`}
                className={`mol-boot-key px-2.5 ${isWide ? "pt-5 pb-8" : "pt-4 pb-7"} text-center ${
                  k.state === "running" ? "is-active" : k.state === "done" ? "is-done" : k.state === "failed" ? "is-failed" : indeterminate ? "is-waiting" : ""
                }`}
                style={{
                  ...(indeterminate ? ({ "--mol-boot-i": i } as CSSProperties) : {}),
                  ...(isWide ? { gridColumn: "1 / -1" } : {}),
                }}
                aria-label={`${k.label}: ${codeLabel}`}
              >
                <span className="block text-[15px] font-bold tracking-[0.08em] text-[var(--eos-fg)] leading-none mb-1.5">
                  {k.key}
                </span>
                <span className={`absolute top-2 right-2.5 text-[8px] tracking-widest uppercase ${codeColor}`}>
                  {codeLabel}
                </span>
                <span className={`block text-[9.5px] leading-tight text-[var(--eos-fg-muted)] ${isWide ? "" : "min-h-[24px]"}`}>
                  {k.label}
                </span>
                {/* Fill color/glow are owned by the keycap state classes in
                    boot-sequence.css; only the width is per-key state here. */}
                <span className="absolute left-3 right-3 bottom-2.5 h-[3px] rounded-[2px] bg-white/[0.06] overflow-hidden">
                  <span className="mol-boot-key-fill block h-full rounded-[2px]" style={{ width: fillWidth }} />
                </span>
              </div>
            );
          })}
        </div>

        {/* Overall progress */}
        <div className="flex items-center gap-3 mb-4">
          <div className="flex-1 h-1 rounded-full bg-white/5 overflow-hidden">
            <span
              className={`block h-full rounded-full transition-[width] duration-500 ease-out ${
                indeterminate ? "mol-boot-bar-indeterminate" : ""
              }`}
              style={{
                width: indeterminate ? "100%" : `${pct}%`,
                background: indeterminate
                  ? undefined
                  : failed
                    ? "var(--eos-bad)"
                    : "linear-gradient(90deg, var(--eos-accent-strong), var(--eos-accent))",
                boxShadow: failed
                  ? "0 0 12px color-mix(in srgb, var(--eos-bad) 60%, transparent)"
                  : "0 0 12px color-mix(in srgb, var(--eos-accent) 60%, transparent)",
              }}
            />
          </div>
          <div className="text-[11px] text-[var(--eos-fg-dim)] tabular-nums min-w-[64px] text-right">
            <span className="text-[var(--eos-fg)]">{pct}</span>% · {doneCount}/{total}
          </div>
        </div>

        {/* Watchdog log */}
        <div className="rounded-xl border border-[var(--eos-border)] overflow-hidden mb-4 bg-[var(--eos-deck)]">
          <div className="flex items-center gap-2 px-3.5 py-2 border-b border-[var(--eos-border)] text-[10px] font-medium tracking-[0.18em] uppercase text-[var(--eos-fg-dim)]">
            <span
              aria-hidden="true"
              className="w-1.5 h-1.5 rounded-full bg-[var(--eos-accent)] motion-safe:animate-pulse"
            />
            Watchdog · live boot telemetry
          </div>
          <ul className="mol-boot-log m-0 px-3.5 py-2.5 h-[150px] overflow-y-auto text-[12px] leading-[1.85]">
            {log.length === 0 ? (
              <li className="flex gap-3 text-[var(--eos-fg-muted)] mol-boot-logline">
                <span className="text-[var(--eos-fg-dim)] tabular-nums shrink-0">0.0s</span>
                <span>
                  watchdog attached · waiting for boot telemetry
                  <span className="motion-safe:animate-pulse"> …</span>
                </span>
              </li>
            ) : (
              log.map((l) => (
                <li
                  key={l.id}
                  className={`flex gap-3 mol-boot-logline ${
                    l.status === "ok"
                      ? "text-[var(--eos-fg-muted)]"
                      : l.status === "failed"
                        ? "text-[var(--eos-bad)]"
                        : "text-[var(--eos-warm)]"
                  }`}
                >
                  <span className="text-[var(--eos-fg-dim)] tabular-nums shrink-0">
                    {(l.t / 1000).toFixed(1)}s
                  </span>
                  <span className="whitespace-pre-wrap">
                    {l.status === "running" && <span aria-hidden="true">› </span>}
                    {renderBootLogText(l.text)}
                  </span>
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
            className="rounded-xl border border-[color-mix(in_srgb,var(--eos-bad)_40%,transparent)] bg-[color-mix(in_srgb,var(--eos-bad)_12%,transparent)] px-3.5 py-2.5 mb-4 text-[11.5px] text-[var(--eos-bad)] leading-relaxed"
          >
            <span className="font-semibold">Boot failed at {failedStep.label}.</span>{" "}
            {bootSteps.find((s) => s.label === failedStep.label && s.status === "failed")?.message ??
              "The runtime reported this step could not complete."}
          </div>
        )}

        {/* ENTER OS bar — armed/hit treatments live in boot-sequence.css. */}
        <button
          type="button"
          disabled={!interactive}
          onClick={interactive ? onEnter : undefined}
          aria-label={armed ? "Enter OS — workspace ready" : failed ? "Boot failed" : "Enter OS — locked, boot in progress"}
          className={`mol-boot-enter relative w-full flex items-center justify-center gap-3.5 px-5 py-5 text-[16px] font-bold tracking-[0.22em] ${
            armed
              ? `is-armed ${interactive ? "cursor-pointer" : "cursor-default"}`
              : hit
                ? "is-hit"
                : "cursor-not-allowed"
          }`}
        >
          <span
            aria-hidden="true"
            className="absolute left-3.5 bottom-2 text-[9px] font-normal tracking-[0.08em] text-[var(--eos-fg-dim)]"
          >
            enter
          </span>
          <span className="text-[17px] opacity-70" aria-hidden="true">⏎</span>
          <span>{failed ? "BOOT FAILED" : "ENTER OS"}</span>
          <span className="mol-boot-enter-sub text-[10.5px] font-medium tracking-[0.12em] uppercase opacity-75">
            {failed ? `halted at step ${(failedStep && keys.indexOf(failedStep) + 1) || "?"}/${total}` : armed ? "ready · entering…" : "locked · boot in progress"}
          </span>
        </button>
      </div>
    </div>
  );
}
