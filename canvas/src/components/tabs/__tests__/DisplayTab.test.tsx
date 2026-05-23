// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";

const { mockGet } = vi.hoisted(() => ({ mockGet: vi.fn() }));

vi.mock("@/lib/api", () => ({
  api: {
    get: mockGet,
  },
}));

import { DisplayTab } from "../DisplayTab";

describe("DisplayTab", () => {
  beforeEach(() => {
    mockGet.mockReset();
  });

  it("renders unavailable state for non-display workspaces", async () => {
    mockGet.mockResolvedValueOnce({
      available: false,
      reason: "display_not_enabled",
    });

    render(<DisplayTab workspaceId="ws-no-display" />);

    await waitFor(() => {
      expect(screen.getByText("Display is not enabled for this workspace.")).toBeTruthy();
    });
    expect(mockGet).toHaveBeenCalledWith("/workspaces/ws-no-display/display");
  });
});
