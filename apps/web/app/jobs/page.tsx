"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";

import { Badge, Button, Card, EmptyState } from "@/components/ui";
import { apiFetch } from "@/lib/api";

type Job = {
  id: number;
  rule_template_id: number;
  status: "pending" | "running" | "paused" | "succeeded" | "failed" | "cancelled";
  totals_json: { processed?: number; failed?: number; total?: number };
  started_at?: string;
  finished_at?: string;
};

function tone(s: Job["status"]) {
  switch (s) {
    case "succeeded": return "ok";
    case "failed": return "fail";
    case "paused": return "warn";
    case "cancelled": return "warn";
    case "running": return "neutral";
    default: return "neutral";
  }
}

export default function JobsPage() {
  const { data, isLoading } = useQuery({
    queryKey: ["jobs"],
    queryFn: () => apiFetch<{ items: Job[] }>("/jobs"),
    refetchInterval: 5000
  });

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold">Jobs</h1>
          <p className="text-sm text-slate-500">Start, monitor, pause, resume, and retry migrations.</p>
        </div>
        <Link href="/jobs/new"><Button>Start a job</Button></Link>
      </div>
      {isLoading ? <Card>Loading&hellip;</Card> : null}
      {data && data.items?.length === 0 ? (
        <EmptyState
          title="No jobs yet"
          hint="Upload a source file and start a migration against a published template."
          action={<Link href="/jobs/new"><Button>Start a job</Button></Link>}
        />
      ) : null}
      <div className="overflow-hidden rounded-lg border border-slate-200 bg-white">
        <table className="w-full text-sm">
          <thead className="bg-slate-50 text-slate-600">
            <tr>
              <th className="px-3 py-2 text-left">Job</th>
              <th className="px-3 py-2 text-left">Template</th>
              <th className="px-3 py-2 text-left">Status</th>
              <th className="px-3 py-2 text-right">Processed</th>
              <th className="px-3 py-2 text-right">Failed</th>
              <th className="px-3 py-2 text-right">Total</th>
              <th className="px-3 py-2 text-left">Started</th>
            </tr>
          </thead>
          <tbody>
            {data?.items?.map((j) => (
              <tr key={j.id} className="border-t border-slate-100 hover:bg-slate-50">
                <td className="px-3 py-2">
                  <Link className="text-brand-700 hover:underline" href={`/jobs/${j.id}`}>#{j.id}</Link>
                </td>
                <td className="px-3 py-2">#{j.rule_template_id}</td>
                <td className="px-3 py-2"><Badge tone={tone(j.status) as any}>{j.status}</Badge></td>
                <td className="px-3 py-2 text-right">{j.totals_json?.processed ?? 0}</td>
                <td className="px-3 py-2 text-right">{j.totals_json?.failed ?? 0}</td>
                <td className="px-3 py-2 text-right">{j.totals_json?.total ?? "—"}</td>
                <td className="px-3 py-2 text-slate-500">{j.started_at ?? "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
