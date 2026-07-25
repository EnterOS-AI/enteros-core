"use client";

/**
 * SelfHostSetupScene — the self-host first-run gated onboarding scene
 * (design SSOT: molecule-selfhost-onboarding-scene, §2 gate / §3 flow /
 * §4 wire sequence / §8 error copy).
 *
 * A fullscreen BLOCKING overlay mounted at the root (page.tsx, above the
 * desktop/mobile view switch) that renders ONLY on positive confirmation of
 * the self-host + unconfigured-root gate (gate.ts — fail-closed-to-invisible;
 * a gate bug can hide the scene on self-host but can never blank a SaaS
 * tenant). It is a pure CONFIGURATOR of the always-seeded platform root:
 * five steps (welcome → runtime → provider+model → API key → configure),
 * then the §4 wire sequence and a provision watch that dismisses on online.
 *
 * Stateless by design: every step is re-derivable from server state — no
 * localStorage anywhere. Mid-flow refresh resumes via deriveResumeView.
 *
 * SSOT consumption (§5.2): WORKSPACE_KIND / WORKSPACE_STATUS /
 * WORKSPACE_ERROR_CODES / WS_EVENTS mirrors, runtimeDisplayName, the
 * ProviderModelSelector data layer (via setup-scene-lib), and /templates
 * registry fields. Zero hardcoded runtime/provider/status/error lists.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api } from "@/lib/api";
import { useCanvasStore } from "@/store/canvas";
import { subscribeSocketEvents } from "@/store/socket-events";
import { WS_EVENTS } from "@/lib/ws-events";
import { WORKSPACE_KIND } from "@/lib/workspace-kind";
import { WORKSPACE_STATUS } from "@/lib/workspace-status";
import { runtimeDisplayName } from "@/lib/runtime-names";
import {
  inferGroup,
  validateSecretValue,
} from "@/lib/validation/secret-formats";
import {
  ProviderModelSelector,
  type SelectorValue,
} from "@/components/ProviderModelSelector";
import { Spinner } from "@/components/Spinner";
import { BootSequenceScreen } from "@/components/BootSequenceScreen";
import {
  defaultGateDeps,
  evaluateSelfHostSetupGate,
  type GateResult,
} from "./gate";
import { runConfigureSequence, runEnsure } from "./configure";
import {
  PLATFORM_AGENT_NAME,
  buildSceneCatalog,
  deriveResumeView,
  focusFirstElement,
  handleFocusTrapKeyDown,
  humanizeSetupError,
  pickPlatformRow,
  type PlatformRowLike,
  type SetupErrorView,
} from "./setup-scene-lib";

/** GET /workspaces polling cadence while watching a provision — the fallback
 *  for a dropped websocket (the store's socket updates are the primary). */
export const WATCH_POLL_MS = 5_000;

/** After this long in the watching phase, surface the §8 slow-provision copy
 *  (first-boot image pulls are slow; the server-side timeout sweep is 12m —
 *  the scene must not false-fail before it). */
export const SLOW_PROVISION_HINT_MS = 90_000;

const EMPTY_SELECTION: SelectorValue = { providerId: "", model: "", envVars: [] };

type Phase =
  | { kind: "form" }
  | { kind: "submitting" }
  | { kind: "watching" }
  | { kind: "error"; view: SetupErrorView };

/** The platform node's status/error read reactively off the canvas store —
 *  the same kind='platform' signal ConciergeShell uses. */
function platformStatusOf(nodes: Array<{ id: string; data: Record<string, unknown> }>): string {
  const node = nodes.find((n) => n.data.kind === WORKSPACE_KIND.Platform);
  return node ? String(node.data.status ?? "") : "";
}
function platformErrorOf(nodes: Array<{ id: string; data: Record<string, unknown> }>): string {
  const node = nodes.find((n) => n.data.kind === WORKSPACE_KIND.Platform);
  return node ? String(node.data.lastSampleError ?? "") : "";
}

const inputClass =
  "w-full bg-surface-sunken border border-line rounded px-2.5 py-2 text-[13px] text-ink font-mono focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1 focus-visible:border-accent transition-colors";
const primaryBtnClass =
  "px-4 py-2 bg-accent hover:bg-accent-strong rounded-lg text-[13px] font-medium text-white transition-colors disabled:opacity-50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/60 focus-visible:ring-offset-2";
const secondaryBtnClass =
  "px-4 py-2 bg-surface-card hover:bg-surface-elevated rounded-lg text-[13px] text-ink-mid hover:text-ink transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-1";

export function SelfHostSetupScene() {
  const [gate, setGate] = useState<GateResult | null>(null);
  const [dismissed, setDismissed] = useState(false);
  const [step, setStep] = useState(1);
  const [runtimeValue, setRuntimeValue] = useState("");
  const [selection, setSelection] = useState<SelectorValue>(EMPTY_SELECTION);
  const [keyValue, setKeyValue] = useState("");
  const [keyBanner, setKeyBanner] = useState<string | null>(null);
  const [phase, setPhase] = useState<Phase>({ kind: "form" });
  const [slowHint, setSlowHint] = useState(false);

  // Keys written by THIS session's wire sequence. Gate-time has_value keys
  // cannot overlap the picker's auth envs (an LLM key with has_value hides
  // the scene entirely), so "already configured" is session-derived: it
  // becomes true after a wrong-key round-trip (§8 → back to step 4).
  const writtenKeysRef = useRef<Set<string>>(new Set());
  const busyRef = useRef(false);
  const containerRef = useRef<HTMLDivElement>(null);

  const gateCtx = gate !== null && gate.show ? gate.context : null;
  const rootId = gateCtx !== null ? gateCtx.rootId : null;
  const seededRuntime = gateCtx !== null ? gateCtx.rootRuntime : "";

  // ── Gate evaluation (once, on mount — the mount point already guarantees
  //    first /workspaces hydration completed, so no pre-hydration flash) ──
  useEffect(() => {
    let cancelled = false;
    evaluateSelfHostSetupGate(defaultGateDeps).then((result) => {
      if (cancelled) return;
      setGate(result);
      // Tell the shell the gate has a verdict (either way) so it can drop
      // its pre-gate loading screen — see selfHostGateResolved in the store.
      useCanvasStore.getState().setSelfHostGateResolved(true);
      if (!result.show) return;
      const ctx = result.context;
      // Pre-select the existing platform root's runtime when offerable.
      const preselect = ctx.runtimeOptions.some(
        (o) => o.value === ctx.rootRuntime,
      )
        ? ctx.rootRuntime
        : "";
      setRuntimeValue(preselect);
      // Stateless resume (§2): an interrupted setup re-enters the progress /
      // error view straight from the root's server-side status.
      const resume = deriveResumeView(ctx.rootStatus);
      if (resume === "progress") {
        setStep(5);
        setPhase({ kind: "watching" });
      } else if (resume === "failed") {
        setStep(5);
        setPhase({
          kind: "error",
          view: humanizeSetupError(
            { message: ctx.rootLastError },
            { runtimeLabel: runtimeDisplayName(preselect), providerLabel: "" },
          ),
        });
      }
    }).catch(() => {
      // Gate evaluation failed — fail-closed-to-invisible for the scene, but
      // the shell must still drop its pre-gate loading screen (a spinner
      // that never resolves is worse than the legacy no-concierge view).
      if (!cancelled) useCanvasStore.getState().setSelfHostGateResolved(true);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  // ── Derived picker state ──
  const runtimeOptions = useMemo(
    () => (gateCtx !== null ? gateCtx.runtimeOptions : []),
    [gateCtx],
  );
  const runtimeOption = useMemo(() => {
    const found = runtimeOptions.find((o) => o.value === runtimeValue);
    return found !== undefined ? found : null;
  }, [runtimeOptions, runtimeValue]);
  const catalog = useMemo(
    () => (runtimeOption !== null ? buildSceneCatalog(runtimeOption) : []),
    [runtimeOption],
  );
  const selectedProvider = useMemo(() => {
    const found = catalog.find((p) => p.id === selection.providerId);
    return found !== undefined ? found : null;
  }, [catalog, selection.providerId]);
  const keyName = selection.envVars.length > 0 ? selection.envVars[0] : "";
  const keyAlreadyConfigured =
    keyName !== "" && writtenKeysRef.current.has(keyName);
  const skipKeyWrite =
    keyName === "" || (keyAlreadyConfigured && keyValue === "");
  const providerLabel = selectedProvider !== null ? selectedProvider.label : "";
  const runtimeLabel = runtimeDisplayName(runtimeValue);
  // Non-blocking client-side format hint (secret-formats.ts). Never blocks
  // submission: legit third-party keys ride Anthropic-compatible env names
  // (e.g. MiniMax via ANTHROPIC_AUTH_TOKEN) and must not be rejected client-side.
  const formatHint =
    keyValue !== "" ? validateSecretValue(keyValue, inferGroup(keyName)) : null;

  // ── Failure mapping shared by the wire sequence + the provision watch ──
  const applyFailure = useCallback(
    (code: string | null, message: string) => {
      const view = humanizeSetupError({ code, message }, {
        runtimeLabel,
        providerLabel,
      });
      if (view.returnToKeyStep) {
        // §8 wrong-key path: back to the API-key step with the copy inline.
        setKeyBanner(view.copy);
        setKeyValue("");
        setStep(4);
        setPhase({ kind: "form" });
      } else {
        setPhase({ kind: "error", view });
      }
    },
    [runtimeLabel, providerLabel],
  );

  // ── Provision watch: primary = the store's socket-driven status ──
  const storeStatus = useCanvasStore((s) => platformStatusOf(s.nodes));
  const storeLastError = useCanvasStore((s) => platformErrorOf(s.nodes));
  // The platform root node itself — handed to BootSequenceScreen so the watching
  // phase renders the real Enter OS boot (keycaps + watchdog log) instead of a
  // bare spinner. Same node the concierge shell boots from (#3942); the scene
  // stays mounted, so a Failed status still flips to the error card below and an
  // Online status still auto-dismisses into the concierge shell.
  const livePlatformNode = useCanvasStore(
    (s) => s.nodes.find((n) => n.data.kind === WORKSPACE_KIND.Platform) ?? null,
  );
  // Hold the last-known platform node so a TRANSIENT store update that drops
  // the Platform node (a mid-provision nodes-array churn) does not flicker the
  // boot UI back to the bare spinner. Only ever advances to a newer node; it
  // never re-nulls once a node has been seen this session.
  const lastPlatformNodeRef = useRef<typeof livePlatformNode>(null);
  if (livePlatformNode) lastPlatformNodeRef.current = livePlatformNode;
  const platformNode = livePlatformNode ?? lastPlatformNodeRef.current;

  // A mid-boot FAILED boot step (BootSequenceScreen paints its own red "Boot
  // failed" banner off this, but a BOOT_STEP is presentation-only — the node's
  // aggregate status may still read `provisioning`). Without this the user is
  // stranded on a dead red boot screen with no retry, so the scene detects a
  // failed step and flips to its own error/retry card. The step's `message`
  // becomes the humanized reason (falling back to the step label).
  const failedBootStep = useCanvasStore((s) => {
    const node = s.nodes.find((n) => n.data.kind === WORKSPACE_KIND.Platform);
    const steps = node?.data.bootSteps;
    if (!Array.isArray(steps)) return null;
    return steps.find((st) => st.status === "failed") ?? null;
  });
  useEffect(() => {
    if (phase.kind !== "watching") return;
    if (storeStatus === WORKSPACE_STATUS.Online) {
      // Scene auto-dismisses into the concierge shell; the concierge's own
      // proactive greeting IS the final onboarding step (§3 step 5).
      setDismissed(true);
      return;
    }
    if (storeStatus === WORKSPACE_STATUS.Failed) {
      applyFailure(null, storeLastError);
      return;
    }
    // Presentation-only BOOT_STEP failure (no aggregate status flip): still a
    // dead-end for the user, so flip to the error/retry card ourselves.
    if (failedBootStep !== null) {
      applyFailure(
        null,
        failedBootStep.message ??
          `Boot failed at ${failedBootStep.label}.`,
      );
    }
  }, [phase.kind, storeStatus, storeLastError, failedBootStep, applyFailure]);

  // Raw socket subscription for the provision-abort CODE — the store keeps
  // only the error text; WORKSPACE_PROVISION_FAILED's payload carries the
  // §8-mappable `code` (workspace-error-codes.ts channel 2).
  useEffect(() => {
    if (phase.kind !== "watching") return;
    return subscribeSocketEvents((msg) => {
      if (msg.event !== WS_EVENTS.WorkspaceProvisionFailed) return;
      if (rootId !== null && msg.workspace_id !== rootId) return;
      const code =
        typeof msg.payload.code === "string" ? msg.payload.code : null;
      const message =
        typeof msg.payload.error === "string" ? msg.payload.error : "";
      applyFailure(code, message);
    });
  }, [phase.kind, rootId, applyFailure]);

  // Fallback poll — GET /workspaces until the platform node terminates
  // (covers a dropped websocket; errors are ignored, next tick retries).
  useEffect(() => {
    if (phase.kind !== "watching") return;
    const timer = setInterval(() => {
      api
        .get<PlatformRowLike[]>("/workspaces")
        .then((rows) => {
          const row = pickPlatformRow(
            Array.isArray(rows) ? rows : [],
            rootId,
          );
          if (row === null) return;
          if (row.status === WORKSPACE_STATUS.Online) {
            setDismissed(true);
          } else if (row.status === WORKSPACE_STATUS.Failed) {
            applyFailure(null, row.last_sample_error ?? "");
          }
        })
        .catch(() => {});
    }, WATCH_POLL_MS);
    return () => clearInterval(timer);
  }, [phase.kind, rootId, applyFailure]);

  // Slow-provision hint (§8 provision-timeout row).
  useEffect(() => {
    if (phase.kind !== "watching") {
      setSlowHint(false);
      return;
    }
    const timer = setTimeout(() => setSlowHint(true), SLOW_PROVISION_HINT_MS);
    return () => clearTimeout(timer);
  }, [phase.kind]);

  // ── Actions ──
  const onConfigure = useCallback(async () => {
    if (busyRef.current) return; // debounce: exactly one wire sequence
    busyRef.current = true;
    setKeyBanner(null);
    setPhase({ kind: "submitting" });
    try {
      await runConfigureSequence({
        rootId,
        seededRuntime,
        runtime: runtimeValue,
        model: selection.model,
        keyName,
        keyValue,
        skipKeyWrite,
      });
      if (!skipKeyWrite) writtenKeysRef.current.add(keyName);
      setPhase({ kind: "watching" });
    } catch (err) {
      applyFailure(null, err instanceof Error ? err.message : String(err));
    } finally {
      busyRef.current = false;
    }
  }, [
    rootId,
    seededRuntime,
    runtimeValue,
    selection.model,
    keyName,
    keyValue,
    skipKeyWrite,
    applyFailure,
  ]);

  const onRetry = useCallback(async () => {
    if (busyRef.current) return;
    busyRef.current = true;
    setPhase({ kind: "submitting" });
    try {
      // Retry = re-run ensure alone (idempotent; §3 step 5).
      await runEnsure(selection.model, rootId === null ? runtimeValue : null);
      setPhase({ kind: "watching" });
    } catch (err) {
      applyFailure(null, err instanceof Error ? err.message : String(err));
    } finally {
      busyRef.current = false;
    }
  }, [selection.model, rootId, runtimeValue, applyFailure]);

  const visible = !dismissed && gate !== null && gate.show;

  // Keyboard users land inside each new view (paired with the focus trap).
  useEffect(() => {
    if (!visible) return;
    focusFirstElement(containerRef.current!);
  }, [visible, step, phase.kind]);

  if (!visible) return null;

  // While the platform agent provisions, hand the whole screen to the Enter OS
  // boot sequence (the same one the concierge shell boots from, #3942) rather
  // than shadowing it with a bare spinner. Gated on the node existing so the
  // brief pre-node window (and node-less tests) still falls through to the
  // spinner card. `scene-progress` / `scene-slow-hint` markers are preserved so
  // the phase + slow-hint contracts hold.
  if (phase.kind === "watching" && platformNode) {
    return (
      <div
        ref={containerRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="selfhost-setup-title"
        data-testid="selfhost-setup-scene"
        tabIndex={-1}
        onKeyDown={(e) => handleFocusTrapKeyDown(containerRef.current!, e)}
        className="fixed inset-0 z-[10000] bg-surface"
      >
        <h1 id="selfhost-setup-title" className="sr-only">
          Set up your platform agent
        </h1>
        <div data-testid="scene-progress" className="h-full">
          {/* No onEnter here: the Online watch above auto-dismisses the scene
              into the concierge shell the instant the root reports online, so
              the ENTER OS key never needs a manual handler in this mount. Left
              handler-less, BootSequenceScreen renders the key non-interactive
              (#9) — it never looks clickable with nothing behind it. */}
          <BootSequenceScreen node={platformNode} />
        </div>
        {slowHint && (
          <p
            data-testid="scene-slow-hint"
            className="absolute inset-x-0 bottom-4 text-center text-[11px] text-ink-mid leading-relaxed px-4"
          >
            Still provisioning — pulling the runtime image can take a few minutes
            on first boot.
          </p>
        )}
      </div>
    );
  }

  return (
    <div
      ref={containerRef}
      role="dialog"
      aria-modal="true"
      aria-labelledby="selfhost-setup-title"
      data-testid="selfhost-setup-scene"
      tabIndex={-1}
      onKeyDown={(e) => handleFocusTrapKeyDown(containerRef.current!, e)}
      className="fixed inset-0 z-[10000] bg-surface overflow-y-auto"
    >
      <div className="min-h-full flex items-center justify-center p-4">
        <div className="w-full max-w-lg bg-surface-sunken/95 border border-line/60 rounded-2xl shadow-2xl shadow-black/40 p-6">
          <div className="text-[10px] font-semibold uppercase tracking-widest text-accent mb-2">
            {phase.kind === "form" ? `Step ${step} of 5` : "Setting up"}
          </div>
          <h1 id="selfhost-setup-title" className="text-lg font-semibold text-ink mb-4">
            Set up your platform agent
          </h1>

          {phase.kind === "form" && step === 1 && (
            <div data-testid="scene-step-welcome">
              <p className="text-[13px] text-ink-mid leading-relaxed mb-3">
                Every Enter OS organization is run by a platform agent — the
                org root that creates workspaces, dispatches work, and manages
                your agent team. Yours is named{" "}
                <span className="font-semibold text-ink">
                  {PLATFORM_AGENT_NAME}
                </span>
                . {/* fixed brand name — deliberately NO name input (§3 step 1);
                    rename later via the standard workspace rename */}
                Pick its runtime, model, and API key to bring it online.
              </p>
              <div className="flex justify-end gap-2">
                <button
                  type="button"
                  data-testid="scene-continue"
                  className={primaryBtnClass}
                  onClick={() => setStep(2)}
                >
                  Get started
                </button>
              </div>
            </div>
          )}

          {phase.kind === "form" && step === 2 && (
            <div data-testid="scene-step-runtime">
              <label
                htmlFor="scene-runtime-select"
                className="text-[10px] uppercase tracking-wide text-ink-mid font-semibold mb-1.5 block"
              >
                Runtime
              </label>
              <select
                id="scene-runtime-select"
                data-testid="scene-runtime-select"
                value={runtimeValue}
                onChange={(e) => {
                  setRuntimeValue(e.target.value);
                  // Strict cascade (§3): an upstream change resets every
                  // downstream pick.
                  setSelection(EMPTY_SELECTION);
                  setKeyValue("");
                }}
                className={inputClass}
              >
                <option value="" disabled>
                  — select runtime —
                </option>
                {runtimeOptions.map((o) => (
                  <option key={o.value} value={o.value}>
                    {o.label}
                  </option>
                ))}
              </select>
              <p className="text-[11px] text-ink-mid mt-2 leading-relaxed">
                The engine your platform agent runs on. You can change this
                later in workspace settings.
              </p>
              <div className="flex justify-between gap-2 mt-4">
                <button
                  type="button"
                  data-testid="scene-back"
                  className={secondaryBtnClass}
                  onClick={() => setStep(1)}
                >
                  Back
                </button>
                <button
                  type="button"
                  data-testid="scene-continue"
                  className={primaryBtnClass}
                  disabled={runtimeValue === ""}
                  onClick={() => setStep(3)}
                >
                  Continue
                </button>
              </div>
            </div>
          )}

          {phase.kind === "form" && step === 3 && (
            <div data-testid="scene-step-model">
              <ProviderModelSelector
                models={[]}
                catalog={catalog}
                value={selection}
                onChange={setSelection}
                variant="stack"
                allowCustomModelEscape={false}
                idPrefix="scene"
              />
              <div className="flex justify-between gap-2 mt-4">
                <button
                  type="button"
                  data-testid="scene-back"
                  className={secondaryBtnClass}
                  onClick={() => setStep(2)}
                >
                  Back
                </button>
                <button
                  type="button"
                  data-testid="scene-continue"
                  className={primaryBtnClass}
                  disabled={selection.model.trim() === ""}
                  onClick={() => setStep(4)}
                >
                  Continue
                </button>
              </div>
            </div>
          )}

          {phase.kind === "form" && step === 4 && (
            <div data-testid="scene-step-key">
              {keyBanner !== null && (
                <div
                  role="alert"
                  data-testid="scene-key-banner"
                  className="text-[12px] text-bad bg-bad/10 border border-bad/40 rounded-lg px-3 py-2 mb-3"
                >
                  {keyBanner}
                </div>
              )}
              {keyName === "" ? (
                <p
                  data-testid="scene-key-none"
                  className="text-[13px] text-ink-mid leading-relaxed"
                >
                  {providerLabel} does not declare an API-key requirement —
                  nothing to configure here.
                </p>
              ) : (
                <div>
                  <label
                    htmlFor="scene-key-input"
                    className="text-[10px] uppercase tracking-wide text-ink-mid font-semibold mb-1.5 block"
                  >
                    {keyName}
                  </label>
                  <input
                    id="scene-key-input"
                    data-testid="scene-key-input"
                    type="password"
                    value={keyValue}
                    onChange={(e) => setKeyValue(e.target.value)}
                    placeholder={
                      keyAlreadyConfigured
                        ? "already configured — leave blank to keep"
                        : `paste your ${providerLabel} key`
                    }
                    spellCheck={false}
                    autoComplete="off"
                    className={inputClass}
                  />
                  {formatHint !== null && (
                    <p
                      data-testid="scene-key-format-hint"
                      className="text-[11px] text-warm mt-1.5 leading-relaxed"
                    >
                      {formatHint} — double-check before continuing.
                    </p>
                  )}
                  <p className="text-[11px] text-ink-mid mt-2 leading-relaxed">
                    Stored as an org-wide secret; the value is never shown
                    again.
                  </p>
                </div>
              )}
              <div className="flex justify-between gap-2 mt-4">
                <button
                  type="button"
                  data-testid="scene-back"
                  className={secondaryBtnClass}
                  onClick={() => setStep(3)}
                >
                  Back
                </button>
                <button
                  type="button"
                  data-testid="scene-continue"
                  className={primaryBtnClass}
                  disabled={
                    keyName !== "" &&
                    !keyAlreadyConfigured &&
                    keyValue.trim() === ""
                  }
                  onClick={() => setStep(5)}
                >
                  Continue
                </button>
              </div>
            </div>
          )}

          {phase.kind === "form" && step === 5 && (
            <div data-testid="scene-step-review">
              <dl className="text-[13px] text-ink-mid space-y-1.5 mb-4">
                <div className="flex justify-between gap-3">
                  <dt>Agent name</dt>
                  <dd className="text-ink font-medium">{PLATFORM_AGENT_NAME}</dd>
                </div>
                <div className="flex justify-between gap-3">
                  <dt>Runtime</dt>
                  <dd className="text-ink font-mono">{runtimeLabel}</dd>
                </div>
                <div className="flex justify-between gap-3">
                  <dt>Model</dt>
                  <dd className="text-ink font-mono">{selection.model}</dd>
                </div>
                <div className="flex justify-between gap-3">
                  <dt>API key</dt>
                  <dd className="text-ink font-mono">
                    {skipKeyWrite ? "unchanged" : keyName}
                  </dd>
                </div>
              </dl>
              <div className="flex justify-between gap-2">
                <button
                  type="button"
                  data-testid="scene-back"
                  className={secondaryBtnClass}
                  onClick={() => setStep(4)}
                >
                  Back
                </button>
                <button
                  type="button"
                  data-testid="scene-configure"
                  className={primaryBtnClass}
                  onClick={onConfigure}
                >
                  Configure
                </button>
              </div>
            </div>
          )}

          {phase.kind === "submitting" && (
            <div
              data-testid="scene-submitting"
              className="flex flex-col items-center gap-3 py-6"
            >
              <Spinner size="lg" />
              <p className="text-[13px] text-ink-mid">Applying configuration…</p>
            </div>
          )}

          {phase.kind === "watching" && (
            <div
              data-testid="scene-progress"
              role="status"
              aria-live="polite"
              className="flex flex-col items-center gap-3 py-6"
            >
              <Spinner size="lg" />
              <p className="text-[13px] text-ink-mid text-center">
                Provisioning {PLATFORM_AGENT_NAME}…
              </p>
              {slowHint && (
                <p
                  data-testid="scene-slow-hint"
                  className="text-[11px] text-ink-mid text-center leading-relaxed"
                >
                  Still provisioning — pulling the runtime image can take a few
                  minutes on first boot.
                </p>
              )}
            </div>
          )}

          {phase.kind === "error" && (
            <div data-testid="scene-error" className="py-2">
              <p
                role="alert"
                className="text-[13px] text-bad leading-relaxed mb-4"
              >
                {phase.view.copy}
              </p>
              <div className="flex justify-between gap-2">
                <button
                  type="button"
                  data-testid="scene-adjust"
                  className={secondaryBtnClass}
                  onClick={() => {
                    setStep(1);
                    setPhase({ kind: "form" });
                  }}
                >
                  Adjust setup
                </button>
                <button
                  type="button"
                  data-testid="scene-retry"
                  className={primaryBtnClass}
                  onClick={onRetry}
                >
                  Retry
                </button>
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}
