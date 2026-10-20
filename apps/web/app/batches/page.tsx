"use client";

import { useQueries, useQuery } from "@tanstack/react-query";
import { clsx } from "clsx";
import Link from "next/link";

import { Button, EmptyState, ErrorState, LoadingState, PageHeader, StatusBadge, formatDateTime } from "@/components/ui";
import { apiFetch } from "@/lib/api";
import { STAGE_STATES, stageState } from "@/lib/package-graph";

type Batch = {
  id: number;
  batch_key?: string | null;
  status: string;
  on_stage_failure: string;
  source_ref?: { key?: string; bucket?: string };
  started_at?: string;
  finished_at?: string;
  error_message?: string | null;
};

type StageSummary = { stage_key: string; status: string; error_message?: string | null };

export default function BatchesPage() {
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["batches"],
    queryFn: () => apiFetch<{ items: Batch[] }>("/batches"),
    refetchInterval: 5000
  });
  const details = useQueries({
    queries: (data?.items ?? []).slice(0, 25).map((b) => ({
      queryKey: ["batches", String(b.id)],
      queryFn: () => apiFetch<{ stages: StageSummary[] }>(`/batches/${b.id}`),
      refetchInterval: b.status === "running" || b.status === "pending" ? 4000 : false
    }))
  });

  return (
    <div className="space-y-6">
      <PageHeader title="Batches" description="Archive packages (.tar.gz / .zip) appear here—not under Jobs—until each stage starts its own job." actions={<>
        <Link href="/jobs"><Button variant="secondary">View jobs</Button></Link>
        <Link href="/batches/new"><Button>Build package</Button></Link>
      </>} />
      {isLoading ? <LoadingState label="Loading batches" /> : null}
      {error ? <ErrorState error={error} retry={() => refetch()} /> : null}
      {data && data.items?.length === 0 ? (
        <EmptyState
          title="No batches yet"
          hint="Build one here — add a stage per file and draw the dependencies — or upload a .tar.gz / .zip with a root manifest.json to a watched prefix."
          action={<Link href="/batches/new"><Button>Build package</Button></Link>}
        />
      ) : null}
      {data?.items.length ? <div className="overflow-x-auto rounded-lg border border-slate-200 bg-white">
        <table className="min-w-[700px] w-full text-sm">
          <caption className="sr-only">Migration batches</caption>
          <thead className="bg-slate-50 text-slate-600">
            <tr>
              <th className="px-3 py-2 text-left">Batch</th>
              <th className="px-3 py-2 text-left">Key</th>
              <th className="px-3 py-2 text-left">Source</th>
              <th className="px-3 py-2 text-left">Status</th>
              <th className="px-3 py-2 text-left">Stages</th>
              <th className="px-3 py-2 text-left">Started</th>
            </tr>
          </thead>
          <tbody>
            {data?.items?.map((b, i) => (
              <tr key={b.id} className="border-t border-slate-100 hover:bg-slate-50">
                <td className="px-3 py-2">
                  <Link className="text-brand-700 hover:underline" href={`/batches/${b.id}`}>#{b.id}</Link>
                </td>
                <td className="px-3 py-2">{b.batch_key ?? "—"}</td>
                <td className="px-3 py-2 font-mono text-xs text-slate-600">
                  {b.source_ref?.key ?? "—"}
                </td>
                <td className="px-3 py-2"><StatusBadge status={b.status} /></td>
                <td className="px-3 py-2"><StageStrip stages={details[i]?.data?.stages} /></td>
                <td className="px-3 py-2 text-slate-500">{formatDateTime(b.started_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div> : null}
    </div>
  );
}

/** One glowing dot per stage plus a "2 of 3 completed" count. */
function StageStrip({ stages }: { stages?: StageSummary[] }) {
  if (!stages) return <span className="text-slate-400">…</span>;
  if (!stages.length) return <span className="text-slate-500">—</span>;
  const states = stages.map((s) => stageState(s.status, s.error_message));
  const label = Object.fromEntries(STAGE_STATES.map((s) => [s.state, s.label]));
  return (
    <span className="inline-flex items-center gap-1.5">
      {stages.map((s, i) => (
        <span key={s.stage_key} title={`${s.stage_key}: ${label[states[i]]}`} className={clsx("inline-flex", `stage-legend-${states[i]}`)}>
          <span className="stage-dot" aria-hidden />
        </span>
      ))}
      <span className="ml-1 text-xs text-slate-500">{states.filter((s) => s === "completed").length} of {stages.length} completed</span>
    </span>
  );
}
