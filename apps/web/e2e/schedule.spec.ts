import { test, expect } from "./fixtures";

test("schedules list is reachable and shows the table chrome", async ({ page }) => {
  await page.goto("/login");
  await page.getByLabel(/email/i).fill(process.env.E2E_EMAIL || "admin@example.com");
  await page.getByLabel(/password/i).fill(process.env.E2E_PASSWORD || "admin123");
  await page.getByRole("button", { name: /sign in/i }).click();
  await page.waitForURL((url) => !url.pathname.startsWith("/login"));

  await page.goto("/schedules");
  await expect(page.getByRole("heading", { name: /schedules/i })).toBeVisible();
  await expect(page.getByRole("link", { name: /new schedule/i })).toBeVisible();
});
