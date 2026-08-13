"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";

import { Breadcrumbs, Card, EmptyState, ErrorState, LoadingState, PageHeader, StatusBadge, formatDateTime } from "@/components/ui";
import { apiFetch } from "@/lib/api";

type Schedule = {
  id: number;
  name: string;
  rule_template_id: number;
  connector_id: number;
  spec_json: { cron: string };
  timezone: string;
  overlap_policy: string;
  enabled: boolean;
  next_run_at?: string;
  last_run_at?: string;
};

type Run = {
  id: number;
  schedule_id: number;
  job_id?: number;
  scheduled_time: string;
  actual_start_time?: string;
  finished_at?: string;
  status: string;
};

export default function ScheduleDetail({ params }: { params: { id: string } }) {
  const sched = useQuery({
    queryKey: ["schedule", params.id],
    queryFn: () => apiFetch<Schedule>(`/schedules/${params.id}`)
  });
  const runs = useQuery({
    queryKey: ["schedule-runs", params.id],
    queryFn: () => apiFetch<{ items: Run[] }>(`/schedules/${params.id}/runs`),
    refetchInterval: 10000
  });

  if (sched.isLoading) return <LoadingState label="Loading schedule" />;
  if (sched.error) return <ErrorState error={sched.error} retry={() => sched.refetch()} />;
  if (!sched.data) return null;
  const s = sched.data;

  return (
    <div className="space-y-4">
      <PageHeader title={s.name} eyebrow={<Breadcrumbs items={[{ label: "Schedules", href: "/schedules" }, { label: s.name }]} />} description={`Recurring migration using template #${s.rule_template_id} and connector #${s.connector_id}.`} actions={<StatusBadge status={s.enabled ? "enabled" : "paused"} />} />
      <Card>
        <dl className="grid gap-4 text-sm sm:grid-cols-2 lg:grid-cols-4">
          <div><dt className="text-xs text-slate-500">Cron</dt><dd className="mt-1 font-mono">{s.spec_json?.cron}</dd></div>
          <div><dt className="text-xs text-slate-500">Timezone</dt><dd className="mt-1">{s.timezone}</dd></div>
          <div><dt className="text-xs text-slate-500">Overlap policy</dt><dd className="mt-1 capitalize">{s.overlap_policy.replaceAll("_", " ")}</dd></div>
          <div><dt className="text-xs text-slate-500">Next run</dt><dd className="mt-1">{formatDateTime(s.next_run_at)}</dd></div>
        </dl>
      </Card>
      <Card>
        <h2 className="mb-2 text-lg font-medium">Recent runs</h2>
        {runs.error ? <ErrorState title="Couldn’t load recent runs" error={runs.error} retry={() => runs.refetch()} /> : null}
        {runs.data?.items.length === 0 ? <EmptyState title="No runs yet" hint="The first run will appear here after the schedule fires or is triggered manually." /> : null}
        {runs.data?.items.length ? <div className="overflow-x-auto rounded border border-slate-200">
          <table className="min-w-[760px] w-full text-sm">
            <caption className="sr-only">Recent schedule runs</caption>
            <thead className="bg-slate-50 text-slate-600">
              <tr>
                <th className="px-3 py-2 text-left">Run</th>
                <th className="px-3 py-2 text-left">Job</th>
                <th className="px-3 py-2 text-left">Scheduled</th>
                <th className="px-3 py-2 text-left">Started</th>
                <th className="px-3 py-2 text-left">Finished</th>
                <th className="px-3 py-2 text-left">Status</th>
              </tr>
            </thead>
            <tbody>
              {runs.data?.items?.map((r) => (
                <tr key={r.id} className="border-t border-slate-100">
                  <td className="px-3 py-2">#{r.id}</td>
                  <td className="px-3 py-2">{r.job_id ? <Link className="text-brand-700 hover:underline" href={`/jobs/${r.job_id}`}>#{r.job_id}</Link> : "—"}</td>
                  <td className="px-3 py-2 text-slate-500">{formatDateTime(r.scheduled_time)}</td>
                  <td className="px-3 py-2 text-slate-500">{formatDateTime(r.actual_start_time)}</td>
                  <td className="px-3 py-2 text-slate-500">{formatDateTime(r.finished_at)}</td>
                  <td className="px-3 py-2"><StatusBadge status={r.status} /></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div> : null}
      </Card>
    </div>
  );
}
