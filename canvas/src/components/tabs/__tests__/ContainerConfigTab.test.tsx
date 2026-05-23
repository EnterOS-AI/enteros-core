// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("@/lib/runtime-names", () => ({
  runtimeDisplayName: (runtime: string) => runtime,
}));

import { ContainerConfigTab } from "../ContainerConfigTab";

afterEach(() => {
  cleanup();
});

describe("ContainerConfigTab", () => {
  it("renders read-only runtime and container settings separate from compute shape", () => {
    render(
      <ContainerConfigTab
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 2,
          maxConcurrentTasks: 3,
          workspaceAccess: "read_write",
          deliveryMode: "poll",
        }}
      />,
    );

    expect(screen.getByText("Runtime image")).toBeTruthy();
    expect(screen.getByText("claude-code")).toBeTruthy();
    expect(screen.getByText("Workspace access")).toBeTruthy();
    expect(screen.getByText("read-write")).toBeTruthy();
    expect(screen.getByText("Max concurrent tasks")).toBeTruthy();
    expect(screen.getByText("3")).toBeTruthy();
    expect(screen.getByText("/workspace")).toBeTruthy();
    expect(screen.getByText("Container privileges")).toBeTruthy();
    expect(screen.queryByText("Instance type")).toBeNull();
    expect(screen.queryByText("Root volume")).toBeNull();
  });
});
