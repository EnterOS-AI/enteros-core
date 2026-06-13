// @vitest-environment jsdom
//
// MobileInbox (core#2697 Phase 1) — Tasks/Approvals on mobile. Verifies it
// loads pending requests from GET /requests/pending?kind=… and that a decision
// POSTs /requests/{id}/respond with the right action + optimistically drops the
// row — the decision-on-the-go flow that was entirely missing on mobile.

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, waitFor } from "@testing-library/react";

afterEach(cleanup);

vi.mock("@/lib/api");
vi.mock("@/lib/auth", () => ({ fetchSession: vi.fn().mockResolvedValue({ user_id: "u-test" }) }));
vi.mock("@/hooks/useSocketEvent", () => ({ useSocketEvent: vi.fn() }));

import { api } from "@/lib/api";
import { MobileInbox } from "../MobileInbox";

const approval = {
  id: "req-1", kind: "approval", requester_type: "workspace", requester_id: "ws-9",
  org_id: null, recipient_type: "user", recipient_id: "", title: "Delete prod secret?",
  detail: "Agent wants to delete TEST_KEY", status: "pending", responder_type: null,
  responder_id: null, priority: null, created_at: "", updated_at: "", responded_at: null,
  workspace_name: "Dev Engineer",
};

beforeEach(() => {
  vi.mocked(api.get).mockResolvedValue([approval]);
  vi.mocked(api.post).mockResolvedValue({});
});

describe("MobileInbox", () => {
  it("loads pending approvals from /requests/pending?kind=approval", async () => {
    const { getByText } = render(<MobileInbox dark={false} />);
    await waitFor(() => getByText("Delete prod secret?"));
    expect(api.get).toHaveBeenCalledWith("/requests/pending?kind=approval");
  });

  it("Approve POSTs /requests/{id}/respond with action=approved and drops the row", async () => {
    const { getByText, queryByText, container } = render(<MobileInbox dark={false} />);
    await waitFor(() => getByText("Delete prod secret?"));
    await act(async () => {
      fireEvent.click(getByText("Approve"));
    });
    expect(api.post).toHaveBeenCalledWith(
      "/requests/req-1/respond",
      expect.objectContaining({ action: "approved", responder_type: "user" }),
    );
    await waitFor(() => expect(queryByText("Delete prod secret?")).toBeNull());
    expect(container.querySelectorAll('[data-testid="inbox-row"]').length).toBe(0);
  });

  it("shows an empty state when there are no pending requests", async () => {
    vi.mocked(api.get).mockResolvedValue([]);
    const { getByText } = render(<MobileInbox dark={false} />);
    await waitFor(() => getByText(/No pending approvals/i));
  });
});
