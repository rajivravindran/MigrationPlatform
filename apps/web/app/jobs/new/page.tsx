"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import { useRef, useState } from "react";
import { toast } from "sonner";

import { Button, Card, Input } from "@/components/ui";
import { apiFetch, getToken } from "@/lib/api";

type Template = { id: number; name: string; published: boolean; version: number };

export default function NewJobPage() {
  const router = useRouter();
  const fileRef = useRef<HTMLInputElement>(null);
  const [templateId, setTemplateId] = useState<number | null>(null);
  const [connectorOverride, setConnectorOverride] = useState("");

  const templates = useQuery({
    queryKey: ["templates", "published"],
    queryFn: () => apiFetch<{ items: Template[] }>("/rule-templates?published=true"),
    refetchOnMount: "always"
  });

  const start = useMutation({
    mutationFn: async () => {
      if (!templateId) throw new Error("pick a template");
      let sourceRef: Record<string, unknown>;
      if (fileRef.current?.files?.[0]) {
        const form = new FormData();
        form.append("file", fileRef.current.files[0]);
        const res = await fetch("/api/files", {
          method: "POST",
          headers: { authorization: `Bearer ${getToken() ?? ""}` },
          body: form
        });
        if (!res.ok) throw new Error(await res.text());
        const body = await res.json();
        sourceRef = body.source_ref;
      } else if (connectorOverride) {
        sourceRef = JSON.parse(connectorOverride);
      } else {
        throw new Error("upload a file or paste a source_ref JSON");
      }
      return apiFetch<{ id: number }>("/jobs", {
        method: "POST",
        body: JSON.stringify({ rule_template_id: templateId, source_ref: sourceRef })
      });
    },
    onSuccess: (job) => {
      toast.success(`Job #${job.id} started`);
      router.push(`/jobs/${job.id}`);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const published = templates.data?.items ?? [];

  return (
    <div className="max-w-2xl space-y-4">
      <h1 className="text-2xl font-semibold">Start a new job</h1>

      <Card>
        <h2 className="mb-2 text-lg font-medium">1. Choose a published template</h2>
        {published.length === 0 ? (
          <p className="text-sm text-slate-500">No published templates. Publish one from the Templates page first.</p>
        ) : (
          <select
            className="w-full rounded border border-slate-300 px-2 py-1.5 text-sm"
            value={templateId ?? ""}
            onChange={(e) => setTemplateId(Number(e.target.value))}
          >
            <option value="">— select —</option>
            {published.map((t) => (
              <option key={t.id} value={t.id}>{t.name} (v{t.version})</option>
            ))}
          </select>
        )}
      </Card>

      <Card>
        <h2 className="mb-2 text-lg font-medium">2. Source</h2>
        <div className="space-y-3">
          <div>
            <div className="mb-1 text-xs text-slate-500">Upload a file (CSV/JSON/XML)</div>
            <Input type="file" ref={fileRef} />
          </div>
          <div className="text-center text-xs uppercase text-slate-400">— or —</div>
          <div>
            <div className="mb-1 text-xs text-slate-500">Paste a source_ref JSON (connector-driven)</div>
            <textarea
              className="h-28 w-full rounded border border-slate-300 px-2 py-1 font-mono text-xs"
              placeholder='{"type":"salesforce","connectorId":3,"extra":{"soql":"SELECT Id FROM Account"}}'
              value={connectorOverride}
              onChange={(e) => setConnectorOverride(e.target.value)}
            />
          </div>
        </div>
      </Card>

      <Button onClick={() => start.mutate()} disabled={start.isPending || !templateId}>
        {start.isPending ? "Starting..." : "Start job"}
      </Button>
    </div>
  );
}
