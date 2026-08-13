"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import Link from "next/link";
import { toast } from "sonner";

import { Button, EmptyState, ErrorState, LoadingState, PageHeader, StatusBadge, formatDateTime } from "@/components/ui";
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

export default function SchedulesPage() {
  const client = useQueryClient();
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["schedules"],
    queryFn: () => apiFetch<{ items: Schedule[] }>("/schedules"),
    refetchInterval: 10000
  });

  const pause = useMutation({
    mutationFn: (id: number) => apiFetch(`/schedules/${id}/pause`, { method: "POST" }),
    onSuccess: () => { toast.success("Schedule paused"); client.invalidateQueries({ queryKey: ["schedules"] }); }
  });
  const resume = useMutation({
    mutationFn: (id: number) => apiFetch(`/schedules/${id}/resume`, { method: "POST" }),
    onSuccess: () => { toast.success("Schedule resumed"); client.invalidateQueries({ queryKey: ["schedules"] }); }
  });
  const triggerNow = useMutation({
    mutationFn: (id: number) => apiFetch(`/schedules/${id}/trigger`, { method: "POST" }),
    onSuccess: () => toast.success("Triggered immediate run")
  });

  return (
    <div className="space-y-6">
      <PageHeader title="Schedules" description="Automate recurring migrations and control how missed or overlapping runs are handled." actions={<Link href="/schedules/new"><Button>New schedule</Button></Link>} />
      {isLoading ? <LoadingState label="Loading schedules" /> : null}
      {error ? <ErrorState error={error} retry={() => refetch()} /> : null}
      {data && data.items?.length === 0 ? (
        <EmptyState
          title="No schedules yet"
          hint="Create a schedule to run a template on a cron expression."
          action={<Link href="/schedules/new"><Button>New schedule</Button></Link>}
        />
      ) : null}
      {data?.items.length ? <div className="overflow-x-auto rounded-lg border border-slate-200 bg-white">
        <table className="min-w-[1000px] w-full text-sm">
          <caption className="sr-only">Recurring migration schedules</caption>
          <thead className="bg-slate-50 text-slate-600">
            <tr>
              <th className="px-3 py-2 text-left">Name</th>
              <th className="px-3 py-2 text-left">Cron</th>
              <th className="px-3 py-2 text-left">Timezone</th>
              <th className="px-3 py-2 text-left">Overlap</th>
              <th className="px-3 py-2 text-left">Enabled</th>
              <th className="px-3 py-2 text-left">Next run</th>
              <th className="px-3 py-2 text-left">Last run</th>
              <th className="px-3 py-2 text-right">Actions</th>
            </tr>
          </thead>
          <tbody>
            {data?.items?.map((s) => (
              <tr key={s.id} className="border-t border-slate-100 hover:bg-slate-50">
                <td className="px-3 py-2">
                  <Link className="text-brand-700 hover:underline" href={`/schedules/${s.id}`}>{s.name}</Link>
                </td>
                <td className="px-3 py-2 font-mono text-xs">{s.spec_json?.cron}</td>
                <td className="px-3 py-2">{s.timezone}</td>
                <td className="px-3 py-2">{s.overlap_policy}</td>
                <td className="px-3 py-2"><StatusBadge status={s.enabled ? "enabled" : "paused"} /></td>
                <td className="px-3 py-2 text-slate-500">{formatDateTime(s.next_run_at)}</td>
                <td className="px-3 py-2 text-slate-500">{formatDateTime(s.last_run_at)}</td>
                <td className="px-3 py-2 text-right">
                  <div className="inline-flex gap-2">
                    {s.enabled ? (
                      <Button variant="ghost" onClick={() => pause.mutate(s.id)}>Pause</Button>
                    ) : (
                      <Button variant="ghost" onClick={() => resume.mutate(s.id)}>Resume</Button>
                    )}
                    <Button size="sm" onClick={() => triggerNow.mutate(s.id)}>Run now</Button>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div> : null}
    </div>
  );
}
