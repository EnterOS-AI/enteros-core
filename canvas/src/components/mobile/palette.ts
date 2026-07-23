// Mobile design system tokens.
//
// SSOT (core#mobile-design-parity): the CORE palette — bg/surface/surface2/
// border/divider/text/text2/text3 + the Enter OS brand accent — is kept in
// sync with the canonical canvas @theme in
// `molecule-core/canvas/src/app/globals.css` (the same app this mobile UI
// ships inside). Earlier this palette shipped a divergent set built from a
// Claude Design handoff (GREEN accent #2f9e6a + lighter warm-paper) — it now
// adopts the canvas Enter OS surfaces (white light / deep-navy dark) and the
// periwinkle accent so the mobile version has the SAME design as the desktop
// canvas.
// `palette.ssot.test.ts` asserts these core values equal the canvas tokens;
// `green`/`online` map to the canvas `good`, status/tier badges stay
// mobile-specific. Don't hand-edit the core values to differ from canvas —
// change the canvas SSOT and re-sync.

export interface MobilePalette {
  bg: string;
  surface: string;
  surface2: string;
  border: string;
  divider: string;
  text: string;
  text2: string;
  text3: string;

  green: string;
  greenSoft: string;
  greenInk: string;

  t1Bg: string; t1Ink: string; t1Br: string;
  t2Bg: string; t2Ink: string; t2Br: string;
  t3Bg: string; t3Ink: string; t3Br: string;
  t4Bg: string; t4Ink: string; t4Br: string;

  t4SoftCard: string;

  online: string;
  starting: string;
  degraded: string;
  failed: string;
  paused: string;
  offline: string;

  remote: string;
  remoteBg: string;
  accent: string;
}

export const MOL_LIGHT: MobilePalette = {
  // Core — canvas @theme light SSOT (surface / surface-elevated /
  // surface-card / line / line-soft / ink / ink-mid / ink-soft).
  bg: "#ffffff",
  surface: "#ffffff",
  surface2: "#fafafc",
  border: "#d6d6d6",
  divider: "#e8e8e8",
  text: "#000000",
  text2: "#4d4d4d",
  text3: "#6e6e78",

  // green/online map to the canvas `good` (#25794a, AA-gated — core#2742);
  // soft/ink tints derived.
  green: "#25794a",
  greenSoft: "#d9ebe0",
  greenInk: "#1f6a47",

  t1Bg: "#dde6f1", t1Ink: "#3a6aa3", t1Br: "#b9c8de",
  t2Bg: "#dbe5f4", t2Ink: "#2f5fb4", t2Br: "#b1c2e0",
  t3Bg: "#e3dcef", t3Ink: "#6a4ba1", t3Br: "#c8b9e1",
  t4Bg: "#f5dcc7", t4Ink: "#a8501d", t4Br: "#e8c6a4",

  t4SoftCard: "#f9ece0",

  online: "#25794a",
  starting: "#e9b53b",
  degraded: "#d28a2a",
  failed: "#c8472a",
  paused: "#7a8696",
  offline: "#9aa0a6",

  remote: "#7a4dd1",
  remoteBg: "#ede2ff",
  accent: "#6663d6", // canvas Enter OS accent (was green #2f9e6a)
};

export const MOL_DARK: MobilePalette = {
  // Core — canvas @theme dark SSOT (deep-navy surfaces + bright ink).
  bg: "#010120",
  surface: "#0b0b38",
  surface2: "#101042",
  border: "#232350",
  divider: "#19193f",
  text: "#f4f4ff",
  text2: "#a3a3c8",
  text3: "#8888b0",

  // green/online map to the canvas dark `good` (#3fd69a).
  green: "#3fd69a",
  greenSoft: "#1f3a2c",
  greenInk: "#7fd3a8",

  t1Bg: "#1a2230", t1Ink: "#7ea4d4", t1Br: "#2a3a52",
  t2Bg: "#1b2434", t2Ink: "#86a6e2", t2Br: "#2c3c58",
  t3Bg: "#251f33", t3Ink: "#b39be0", t3Br: "#3e3450",
  t4Bg: "#332316", t4Ink: "#e5a878", t4Br: "#553622",

  t4SoftCard: "#2a1f17",

  online: "#3fd69a",
  starting: "#e9b53b",
  degraded: "#d28a2a",
  failed: "#d65a3e",
  paused: "#8a96a6",
  offline: "#6a6a6a",

  remote: "#a38aff",
  remoteBg: "#2a1f44",
  accent: "#bdbbff", // canvas Enter OS dark accent (was green #3eb37c)
};

/**
 * Pure-function variant of palette resolution. No React, no context,
 * no mutation — for tests and other non-component code.
 *
 * Components should import `usePalette` from `./palette-context` so the
 * user's accent override (held in context, not in module state) flows
 * through automatically. Re-exported below so the existing
 * `import { usePalette } from "./palette"` call sites keep working.
 */
export const getPalette = (dark: boolean): MobilePalette => (dark ? MOL_DARK : MOL_LIGHT);

// Back-compat re-export. Once we're confident nothing imports
// `usePalette` from this file we can drop this line.
export { usePalette } from "./palette-context";

// References the CSS variables that next/font/local emits in
// app/layout.tsx. Falls through to system fonts if the variable is
// undefined (e.g. in unit tests with no <body> font class).
export const MOBILE_FONT_SANS = "var(--font-inter-tight), 'Inter Tight', ui-sans-serif, system-ui, sans-serif";
export const MOBILE_FONT_MONO = "var(--font-jetbrains), 'JetBrains Mono', ui-monospace, monospace";

// Status keys we surface in the mobile UI. Anything else from the
// platform falls back to "offline" tinting — the desktop has more
// statuses ("provisioning", etc.) than the design's 6-key palette.
export type MobileStatus =
  | "online" | "starting" | "degraded" | "failed" | "paused" | "offline";

export function normalizeStatus(s: string | undefined | null): MobileStatus {
  if (s === "online" || s === "degraded" || s === "failed" || s === "paused" || s === "offline") {
    return s;
  }
  if (s === "provisioning" || s === "starting") return "starting";
  return "offline";
}

// Platform tier (number 1-4) → design tier code "T1".."T4"
export function tierCode(tier: number | undefined | null): "T1" | "T2" | "T3" | "T4" {
  const n = typeof tier === "number" ? tier : 2;
  if (n <= 1) return "T1";
  if (n === 2) return "T2";
  if (n === 3) return "T3";
  return "T4";
}
