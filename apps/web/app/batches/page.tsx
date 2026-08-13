"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";

import { Button, EmptyState, ErrorState, LoadingState, PageHeader, StatusBadge, formatDateTime } from "@/components/ui";
import { apiFetch } from "@/lib/api";

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

export default function BatchesPage() {
  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ["batches"],
    queryFn: () => apiFetch<{ items: Batch[] }>("/batches"),
    refetchInterval: 5000
  });

  return (
    <div className="space-y-6">
      <PageHeader title="Batches" description="Archive packages (.tar.gz / .zip) appear here—not under Jobs—until each stage starts its own job." actions={<Link href="/jobs"><Button variant="secondary">View jobs</Button></Link>} />
      {isLoading ? <LoadingState label="Loading batches" /> : null}
      {error ? <ErrorState error={error} retry={() => refetch()} /> : null}
      {data && data.items?.length === 0 ? (
        <EmptyState
          title="No batches yet"
          hint="Upload a .tar.gz or .zip with root manifest.json to a watched prefix, or POST /batches."
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
              <th className="px-3 py-2 text-left">Started</th>
            </tr>
          </thead>
          <tbody>
            {data?.items?.map((b) => (
              <tr key={b.id} className="border-t border-slate-100 hover:bg-slate-50">
                <td className="px-3 py-2">
                  <Link className="text-brand-700 hover:underline" href={`/batches/${b.id}`}>#{b.id}</Link>
                </td>
                <td className="px-3 py-2">{b.batch_key ?? "—"}</td>
                <td className="px-3 py-2 font-mono text-xs text-slate-600">
                  {b.source_ref?.key ?? "—"}
                </td>
                <td className="px-3 py-2"><StatusBadge status={b.status} /></td>
                <td className="px-3 py-2 text-slate-500">{formatDateTime(b.started_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div> : null}
    </div>
  );
}
