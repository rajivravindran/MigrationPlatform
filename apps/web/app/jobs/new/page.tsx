"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { useRouter } from "next/navigation";
import { useRef, useState } from "react";
import { toast } from "sonner";

import { Breadcrumbs, Button, Card, FormField, Input, PageHeader, Select, Textarea } from "@/components/ui";
import { apiFetch, getToken } from "@/lib/api";

type Template = { id: number; name: string; published: boolean; version: number };

export default function NewJobPage() {
  const router = useRouter();
  const fileRef = useRef<HTMLInputElement>(null);
  const [templateId, setTemplateId] = useState<number | null>(null);
  const [connectorOverride, setConnectorOverride] = useState("");
  const [advanced, setAdvanced] = useState(false);
  const [submitted, setSubmitted] = useState(false);

  const templates = useQuery({
    queryKey: ["templates", "published"],
    queryFn: () => apiFetch<{ items: Template[] }>("/rule-templates?published=true"),
    refetchOnMount: "always"
  });

  const start = useMutation({
    mutationFn: async () => {
      setSubmitted(true);
      if (!templateId) throw new Error("Select a published template.");
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
        throw new Error("Upload a source file or provide an advanced source reference.");
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
    <div className="mx-auto max-w-3xl space-y-6">
      <PageHeader title="Start a job" description="Run a published migration template against an uploaded source file." eyebrow={<Breadcrumbs items={[{ label: "Jobs", href: "/jobs" }, { label: "Start" }]} />} />

      <Card>
        <h2 className="mb-1 text-lg font-semibold">1. Choose a migration template</h2>
        <p className="mb-4 text-sm text-slate-500">Jobs use the selected published version.</p>
        {published.length === 0 ? (
          <p className="text-sm text-slate-500">No published templates. Publish one from the Templates page first.</p>
        ) : (
          <FormField label="Published template" required error={submitted && !templateId ? "Select a template." : undefined}>
          <Select
            value={templateId ?? ""}
            onChange={(e) => setTemplateId(e.target.value ? Number(e.target.value) : null)}
          >
            <option value="">Select a template…</option>
            {published.map((t) => (
              <option key={t.id} value={t.id}>{t.name} (v{t.version})</option>
            ))}
          </Select>
          </FormField>
        )}
      </Card>

      <Card>
        <h2 className="mb-1 text-lg font-semibold">2. Add source data</h2>
        <p className="mb-4 text-sm text-slate-500">CSV, JSON, NDJSON, and XML files are supported.</p>
        <div className="space-y-3">
          <FormField label="Source file" required={!advanced} hint="The file is uploaded securely before the job starts.">
            <Input type="file" ref={fileRef} accept=".csv,.json,.ndjson,.jsonl,.xml" />
          </FormField>
          <details onToggle={(event) => setAdvanced(event.currentTarget.open)} className="rounded border border-slate-200">
            <summary className="cursor-pointer px-3 py-2 text-sm font-medium text-slate-700">Advanced source reference</summary>
            <div className="border-t border-slate-100 p-3">
            <FormField label="Source reference JSON" hint="For connector-driven sources. This object is passed to the jobs API unchanged.">
            <Textarea
              className="font-mono text-xs"
              rows={6}
              placeholder='{"type":"salesforce","connectorId":3,"extra":{"soql":"SELECT Id FROM Account"}}'
              value={connectorOverride}
              onChange={(e) => setConnectorOverride(e.target.value)}
            />
            </FormField>
            </div>
          </details>
        </div>
      </Card>

      <div className="flex justify-end border-t border-slate-200 pt-5"><Button onClick={() => start.mutate()} disabled={start.isPending || !templateId}>
        {start.isPending ? "Uploading and starting…" : "Start job"}
      </Button></div>
    </div>
  );
}
