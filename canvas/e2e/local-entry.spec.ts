import { test, expect } from "@playwright/test";

test("locale entry loads Canvas without a redirect", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const response = await page.goto("/en");
  expect(response?.status()).toBe(200);
  expect(response?.request().redirectedFrom()).toBeNull();
  await expect(page).toHaveTitle(/org chart canvas/i);
  await expect(page.locator("body")).toContainText(/Molecule|workspace|setup/i);
  expect(errors).toEqual([]);
});

test("a root-to-locale redirect cannot form a loop", async ({ page, baseURL }) => {
  // Model a browser's cached permanent redirect from an earlier app using the
  // same local origin. The locale entry must terminate that chain with HTML.
  await page.route(`${baseURL}/`, (route) => route.fulfill({
    status: 308,
    headers: { location: "/en" },
    body: "",
  }));
  const response = await page.goto("/");
  expect(response?.status()).toBe(200);
  await expect(page).toHaveURL(/\/en$/);
  await expect(page).toHaveTitle(/org chart canvas/i);
});
