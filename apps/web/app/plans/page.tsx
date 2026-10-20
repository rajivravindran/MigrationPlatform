"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";

import { Chip } from "@/components/ops";
import { Button, EmptyState, ErrorState, LoadingState, PageHeader, formatDateTime } from "@/components/ui";
import { apiFetch } from "@/lib/api";
import type { PlanSummary } from "@/lib/plans";

export default function PlansPage() {
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["plans"],
    queryFn: () => apiFetch<{ items: PlanSummary[] }>("/plans"),
    refetchInterval: 5000
  });
  return (
    <div className="space-y-6">
      <PageHeader
        title="Plans"
        description="A plan is the dependency tree for a migration, designed up front. Files run against its steps as they arrive, each held until what it depends on has completed."
        actions={<Link href="/plans/new"><Button>Design a plan</Button></Link>}
      />
      {isLoading ? <LoadingState label="Loading plans" /> : null}
      {error ? <ErrorState error={error} retry={() => refetch()} /> : null}
      {data && data.items.length === 0 ? (
        <EmptyState title="No plans yet" hint="Design the steps of a migration and what each waits for. Files can be run against it later." action={<Link href="/plans/new"><Button>Design a plan</Button></Link>} />
      ) : null}
      {data?.items.length ? (
        <ul className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {data.items.map((p) => {
            const pct = p.steps ? Math.round((p.steps_completed / p.steps) * 100) : 0;
            return (
              <li key={p.id}>
                <Link href={p.status === "draft" ? `/plans/${p.id}/edit` : `/plans/${p.id}`} className="block rounded-lg border border-slate-200 bg-white p-5 shadow-sm transition hover:border-brand-300">
                  <div className="flex items-start gap-2">
                    <div className="min-w-0 flex-1">
                      <div className="truncate text-base text-slate-950">{p.name}</div>
                      <div className="text-xs text-slate-500">Version {p.version} · updated {formatDateTime(p.updated_at)}</div>
                    </div>
                    <Chip tone={p.status === "published" ? "ok" : "warn"}>{p.status}</Chip>
                  </div>
                  <div className="mt-4 flex items-baseline justify-between text-sm">
                    <span className="text-slate-600">{p.steps_completed} of {p.steps} steps completed</span>
                    <span className="text-slate-500">{pct}%</span>
                  </div>
                  <div className="mt-1.5 h-1.5 overflow-hidden rounded bg-slate-200" aria-hidden>
                    <div className="h-full bg-emerald-500" style={{ width: `${pct}%` }} />
                  </div>
                  <div className="mt-3 flex flex-wrap gap-2 text-xs">
                    {p.runs_running ? <Chip tone="info">{p.runs_running} running</Chip> : null}
                    {p.runs_waiting ? <Chip tone="neutral">{p.runs_waiting} waiting {p.runs_waiting === 1 ? "file" : "files"}</Chip> : null}
                    {p.runs_failed ? <Chip tone="fail">{p.runs_failed} failed {p.runs_failed === 1 ? "run" : "runs"}</Chip> : null}
                  </div>
                </Link>
              </li>
            );
          })}
        </ul>
      ) : null}
    </div>
  );
}
