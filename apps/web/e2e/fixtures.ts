import { test as base, expect, request } from "@playwright/test";

const API = process.env.API_BASE_URL || "http://localhost:8080";
const EMAIL = process.env.E2E_EMAIL || "admin@example.com";
const PASSWORD = process.env.E2E_PASSWORD || "admin123";

type Seed = {
  token: string;
  templateId: number;
};

export const test = base.extend<{ seed: Seed }>({
  seed: async ({}, use) => {
    const ctx = await request.newContext({ baseURL: API });
    const login = await ctx.post("/auth/login", { data: { email: EMAIL, password: PASSWORD } });
    expect(login.ok(), `login failed: ${login.status()} ${await login.text()}`).toBeTruthy();
    const { token } = await login.json();

    const list = await ctx.get("/rule-templates", { headers: { Authorization: `Bearer ${token}` } });
    expect(list.ok()).toBeTruthy();
    const body = await list.json();
    let tpl = body.items?.find((t: any) => t.published) ?? body.items?.[0];
    if (!tpl) throw new Error("No rule templates seeded — run ./scripts/seed.sh first");

    await use({ token, templateId: tpl.id });
  }
});

export { expect };
