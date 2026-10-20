"use client";

import { useQueries, useQuery } from "@tanstack/react-query";
import Link from "next/link";
import { useParams } from "next/navigation";

import { StageGraph, StageLegend, type GraphNode } from "@/components/dependency/StageGraph";
import { Breadcrumbs, Card, ErrorState, LoadingState, PageHeader, StatusBadge, formatDateTime } from "@/components/ui";
import { apiFetch } from "@/lib/api";
import { layoutStages, stageState, type StageState } from "@/lib/package-graph";

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

type StageJob = {
  id: number;
  status: string;
  totals_json?: { processed?: number; failed?: number; total?: number } | null;
};

const TERMINAL = new Set(["succeeded", "failed", "cancelled"]);

export default function BatchDetailPage() {
  const params = useParams<{ id: string }>();
  const id = params.id;
  const { data, isLoading, error } = useQuery({
    queryKey: ["batches", id],
    queryFn: () => apiFetch<BatchDetail>(`/batches/${id}`),
    refetchInterval: 4000
  });

  const stageJobs = useQueries({
    queries: (data?.stages ?? []).map((s) => ({
      queryKey: ["jobs", s.job_id],
      queryFn: () => apiFetch<StageJob>(`/jobs/${s.job_id}`),
      enabled: Boolean(s.job_id),
      refetchInterval: (q: { state: { data?: StageJob } }) => (q.state.data && TERMINAL.has(q.state.data.status) ? false : 4000)
    }))
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

      {data.stages?.length ? <DependencyPanel batchId={data.id} stages={data.stages} jobs={stageJobs.map((q) => q.data)} /> : null}

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

function DependencyPanel({ batchId, stages, jobs }: { batchId: number; stages: Stage[]; jobs: (StageJob | undefined)[] }) {
  const nodes: GraphNode[] = stages.map((s, i) => {
    const job = jobs[i];
    const t = job?.totals_json;
    return {
      key: s.stage_key,
      dependsOn: s.depends_on ?? [],
      state: stageState(s.status, s.error_message, job?.status),
      detail: s.template_key,
      progress: t?.total ? { processed: t.processed ?? 0, total: t.total, failed: t.failed ?? 0 } : null,
      note: s.error_message,
      href: s.job_id ? `/jobs/${s.job_id}` : null
    };
  });
  const counts: Partial<Record<StageState, number>> = {};
  nodes.forEach((n) => (counts[n.state] = (counts[n.state] ?? 0) + 1));
  const done = counts.completed ?? 0;
  const pct = Math.round((done / nodes.length) * 100);
  const sequential = !stages.some((s) => (s.depends_on ?? []).length > 0);

  return (
    <section aria-labelledby="dependency-heading" className="space-y-3">
      <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
        <h2 id="dependency-heading" className="text-lg font-medium">Dependency graph</h2>
        <span className="text-sm text-slate-500">
          {done} of {nodes.length} stages completed ({pct}%)
          {sequential ? " · no dependencies declared, so stages run one after another (dashed)" : ""}
        </span>
      </div>
      <div className="h-1.5 overflow-hidden rounded bg-slate-200" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100} aria-label="Stages completed">
        <div className="h-full bg-emerald-500 transition-all" style={{ width: `${pct}%` }} />
      </div>
      <StageLegend counts={counts} states={["pending", "running", "completed", "failed", "cancelled"]} />
      <StageGraph nodes={nodes} layoutKey={`batch-${batchId}`} height={canvasHeight(nodes)} />
      <p className="text-xs text-slate-500">Drag stages to arrange them (remembered in this browser); select a stage to open its migration. The table below lists the same stages for keyboard and screen-reader use.</p>
    </section>
  );
}


/** Fit the canvas to the tallest column of the layout. */
function canvasHeight(nodes: GraphNode[]): number {
  const maxY = Math.max(0, ...Array.from(layoutStages(nodes).values(), (p) => p.y));
  return Math.min(560, Math.max(240, maxY + 240));
}
