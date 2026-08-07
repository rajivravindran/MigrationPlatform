import { test, expect } from "./fixtures";

test("navigates to template list and opens the latest", async ({ page, seed }) => {
  await page.goto("/login");
  await page.getByLabel(/email/i).fill(process.env.E2E_EMAIL || "admin@example.com");
  await page.getByLabel(/password/i).fill(process.env.E2E_PASSWORD || "admin123");
  await page.getByRole("button", { name: /sign in/i }).click();

  await page.waitForURL((url) => !url.pathname.startsWith("/login"));
  await page.goto("/templates");
  await expect(page.getByRole("heading", { name: /templates/i })).toBeVisible();
  await page.goto(`/templates/${seed.templateId}`);
  await expect(page.getByRole("button", { name: /(save|publish)/i }).first()).toBeVisible();
});
