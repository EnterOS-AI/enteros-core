// @vitest-environment jsdom
import { describe, it, expect } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import { ToolTraceChips } from "../ToolTraceChips";

describe("ToolTraceChips (core#2636 tool-chain persistence)", () => {
  it("renders nothing for an empty trace", () => {
    const { container } = render(<ToolTraceChips trace={[]} />);
    expect(container.firstChild).toBeNull();
  });

  it("shows a collapsed count and expands to the tool list on click", () => {
    render(
      <ToolTraceChips
        trace={[
          { tool: "mcp__platform__create_request", input: "{}" },
          { tool: "Read", input: "/tmp/foo" },
        ]}
      />,
    );
    // Collapsed: count visible, individual tools hidden.
    expect(screen.getByText("2 tools used")).toBeTruthy();
    expect(screen.queryByText(/create_request/)).toBeNull();

    fireEvent.click(screen.getByRole("button"));
    expect(screen.getByText(/mcp__platform__create_request/)).toBeTruthy();
    expect(screen.getByText(/Read/)).toBeTruthy();
  });

  it("singularizes the header for one tool", () => {
    render(<ToolTraceChips trace={[{ tool: "Bash" }]} />);
    expect(screen.getByText("1 tool used")).toBeTruthy();
  });
});
