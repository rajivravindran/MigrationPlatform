import { test, expect } from "./fixtures";

const EMAIL = process.env.E2E_EMAIL || "admin@example.com";
const PASSWORD = process.env.E2E_PASSWORD || "admin123";

test("user can sign in and see the templates page", async ({ page }) => {
  await page.goto("/login");
  await page.getByLabel(/email/i).fill(EMAIL);
  await page.getByLabel(/password/i).fill(PASSWORD);
  await page.getByRole("button", { name: /sign in/i }).click();

  await page.waitForURL((url) => !url.pathname.startsWith("/login"));
  await expect(page.getByText(EMAIL)).toBeVisible();
  await page.goto("/templates");
  await expect(page.getByRole("heading", { name: /templates/i })).toBeVisible();
});

test("sign out clears the session header", async ({ page }) => {
  await page.goto("/login");
  await page.getByLabel(/email/i).fill(EMAIL);
  await page.getByLabel(/password/i).fill(PASSWORD);
  await page.getByRole("button", { name: /sign in/i }).click();
  await page.waitForURL((url) => !url.pathname.startsWith("/login"));
  await page.getByRole("button", { name: /sign out/i }).click();
  await page.waitForURL(/\/login/);
});
