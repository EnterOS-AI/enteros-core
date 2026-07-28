import { test, expect } from "@playwright/test";

const API = process.env.E2E_API_URL ?? "http://localhost:8080";

interface OrgTemplate {
  dir: string;
  name: string;
  workspaces: number;
}

test.describe("Org template import (PLAN.md §20.3)", () => {
  let orgs: OrgTemplate[] = [];

  test.beforeAll(async ({ request }) => {
    const res = await request.get(`${API}/org/templates`);
    expect(res.ok(), "GET /org/templates must succeed").toBeTruthy();
    orgs = (await res.json()) as OrgTemplate[];

    // Fail-closed: this E2E exists to prove the org-template import surface
    // renders real templates. An empty registry is a setup failure, not a
    // reason to skip coverage. Component tests cover the empty-state UI.
    expect(
      orgs.length,
      "No org templates configured — run scripts/clone-manifest.sh (or equivalent) before this E2E suite",
    ).toBeGreaterThan(0);
  });

  test("org templates section renders inside the palette", async ({ page }) => {
    await page.goto("/", { waitUntil: "networkidle" });

    // The Org Templates section lives in TWO places: inside the EmptyState
    // (visible only when there are 0 workspaces) and inside the
    // TemplatePalette sidebar. Open the palette so the section is reachable
    // regardless of workspace count.
    const paletteToggle = page.getByTitle("Template Palette");
    if (await paletteToggle.isVisible()) {
      await paletteToggle.click({ force: true });
    }

    const section = page.getByTestId("org-templates-section").first();
    await expect(section).toBeVisible({ timeout: 15000 });
    await expect(section.getByText("Org Templates")).toBeVisible();

    // Wait for the API fetch to populate (auto-waits via toBeVisible).
    // Pick the first LOADABLE template: /org/templates now returns entries the
    // server could not load as present-but-unavailable (error/reason,
    // workspaces: 0), and those render "Unavailable" with no workspace badge.
    // os.ReadDir sorts lexically, so a broken dir can sort first.
    const first = orgs.find((o) => !o.reason) ?? orgs[0];
    const label = first.name || first.dir;
    await expect(section.getByText(label, { exact: false })).toBeVisible({ timeout: 15000 });
    await expect(section.getByText(`${first.workspaces}w`)).toBeVisible();
    await expect(section.getByRole("button", { name: /Import org/i }).first()).toBeVisible();
  });

  test("import button exists for every org template returned by the API", async ({ page }) => {
    await page.goto("/", { waitUntil: "networkidle" });
    const paletteToggle = page.getByTitle("Template Palette");
    if (await paletteToggle.isVisible()) {
      await paletteToggle.click({ force: true });
    }
    const section = page.getByTestId("org-templates-section").first();
    await expect(section).toBeVisible({ timeout: 15000 });

    // One "Import org" button per LOADABLE org. A template the server could
    // not load renders a disabled "Unavailable" button instead, so counting
    // every entry would over-count once any template is broken.
    const importable = orgs.filter((o) => !o.reason);
    const buttons = section.getByRole("button", { name: /Import org/i });
    await expect(buttons).toHaveCount(importable.length, { timeout: 15000 });
  });
});
