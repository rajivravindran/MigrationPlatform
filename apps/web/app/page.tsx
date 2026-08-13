"use client";

import { useQuery } from "@tanstack/react-query";
import { AlertTriangle, ArrowRight, Check, Circle, Play } from "lucide-react";
import Link from "next/link";

import { Button, Card, ErrorState, LoadingState, PageHeader, StatusBadge, formatDateTime } from "@/components/ui";
import { apiFetch } from "@/lib/api";

type Template = { id: number; name: string; published: boolean };
type Connector = { id: number; name: string };
type Schedule = { id: number; name: string; enabled: boolean; next_run_at?: string };
type Job = { id: number; status: string; started_at?: string; totals_json?: { processed?: number; failed?: number; total?: number } };

export default function HomePage() {
  const templates = useQuery({ queryKey: ["templates"], queryFn: () => apiFetch<{ items: Template[] }>("/rule-templates") });
  const connectors = useQuery({ queryKey: ["connectors"], queryFn: () => apiFetch<{ items: Connector[] }>("/connectors") });
  const schedules = useQuery({ queryKey: ["schedules"], queryFn: () => apiFetch<{ items: Schedule[] }>("/schedules") });
  const jobs = useQuery({ queryKey: ["jobs"], queryFn: () => apiFetch<{ items: Job[] }>("/jobs"), refetchInterval: 10_000 });

  const queries = [templates, connectors, schedules, jobs];
  if (queries.some((query) => query.isLoading)) return <LoadingState label="Loading operations overview" />;
  const firstError = queries.find((query) => query.error);
  if (firstError) return <ErrorState error={firstError.error} retry={() => queries.forEach((query) => query.refetch())} />;

  const published = templates.data?.items.filter((item) => item.published).length ?? 0;
  const connectorCount = connectors.data?.items.length ?? 0;
  const enabledSchedules = schedules.data?.items.filter((item) => item.enabled).length ?? 0;
  const activeJobs = jobs.data?.items.filter((item) => ["pending", "running", "paused"].includes(item.status)).length ?? 0;
  const recentJobs = (jobs.data?.items ?? []).slice(0, 5);
  const failedJobs = jobs.data?.items.filter((item) => item.status === "failed").length ?? 0;

  const journey = [
    { title: "Publish a template", description: "Define source fields and destination mappings.", href: "/templates/new", done: published > 0, action: published ? "Manage templates" : "Create template", completedHref: "/templates" },
    { title: "Configure a connector", description: "Connect the source that supplies migration data.", href: "/connectors", done: connectorCount > 0, action: connectorCount ? "Manage connectors" : "Add connector" },
    { title: "Schedule or run", description: "Automate a recurring run or start one with a file.", href: "/schedules/new", done: enabledSchedules + activeJobs > 0, action: "Create schedule" }
  ];

  return (
    <div className="space-y-6">
      <PageHeader
        title="Operations overview"
        description="Configure migration inputs, automate runs, and resolve failures from one place."
        actions={<Link href="/jobs/new"><Button><Play className="h-4 w-4" />Start a job</Button></Link>}
      />

      <section aria-label="Operational summary" className="grid grid-cols-2 border-y border-slate-200 bg-white sm:grid-cols-4">
        {[
          ["Published templates", published],
          ["Connectors", connectorCount],
          ["Active schedules", enabledSchedules],
          ["Jobs in progress", activeJobs]
        ].map(([label, value], index) => (
          <div key={label} className={`px-4 py-4 ${index ? "border-l border-slate-200" : ""}`}>
            <div className="text-2xl font-semibold tabular-nums text-slate-950">{value}</div>
            <div className="mt-1 text-xs text-slate-500">{label}</div>
          </div>
        ))}
      </section>

      <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(360px,0.8fr)]">
        <section aria-labelledby="journey-title">
          <div className="mb-3 flex items-center justify-between">
            <h2 id="journey-title" className="text-lg font-semibold">Set up your first migration</h2>
            <span className="text-sm text-slate-500">{journey.filter((item) => item.done).length} of 3 ready</span>
          </div>
          <Card className="!p-0">
            <ol>
              {journey.map((item, index) => (
                <li key={item.title} className="flex gap-3 border-b border-slate-100 p-4 last:border-0">
                  <span className={`mt-0.5 flex h-6 w-6 shrink-0 items-center justify-center rounded-full border text-xs font-semibold ${item.done ? "border-emerald-300 bg-emerald-50 text-emerald-700" : "border-slate-300 text-slate-500"}`}>
                    {item.done ? <Check className="h-3.5 w-3.5" /> : index + 1}
                  </span>
                  <div className="min-w-0 flex-1">
                    <div className="font-medium text-slate-900">{item.title}</div>
                    <p className="mt-0.5 text-sm text-slate-500">{item.description}</p>
                  </div>
                  <Link className="self-center text-sm font-medium text-brand-700 hover:underline" href={item.done && item.completedHref ? item.completedHref : item.href}>
                    {item.action}
                  </Link>
                </li>
              ))}
            </ol>
          </Card>
        </section>

        <section aria-labelledby="recent-jobs-title">
          <div className="mb-3 flex items-center justify-between">
            <h2 id="recent-jobs-title" className="text-lg font-semibold">Recent jobs</h2>
            <Link className="inline-flex items-center gap-1 text-sm font-medium text-brand-700 hover:underline" href="/jobs">
              View all <ArrowRight className="h-3.5 w-3.5" />
            </Link>
          </div>
          <Card className="!p-0">
            {failedJobs > 0 ? (
              <Link href="/jobs?status=failed" className="flex items-center gap-2 border-b border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-900">
                <AlertTriangle className="h-4 w-4" />
                {failedJobs} failed job{failedJobs === 1 ? "" : "s"} need attention
              </Link>
            ) : null}
            {recentJobs.length ? recentJobs.map((job) => (
              <Link key={job.id} href={`/jobs/${job.id}`} className="flex items-center justify-between gap-3 border-b border-slate-100 px-4 py-3 last:border-0 hover:bg-slate-50">
                <div className="min-w-0">
                  <div className="font-medium text-slate-900">Job #{job.id}</div>
                  <div className="text-xs text-slate-500">{formatDateTime(job.started_at)}</div>
                </div>
                <StatusBadge status={job.status} />
              </Link>
            )) : (
              <div className="flex items-center gap-2 p-5 text-sm text-slate-500"><Circle className="h-4 w-4" />No jobs have run yet.</div>
            )}
          </Card>
        </section>
      </div>
    </div>
  );
}
