"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { toast } from "sonner";

import { Breadcrumbs, Button, Card, FormField, Input, PageHeader } from "@/components/ui";
import { apiFetch } from "@/lib/api";

const SKELETON = {
  id: "rt_new",
  version: 1,
  name: "New template",
  source: { type: "csv", schema: [], options: { header: true, delimiter: "," } },
  preprocess: [],
  mapping: { payload: {} },
  destination: { type: "http", method: "POST", url: "https://example.com/v1/items" }
};

export default function NewTemplatePage() {
  const router = useRouter();
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);

  async function create() {
    setBusy(true);
    try {
      const created = await apiFetch<{ id: number }>("/rule-templates", {
        method: "POST",
        body: JSON.stringify({ name, schema_json: { ...SKELETON, name } })
      });
      toast.success(`Created template #${created.id}`);
      router.push(`/templates/${created.id}`);
    } catch (err) {
      toast.error(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="mx-auto max-w-2xl space-y-6">
      <PageHeader title="Create template" description="Start a draft mapping definition. Source fields, destination operations, and validation are configured in the designer." eyebrow={<Breadcrumbs items={[{ label: "Templates", href: "/templates" }, { label: "Create" }]} />} />
      <Card>
        <FormField label="Template name" required hint="Use a stable business name operators can recognize in jobs and schedules.">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Customer account import" />
        </FormField>
        <p className="mb-4 text-xs text-slate-500">
          A draft is created first. It cannot be used by jobs or schedules until its mapping is valid and the version is published.
        </p>
        <Button onClick={create} disabled={busy || !name}>
          {busy ? "Creating…" : "Create draft"}
        </Button>
      </Card>
    </div>
  );
}
