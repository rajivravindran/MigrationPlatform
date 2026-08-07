"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import { useMemo, useState } from "react";
import { toast } from "sonner";

import { Button, Card, Input } from "@/components/ui";
import { apiFetch } from "@/lib/api";
import { previewCron } from "@/lib/cron";

type Template = { id: number; name: string; published: boolean; version: number };
type Connector = { id: number; name: string; connector_kind: string };

const OVERLAPS = ["skip", "buffer_one", "buffer_all", "cancel_other", "allow_all"] as const;

export default function NewSchedulePage() {
  const router = useRouter();
  const [name, setName] = useState("");
  const [templateId, setTemplateId] = useState<number | null>(null);
  const [connectorId, setConnectorId] = useState<number | null>(null);
  const [cron, setCron] = useState("0 2 * * *");
  const [timezone, setTimezone] = useState("UTC");
  const [overlap, setOverlap] = useState<(typeof OVERLAPS)[number]>("skip");
  const [catchup, setCatchup] = useState(3600);

  const templates = useQuery({
    queryKey: ["templates"],
    queryFn: () => apiFetch<{ items: Template[] }>("/rule-templates")
  });
  const connectors = useQuery({
    queryKey: ["connectors"],
    queryFn: () => apiFetch<{ items: Connector[] }>("/connectors")
  });

  const nextFive = useMemo(() => previewCron(cron, timezone), [cron, timezone]);

  const create = useMutation({
    mutationFn: async () => {
      if (!templateId || !connectorId) throw new Error("select template + connector");
      const tpl = (templates.data?.items ?? []).find((t) => t.id === templateId);
      if (!tpl?.published) throw new Error("select a published template");
      return apiFetch<{ id: number }>("/schedules", {
        method: "POST",
        body: JSON.stringify({
          name,
          rule_template_id: templateId,
          rule_template_version: tpl.version,
          connector_id: connectorId,
          spec: { cron },
          timezone,
          overlap_policy: overlap,
          catchup_window_seconds: catchup,
          enabled: true
        })
      });
    },
    onSuccess: (s) => {
      toast.success(`Schedule #${s.id} created`);
      router.push(`/schedules/${s.id}`);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const published = (templates.data?.items ?? []).filter((t) => t.published);

  return (
    <div className="max-w-2xl space-y-4">
      <h1 className="text-2xl font-semibold">New schedule</h1>

      <Card>
        <label className="mb-3 block text-sm">
          Name
          <Input className="mt-1" value={name} onChange={(e) => setName(e.target.value)} placeholder="Nightly Salesforce sync" />
        </label>
        <div className="grid grid-cols-2 gap-3">
          <label className="block text-sm">
            Template
            <select
              className="mt-1 w-full rounded border border-slate-300 px-2 py-1.5 text-sm"
              value={templateId ?? ""}
              onChange={(e) => setTemplateId(Number(e.target.value))}
            >
              <option value="">— select —</option>
              {published.map((t) => (
                <option key={t.id} value={t.id}>{t.name} (v{t.version})</option>
              ))}
            </select>
          </label>
          <label className="block text-sm">
            Connector
            <select
              className="mt-1 w-full rounded border border-slate-300 px-2 py-1.5 text-sm"
              value={connectorId ?? ""}
              onChange={(e) => setConnectorId(Number(e.target.value))}
            >
              <option value="">— select —</option>
              {connectors.data?.items?.map((c) => (
                <option key={c.id} value={c.id}>{c.name} ({c.connector_kind})</option>
              ))}
            </select>
          </label>
        </div>
      </Card>

      <Card>
        <h2 className="mb-2 text-lg font-medium">Trigger</h2>
        <div className="grid grid-cols-2 gap-3">
          <label className="block text-sm">
            Cron
            <Input
              className="mt-1 font-mono"
              value={cron}
              onChange={(e) => setCron(e.target.value)}
              placeholder="m h dom mon dow"
            />
          </label>
          <label className="block text-sm">
            Timezone
            <Input className="mt-1" value={timezone} onChange={(e) => setTimezone(e.target.value)} />
          </label>
          <label className="block text-sm">
            Overlap policy
            <select
              className="mt-1 w-full rounded border border-slate-300 px-2 py-1.5 text-sm"
              value={overlap}
              onChange={(e) => setOverlap(e.target.value as any)}
            >
              {OVERLAPS.map((o) => <option key={o}>{o}</option>)}
            </select>
          </label>
          <label className="block text-sm">
            Catchup window (s)
            <Input
              type="number"
              className="mt-1"
              value={catchup}
              onChange={(e) => setCatchup(Number(e.target.value))}
            />
          </label>
        </div>
        <div className="mt-3 rounded bg-slate-50 p-3 text-xs">
          <div className="mb-1 font-medium text-slate-700">Next 5 fires (preview)</div>
          {nextFive.length === 0 ? (
            <div className="text-slate-500">Invalid or unsupported cron expression.</div>
          ) : (
            <ul className="list-disc pl-5">
              {nextFive.map((d, i) => <li key={i}>{d}</li>)}
            </ul>
          )}
        </div>
      </Card>

      <Button onClick={() => create.mutate()} disabled={create.isPending}>
        {create.isPending ? "Creating..." : "Create schedule"}
      </Button>
    </div>
  );
}
