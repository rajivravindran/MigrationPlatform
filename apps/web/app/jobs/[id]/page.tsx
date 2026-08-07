"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useVirtualizer } from "@tanstack/react-virtual";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import { Badge, Button, Card } from "@/components/ui";
import { apiFetch, eventStream } from "@/lib/api";

type Job = {
  id: number;
  status: "pending" | "running" | "paused" | "succeeded" | "failed" | "cancelled";
  totals_json: { processed?: number; failed?: number; total?: number };
  rule_template_id: number;
  started_at?: string;
  finished_at?: string;
};

type JobRow = {
  row_index: number;
  status: "pending" | "running" | "succeeded" | "failed" | "skipped";
  attempts: number;
  last_error?: string;
};

type JobRowStep = {
  step_index: number;
  step_name: string;
  status: string;
  attempts: number;
  request_url?: string;
  request_json?: unknown;
  response_status?: number;
  response_json?: unknown;
  last_error?: string;
  updated_at: string;
};

function tone(s: string) {
  if (s === "succeeded") return "ok";
  if (s === "failed") return "fail";
  if (s === "paused") return "warn";
  return "neutral";
}

/** Per-step trail for one row of a multi-step (chained) template. */
function RowStepTrail({ jobId, rowIndex }: { jobId: string; rowIndex: number }) {
  const stepsQ = useQuery({
    queryKey: ["job-row-steps", jobId, rowIndex],
    queryFn: () => apiFetch<{ items: JobRowStep[] }>(`/jobs/${jobId}/rows/${rowIndex}/steps`)
  });

  if (stepsQ.isLoading) return <div className="p-3 text-sm text-slate-500">Loading step trail…</div>;
  const steps = stepsQ.data?.items ?? [];
  if (steps.length === 0) {
    return (
      <div className="p-3 text-sm text-slate-500">
        No per-step records — this row ran a single-call template.
      </div>
    );
  }
  return (
    <div className="space-y-2 p-3">
      {steps.map((s) => (
        <div key={s.step_index} className="rounded border border-slate-200">
          <div className="flex items-center gap-2 border-b border-slate-100 bg-slate-50 px-3 py-1.5 text-sm">
            <span className="font-medium">
              {s.step_index + 1}. {s.step_name}
            </span>
            <Badge tone={tone(s.status) as "ok" | "fail" | "warn" | "neutral"}>{s.status}</Badge>
            {s.response_status ? <span className="text-xs text-slate-500">HTTP {s.response_status}</span> : null}
            <span className="text-xs text-slate-500">attempts: {s.attempts}</span>
            {s.request_url ? (
              <span className="ml-auto truncate text-xs text-slate-400" title={s.request_url}>
                {s.request_url}
              </span>
            ) : null}
          </div>
          {s.last_error ? (
            <div className="border-b border-slate-100 px-3 py-1.5 text-xs text-rose-700">{s.last_error}</div>
          ) : null}
          <div className="grid grid-cols-1 gap-2 p-2 md:grid-cols-2">
            <div>
              <div className="mb-1 text-[11px] font-medium uppercase text-slate-400">Request</div>
              <pre className="max-h-40 overflow-auto rounded bg-slate-900 p-2 text-[11px] leading-4 text-slate-100">
                {JSON.stringify(s.request_json ?? null, null, 2)}
              </pre>
            </div>
            <div>
              <div className="mb-1 text-[11px] font-medium uppercase text-slate-400">Response</div>
              <pre className="max-h-40 overflow-auto rounded bg-slate-900 p-2 text-[11px] leading-4 text-slate-100">
                {JSON.stringify(s.response_json ?? null, null, 2)}
              </pre>
            </div>
          </div>
        </div>
      ))}
    </div>
  );
}

export default function JobDetailPage({ params }: { params: { id: string } }) {
  const client = useQueryClient();
  const jobQ = useQuery({
    queryKey: ["job", params.id],
    queryFn: () => apiFetch<Job>(`/jobs/${params.id}`),
    refetchInterval: 5000
  });
  const rowsQ = useQuery({
    queryKey: ["job-rows", params.id],
    queryFn: () => apiFetch<{ items: JobRow[] }>(`/jobs/${params.id}/rows?limit=2000`),
    refetchInterval: 10000
  });

  const [live, setLive] = useState<{ processed: number; failed: number } | null>(null);
  const [selectedRow, setSelectedRow] = useState<number | null>(null);
  useEffect(() => {
    const es = eventStream(`/jobs/${params.id}/stream`);
    es.onmessage = (evt) => {
      try {
        const data = JSON.parse(evt.data);
        setLive({ processed: data.processed ?? 0, failed: data.failed ?? 0 });
      } catch {}
    };
    es.onerror = () => es.close();
    return () => es.close();
  }, [params.id]);

  const parent = useRef<HTMLDivElement>(null);
  const items = rowsQ.data?.items ?? [];
  const virt = useVirtualizer({
    count: items.length,
    estimateSize: () => 32,
    getScrollElement: () => parent.current
  });

  const pause = useMutation({
    mutationFn: () => apiFetch(`/jobs/${params.id}/pause`, { method: "POST" }),
    onSuccess: () => { toast.success("Paused"); client.invalidateQueries({ queryKey: ["job", params.id] }); },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });
  const resume = useMutation({
    mutationFn: () => apiFetch(`/jobs/${params.id}/resume`, { method: "POST" }),
    onSuccess: () => { toast.success("Resumed"); client.invalidateQueries({ queryKey: ["job", params.id] }); },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });
  const cancel = useMutation({
    mutationFn: () => apiFetch(`/jobs/${params.id}/cancel`, { method: "POST" }),
    onSuccess: () => { toast.success("Cancelled"); client.invalidateQueries({ queryKey: ["job", params.id] }); },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });
  const retryAll = useMutation({
    mutationFn: () => apiFetch(`/jobs/${params.id}/retry-failed`, { method: "POST" }),
    onSuccess: () => { toast.success("Retrying failed rows"); client.invalidateQueries({ queryKey: ["job-rows", params.id] }); },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });
  const retryOne = useMutation({
    mutationFn: ({ rowIndex, fromStart }: { rowIndex: number; fromStart?: boolean }) =>
      apiFetch(`/jobs/${params.id}/rows/${rowIndex}/retry`, {
        method: "POST",
        body: JSON.stringify({ from_start: fromStart ?? false })
      }),
    onSuccess: (_res, vars) => {
      toast.success(`Row #${vars.rowIndex} queued for retry${vars.fromStart ? " (from start)" : ""}`);
      client.invalidateQueries({ queryKey: ["job-rows", params.id] });
      client.invalidateQueries({ queryKey: ["job-row-steps", params.id, vars.rowIndex] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  if (jobQ.isLoading) return <Card>Loading&hellip;</Card>;
  if (!jobQ.data) return null;
  const job = jobQ.data;

  const processed = live?.processed ?? job.totals_json.processed ?? 0;
  const failed = live?.failed ?? job.totals_json.failed ?? 0;
  const total = job.totals_json.total ?? processed + failed;
  const pct = total ? Math.min(100, Math.round(((processed + failed) / total) * 100)) : 0;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold">Job #{job.id}</h1>
          <p className="text-sm text-slate-500">Template #{job.rule_template_id} &middot; started {job.started_at ?? "—"}</p>
        </div>
        <div className="flex items-center gap-2">
          <span data-testid="job-status"><Badge tone={tone(job.status) as "ok" | "fail" | "warn" | "neutral"}>{job.status}</Badge></span>
          <Button variant="ghost" onClick={() => pause.mutate()} disabled={job.status !== "running"}>Pause</Button>
          <Button variant="ghost" onClick={() => resume.mutate()} disabled={job.status !== "paused"}>Resume</Button>
          <Button variant="danger" onClick={() => cancel.mutate()} disabled={!["running", "paused"].includes(job.status)}>Cancel</Button>
          <Button onClick={() => retryAll.mutate()} disabled={failed === 0}>Retry failed</Button>
        </div>
      </div>

      <Card>
        <div className="mb-2 flex items-center justify-between text-sm">
          <span>{processed} processed · {failed} failed · {total} total</span>
          <span>{pct}%</span>
        </div>
        <div className="h-2 w-full rounded bg-slate-200">
          <div className="h-2 rounded bg-brand-600" style={{ width: `${pct}%` }} />
        </div>
      </Card>

      <Card>
        <div className="mb-2 flex items-center justify-between">
          <h2 className="text-lg font-medium">Rows</h2>
          <span className="text-xs text-slate-500">
            {items.length.toLocaleString()} loaded · click a row for its step-by-step trail
          </span>
        </div>
        <div ref={parent} className="max-h-[420px] overflow-auto rounded border border-slate-200">
          <div style={{ height: virt.getTotalSize(), position: "relative" }}>
            {virt.getVirtualItems().map((vi) => {
              const r = items[vi.index];
              const selected = selectedRow === r.row_index;
              return (
                <div
                  key={vi.key}
                  className={
                    "flex cursor-pointer items-center gap-2 border-b border-slate-100 px-3 py-1 text-sm " +
                    (selected ? "bg-brand-50" : "hover:bg-slate-50")
                  }
                  style={{ position: "absolute", top: 0, left: 0, right: 0, transform: `translateY(${vi.start}px)`, height: vi.size }}
                  onClick={() => setSelectedRow(selected ? null : r.row_index)}
                >
                  <span className="w-16 text-slate-500">#{r.row_index}</span>
                  <Badge tone={tone(r.status) as "ok" | "fail" | "warn" | "neutral"}>{r.status}</Badge>
                  <span className="text-xs text-slate-500">attempts: {r.attempts}</span>
                  {r.last_error ? <span className="truncate text-xs text-rose-600" title={r.last_error}>· {r.last_error}</span> : null}
                  <div className="ml-auto flex gap-2">
                    {r.status === "failed" ? (
                      <Button
                        variant="ghost"
                        onClick={(e) => {
                          e.stopPropagation();
                          retryOne.mutate({ rowIndex: r.row_index });
                        }}
                      >
                        Retry
                      </Button>
                    ) : null}
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      </Card>

      {selectedRow !== null ? (
        <Card>
          <div className="mb-1 flex items-center justify-between">
            <h2 className="text-lg font-medium">Row #{selectedRow} — step trail</h2>
            <div className="flex gap-2">
              {items.find((r) => r.row_index === selectedRow)?.status === "failed" ? (
                <>
                  <Button variant="ghost" onClick={() => retryOne.mutate({ rowIndex: selectedRow })}>
                    Retry (resume at failed step)
                  </Button>
                  <Button
                    variant="ghost"
                    title="Re-runs every step, including ones that already succeeded. Only safe if earlier calls are idempotent."
                    onClick={() => retryOne.mutate({ rowIndex: selectedRow, fromStart: true })}
                  >
                    Retry from start
                  </Button>
                </>
              ) : null}
              <Button variant="ghost" onClick={() => setSelectedRow(null)}>Close</Button>
            </div>
          </div>
          <RowStepTrail jobId={params.id} rowIndex={selectedRow} />
        </Card>
      ) : null}
    </div>
  );
}
