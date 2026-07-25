// @vitest-environment jsdom
/**
 * BootSequenceScreen — ENTER OS key interactivity contract (#9).
 *
 * The armed "ENTER OS" key used to render `disabled={!armed}` with NO onClick,
 * so in a mount where it armed (online, pre-fade) it looked clickable
 * ("ready · entering…", cursor-pointer) but did nothing. The fix splits the
 * *visual* armed state from *interactivity*: the key is only clickable when an
 * `onEnter` handler is supplied, and stays a non-interactive affordance
 * otherwise so it never looks falsely clickable.
 */
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import type { Node } from "@xyflow/react";
import type { WorkspaceNodeData } from "@/store/canvas";
import { BootSequenceScreen } from "../BootSequenceScreen";

function node(data: Partial<WorkspaceNodeData> = {}): Node<WorkspaceNodeData> {
  return {
    id: "root-1",
    position: { x: 0, y: 0 },
    data: {
      kind: "platform",
      status: "online",
      runtime: "hermes",
      name: "Enter OS Agent",
      ...data,
    },
  } as Node<WorkspaceNodeData>;
}

afterEach(cleanup);

describe("BootSequenceScreen — ENTER OS key (#9)", () => {
  it("renders the key NON-interactive when online with no onEnter handler", () => {
    render(<BootSequenceScreen node={node({ status: "online" })} />);
    const btn = screen.getByRole("button", { name: /Enter OS/ });
    // Armed visual (online, pre-fade) but no handler → disabled, not clickable.
    expect((btn as HTMLButtonElement).disabled).toBe(true);
    expect(btn.className).toContain("cursor-default");
    expect(btn.className).not.toContain("cursor-pointer");
  });

  it("arms + fires onEnter when online with a handler", () => {
    const onEnter = vi.fn();
    render(
      <BootSequenceScreen node={node({ status: "online" })} onEnter={onEnter} />,
    );
    const btn = screen.getByRole("button", { name: /Enter OS/ });
    expect((btn as HTMLButtonElement).disabled).toBe(false);
    expect(btn.className).toContain("cursor-pointer");
    fireEvent.click(btn);
    expect(onEnter).toHaveBeenCalledTimes(1);
  });

  it("stays locked (never interactive) while still booting, even with a handler", () => {
    const onEnter = vi.fn();
    render(
      <BootSequenceScreen
        node={node({ status: "provisioning" })}
        onEnter={onEnter}
      />,
    );
    const btn = screen.getByRole("button", { name: /locked, boot in progress/ });
    expect((btn as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(btn);
    expect(onEnter).not.toHaveBeenCalled();
  });
});

describe("BootSequenceScreen — keycap states", () => {
  const step = (
    n: number,
    key: string,
    label: string,
    status: "running" | "ok" | "failed",
  ) => ({ step: n, total: 8, key, label, status });

  it("displays a running step as done once a LATER step has reported (implicit completion)", () => {
    // The provisioner deliberately never flips its own step 1 to ok
    // (provisioner.go startWorkspace) — once the runtime's steps take over,
    // step 1 must not breathe "running" under an almost-finished boot
    // (2026-07-18 live-boot regression: PWR RUN while ONLINE was ok).
    render(
      <BootSequenceScreen
        node={node({
          status: "provisioning",
          bootSteps: [
            step(1, "PWR", "Provision compute", "running"),
            step(5, "TOOL", "Enumerate tools", "ok"),
          ],
        })}
      />,
    );
    expect(screen.getByLabelText("Provision compute: ok")).toBeTruthy();
    expect(screen.getByLabelText("Enumerate tools: ok")).toBeTruthy();
  });

  it("keeps the HIGHEST reported step running (no implicit completion for the frontier)", () => {
    render(
      <BootSequenceScreen
        node={node({
          status: "provisioning",
          bootSteps: [
            step(1, "PWR", "Provision compute", "ok"),
            step(2, "ID", "Load identity", "running"),
          ],
        })}
      />,
    );
    expect(screen.getByLabelText("Load identity: run")).toBeTruthy();
  });

  it("never implicitly completes a FAILED step", () => {
    render(
      <BootSequenceScreen
        node={node({
          status: "provisioning",
          bootSteps: [
            step(3, "RT", "Start runtime", "failed"),
            step(4, "MCP", "Management MCP", "running"),
          ],
        })}
      />,
    );
    expect(screen.getByLabelText("Start runtime: error")).toBeTruthy();
  });
});
