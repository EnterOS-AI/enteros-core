// @vitest-environment jsdom
/**
 * AttachmentTextPreview — inline text/code preview with expand + truncate.
 *
 * Uses a streaming fetch (ReadableStream) to read up to 256 KB of text.
 * State machine: idle → loading → ready/error. Ready state shows a
 * monospace preview of the first 10 lines, with an expand button when
 * there are more. Shows a "truncated" note when the file exceeds 256 KB.
 * Error falls back to AttachmentChip.
 *
 * NOTE: No @testing-library/jest-dom import — use DOM APIs for assertions.
 *
 * Covers:
 *   - Renders loading skeleton (320×80) with aria-label
 *   - Renders text preview with correct content in ready state
 *   - Shows filename in header
 *   - Expand button appears when lines > 10
 *   - Expand button hidden when all lines shown
 *   - Expand button calls setExpanded(true) and button text updates
 *   - Download button calls onDownload
 *   - tone=user applies blue/accent border
 *   - tone=agent applies neutral border
 *   - Error state renders AttachmentChip fallback
 *   - Cleans up on unmount
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, waitFor } from "@testing-library/react";
import React from "react";

import { AttachmentTextPreview } from "../AttachmentTextPreview";
import type { ChatAttachment } from "../types";

// ─── Mocks ────────────────────────────────────────────────────────────────────

const mockResolveAttachmentHref = vi.fn<(id: string, uri: string) => string>(
  (id, uri) => `https://api.moleculesai.app/attachments/${uri}`,
);
const mockIsPlatformAttachment = vi.fn<(uri: string) => boolean>(() => true);

vi.mock("../uploads", () => ({
  isPlatformAttachment: (uri: string) => mockIsPlatformAttachment(uri),
  resolveAttachmentHref: (id: string, uri: string) =>
    mockResolveAttachmentHref(id, uri),
}));

vi.mock("@/lib/api", () => ({
  platformAuthHeaders: () => ({ Authorization: "Bearer fixture-token" }),
}));

// ─── Helpers ──────────────────────────────────────────────────────────────────

function makeAttachment(name: string, size?: number): ChatAttachment {
  return { name, uri: `workspace:/tmp/${name}`, size };
}

beforeEach(() => {
  mockIsPlatformAttachment.mockReturnValue(true);
  mockResolveAttachmentHref.mockImplementation(
    (id: string, uri: string) => `https://api.moleculesai.app/attachments/${uri}`,
  );
});

afterEach(() => {
  cleanup();
});

// ─── Fetch mock helpers ───────────────────────────────────────────────────────

/**
 * Mock a streaming fetch that returns text content.
 * Mimics ReadableStream.read() yielding text chunks.
 */
function mockFetchText(completeText: string) {
  const encoder = new TextEncoder();
  global.fetch = vi.fn(async () => new Response(new ReadableStream<Uint8Array>({
    start(controller) {
      for (let offset = 0; offset < completeText.length; offset += 50) {
        controller.enqueue(encoder.encode(completeText.slice(offset, offset + 50)));
      }
      controller.close();
    },
  }), { headers: { "content-type": "text/plain" } }));
}

function mockFetchError() {
  global.fetch = vi.fn(async () => new Response(null, { status: 500 }));
}

// ─── Loading / idle state ─────────────────────────────────────────────────────

describe("AttachmentTextPreview — loading/idle", () => {
  it("renders loading skeleton (320×80) with aria-label", () => {
    mockFetchText("hello world");
    const att = makeAttachment("log.txt", 1024);
    const { container } = render(
      <AttachmentTextPreview
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    const skeleton = container.querySelector('[aria-label]') as HTMLElement;
    expect(skeleton?.getAttribute("aria-label")).toContain("log.txt");
    expect(skeleton?.getAttribute("aria-label")).toContain("Loading");
    expect(skeleton?.style.width).toBe("320px");
    expect(skeleton?.style.height).toBe("80px");
  });
});

// ─── Ready state ───────────────────────────────────────────────────────────────

describe("AttachmentTextPreview — ready", () => {
  beforeEach(() => {
    mockFetchText("hello world");
  });

  it("renders text preview with correct content", async () => {
    mockFetchText("line1\nline2\nline3");
    const att = makeAttachment("log.txt");
    render(
      <AttachmentTextPreview
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      const code = document.querySelector("code");
      expect(code).toBeTruthy();
    });
    const code = document.querySelector("code");
    expect(code?.textContent).toContain("line1");
  });

  it("shows filename in header", async () => {
    mockFetchText("hello");
    const att = makeAttachment("config.yaml");
    render(
      <AttachmentTextPreview
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("code")).toBeTruthy();
    });
    // Header should contain the filename
    const header = document.querySelector("code")?.closest("div");
    expect(header?.textContent).toContain("config.yaml");
  });

  it("shows expand button when lines > 10", async () => {
    const longText = Array.from({ length: 15 }, (_, i) => `line ${i + 1}`).join("\n");
    mockFetchText(longText);
    const att = makeAttachment("long.txt");
    render(
      <AttachmentTextPreview
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      const btn = document.querySelector("button");
      expect(btn).toBeTruthy();
    });
    // Should have a button saying "Show all N lines"
    const btns = Array.from(document.querySelectorAll("button"));
    const expandBtn = btns.find((b) => b.textContent?.includes("Show all"));
    expect(expandBtn).toBeTruthy();
    expect(expandBtn?.textContent).toContain("15 lines");
  });

  it("hides expand button when all lines shown (<= 10)", async () => {
    const shortText = Array.from({ length: 5 }, (_, i) => `line ${i + 1}`).join("\n");
    mockFetchText(shortText);
    const att = makeAttachment("short.txt");
    render(
      <AttachmentTextPreview
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("code")).toBeTruthy();
    });
    const btns = Array.from(document.querySelectorAll("button"));
    const expandBtn = btns.find((b) => b.textContent?.includes("Show all"));
    expect(expandBtn).toBeUndefined();
  });

  it("expand button updates button text to all lines", async () => {
    const longText = Array.from({ length: 15 }, (_, i) => `line ${i + 1}`).join("\n");
    mockFetchText(longText);
    const att = makeAttachment("long.txt");
    render(
      <AttachmentTextPreview
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      const btns = Array.from(document.querySelectorAll("button"));
      expect(btns.find((b) => b.textContent?.includes("Show all"))).toBeTruthy();
    });
    const btns = Array.from(document.querySelectorAll("button"));
    const expandBtn = btns.find((b) => b.textContent?.includes("Show all")) as HTMLButtonElement;
    expandBtn.click();
    await vi.waitFor(() => {
      const newBtns = Array.from(document.querySelectorAll("button"));
      expect(newBtns.find((b) => b.textContent?.includes("Show all"))).toBeUndefined();
    });
  });

  it("download button calls onDownload", async () => {
    mockFetchText("hello");
    const onDownload = vi.fn();
    const att = makeAttachment("log.txt");
    render(
      <AttachmentTextPreview
        workspaceId="ws1"
        attachment={att}
        onDownload={onDownload}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("code")).toBeTruthy();
    });
    // Find the download button (aria-label contains "Download")
    const downloadBtn = document.querySelector('[aria-label^="Download"]') as HTMLButtonElement;
    expect(downloadBtn).toBeTruthy();
    downloadBtn.click();
    expect(onDownload).toHaveBeenCalledWith(att);
  });

  it("tone=user applies blue/accent border classes", async () => {
    mockFetchText("hello");
    const att = makeAttachment("log.txt");
    const { container } = render(
      <AttachmentTextPreview
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("code")).toBeTruthy();
    });
    const rootDiv = container.firstChild as HTMLElement;
    expect(rootDiv.className).toContain("border-blue-400");
    expect(rootDiv.className).toContain("accent-strong");
  });

  it("tone=agent applies neutral border class (no blue)", async () => {
    mockFetchText("hello");
    const att = makeAttachment("log.txt");
    const { container } = render(
      <AttachmentTextPreview
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("code")).toBeTruthy();
    });
    const rootDiv = container.firstChild as HTMLElement;
    expect(rootDiv.className).not.toContain("border-blue-400");
  });
});

// ─── Truncated state ───────────────────────────────────────────────────────────

describe("AttachmentTextPreview — truncated", () => {
  it("shows truncated notice when file exceeds 256 KB", async () => {
    // Simulate a response where the reader yields chunks until MAX_FETCH_BYTES (256KB)
    const encoder = new TextEncoder();
    const cancel = vi.fn();
    global.fetch = vi.fn(async () => new Response(new ReadableStream<Uint8Array>({
      pull(controller) {
        controller.enqueue(encoder.encode("x".repeat(300 * 1024)));
      },
      cancel,
    }), { headers: { "content-type": "text/plain" } }));
    const att = makeAttachment("huge.log");
    render(
      <AttachmentTextPreview
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      const truncated = document.querySelector("code");
      expect(truncated).toBeTruthy();
    });
    // Should show truncated notice
    const truncatedNote = Array.from(document.querySelectorAll("button")).find(
      (b) => b.textContent?.includes("download full file"),
    );
    expect(truncatedNote).toBeTruthy();
    expect(cancel).toHaveBeenCalledOnce();
  });
});

// ─── Error state ───────────────────────────────────────────────────────────────

describe("AttachmentTextPreview — error", () => {
  it("renders AttachmentChip fallback when fetch fails", async () => {
    mockFetchError();
    const onDownload = vi.fn();
    const att = makeAttachment("broken.txt", 256);
    render(
      <AttachmentTextPreview
        workspaceId="ws1"
        attachment={att}
        onDownload={onDownload}
        tone="agent"
      />,
    );
    await vi.waitFor(() => {
      const chip = document.querySelector("button");
      expect(chip).toBeTruthy();
      expect(chip?.textContent).toContain("broken.txt");
    });
    const chip = document.querySelector("button") as HTMLButtonElement;
    chip.click();
    expect(onDownload).toHaveBeenCalledWith(att);
  });
});

// ─── Cleanup ──────────────────────────────────────────────────────────────────

describe("AttachmentTextPreview — cleanup", () => {
  it("cleans up on unmount", async () => {
    mockFetchText("hello");
    const att = makeAttachment("log.txt");
    const { unmount } = render(
      <AttachmentTextPreview
        workspaceId="ws1"
        attachment={att}
        onDownload={vi.fn()}
        tone="user"
      />,
    );
    await vi.waitFor(() => {
      expect(document.querySelector("code")).toBeTruthy();
    });
    expect(document.querySelector("code")).toBeTruthy();
    unmount();
    expect(document.querySelector("code")).toBeNull();
  });
});
