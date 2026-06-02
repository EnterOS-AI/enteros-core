// @vitest-environment jsdom
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, fireEvent, waitFor, cleanup } from "@testing-library/react";
import { CreateWorkspaceButton } from "../CreateWorkspaceDialog";

vi.mock("@/lib/api", () => ({
  api: {
    get: vi.fn(),
    post: vi.fn(),
  },
}));

import { api } from "@/lib/api";

const mockGet = vi.mocked(api.get);
const mockPost = vi.mocked(api.post);

const SAMPLE_WORKSPACES = [
  { id: "ws-1", name: "Platform Team", tier: 1 },
  { id: "ws-2", name: "Research Agent", tier: 2 },
];

const SAMPLE_TEMPLATES = [
  {
    id: "claude-code-default",
    name: "Claude Code Agent",
    runtime: "claude-code",
    model: "moonshot/kimi-k2.6",
    providers: ["platform", "minimax", "kimi-coding", "anthropic", "anthropic-oauth"],
    models: [
      { id: "moonshot/kimi-k2.6", name: "Kimi K2.6", provider: "platform", required_env: [] },
      { id: "MiniMax-M2.7", name: "MiniMax M2.7", required_env: ["MINIMAX_API_KEY"] },
      { id: "kimi-k2-turbo-preview", name: "Kimi K2 Turbo Preview", required_env: ["KIMI_API_KEY"] },
      { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6", required_env: ["ANTHROPIC_API_KEY"] },
      { id: "sonnet", name: "Claude Sonnet", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
      { id: "opus", name: "Claude Opus", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
      { id: "haiku", name: "Claude Haiku", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
    ],
  },
  {
    id: "seo-agent",
    name: "SEO Agent",
    runtime: "claude-code",
    model: "moonshot/kimi-k2.6",
    providers: ["platform", "minimax", "kimi-coding", "anthropic", "anthropic-oauth"],
    models: [
      { id: "moonshot/kimi-k2.6", name: "Kimi K2.6", provider: "platform", required_env: [] },
      { id: "MiniMax-M2.7", name: "MiniMax M2.7", required_env: ["MINIMAX_API_KEY"] },
      { id: "kimi-k2-turbo-preview", name: "Kimi K2 Turbo Preview", required_env: ["KIMI_API_KEY"] },
      { id: "claude-sonnet-4-6", name: "Claude Sonnet 4.6", required_env: ["ANTHROPIC_API_KEY"] },
      { id: "sonnet", name: "Claude Sonnet", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
      { id: "opus", name: "Claude Opus", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
      { id: "haiku", name: "Claude Haiku", required_env: ["CLAUDE_CODE_OAUTH_TOKEN"] },
    ],
  },
  {
    id: "hermes",
    name: "Hermes",
    runtime: "hermes",
    model: "openai/gpt-4o",
    providers: ["openai", "anthropic", "platform"],
    models: [
      { id: "openai/gpt-4o", name: "GPT-4o", required_env: ["OPENAI_API_KEY"] },
      { id: "anthropic/claude-sonnet-4-5", name: "Claude Sonnet 4.5", required_env: ["ANTHROPIC_API_KEY"] },
      { id: "moonshot/kimi-k2.6", name: "Kimi K2.6", provider: "platform", required_env: [] },
    ],
  },
];

beforeEach(() => {
  vi.clearAllMocks();
  mockGet.mockImplementation(async (url: string) => {
    if (url === "/templates") {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      return SAMPLE_TEMPLATES as any;
    }
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    return SAMPLE_WORKSPACES as any;
  });
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  mockPost.mockResolvedValue({} as any);
});

afterEach(() => {
  cleanup();
});

async function openDialog() {
  render(<CreateWorkspaceButton />);
  const btn = screen.getAllByRole("button").find((b) => b.textContent?.includes("New Workspace"));
  expect(btn).toBeTruthy();
  fireEvent.click(btn!);
  await waitFor(() => expect(screen.getByText("Create Workspace")).toBeTruthy());
}

async function setTemplate(value: string) {
  fireEvent.change(
    screen.getByLabelText("Workspace Template"),
    { target: { value } }
  );
}

async function setRuntime(value: string) {
  fireEvent.change(
    screen.getByLabelText("Runtime"),
    { target: { value } }
  );
}

describe("CreateWorkspaceDialog", () => {
  it("opens the dialog when New Workspace button is clicked", async () => {
    await openDialog();
    expect(screen.getByText("Create Workspace")).toBeTruthy();
  });

  it("renders a <select> for parent workspace — not a text input", async () => {
    await openDialog();
    const selects = document.querySelectorAll("select");
    expect(selects.length).toBeGreaterThanOrEqual(1);
    // The old raw UUID text input is gone
    expect(screen.queryByPlaceholderText("Leave empty for root-level")).toBeNull();
  });

  it('first option is "None (root level)" with empty value', async () => {
    await openDialog();
    const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
    expect(select).toBeTruthy();
    const firstOption = select.options[0];
    expect(firstOption.value).toBe("");
    expect(firstOption.text.trim()).toBe("None (root level)");
  });

  it("populates select with workspace names from GET /workspaces", async () => {
    await openDialog();
    await waitFor(() => {
      const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
      const optionValues = Array.from(select.options).map((o) => o.value);
      expect(optionValues).toContain("ws-1");
      expect(optionValues).toContain("ws-2");
    });
    const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
    const optionTexts = Array.from(select.options).map((o) => o.text.trim());
    expect(optionTexts.some((t) => t.includes("Platform Team"))).toBe(true);
    expect(optionTexts.some((t) => t.includes("Research Agent"))).toBe(true);
  });

  it("sends parent_id in POST body when a workspace is selected", async () => {
    await openDialog();
    await waitFor(() => {
      const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
      expect(select.options.length).toBeGreaterThan(1);
    });

    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "My Agent" },
    });

    const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "ws-1" } });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.parent_id).toBe("ws-1");
  });

  it("sends parent_id as undefined when None (root level) is selected", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Root Agent" },
    });

    const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
    fireEvent.change(select, { target: { value: "" } });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.parent_id).toBeUndefined();
  });

  it("sends the cost-efficient headless compute profile by default", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Plain Agent" },
    });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.compute).toEqual({
      instance_type: "t3.medium",
      volume: { root_gb: 30 },
      display: { mode: "none" },
    });
    expect(body.model).toBe("moonshot/kimi-k2.6");
    expect(body.llm_provider).toBe("platform");
    expect(body.runtime).toBe("claude-code");
    expect(body.secrets).toBeUndefined();
  });

  it("keeps runtime and workspace template as separate selectors", async () => {
    await openDialog();

    const runtimeSelect = screen.getByLabelText("Runtime") as HTMLSelectElement;
    const runtimeTexts = Array.from(runtimeSelect.options).map((o) => o.text.trim());
    expect(runtimeTexts).toEqual([
      "Claude Code",
      "OpenAI Codex CLI",
      "Google ADK",
      "Hermes",
      "OpenClaw",
    ]);
    expect(runtimeTexts).not.toContain("SEO Agent");

    await waitFor(() => {
      const templateSelect = screen.getByLabelText("Workspace Template") as HTMLSelectElement;
      const templateTexts = Array.from(templateSelect.options).map((o) => o.text.trim());
      expect(templateTexts).toContain("SEO Agent");
      expect(templateTexts).not.toContain("Hermes");
    });
  });

  it("does not send managed compute for external agents", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "External Agent" },
    });
    fireEvent.click(screen.getByLabelText(/External agent/));

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.compute).toBeUndefined();
    expect(body.runtime).toBe("external");
  });

  it("sends display compute profile when desktop display is enabled", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Desktop Agent" },
    });
    fireEvent.click(screen.getByLabelText("Enable display"));

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.model).toBe("moonshot/kimi-k2.6");
    expect(body.llm_provider).toBe("platform");
    expect(body.compute).toEqual({
      instance_type: "t3.xlarge",
      volume: { root_gb: 80 },
      display: {
        mode: "desktop-control",
        protocol: "novnc",
        width: 1920,
        height: 1080,
      },
    });
  });

  it("sends BYOK API key secrets when API key auth mode is selected", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "BYOK Agent" },
    });
    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "minimax|MINIMAX_API_KEY" },
    });
    fireEvent.change(document.getElementById("llm-secret-input") as HTMLInputElement, {
      target: { value: "sk-minimax-test" },
    });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.model).toBe("MiniMax-M2.7");
    expect(body.llm_provider).toBe("minimax");
    expect(body.secrets).toEqual({ MINIMAX_API_KEY: "sk-minimax-test" });
  });

  it("sends Claude OAuth token separately from platform-managed mode", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "OAuth Agent" },
    });
    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "anthropic-oauth|CLAUDE_CODE_OAUTH_TOKEN" },
    });
    fireEvent.change(document.querySelector("[data-testid='model-select']") as HTMLSelectElement, {
      target: { value: "sonnet" },
    });
    fireEvent.change(document.getElementById("llm-secret-input") as HTMLInputElement, {
      target: { value: "oauth-token" },
    });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.model).toBe("sonnet");
    expect(body.llm_provider).toBe("anthropic-oauth");
    expect(body.secrets).toEqual({ CLAUDE_CODE_OAUTH_TOKEN: "oauth-token" });
  });

  it("lists all Claude Code subscription aliases for blank workspaces", async () => {
    await openDialog();

    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "anthropic-oauth|CLAUDE_CODE_OAUTH_TOKEN" },
    });

    const modelSelect = document.querySelector("[data-testid='model-select']") as HTMLSelectElement;
    const optionValues = Array.from(modelSelect.options).map((option) => option.value);
    expect(optionValues).toEqual(expect.arrayContaining(["sonnet", "opus", "haiku"]));
  });

  it("renders gracefully when GET /workspaces fails", async () => {
    mockGet.mockRejectedValueOnce(new Error("Network error"));
    await openDialog();

    // Dialog still renders; select exists with only the root option
    await waitFor(() => {
      const select = screen.getByLabelText("Parent Workspace") as HTMLSelectElement;
      expect(select.options.length).toBe(1);
      expect(select.options[0].value).toBe("");
    });
  });
});

// ---------------------------------------------------------------------------
// Dynamic runtime provider picker tests
// ---------------------------------------------------------------------------

describe("CreateWorkspaceDialog — dynamic runtime provider picker", () => {
  it("does not render the old Hermes-only provider section", async () => {
    await openDialog();
    await setRuntime("hermes");
    expect(document.querySelector("[data-testid='hermes-provider-section']")).toBeNull();
  });

  it("derives Hermes provider and model options from the /templates runtime row", async () => {
    await openDialog();
    await setRuntime("hermes");

    const providerSelect = document.querySelector("[data-testid='provider-select']") as HTMLSelectElement;
    await waitFor(() => expect(providerSelect.options.length).toBe(4));

    const providerValues = Array.from(providerSelect.options).map((option) => option.value);
    expect(providerValues).toEqual(expect.arrayContaining([
      "platform|",
      "openai|OPENAI_API_KEY",
      "anthropic|ANTHROPIC_API_KEY",
    ]));
    expect(providerValues).not.toContain("gemini|GEMINI_API_KEY");
  });

  it("uses the template-declared default provider/model for Hermes", async () => {
    await openDialog();
    await setRuntime("hermes");

    await waitFor(() => {
      const providerSelect = document.querySelector("[data-testid='provider-select']") as HTMLSelectElement;
      expect(providerSelect.value).toBe("platform|");
    });
    const modelSelect = document.querySelector("[data-testid='model-select']") as HTMLSelectElement;
    expect(modelSelect.value).toBe("moonshot/kimi-k2.6");
  });

  it("prompts for the provider credential required by the selected Hermes model", async () => {
    await openDialog();
    await setRuntime("hermes");

    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "openai|OPENAI_API_KEY" },
    });

    const keyInput = document.getElementById("llm-secret-input") as HTMLInputElement;
    expect(keyInput).toBeTruthy();
    expect(keyInput.type).toBe("password");
  });

  it("shows an error if the selected runtime provider requires a credential", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Hermes Agent" },
    });
    await setRuntime("hermes");
    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "openai|OPENAI_API_KEY" },
    });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => {
      const alert = screen.getByRole("alert");
      expect(alert.textContent).toContain("Provider credential");
    });
    expect(mockPost).not.toHaveBeenCalled();
  });

  it("includes runtime-derived provider/model/secrets in POST body", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Hermes OpenAI" },
    });
    await setRuntime("hermes");
    fireEvent.change(document.querySelector("[data-testid='provider-select']") as HTMLSelectElement, {
      target: { value: "openai|OPENAI_API_KEY" },
    });
    fireEvent.change(document.getElementById("llm-secret-input") as HTMLInputElement, {
      target: { value: "sk-openai-test" },
    });

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.runtime).toBe("hermes");
    expect(body.template).toBeUndefined();
    expect(body.model).toBe("openai/gpt-4o");
    expect(body.llm_provider).toBe("openai");
    expect(body.secrets).toEqual({ OPENAI_API_KEY: "sk-openai-test" });
  });

  it("does NOT include secrets field when provider is platform-managed", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Normal Agent" },
    });
    await setTemplate("seo-agent");

    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.secrets).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// budget_limit field tests (#541)
// ---------------------------------------------------------------------------

describe("CreateWorkspaceDialog — budget_limit field", () => {
  it("renders a Budget limit (USD) input", async () => {
    await openDialog();
    const budgetInput = screen.getByPlaceholderText("e.g. 100");
    expect(budgetInput).toBeTruthy();
  });

  it("renders helper text 'Leave blank for unlimited'", async () => {
    await openDialog();
    expect(screen.getByText("Leave blank for unlimited")).toBeTruthy();
  });

  it("sends budget_limit as a number when a value is entered", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Budget Agent" },
    });
    fireEvent.change(screen.getByPlaceholderText("e.g. 100"), {
      target: { value: "250" },
    });
    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.budget_limit).toBe(250);
  });

  it("sends budget_limit as null when the field is left blank", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Unlimited Agent" },
    });
    // Leave budget_limit empty
    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.budget_limit).toBeNull();
  });

  it("sends budget_limit as a float when a decimal value is entered", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. SEO Agent"), {
      target: { value: "Float Budget Agent" },
    });
    fireEvent.change(screen.getByPlaceholderText("e.g. 100"), {
      target: { value: "49.99" },
    });
    const createBtn = screen.getAllByRole("button").find((b) => b.textContent === "Create");
    fireEvent.click(createBtn!);

    await waitFor(() => expect(mockPost).toHaveBeenCalled());
    const body = mockPost.mock.calls[0][1] as Record<string, unknown>;
    expect(body.budget_limit).toBeCloseTo(49.99);
  });

  it("resets budget_limit to empty when dialog is reopened", async () => {
    await openDialog();
    fireEvent.change(screen.getByPlaceholderText("e.g. 100"), {
      target: { value: "500" },
    });

    // Close dialog
    const cancelBtn = screen.getAllByRole("button").find((b) =>
      b.textContent === "Cancel"
    );
    fireEvent.click(cancelBtn!);
    cleanup();

    // Re-open
    await openDialog();
    const budgetInput = screen.getByPlaceholderText("e.g. 100") as HTMLInputElement;
    expect(budgetInput.value).toBe("");
  });
});
