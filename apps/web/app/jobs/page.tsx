"use client";

import { useQuery } from "@tanstack/react-query";
import { clsx } from "clsx";
import { Search, Upload } from "lucide-react";
import Link from "next/link";
import { useEffect, useMemo, useState } from "react";

import { Chip, Panel, PanelHeader, relativeTime } from "@/components/ops";
import { Button, ErrorState, LoadingState } from "@/components/ui";
import { apiFetch } from "@/lib/api";
import { migrationIdentity, outcomeLabel, searchText, templateNameIndex } from "@/lib/identity";
import type { JobSource } from "@/lib/job-source";

type Job = {
  id: number;
  rule_template_id: number;
  rule_template_version?: number | null;
  status: "pending" | "running" | "paused" | "succeeded" | "failed" | "cancelled";
  totals_json: { processed?: number; failed?: number; unresolved?: number; total?: number };
  started_at?: string;
  finished_at?: string;
  updated_at?: string;
  batch_id?: number | null;
  source?: JobSource;
  source_ref?: JobSource;
  last_error?: string | null;
};
type Template = { id: number; name: string };

const FILTERS = [
  ["all", "All"],
  ["running", "Running"],
  ["failed", "Needs attention"],
  ["paused", "Paused"],
  ["succeeded", "Reconciled"],
  ["cancelled", "Cancelled"]
] as const;
type Filter = (typeof FILTERS)[number][0];

export default function JobsPage() {
  const [status, setStatus] = useState<Filter>("all");
  const [q, setQ] = useState("");
  useEffect(() => {
    const s = new URLSearchParams(window.location.search).get("status") as Filter | null;
    if (s && FILTERS.some(([f]) => f === s)) setStatus(s);
  }, []);
  const jobs = useQuery({ queryKey: ["jobs"], queryFn: () => apiFetch<{ items: Job[] }>("/jobs?limit=200"), refetchInterval: 5000 });
  const templates = useQuery({ queryKey: ["templates"], queryFn: () => apiFetch<{ items: Template[] }>("/rule-templates"), staleTime: 60_000 });
  const names = useMemo(() => templateNameIndex(templates.data?.items), [templates.data]);

  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase();
    return (jobs.data?.items ?? [])
      .map((job) => ({ job, id: migrationIdentity(job, names), outcome: outcomeLabel(job.status, job.totals_json) }))
      .filter((r) => {
        if (status === "running") return ["pending", "running"].includes(r.job.status);
        if (status !== "all" && r.job.status !== status) return false;
        return !needle || searchText(r.id).includes(needle);
      });
  }, [jobs.data, names, status, q]);

  if (jobs.isLoading) return <LoadingState label="Loading migrations" />;
  if (jobs.error) return <ErrorState error={jobs.error} retry={() => jobs.refetch()} />;

  return (
    <div className="space-y-5">
      <div className="flex flex-col justify-between gap-4 sm:flex-row sm:items-end">
        <div>
          <h1 className="text-[28px] leading-tight text-slate-950">Migrations</h1>
          <p className="mt-1 text-[15px] text-slate-500">Every file-level execution, newest first. Open one to inspect records, retry and reconcile.</p>
        </div>
        <Link href="/jobs/new">
          <Button>
            <Upload className="h-4 w-4" /> Start a migration
          </Button>
        </Link>
      </div>

      <Panel>
        <PanelHeader>
          <div className="flex flex-wrap gap-1" role="group" aria-label="Filter by outcome">
            {FILTERS.map(([f, label]) => (
              <button
                key={f}
                type="button"
                aria-pressed={status === f}
                onClick={() => setStatus(f)}
                className={clsx("rounded-md px-2.5 py-1 text-sm", status === f ? "bg-slate-100 text-slate-950 ring-1 ring-slate-300" : "text-slate-500 hover:text-slate-800")}
              >
                {label}
              </button>
            ))}
          </div>
          <label className="ml-auto flex min-w-[240px] items-center gap-2 rounded-md bg-slate-50 px-2.5 py-1.5 text-sm ring-1 ring-slate-300 focus-within:ring-brand-400">
            <Search className="h-4 w-4 text-slate-500" aria-hidden />
            <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Name, archive, file or execution id" className="w-full bg-transparent text-slate-900 placeholder:text-slate-500 focus:outline-none" aria-label="Search migrations" />
          </label>
        </PanelHeader>
        {rows.length === 0 ? (
          <div className="px-5 py-10 text-center text-sm text-slate-500">
            {jobs.data?.items.length ? "No migration matches." : "No migrations yet. Start one from a published template or upload a package."}
          </div>
        ) : (
          <ul className="divide-y divide-slate-200">
            {rows.map(({ job, id, outcome }) => {
              const t = job.totals_json ?? {};
              const done = (t.processed ?? 0) + (t.failed ?? 0) + (t.unresolved ?? 0);
              const total = t.total ?? done;
              return (
                <li key={job.id}>
                  <Link href={`/jobs/${job.id}`} className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-4 gap-y-1 px-5 py-3.5 hover:bg-slate-50 sm:grid-cols-[minmax(0,1fr)_140px_150px_110px]">
                    <div className="min-w-0">
                      <div className="truncate text-[15px] text-slate-950">
                        {id.purpose}
                        {id.purposeIsFallback ? <span className="ml-2 text-xs text-slate-500">name not recorded</span> : null}
                      </div>
                      <div className="mt-0.5 truncate text-[13px] text-slate-500">
                        {id.packageName ? <>{id.packageName} › </> : null}
                        {id.fileName ?? "source not recorded"}
                        <span className="text-slate-400"> · Execution {id.executionId}</span>
                        {id.templateVersion ? <span className="text-slate-400"> · v{id.templateVersion}</span> : null}
                      </div>
                    </div>
                    <div className="row-start-1 sm:row-auto">
                      <Chip tone={outcome.tone}>{outcome.label}</Chip>
                    </div>
                    <div className="tnum self-center text-[13px] text-slate-600">
                      <span className="text-slate-950">{(t.processed ?? 0).toLocaleString()}</span> ok
                      {t.failed ? <span className="text-rose-700"> · {t.failed} failed</span> : null}
                      {t.unresolved ? <span className="text-amber-700"> · {t.unresolved} unresolved</span> : null}
                      {total ? <span className="text-slate-500"> / {total.toLocaleString()}</span> : null}
                    </div>
                    <div className="self-center text-[13px] text-slate-500">{relativeTime(job.updated_at ?? job.finished_at ?? job.started_at)}</div>
                  </Link>
                </li>
              );
            })}
          </ul>
        )}
      </Panel>
    </div>
  );
}
