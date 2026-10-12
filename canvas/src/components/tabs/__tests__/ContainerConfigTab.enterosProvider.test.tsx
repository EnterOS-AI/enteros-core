// @vitest-environment jsdom
//
// The NON-SaaS (Enter OS Server) path. Every assertion in the sibling
// ContainerConfigTab.test.tsx runs with `isSaaSTenant: () => true`, so the
// substrate this platform actually provisions on had no coverage at all.
//
// Two consequences of that gap, both fixed here:
//
//  1. `cloudProviderLabel` fell through to "AWS" for anything that wasn't
//     gcp/hetzner, so every workspace on a local Enter OS Server box rendered a
//     provider badge reading "AWS" -- a platform that provisions zero AWS.
//
//  2. The PATCH body omitted `provider` whenever it normalized to "aws", and
//     `normalizeProvider` coerced local/docker/molecules-server -> "aws". So the
//     canvas never sent a provider for a local box, workspace_compute.go's
//     `if compute.Provider != ""` left the key out of the persisted JSON, and
//     `compute->>'provider'` was absent on every production row. Downstream,
//     CP's ensure-image received an empty provider, normalized it to the SDK
//     default, and answered `200 not_applicable` -- which is why the core#5019
//     pull-before-stop guard was inert in production.
//
// Sending the provider explicitly also removes the ambiguity that the SDK
// default flip (molecule-ai-sdk#195, "" -> molecules-server) would otherwise
// create for a row that recorded nothing.

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const apiPatch = vi.fn();
const apiGet = vi.fn();
const updateNodeData = vi.fn();
const restartWorkspace = vi.fn();

vi.mock("@/lib/api", () => ({
  api: {
    patch: (path: string, body: unknown) => apiPatch(path, body),
    get: (path: string) => apiGet(path),
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

// The whole point of this file: the local/Docker substrate, where the provider
// renders as a read-only badge rather than an editable selector.
vi.mock("@/lib/tenant", () => ({
  isSaaSTenant: () => false,
}));

import { ContainerConfigTab } from "../ContainerConfigTab";

afterEach(() => {
  cleanup();
});

beforeEach(() => {
  apiPatch.mockReset();
  apiGet.mockReset();
  apiPatch.mockResolvedValue({});
  apiGet.mockRejectedValue(new Error("no /compute/metadata in this test"));
  restartWorkspace.mockReset();
  updateNodeData.mockReset();
});

const dataWithProvider = (provider: string) => ({
  runtime: "claude-code",
  status: "online" as const,
  needsRestart: false,
  activeTasks: 0,
  maxConcurrentTasks: null,
  workspaceAccess: "read-write" as const,
  deliveryMode: "push" as const,
  compute: { instance_type: "", provider, volume: { root_gb: 30 } },
});

describe("ContainerConfigTab on Enter OS Server (non-SaaS)", () => {
  // `local` is what actually lands in the row: workspace_set_compute_instance
  // persists the BACKEND KEY, not the canonical id.
  it("labels the local backend key as Enter OS Server, not AWS", () => {
    render(<ContainerConfigTab workspaceId="ws-local" data={dataWithProvider("local")} />);
    expect(
      screen.getByTitle("Cloud provider for this workspace's compute").textContent,
    ).toBe("Enter OS Server");
  });

  // The canonical SSOT id must label identically -- a row written by a newer
  // component (or by the SDK default after #195) must not read "AWS" either.
  it("labels the canonical molecules-server id as Enter OS Server", () => {
    render(<ContainerConfigTab workspaceId="ws-canon" data={dataWithProvider("molecules-server")} />);
    expect(
      screen.getByTitle("Cloud provider for this workspace's compute").textContent,
    ).toBe("Enter OS Server");
  });

  // The commonest real row: production recorded no provider at all (see the
  // header). This badge only ever renders when !isSaaS, i.e. on a local box, so
  // an unrecorded provider there IS an Enter OS Server container -- and it also
  // matches what the SDK default resolves "" to after #195.
  it("labels an unrecorded provider as Enter OS Server, since the badge is non-SaaS only", () => {
    render(<ContainerConfigTab workspaceId="ws-blank" data={dataWithProvider("")} />);
    expect(
      screen.getByTitle("Cloud provider for this workspace's compute").textContent,
    ).toBe("Enter OS Server");
  });

  // A real cloud must keep its own label -- this must not become a blanket
  // "everything is Enter OS Server" rename.
  it("still labels a real cloud provider correctly", () => {
    render(<ContainerConfigTab workspaceId="ws-hz" data={dataWithProvider("hetzner")} />);
    expect(
      screen.getByTitle("Cloud provider for this workspace's compute").textContent,
    ).toBe("Hetzner");
  });

  // The recording fix. Without this the row never gets a provider and every
  // downstream consumer has to guess from a default.
  it("sends the local provider on save instead of dropping it from the PATCH", async () => {
    render(<ContainerConfigTab workspaceId="ws-local-save" data={dataWithProvider("local")} />);

    fireEvent.change(screen.getByLabelText("Root volume"), { target: { value: "40" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));

    const body = apiPatch.mock.calls[0][1] as { compute: { provider?: string } };
    expect(body.compute.provider).toBe("local");
  });

  // A workspace that never recorded a provider must not have one invented for
  // it: an absent provider is "not recorded", and workspace_compute.go treats
  // it as leave-unset. Manufacturing a value here would write a guess into the
  // row and, on a SaaS box, could retarget it.
  it("does not invent a provider for a workspace that never recorded one", async () => {
    render(<ContainerConfigTab workspaceId="ws-none" data={dataWithProvider("")} />);

    fireEvent.change(screen.getByLabelText("Root volume"), { target: { value: "40" } });
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(apiPatch).toHaveBeenCalledTimes(1));

    const body = apiPatch.mock.calls[0][1] as { compute: { provider?: string } };
    expect(body.compute.provider).toBeUndefined();
  });
});
