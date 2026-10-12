// @vitest-environment jsdom
//
// WCAG accessibility tests for the Toolbar component.
//
// Complements Toolbar.test.tsx (behavioral coverage) with accessibility
// coverage:
//   - aria-expanded on the help button reflects popover state
//   - aria-label on all icon-only buttons
//   - aria-pressed on the A2A topology toggle
//   - role=dialog + aria-label + aria-modal on the help popover
//   - aria-hidden suppression of decorative elements
//   - StatusPill aria-label with count and status name
//   - WsStatusPill: decorative dot aria-hidden, status text exposed
//   - focus-visible:ring class presence on all interactive buttons
//
// Pattern: no @testing-library/jest-dom — use getAttribute, className,
// classList.contains, role queries.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import React from "react";

afterEach(cleanup);

// ── Mock targets ───────────────────────────────────────────────────────────────

vi.mock("@/components/Toaster", () => ({
  showToast: vi.fn(),
}));
vi.mock("@/components/ConfirmDialog", () => ({
  ConfirmDialog: () => null,
}));
vi.mock("@/components/settings/SettingsButton", () => ({
  SettingsButton: () => null,
}));
vi.mock("@/components/settings/SettingsPanel", () => ({
  settingsGearRef: { current: null },
}));
vi.mock("@/components/ThemeToggle", () => ({
  ThemeToggle: () => null,
}));
vi.mock("@/components/KeyboardShortcutsDialog", () => ({
  KeyboardShortcutsDialog: ({ open }: { open: boolean; onClose: () => void }) =>
    open ? <div role="dialog" aria-label="Keyboard shortcuts" data-testid="shortcuts-dialog">Shortcuts</div> : null,
}));
vi.mock("@/lib/design-tokens", () => ({
  statusDotClass: (status: string) => {
    const map: Record<string, string> = {
      online: "bg-emerald-400",
      offline: "bg-zinc-500",
      paused: "bg-indigo-400",
      degraded: "bg-amber-400",
      failed: "bg-red-400",
      provisioning: "bg-sky-400",
    };
    return map[status] ?? "bg-zinc-500";
  },
}));
vi.mock("@/lib/api", () => ({
  api: {
    post: vi.fn(() => Promise.resolve()),
  },
}));

// ── Store mocks ───────────────────────────────────────────────────────────────

const mockSetShowA2AEdges = vi.fn();
const mockSetPanelTab = vi.fn();
const mockSetSearchOpen = vi.fn();
const mockUpdateNodeData = vi.fn();

const defaultStore = {
  nodes: [] as Array<{
    id: string;
    data: { name: string; role: string; tier: number; status: string; parentId: string | null; activeTasks: number; needsRestart: boolean };
  }>,
  wsStatus: "connected" as "connected" | "connecting" | "disconnected",
  showA2AEdges: false,
  selectedNodeId: null as string | null,
  sidePanelWidth: 480,
  setShowA2AEdges: mockSetShowA2AEdges,
  setPanelTab: mockSetPanelTab,
  setSearchOpen: mockSetSearchOpen,
  updateNodeData: mockUpdateNodeData,
  selectedNodeIds: new Set<string>(),
  clearSelection: vi.fn(),
  batchRestart: vi.fn(() => Promise.resolve()),
  batchPause: vi.fn(() => Promise.resolve()),
  batchDelete: vi.fn(() => Promise.resolve()),
};

beforeEach(() => {
  defaultStore.nodes = [];
  defaultStore.wsStatus = "connected";
  defaultStore.showA2AEdges = false;
  defaultStore.selectedNodeId = null;
  mockSetShowA2AEdges.mockClear();
  mockSetPanelTab.mockClear();
  mockSetSearchOpen.mockClear();
  mockUpdateNodeData.mockClear();
});

vi.mock("@/store/canvas", () => ({
  useCanvasStore: vi.fn((selector: (s: typeof defaultStore) => unknown) =>
    selector(defaultStore)
  ),
}));

// ── Component under test ─────────────────────────────────────────────────────
import { Toolbar } from "../Toolbar";

// ── aria-expanded on help button ─────────────────────────────────────────────

describe("Toolbar — aria-expanded on help button", () => {
  it("help button has aria-expanded=false when popover is closed", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    expect(helpBtn.getAttribute("aria-expanded")).toBe("false");
  });

  it("help button has aria-expanded=true after click", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    expect(helpBtn.getAttribute("aria-expanded")).toBe("true");
  });

  it("help button aria-expanded flips back to false after close", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    expect(helpBtn.getAttribute("aria-expanded")).toBe("true");
    const closeBtn = screen.getByRole("button", { name: /close help dialog/i });
    fireEvent.click(closeBtn);
    expect(helpBtn.getAttribute("aria-expanded")).toBe("false");
  });
});

// ── aria-label on icon-only buttons ─────────────────────────────────────────

describe("Toolbar — aria-label on icon-only buttons", () => {
  beforeEach(() => {
    defaultStore.nodes = [];
    defaultStore.wsStatus = "connected";
    defaultStore.selectedNodeId = "ws-1";
  });

  it("A2A topology toggle has aria-label", () => {
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /show a2a edges/i });
    expect(btn.getAttribute("aria-label")).toBeTruthy();
  });

  it("Search button has aria-label", () => {
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /search workspaces/i });
    expect(btn.getAttribute("aria-label")).toBe("Search workspaces");
  });

  it("Help button has aria-label", () => {
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    expect(btn.getAttribute("aria-label")).toBe("Open shortcuts and tips");
  });

  it("Audit trail button has aria-label", () => {
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /open audit trail/i });
    expect(btn.getAttribute("aria-label")).toBe("Open audit trail for selected workspace");
  });
});

// ── aria-pressed on A2A toggle ────────────────────────────────────────────────

describe("Toolbar — aria-pressed on A2A topology toggle", () => {
  it("aria-pressed=false when A2A edges are hidden", () => {
    defaultStore.showA2AEdges = false;
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /show a2a edges/i });
    expect(btn.getAttribute("aria-pressed")).toBe("false");
  });

  it("aria-pressed=true when A2A edges are shown", () => {
    defaultStore.showA2AEdges = true;
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /hide a2a edges/i });
    expect(btn.getAttribute("aria-pressed")).toBe("true");
  });

  it("aria-pressed reflects store state (pre-condition: false when store is false)", () => {
    defaultStore.showA2AEdges = false;
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /show a2a edges/i });
    expect(btn.getAttribute("aria-pressed")).toBe("false");
  });

  it("aria-pressed reflects store state (pre-condition: true when store is true)", () => {
    defaultStore.showA2AEdges = true;
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /hide a2a edges/i });
    expect(btn.getAttribute("aria-pressed")).toBe("true");
  });

  it("aria-pressed flips after toggle click (mock verifies correct value passed)", () => {
    defaultStore.showA2AEdges = false;
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /show a2a edges/i });
    fireEvent.click(btn);
    // The mock confirms the correct boolean was passed to setShowA2AEdges.
    // The aria-pressed attribute reflects the pre-click store value (false)
    // which is correct — the re-render driven by the store update is tested
    // in the two tests above.
    expect(mockSetShowA2AEdges).toHaveBeenCalledWith(true);
  });
});

// ── Help popover dialog ARIA ─────────────────────────────────────────────────

describe("Toolbar — help popover dialog ARIA", () => {
  it("open popover has role=dialog", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    const dialog = screen.getByRole("dialog");
    expect(dialog).not.toBeNull();
  });

  it("popover has aria-label describing its purpose", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    const dialog = screen.getByRole("dialog");
    expect(dialog.getAttribute("aria-label")).toBe("Shortcuts and tips");
  });

  it("popover has aria-modal=false (non-blocking popover, not a true modal)", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    const dialog = screen.getByRole("dialog");
    expect(dialog.getAttribute("aria-modal")).toBe("false");
  });

  it("close button inside popover has aria-label", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    const closeBtn = screen.getByRole("button", { name: /close help dialog/i });
    expect(closeBtn.getAttribute("aria-label")).toBe("Close help dialog");
  });
});

// ── aria-hidden on decorative elements ──────────────────────────────────────

describe("Toolbar — aria-hidden on decorative elements", () => {
  it("logo image has alt=text (product name)", () => {
    render(<Toolbar />);
    const logo = document.querySelector("img[alt='Enter OS']") as HTMLImageElement;
    expect(logo).not.toBeNull();
  });

  it("StatusPill decorative dot has aria-hidden=true", () => {
    defaultStore.nodes = [{ id: "ws-1", data: { name: "Test", role: "agent", tier: 1, status: "online", parentId: null, activeTasks: 0, needsRestart: false } }];
    render(<Toolbar />);
    const dots = document.querySelectorAll(".w-1\\.5");
    // The first dot (online status) should have aria-hidden
    expect(dots.length).toBeGreaterThan(0);
    // Check the first visible dot has aria-hidden="true"
    const firstDot = dots[0] as HTMLElement;
    expect(firstDot.getAttribute("aria-hidden")).toBe("true");
  });

  it("WsStatusPill decorative dot has aria-hidden=true", () => {
    defaultStore.wsStatus = "connected";
    render(<Toolbar />);
    // The Live status has a decorative dot
    const dots = document.querySelectorAll(".w-1\\.5");
    const connectedDot = Array.from(dots).find(
      (d) => (d as HTMLElement).classList.contains("bg-emerald-400")
    ) as HTMLElement;
    expect(connectedDot).not.toBeUndefined();
    expect(connectedDot.getAttribute("aria-hidden")).toBe("true");
  });

  it("StatusPill count text is aria-hidden (decorative — count also in aria-label)", () => {
    defaultStore.nodes = [{ id: "ws-1", data: { name: "Test", role: "agent", tier: 1, status: "online", parentId: null, activeTasks: 0, needsRestart: false } }];
    render(<Toolbar />);
    // The count span inside StatusPill uses aria-hidden="true"
    const pill = screen.getByLabelText(/1 online/i);
    // Direct assertion (RC 13312): the StatusPill renders TWO
    // aria-hidden children — a dot (decorative) and a count-text
    // span (also decorative; the count is exposed via aria-label on
    // the parent). The pre-fix version asserted that AT LEAST ONE
    // descendant has aria-hidden="true", which the decorative dot
    // alone satisfies — vacuous. Pin the count-text span specifically:
    // it must exist, must have aria-hidden="true", and must contain
    // the numeric count as text content (so a future regression
    // that accidentally exposes the count as readable text — e.g. by
    // removing the aria-hidden — would fail this test).
    const countSpan = pill.querySelector("span[aria-hidden='true']");
    expect(countSpan).not.toBeNull();
    expect(countSpan!.getAttribute("aria-hidden")).toBe("true");
    expect(countSpan!.textContent).toBe("1");
  });
});

// ── focus-visible:ring on interactive buttons ─────────────────────────────────

describe("Toolbar — focus-visible:ring on interactive buttons", () => {
  it("A2A toggle button has focus-visible:ring class in className", () => {
    defaultStore.showA2AEdges = false;
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /show a2a edges/i });
    const cls = btn.className;
    expect(cls.includes("focus-visible:ring")).toBeTruthy();
  });

  it("Search button has focus-visible:ring class in className", () => {
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /search workspaces/i });
    const cls = btn.className;
    expect(cls.includes("focus-visible:ring")).toBeTruthy();
  });

  it("Help button has focus-visible:ring class in className", () => {
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    const cls = btn.className;
    expect(cls.includes("focus-visible:ring")).toBeTruthy();
  });

  it("Audit trail button has focus-visible:ring class in className", () => {
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /open audit trail/i });
    const cls = btn.className;
    expect(cls.includes("focus-visible:ring")).toBeTruthy();
  });

  it("Help popover close button has focus-visible:underline class (small text-button design — not ring)", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    const closeBtn = screen.getByRole("button", { name: /close help dialog/i });
    const cls = closeBtn.className;
    // Direct assertion (RC 13312): the close button's design uses
    // focus-visible:underline (small text-button convention), NOT
    // focus-visible:ring (the larger-icon-button convention). The
    // pre-fix version of this test only asserted that className was
    // truthy, which passed for ANY styled button — vacuous. Pin the
    // exact class so the design intent is documented + tested.
    expect(cls.includes("focus-visible:underline")).toBeTruthy();
    // Negative: also pin that it does NOT use the ring convention
    // (a regression to the icon-button class would make the close
    // button visually inconsistent with the rest of the toolbar).
    expect(cls.includes("focus-visible:ring")).toBeFalsy();
  });
});

// ── Stop All / Restart aria-label ────────────────────────────────────────────

describe("Toolbar — Stop All / Restart aria-label", () => {
  it("Stop All button has aria-label describing the action and count", () => {
    defaultStore.nodes = [
      { id: "ws-1", data: { name: "Test", role: "agent", tier: 1, status: "online", parentId: null, activeTasks: 2, needsRestart: false } },
    ];
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /stop all running tasks/i });
    // counts.activeTasks counts NODES with activeTasks > 0, not the sum of task counts.
    // One node with activeTasks=2 contributes count=1.
    expect(btn.getAttribute("aria-label")).toBe("Stop all running tasks (1 active)");
  });

  it("Restart Pending button has aria-label with workspace count", () => {
    defaultStore.nodes = [
      { id: "ws-1", data: { name: "Test", role: "agent", tier: 1, status: "online", parentId: null, activeTasks: 0, needsRestart: true } },
    ];
    render(<Toolbar />);
    const btn = screen.getByRole("button", { name: /restart 1 workspace/i });
    expect(btn.getAttribute("aria-label")).toBe(
      "Restart 1 workspace pending config or secret changes"
    );
  });
});

// ── Keyboard shortcut: Escape closes help popover ─────────────────────────────

describe("Toolbar — Escape closes help popover", () => {
  it("Escape key closes the help popover", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    expect(screen.getByRole("dialog")).not.toBeNull();
    // The component listens on window for Escape
    fireEvent.keyDown(window, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("Escape also resets help button aria-expanded to false", () => {
    render(<Toolbar />);
    const helpBtn = screen.getByRole("button", { name: /open shortcuts and tips/i });
    fireEvent.click(helpBtn);
    fireEvent.keyDown(window, { key: "Escape" });
    expect(helpBtn.getAttribute("aria-expanded")).toBe("false");
  });
});

// ── Screen reader summary ─────────────────────────────────────────────────────

describe("Toolbar — screen reader summary", () => {
  it("toolbar container has no implicit role (div is fine for a toolbar widget)", () => {
    render(<Toolbar />);
    // Direct assertion (RC 13312): the toolbar's root element is a
    // plain <div> with no `role` attribute (the design rationale is
    // that the HTML landmark structure + the surrounding <main>
    // landmark on the page is sufficient for AT navigation; adding
    // role="toolbar" would require the full WCAG toolbar pattern
    // (single-tab-stop roving tabindex, etc.) which is out of scope).
    // The pre-fix version only checked that `.fixed.top-3` exists —
    // vacuous, since any container matching that selector would
    // pass. Pin the NEGATIVE: the `role` attribute must be absent
    // (null) so a future regression that adds role="toolbar"
    // without the full toolbar pattern is caught.
    const container = document.querySelector(".fixed.top-3");
    expect(container).not.toBeNull();
    expect(container!.getAttribute("role")).toBeNull();
  });

  it("workspace count is exposed as text content (not only aria-label)", () => {
    defaultStore.nodes = [
      { id: "ws-1", data: { name: "Test", role: "agent", tier: 1, status: "online", parentId: null, activeTasks: 0, needsRestart: false } },
    ];
    render(<Toolbar />);
    // The workspace count text should be in the DOM for screen readers
    expect(screen.getByText(/1 workspace/i)).not.toBeNull();
  });
});
