"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";

import { Badge, Card } from "@/components/ui";
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

  if (sched.isLoading) return <Card>Loading&hellip;</Card>;
  if (!sched.data) return null;
  const s = sched.data;

  return (
    <div className="space-y-4">
      <h1 className="text-2xl font-semibold">{s.name}</h1>
      <Card>
        <div className="grid grid-cols-2 gap-3 text-sm">
          <div><span className="text-slate-500">Cron:</span> <code>{s.spec_json?.cron}</code></div>
          <div><span className="text-slate-500">Timezone:</span> {s.timezone}</div>
          <div><span className="text-slate-500">Overlap:</span> {s.overlap_policy}</div>
          <div><span className="text-slate-500">Enabled:</span> <Badge tone={s.enabled ? "ok" : "neutral"}>{s.enabled ? "enabled" : "paused"}</Badge></div>
          <div><span className="text-slate-500">Template:</span> #{s.rule_template_id}</div>
          <div><span className="text-slate-500">Connector:</span> #{s.connector_id}</div>
          <div><span className="text-slate-500">Last run:</span> {s.last_run_at ?? "—"}</div>
          <div><span className="text-slate-500">Next run:</span> {s.next_run_at ?? "—"}</div>
        </div>
      </Card>
      <Card>
        <h2 className="mb-2 text-lg font-medium">Recent runs</h2>
        <div className="overflow-hidden rounded border border-slate-200">
          <table className="w-full text-sm">
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
                  <td className="px-3 py-2 text-slate-500">{r.scheduled_time}</td>
                  <td className="px-3 py-2 text-slate-500">{r.actual_start_time ?? "—"}</td>
                  <td className="px-3 py-2 text-slate-500">{r.finished_at ?? "—"}</td>
                  <td className="px-3 py-2"><Badge tone={r.status === "succeeded" ? "ok" : r.status === "failed" ? "fail" : "neutral"}>{r.status}</Badge></td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Card>
    </div>
  );
}
