"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";
import { useParams } from "next/navigation";

import { Breadcrumbs, Card, ErrorState, LoadingState, PageHeader, StatusBadge, formatDateTime } from "@/components/ui";
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
  depends_on?: string[];
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

export default function BatchDetailPage() {
  const params = useParams<{ id: string }>();
  const id = params.id;
  const { data, isLoading, error } = useQuery({
    queryKey: ["batches", id],
    queryFn: () => apiFetch<BatchDetail>(`/batches/${id}`),
    refetchInterval: 4000
  });

  if (isLoading) return <LoadingState label="Loading batch" />;
  if (error || !data) return <ErrorState title="Batch not found" error={error} />;

  return (
    <div className="space-y-4">
      <PageHeader
        title={`Batch #${data.id}`}
        eyebrow={<Breadcrumbs items={[{ label: "Batches", href: "/batches" }, { label: `Batch #${data.id}` }]} />}
        description={
          [
            data.batch_key ? `Key ${data.batch_key}` : null,
            data.source_ref?.bucket && data.source_ref?.key ? `${data.source_ref.bucket}/${data.source_ref.key}` : null
          ].filter(Boolean).join(" · ")
        }
        actions={<StatusBadge status={data.status} />}
      />

      <div className="flex flex-wrap gap-3 text-sm">
        <span className="text-slate-500">Stage failure policy: <strong className="font-medium text-slate-700">{data.on_stage_failure.replaceAll("_", " ")}</strong></span>
        {data.started_at ? <span className="text-slate-500">Started {formatDateTime(data.started_at)}</span> : null}
        {data.finished_at ? <span className="text-slate-500">Finished {formatDateTime(data.finished_at)}</span> : null}
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
        <div className="overflow-x-auto rounded-lg border border-slate-200 bg-white">
          <table className="min-w-[950px] w-full text-sm">
            <caption className="sr-only">Batch stages in execution order</caption>
            <thead className="bg-slate-50 text-slate-600">
              <tr>
                <th className="px-3 py-2 text-left">#</th>
                <th className="px-3 py-2 text-left">Stage</th>
                <th className="px-3 py-2 text-left">Depends on</th>
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
                  <td className="px-3 py-2"><span className="inline-flex h-6 w-6 items-center justify-center rounded-full bg-slate-100 text-xs font-medium">{s.stage_index + 1}</span></td>
                  <td className="px-3 py-2 font-medium">{s.stage_key}</td>
                  <td className="px-3 py-2 font-mono text-xs text-slate-600">
                    {(s.depends_on ?? []).length > 0 ? (s.depends_on ?? []).join(", ") : "—"}
                  </td>
                  <td className="px-3 py-2 font-mono text-xs">{s.file_path}</td>
                  <td className="px-3 py-2">{s.template_key}</td>
                  <td className="px-3 py-2">
                    {s.job_id ? (
                      <Link className="text-brand-700 hover:underline" href={`/jobs/${s.job_id}`}>#{s.job_id}</Link>
                    ) : "—"}
                  </td>
                  <td className="px-3 py-2"><StatusBadge status={s.status} /></td>
                  <td className="max-w-xs px-3 py-2 text-xs text-slate-500"><span className="line-clamp-2" title={s.error_message ?? undefined}>{s.error_message ?? "—"}</span></td>
                </tr>
              ))}
              {(!data.stages || data.stages.length === 0) ? (
                <tr>
                  <td colSpan={8} className="px-3 py-4 text-slate-500">
                    {data.status === "running"
                      ? "No stages yet — archive unpack is still in progress. Refresh this page (it auto-updates every few seconds). Jobs appear under Jobs only after stages start."
                      : "No stages recorded (unpack never completed, or the package was quarantined)."}
                    {data.temporal_workflow_id ? (
                      <> Workflow <code className="font-mono text-xs">{data.temporal_workflow_id}</code>.</>
                    ) : null}
                  </td>
                </tr>
              ) : null}
            </tbody>
          </table>
        </div>
      </div>
    </div>
  );
}
