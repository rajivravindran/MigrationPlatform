"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";
import { useEffect, useState } from "react";

import { Button, EmptyState, ErrorState, LoadingState, PageHeader, Select, StatusBadge, formatDateTime } from "@/components/ui";
import { apiFetch } from "@/lib/api";
import { sourceListLabel, type JobSource } from "@/lib/job-source";

type Job = {
  id: number;
  rule_template_id: number;
  status: "pending" | "running" | "paused" | "succeeded" | "failed" | "cancelled";
  totals_json: { processed?: number; failed?: number; total?: number };
  started_at?: string;
  finished_at?: string;
  batch_id?: number | null;
  source?: JobSource;
};

export default function JobsPage() {
  const [status, setStatus] = useState("all");
  useEffect(() => {
    setStatus(new URLSearchParams(window.location.search).get("status") ?? "all");
  }, []);
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["jobs"],
    queryFn: () => apiFetch<{ items: Job[] }>("/jobs"),
    refetchInterval: 5000
  });
  const items = (data?.items ?? []).filter((job) => status === "all" || job.status === status);

  return (
    <div className="space-y-6">
      <PageHeader title="Jobs" description="Monitor migration runs, inspect row-level failures, and retry recoverable work." actions={<Link href="/jobs/new"><Button>Start a job</Button></Link>} />
      {isLoading ? <LoadingState label="Loading jobs" /> : null}
      {error ? <ErrorState error={error} retry={() => refetch()} /> : null}
      {data && data.items?.length === 0 ? (
        <EmptyState
          title="No jobs yet"
          hint="Upload a source file and start a migration against a published template."
          action={<Link href="/jobs/new"><Button>Start a job</Button></Link>}
        />
      ) : null}
      {data?.items.length ? (
        <>
        <div className="flex items-center justify-between gap-3">
          <div className="text-sm text-slate-500">{items.length} of {data.items.length} jobs</div>
          <label className="flex items-center gap-2 text-sm text-slate-600">Status
            <Select className="!min-h-9 w-44" value={status} onChange={(event) => setStatus(event.target.value)}>
              <option value="all">All statuses</option><option value="running">Running</option><option value="paused">Paused</option><option value="succeeded">Succeeded</option><option value="failed">Failed</option><option value="cancelled">Cancelled</option>
            </Select>
          </label>
        </div>
      <div className="overflow-x-auto rounded-lg border border-slate-200 bg-white">
        <table className="min-w-[1100px] w-full text-sm">
          <caption className="sr-only">Migration jobs</caption>
          <thead className="bg-slate-50 text-slate-600">
            <tr>
              <th className="px-3 py-2 text-left">Job</th>
              <th className="px-3 py-2 text-left">Source</th>
              <th className="px-3 py-2 text-left">Template</th>
              <th className="px-3 py-2 text-left">Status</th>
              <th className="px-3 py-2 text-left">Progress</th>
              <th className="px-3 py-2 text-right">Processed</th>
              <th className="px-3 py-2 text-right">Failed</th>
              <th className="px-3 py-2 text-left">Started</th>
            </tr>
          </thead>
          <tbody>
            {items.map((j) => {
              const processed = j.totals_json?.processed ?? 0;
              const failed = j.totals_json?.failed ?? 0;
              const total = j.totals_json?.total;
              const pct = total ? Math.min(100, Math.round(((processed + failed) / total) * 100)) : 0;
              return (
              <tr key={j.id} className="border-t border-slate-100 hover:bg-slate-50">
                <td className="px-3 py-2">
                  <Link className="text-brand-700 hover:underline" href={`/jobs/${j.id}`}>#{j.id}</Link>
                  {j.batch_id ? (
                    <>
                      {" "}
                      <Link className="text-xs text-slate-500 hover:underline" href={`/batches/${j.batch_id}`}>
                        Batch #{j.batch_id}
                      </Link>
                    </>
                  ) : null}
                </td>
                <td className="max-w-[280px] truncate px-3 py-2 text-slate-600" title={sourceListLabel(j)}>
                  {sourceListLabel(j)}
                </td>
                <td className="px-3 py-2">#{j.rule_template_id}</td>
                <td className="px-3 py-2"><StatusBadge status={j.status} /></td>
                <td className="w-44 px-3 py-2"><div className="flex items-center gap-2"><div className="h-1.5 flex-1 rounded bg-slate-200"><div className={`h-1.5 rounded ${failed ? "bg-rose-500" : "bg-brand-600"}`} style={{ width: `${pct}%` }} /></div><span className="w-9 text-right text-xs tabular-nums text-slate-500">{total ? `${pct}%` : "—"}</span></div></td>
                <td className="px-3 py-2 text-right tabular-nums">{processed}</td>
                <td className={`px-3 py-2 text-right tabular-nums ${failed ? "font-medium text-rose-700" : ""}`}>{failed}</td>
                <td className="px-3 py-2 text-slate-500">{formatDateTime(j.started_at)}</td>
              </tr>
            )})}
          </tbody>
        </table>
      </div>
      {items.length === 0 ? <EmptyState title="No jobs match this filter" hint="Choose another status to see jobs." /> : null}
      </>
      ) : null}
    </div>
  );
}
