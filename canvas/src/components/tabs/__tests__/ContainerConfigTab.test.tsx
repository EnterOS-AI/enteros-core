// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const apiPatch = vi.fn();
const updateNodeData = vi.fn();
const restartWorkspace = vi.fn();

vi.mock("@/lib/api", () => ({
  api: {
    patch: (path: string, body: unknown) => apiPatch(path, body),
  },
}));

vi.mock("@/lib/runtime-names", () => ({
  runtimeDisplayName: (runtime: string) => runtime,
}));

vi.mock("@/store/canvas", () => ({
  useCanvasStore: Object.assign(
    (selector: (s: unknown) => unknown) => selector({ restartWorkspace, updateNodeData }),
    { getState: () => ({ restartWorkspace, updateNodeData }) },
  ),
}));

// SaaS so the editable cloud-provider selector renders (non-SaaS shows a read-only
// badge). Existing tests keep provider=aws (default), which is omitted from the
// PATCH payload, so their assertions are unaffected.
vi.mock("@/lib/tenant", () => ({
  isSaaSTenant: () => true,
}));

import { ContainerConfigTab } from "../ContainerConfigTab";

afterEach(() => {
  cleanup();
});

beforeEach(() => {
  apiPatch.mockReset();
  restartWorkspace.mockReset();
  updateNodeData.mockReset();
});

describe("ContainerConfigTab", () => {
  it("defaults missing compute to the cost-efficient headless profile", () => {
    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: undefined,
        }}
      />,
    );

    expect(screen.getByLabelText("Instance type")).toHaveProperty("value", "t3.medium");
    expect(screen.getByLabelText("Root volume")).toHaveProperty("value", "30");
  });

  it("renders persisted compute and status settings", () => {
    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 2,
          maxConcurrentTasks: 3,
          workspaceAccess: "read_write",
          deliveryMode: "poll",
          compute: {
            instance_type: "t3.xlarge",
            volume: { root_gb: 80 },
            display: { mode: "desktop-control", protocol: "novnc", width: 1920, height: 1080 },
          },
        }}
      />,
    );

    expect(screen.getByLabelText("Runtime image")).toHaveProperty("value", "claude-code");
    expect(screen.getByLabelText("Instance type")).toHaveProperty("value", "t3.xlarge");
    expect(screen.getByLabelText("Root volume")).toHaveProperty("value", "80");
    expect(screen.getByLabelText("Enable display")).toHaveProperty("checked", true);
    expect(screen.getByLabelText("Resolution")).toHaveProperty("value", "1920x1080");
    expect(screen.getByText("Workspace access")).toBeTruthy();
    expect(screen.getByText("read-write")).toBeTruthy();
  });

  it("does not reset dirty form edits on unrelated status rerender", () => {
    const { rerender } = render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "none" },
          },
        }}
      />,
    );

    fireEvent.change(screen.getByLabelText("Root volume"), { target: { value: "120" } });

    rerender(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 1,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "none" },
          },
        }}
      />,
    );

    expect(screen.getByLabelText("Root volume")).toHaveProperty("value", "120");
  });

  it("saves runtime and compute changes through workspace PATCH", async () => {
    apiPatch.mockResolvedValueOnce({ needs_restart: true });

    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "none" },
          },
        }}
      />,
    );

    fireEvent.change(screen.getByLabelText("Runtime image"), { target: { value: "hermes" } });
    fireEvent.change(screen.getByLabelText("Instance type"), { target: { value: "m6i.xlarge" } });
    fireEvent.change(screen.getByLabelText("Root volume"), { target: { value: "100" } });
    fireEvent.click(screen.getByLabelText("Enable display"));
    fireEvent.change(screen.getByLabelText("Resolution"), { target: { value: "2560x1440" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));
    expect(apiPatch).toHaveBeenCalledWith("/workspaces/ws-compute", {
      runtime: "hermes",
      compute: {
        instance_type: "m6i.xlarge",
        volume: { root_gb: 100 },
        display: { mode: "desktop-control", protocol: "novnc", width: 2560, height: 1440 },
      },
    });
    expect(updateNodeData).toHaveBeenCalledWith("ws-compute", {
      runtime: "hermes",
      compute: {
        instance_type: "m6i.xlarge",
        volume: { root_gb: 100 },
        display: { mode: "desktop-control", protocol: "novnc", width: 2560, height: 1440 },
      },
      needsRestart: true,
      applyTemplateOnRestart: true,
    });
    expect(restartWorkspace).not.toHaveBeenCalled();
  });

  it("preserves existing custom display mode and resolution when saving unrelated compute", async () => {
    apiPatch.mockResolvedValueOnce({ needs_restart: true });

    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "gpu-desktop-control", protocol: "dcv", width: 1600, height: 1000 },
          },
        }}
      />,
    );

    expect(screen.getByLabelText("Resolution")).toHaveProperty("value", "1600x1000");

    fireEvent.change(screen.getByLabelText("Instance type"), { target: { value: "t3.xlarge" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));
    expect(apiPatch).toHaveBeenCalledWith("/workspaces/ws-compute", {
      runtime: "claude-code",
      compute: {
        instance_type: "t3.xlarge",
        volume: { root_gb: 50 },
        display: { mode: "gpu-desktop-control", protocol: "dcv", width: 1600, height: 1000 },
      },
    });
  });

  it("can save changed compute and restart the workspace to apply it", async () => {
    apiPatch.mockResolvedValueOnce({ needs_restart: true });
    restartWorkspace.mockResolvedValueOnce(undefined);

    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "none" },
          },
        }}
      />,
    );

    fireEvent.change(screen.getByLabelText("Instance type"), { target: { value: "t3.xlarge" } });
    fireEvent.click(screen.getByRole("button", { name: "Save & Restart" }));

    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(restartWorkspace).toHaveBeenCalledWith("ws-compute", { applyTemplate: false }));
  });

  it("requests template re-apply when saving a runtime change and restarting", async () => {
    apiPatch.mockResolvedValueOnce({ needs_restart: true });
    restartWorkspace.mockResolvedValueOnce(undefined);

    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "none" },
          },
        }}
      />,
    );

    fireEvent.change(screen.getByLabelText("Runtime image"), { target: { value: "hermes" } });
    fireEvent.click(screen.getByRole("button", { name: "Save & Restart" }));

    await waitFor(() => expect(restartWorkspace).toHaveBeenCalledWith("ws-compute", { applyTemplate: true }));
  });

  it("can restart without re-saving when changes are already pending", async () => {
    restartWorkspace.mockResolvedValueOnce(undefined);

    render(
      <ContainerConfigTab
        workspaceId="ws-compute"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: true,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "none",
          deliveryMode: "push",
          applyTemplateOnRestart: true,
          compute: {
            instance_type: "t3.large",
            volume: { root_gb: 50 },
            display: { mode: "none" },
          },
        }}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Restart to apply" }));

    await waitFor(() => expect(restartWorkspace).toHaveBeenCalledWith("ws-compute", { applyTemplate: true }));
    expect(apiPatch).not.toHaveBeenCalled();
  });

  it("switches cloud provider — keys the instance-type list to the provider, confirms the recreate, and PATCHes the new provider", async () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    render(
      <ContainerConfigTab
        workspaceId="ws-switch"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "read-write",
          deliveryMode: "push",
          compute: { instance_type: "t3.large", provider: "aws", volume: { root_gb: 30 } },
        }}
      />,
    );

    const providerSel = screen.getByLabelText("Cloud provider");
    expect(providerSel).toHaveProperty("value", "aws");
    expect(screen.getByLabelText("Instance type")).toHaveProperty("value", "t3.large");

    // Switch to Hetzner → the instance type resets to the Hetzner default (an AWS
    // t3.* is invalid on Hetzner) and the options become Hetzner sizes.
    fireEvent.change(providerSel, { target: { value: "hetzner" } });
    expect(screen.getByLabelText("Instance type")).toHaveProperty("value", "cpx31");

    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));
    expect(confirmSpy).toHaveBeenCalled(); // destructive recreate confirmed
    const body = apiPatch.mock.calls[0][1] as { compute: { provider?: string; instance_type?: string } };
    expect(body.compute.provider).toBe("hetzner");
    expect(body.compute.instance_type).toBe("cpx31");
    confirmSpy.mockRestore();
  });

  it("does not treat a non-provider edit as a recreate (no confirm; aws default omitted)", async () => {
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);
    render(
      <ContainerConfigTab
        workspaceId="ws-noswitch"
        data={{
          runtime: "claude-code",
          status: "online",
          needsRestart: false,
          activeTasks: 0,
          maxConcurrentTasks: null,
          workspaceAccess: "read-write",
          deliveryMode: "push",
          compute: { instance_type: "t3.large", provider: "aws", volume: { root_gb: 30 } },
        }}
      />,
    );

    fireEvent.change(screen.getByLabelText("Root volume"), { target: { value: "60" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));
    expect(confirmSpy).not.toHaveBeenCalled();
    const body = apiPatch.mock.calls[0][1] as { compute: { provider?: string } };
    expect(body.compute.provider).toBeUndefined(); // aws default omitted (wire unchanged)
    confirmSpy.mockRestore();
  });
});
