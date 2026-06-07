// @vitest-environment jsdom
/**
 * Tests for TopViewTabs — the top-level Home/Map view switcher.
 *
 * Covers: renders both tabs, reflects the store's topView via
 * aria-selected, and flips the store on click.
 */
import React from "react";
import { render, screen, fireEvent, cleanup, act } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { TopViewTabs } from "../TopViewTabs";
import { useCanvasStore } from "@/store/canvas";

afterEach(() => {
  act(() => { cleanup(); });
  // Reset to the default view so tests don't bleed into one another.
  act(() => { useCanvasStore.getState().setTopView("map"); });
});

describe("TopViewTabs", () => {
  beforeEach(() => {
    act(() => { useCanvasStore.getState().setTopView("map"); });
  });

  it("renders Home and Map tabs", () => {
    render(<TopViewTabs />);
    expect(screen.getByRole("tab", { name: "Home" })).toBeTruthy();
    expect(screen.getByRole("tab", { name: "Map" })).toBeTruthy();
  });

  it("marks the active view with aria-selected (map by default)", () => {
    render(<TopViewTabs />);
    expect(screen.getByRole("tab", { name: "Map" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByRole("tab", { name: "Home" }).getAttribute("aria-selected")).toBe("false");
  });

  it("switches the store's topView to home on click", () => {
    render(<TopViewTabs />);
    act(() => {
      fireEvent.click(screen.getByRole("tab", { name: "Home" }));
    });
    expect(useCanvasStore.getState().topView).toBe("home");
    expect(screen.getByRole("tab", { name: "Home" }).getAttribute("aria-selected")).toBe("true");
  });

  it("switches back to map", () => {
    act(() => { useCanvasStore.getState().setTopView("home"); });
    render(<TopViewTabs />);
    act(() => {
      fireEvent.click(screen.getByRole("tab", { name: "Map" }));
    });
    expect(useCanvasStore.getState().topView).toBe("map");
  });
});
