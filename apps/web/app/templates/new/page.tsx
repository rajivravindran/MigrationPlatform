"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { toast } from "sonner";

import { Button, Card, Input } from "@/components/ui";
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
    <div className="max-w-xl">
      <Card>
        <h1 className="mb-4 text-xl font-semibold">Create rule template</h1>
        <label className="mb-3 block text-sm">
          Name
          <Input className="mt-1" value={name} onChange={(e) => setName(e.target.value)} />
        </label>
        <p className="mb-4 text-xs text-slate-500">
          A blank template is created in draft. You&apos;ll configure source schema, mapping, and destination on the next screen.
        </p>
        <Button onClick={create} disabled={busy || !name}>
          {busy ? "Creating..." : "Create"}
        </Button>
      </Card>
    </div>
  );
}
