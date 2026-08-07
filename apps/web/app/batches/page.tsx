"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";

import { Badge, Button, Card, EmptyState } from "@/components/ui";
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

function tone(s: string) {
  switch (s) {
    case "succeeded": return "ok";
    case "failed":
    case "quarantined": return "fail";
    case "partial": return "warn";
    case "running": return "neutral";
    default: return "neutral";
  }
}

export default function BatchesPage() {
  const { data, isLoading } = useQuery({
    queryKey: ["batches"],
    queryFn: () => apiFetch<{ items: Batch[] }>("/batches"),
    refetchInterval: 5000
  });

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold">Batches</h1>
          <p className="text-sm text-slate-500">
            Archive packages with ordered stages (manifest.json inside tar.gz / zip).
          </p>
        </div>
        <Link href="/jobs"><Button variant="ghost">Jobs</Button></Link>
      </div>
      {isLoading ? <Card>Loading&hellip;</Card> : null}
      {data && data.items?.length === 0 ? (
        <EmptyState
          title="No batches yet"
          hint="Upload a .tar.gz or .zip with root manifest.json to a watched prefix, or POST /batches."
        />
      ) : null}
      <div className="overflow-hidden rounded-lg border border-slate-200 bg-white">
        <table className="w-full text-sm">
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
                <td className="px-3 py-2"><Badge tone={tone(b.status) as any}>{b.status}</Badge></td>
                <td className="px-3 py-2 text-slate-500">{b.started_at ?? "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
