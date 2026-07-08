// @vitest-environment jsdom
/**
 * page.tsx mount contract for the self-host setup scene: the scene mounts
 * ABOVE the desktop/mobile view switch (one scene covers both) and ONLY
 * after first /workspaces hydration completes — never during the loading
 * spinner (no pre-hydration flash).
 */
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";

vi.mock("@/store/socket", () => ({
  connectSocket: vi.fn(),
  disconnectSocket: vi.fn(),
  // Re-exported constant consumed transitively via @/store/canvas →
  // deleteTombstones.ts; the page only uses connect/disconnect.
  FALLBACK_POLL_MS: 10_000,
}));
vi.mock("@/lib/api", () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), patch: vi.fn(), del: vi.fn() },
  PlatformUnavailableError: class PlatformUnavailableError extends Error {},
}));
vi.mock("@/components/concierge/ConciergeShell", () => ({
  ConciergeShell: () => <div data-testid="concierge-shell" />,
}));
vi.mock("@/components/mobile/MobileApp", () => ({
  MobileApp: () => <div data-testid="mobile-app" />,
}));
vi.mock("@/components/onboarding/SelfHostSetupScene", () => ({
  SelfHostSetupScene: () => <div data-testid="scene-sentinel" />,
}));

import { api } from "@/lib/api";
import Home from "../page";

function stubMatchMedia(matches: boolean) {
  window.matchMedia = vi.fn().mockReturnValue({
    matches,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  }) as unknown as typeof window.matchMedia;
}

beforeEach(() => {
  vi.clearAllMocks();
  stubMatchMedia(false);
});

afterEach(() => {
  cleanup();
});

describe("page.tsx scene mount", () => {
  it("does NOT mount the scene while hydration is in flight (no flash)", () => {
    (api.get as Mock).mockImplementation(() => new Promise(() => {}));
    render(<Home />);
    expect(screen.queryByTestId("scene-sentinel")).toBeNull();
    expect(screen.queryByTestId("concierge-shell")).toBeNull();
  });

  it("mounts the scene as a sibling ABOVE the desktop shell after hydration", async () => {
    (api.get as Mock).mockResolvedValue([]);
    render(<Home />);
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    const scene = screen.getByTestId("scene-sentinel");
    const shell = screen.getByTestId("concierge-shell");
    // "Above the view switch": the scene precedes the shell in DOM order.
    expect(
      scene.compareDocumentPosition(shell) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });

  it("mounts the same scene above the mobile shell (<640px)", async () => {
    stubMatchMedia(true);
    (api.get as Mock).mockResolvedValue([]);
    render(<Home />);
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    const scene = screen.getByTestId("scene-sentinel");
    const mobile = screen.getByTestId("mobile-app");
    expect(
      scene.compareDocumentPosition(mobile) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });
});
