import fs from "node:fs";
import path from "node:path";
import os from "node:os";

import { test, expect } from "./fixtures";

test.describe("job lifecycle", () => {
  test("uploads CSV, starts a job, watches it to completion", async ({ page, seed }) => {
    const csv = path.join(os.tmpdir(), `e2e-${Date.now()}.csv`);
    fs.writeFileSync(
      csv,
      "email,country,amount\n" +
        "alice@example.com,usa,10\n" +
        "bob@example.com,uk,42\n" +
        "carol@example.com,canada,7\n"
    );

    await page.goto("/login");
    await page.getByLabel(/email/i).fill(process.env.E2E_EMAIL || "admin@example.com");
    await page.getByLabel(/password/i).fill(process.env.E2E_PASSWORD || "admin123");
    await page.getByRole("button", { name: /sign in/i }).click();
    await page.waitForURL((url) => !url.pathname.startsWith("/login"));

    await page.goto("/jobs/new");
    const tplSelect = page.locator("select").first();
    await tplSelect.selectOption(String(seed.templateId));

    await page.locator('input[type="file"]').setInputFiles(csv);
    await page.getByRole("button", { name: /start job/i }).click();

    await page.waitForURL(/\/jobs\/\d+/);
    await expect(page.getByText(/processed/i)).toBeVisible();

    await expect
      .poll(
        async () => (await page.getByTestId("job-status").textContent())?.trim().toLowerCase(),
        { timeout: 60_000, intervals: [1000, 2000, 5000] }
      )
      .toMatch(/succeeded|failed|cancelled/);
  });
});
