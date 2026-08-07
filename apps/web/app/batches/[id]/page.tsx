"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";
import { useParams } from "next/navigation";

import { Badge, Card } from "@/components/ui";
import { apiFetch } from "@/lib/api";

type Stage = {
  id: number;
  stage_index: number;
  stage_key: string;
  file_path: string;
  template_key: string;
  rule_template_id?: number | null;
  job_id?: number | null;
  status: string;
  on_stage_failure?: string | null;
  error_message?: string | null;
};

type BatchDetail = {
  id: number;
  batch_key?: string | null;
  status: string;
  on_stage_failure: string;
  source_ref?: { bucket?: string; key?: string };
  manifest_json?: unknown;
  quarantine_ref?: { key?: string; reason?: string } | null;
  error_message?: string | null;
  started_at?: string;
  finished_at?: string;
  temporal_workflow_id?: string | null;
  stages: Stage[];
};

function tone(s: string) {
  switch (s) {
    case "succeeded": return "ok";
    case "failed":
    case "quarantined": return "fail";
    case "partial": return "warn";
    default: return "neutral";
  }
}

export default function BatchDetailPage() {
  const params = useParams<{ id: string }>();
  const id = params.id;
  const { data, isLoading, error } = useQuery({
    queryKey: ["batches", id],
    queryFn: () => apiFetch<BatchDetail>(`/batches/${id}`),
    refetchInterval: 4000
  });

  if (isLoading) return <Card>Loading&hellip;</Card>;
  if (error || !data) return <Card>Batch not found.</Card>;

  return (
    <div className="space-y-4">
      <div>
        <Link href="/batches" className="text-sm text-brand-700 hover:underline">← Batches</Link>
        <h1 className="mt-1 text-2xl font-semibold">Batch #{data.id}</h1>
        <p className="text-sm text-slate-500">
          {data.batch_key ? `Key ${data.batch_key} · ` : null}
          {data.source_ref?.bucket}/{data.source_ref?.key}
        </p>
      </div>

      <div className="flex flex-wrap gap-3 text-sm">
        <Badge tone={tone(data.status) as any}>{data.status}</Badge>
        <span className="text-slate-500">onStageFailure: {data.on_stage_failure}</span>
        {data.started_at ? <span className="text-slate-500">started {data.started_at}</span> : null}
        {data.finished_at ? <span className="text-slate-500">finished {data.finished_at}</span> : null}
      </div>

      {data.error_message ? (
        <Card className="border-red-200 bg-red-50 text-sm text-red-800">{data.error_message}</Card>
      ) : null}
      {data.quarantine_ref?.key ? (
        <Card className="text-sm">
          Quarantined to <code className="font-mono">{data.quarantine_ref.key}</code>
        </Card>
      ) : null}

      <div>
        <h2 className="mb-2 text-lg font-medium">Stages</h2>
        <div className="overflow-hidden rounded-lg border border-slate-200 bg-white">
          <table className="w-full text-sm">
            <thead className="bg-slate-50 text-slate-600">
              <tr>
                <th className="px-3 py-2 text-left">#</th>
                <th className="px-3 py-2 text-left">Stage</th>
                <th className="px-3 py-2 text-left">File</th>
                <th className="px-3 py-2 text-left">Template</th>
                <th className="px-3 py-2 text-left">Job</th>
                <th className="px-3 py-2 text-left">Status</th>
                <th className="px-3 py-2 text-left">Error</th>
              </tr>
            </thead>
            <tbody>
              {data.stages?.map((s) => (
                <tr key={s.id} className="border-t border-slate-100">
                  <td className="px-3 py-2">{s.stage_index}</td>
                  <td className="px-3 py-2 font-medium">{s.stage_key}</td>
                  <td className="px-3 py-2 font-mono text-xs">{s.file_path}</td>
                  <td className="px-3 py-2">{s.template_key}</td>
                  <td className="px-3 py-2">
                    {s.job_id ? (
                      <Link className="text-brand-700 hover:underline" href={`/jobs/${s.job_id}`}>#{s.job_id}</Link>
                    ) : "—"}
                  </td>
                  <td className="px-3 py-2"><Badge tone={tone(s.status) as any}>{s.status}</Badge></td>
                  <td className="px-3 py-2 text-xs text-slate-500 max-w-xs truncate">{s.error_message ?? "—"}</td>
                </tr>
              ))}
              {(!data.stages || data.stages.length === 0) ? (
                <tr><td colSpan={7} className="px-3 py-4 text-slate-500">No stages yet (unpack still running or quarantined).</td></tr>
              ) : null}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}
