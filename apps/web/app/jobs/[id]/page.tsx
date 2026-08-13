"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useVirtualizer } from "@tanstack/react-virtual";
import { useEffect, useRef, useState } from "react";
import { toast } from "sonner";

import Link from "next/link";

import { Badge, Breadcrumbs, Button, Card, ErrorState, LoadingState, PageHeader, StatusBadge, formatDateTime } from "@/components/ui";
import { apiDownload, apiFetch, eventStream } from "@/lib/api";
import { formatBytes, sourceSummary, type JobSource } from "@/lib/job-source";

type Job = {
  id: number;
  status: "pending" | "running" | "paused" | "succeeded" | "failed" | "cancelled";
  totals_json: { processed?: number; failed?: number; total?: number };
  rule_template_id: number;
  started_at?: string;
  finished_at?: string;
  batch_id?: number | null;
  source?: JobSource;
  source_ref?: JobSource;
  results_ref?: { key?: string; failed_key?: string; rows?: number; failed_rows?: number } | null;
};

type JobRow = {
  row_index: number;
  status: "pending" | "running" | "succeeded" | "failed" | "skipped";
  attempts: number;
  last_error?: string | null;
  payload_json?: unknown;
  response_json?: unknown;
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

/** HTTP status from CallEndpoint last_error ("status 404: …" / "retryable 503: …"). */
function httpStatusFromError(error?: string | null): number | undefined {
  if (!error) return undefined;
  const match = error.match(/\b(?:status|retryable|HTTP)\s+(\d{3})\b/i);
  if (!match) return undefined;
  const code = Number(match[1]);
  return code >= 100 && code <= 599 ? code : undefined;
}

function JsonPanel({ label, value }: { label: string; value: unknown }) {
  return (
    <div>
      <div className="mb-1 text-[11px] font-medium uppercase text-slate-400">{label}</div>
      <pre className="max-h-40 overflow-auto rounded bg-slate-900 p-2 text-[11px] leading-4 text-slate-100">
        {JSON.stringify(value ?? null, null, 2)}
      </pre>
    </div>
  );
}

function CallPanels({
  request,
  response,
  error
}: {
  request: unknown;
  response: unknown;
  error?: string | null;
}) {
  return (
    <>
      {error ? (
        <div className="whitespace-pre-wrap break-words border-b border-slate-100 px-3 py-1.5 text-xs text-rose-700" data-testid="row-error">
          {error}
        </div>
      ) : null}
      <div className="grid grid-cols-1 gap-2 p-2 md:grid-cols-2">
        <JsonPanel label="Request" value={request} />
        <JsonPanel label="Response" value={response} />
      </div>
    </>
  );
}

function SingleCallDetail({ row }: { row: JobRow }) {
  const http = httpStatusFromError(row.last_error);
  const pending = row.status === "pending" || row.status === "running";
  const empty = row.payload_json == null && row.response_json == null && !row.last_error;
  if (pending && empty) {
    return <div className="p-3 text-sm text-slate-500">This row has not been sent yet.</div>;
  }
  return (
    <div className="rounded border border-slate-200" data-testid="single-call-detail">
      <div className="flex items-center gap-2 border-b border-slate-100 bg-slate-50 px-3 py-1.5 text-sm">
        <span className="font-medium">Destination call</span>
        <Badge tone={tone(row.status) as "ok" | "fail" | "warn" | "neutral"}>{row.status}</Badge>
        {http ? <span className="text-xs text-slate-500" data-testid="row-http-status">HTTP {http}</span> : null}
        <span className="text-xs text-slate-500">attempts: {row.attempts}</span>
      </div>
      <CallPanels request={row.payload_json} response={row.response_json} error={row.last_error} />
    </div>
  );
}

/** Multi-step trail, or the single-call request/response already on the row. */
function RowOutcome({ jobId, row }: { jobId: string; row: JobRow }) {
  const stepsQ = useQuery({
    queryKey: ["job-row-steps", jobId, row.row_index],
    queryFn: () => apiFetch<{ items: JobRowStep[] }>(`/jobs/${jobId}/rows/${row.row_index}/steps`)
  });

  if (stepsQ.isLoading) return <div className="p-3 text-sm text-slate-500">Loading request / response…</div>;
  const steps = stepsQ.data?.items ?? [];
  if (steps.length === 0) {
    return (
      <div className="p-3">
        <SingleCallDetail row={row} />
      </div>
    );
  }
  return (
    <div className="space-y-2 p-3" data-testid="step-trail">
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
          <CallPanels request={s.request_json} response={s.response_json} error={s.last_error} />
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

  if (jobQ.isLoading) return <LoadingState label="Loading job" />;
  if (jobQ.error) return <ErrorState error={jobQ.error} retry={() => jobQ.refetch()} />;
  if (!jobQ.data) return null;
  const job = jobQ.data;

  const processed = live?.processed ?? job.totals_json.processed ?? 0;
  const failed = live?.failed ?? job.totals_json.failed ?? 0;
  const total = job.totals_json.total ?? processed + failed;
  const pct = total ? Math.min(100, Math.round(((processed + failed) / total) * 100)) : 0;
  const selected = selectedRow !== null ? items.find((r) => r.row_index === selectedRow) : undefined;
  const source = sourceSummary(job);

  return (
    <div className="space-y-4">
      <PageHeader
        title={`Job #${job.id}`}
        description={`Template #${job.rule_template_id} · Started ${formatDateTime(job.started_at)}`}
        eyebrow={<Breadcrumbs items={[{ label: "Jobs", href: "/jobs" }, { label: `Job #${job.id}` }]} />}
        actions={<>}
          <span data-testid="job-status"><StatusBadge status={job.status} /></span>
          <Button variant="ghost" onClick={() => pause.mutate()} disabled={job.status !== "running"}>Pause</Button>
          <Button variant="ghost" onClick={() => resume.mutate()} disabled={job.status !== "paused"}>Resume</Button>
          <Button variant="danger" onClick={() => window.confirm("Cancel this job? In-flight work may finish, but no new rows will start.") && cancel.mutate()} disabled={!["running", "paused"].includes(job.status)}>Cancel job</Button>
          <Button onClick={() => retryAll.mutate()} disabled={failed === 0}>Retry failed</Button>
          {["succeeded", "failed", "cancelled"].includes(job.status) ? (
            <>
              <Button
                variant="secondary"
                onClick={() => {
                  apiDownload(`/jobs/${job.id}/results`, `job-${job.id}-results.csv`).catch((err) =>
                    toast.error(err instanceof Error ? err.message : String(err))
                  );
                }}
              >
                Download results
              </Button>
              {failed > 0 ? (
                <Button
                  variant="secondary"
                  onClick={() => {
                    apiDownload(`/jobs/${job.id}/results?failed=true`, `job-${job.id}-results-failed.csv`).catch((err) =>
                      toast.error(err instanceof Error ? err.message : String(err))
                    );
                  }}
                >
                  Download failures
                </Button>
              ) : null}
            </>
          ) : null}
        </>}
      />

      <Card className="text-sm" data-testid="job-source">
        <dl className="grid gap-2 sm:grid-cols-2">
          {source.batch_id ? (
            <>
              <div>
                <dt className="text-xs uppercase tracking-wide text-slate-400">Package</dt>
                <dd className="font-medium text-slate-800">
                  {source.package_filename || source.package_key || "—"}
                </dd>
              </div>
              <div>
                <dt className="text-xs uppercase tracking-wide text-slate-400">Stage file</dt>
                <dd className="font-medium text-slate-800">
                  {source.batch_stage_file || source.filename || source.key || "—"}
                  {source.batch_stage_key ? (
                    <span className="ml-2 text-xs font-normal text-slate-500">key {source.batch_stage_key}</span>
                  ) : null}
                </dd>
              </div>
              <div>
                <dt className="text-xs uppercase tracking-wide text-slate-400">Batch</dt>
                <dd>
                  <Link className="text-brand-700 hover:underline" href={`/batches/${source.batch_id}`}>
                    Batch #{source.batch_id}
                  </Link>
                </dd>
              </div>
            </>
          ) : (
            <div>
              <dt className="text-xs uppercase tracking-wide text-slate-400">Source</dt>
              <dd className="font-medium text-slate-800">{source.filename || source.key || "—"}</dd>
            </div>
          )}
          {source.bucket && source.key ? (
            <div className="sm:col-span-2">
              <dt className="text-xs uppercase tracking-wide text-slate-400">Object</dt>
              <dd className="truncate font-mono text-xs text-slate-600" title={`${source.bucket}/${source.key}`}>
                {source.bucket}/{source.key}
              </dd>
            </div>
          ) : null}
          <div className="flex flex-wrap gap-4 text-slate-500 sm:col-span-2">
            {source.kind ? <span>type {source.kind}</span> : null}
            {formatBytes(source.size) ? <span>{formatBytes(source.size)}</span> : null}
            {source.etag ? <span title={source.etag}>etag {source.etag}</span> : null}
          </div>
        </dl>
      </Card>

      <Card>
        <div className="mb-2 flex items-center justify-between text-sm">
          <span>{processed} processed · {failed} failed · {total} total</span>
          <span>{pct}%</span>
        </div>
        <div className="h-2 w-full rounded bg-slate-200" role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct} aria-label="Job progress">
          <div className={`h-2 rounded ${failed ? "bg-rose-500" : "bg-brand-600"}`} style={{ width: `${pct}%` }} />
        </div>
      </Card>

      <Card>
        <div className="mb-2 flex items-center justify-between">
          <h2 className="text-lg font-medium">Rows</h2>
          <span className="text-xs text-slate-500">
            {items.length.toLocaleString()} loaded · click a row for request / response
          </span>
        </div>
        <div ref={parent} className="max-h-[420px] overflow-auto rounded border border-slate-200">
          <div style={{ height: virt.getTotalSize(), position: "relative" }}>
            {virt.getVirtualItems().map((vi) => {
              const r = items[vi.index];
              const selected = selectedRow === r.row_index;
              const http = httpStatusFromError(r.last_error);
              return (
                <div
                  key={vi.key}
                  role="button"
                  tabIndex={0}
                  aria-expanded={selected}
                  className={
                    "flex cursor-pointer items-center gap-2 border-b border-slate-100 px-3 py-1 text-sm " +
                    (selected ? "bg-brand-50" : "hover:bg-slate-50")
                  }
                  style={{ position: "absolute", top: 0, left: 0, right: 0, transform: `translateY(${vi.start}px)`, height: vi.size }}
                  onClick={() => setSelectedRow(selected ? null : r.row_index)}
                  onKeyDown={(event) => {
                    if (event.key === "Enter" || event.key === " ") {
                      event.preventDefault();
                      setSelectedRow(selected ? null : r.row_index);
                    }
                  }}
                >
                  <span className="w-16 text-slate-500">#{r.row_index}</span>
                  <Badge tone={tone(r.status) as "ok" | "fail" | "warn" | "neutral"}>{r.status}</Badge>
                  <span className="text-xs text-slate-500">attempts: {r.attempts}</span>
                  {http ? <span className="text-xs text-slate-500">HTTP {http}</span> : null}
                  {r.last_error ? (
                    <span className="min-w-0 flex-1 truncate text-xs text-rose-600" title={r.last_error}>
                      · {r.last_error}
                    </span>
                  ) : null}
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
            <h2 className="text-lg font-medium">Row #{selectedRow} — request / response</h2>
            <div className="flex gap-2">
              {selected?.status === "failed" ? (
                <>
                  <Button variant="ghost" onClick={() => retryOne.mutate({ rowIndex: selectedRow })}>
                    Retry (resume at failed step)
                  </Button>
                  <Button
                    variant="ghost"
                    title="Re-runs every step, including ones that already succeeded. Only safe if earlier calls are idempotent."
                    onClick={() => window.confirm("Retry from the first step? Previously successful destination calls will run again and must be idempotent.") && retryOne.mutate({ rowIndex: selectedRow, fromStart: true })}
                  >
                    Retry from start
                  </Button>
                </>
              ) : null}
              <Button variant="ghost" onClick={() => setSelectedRow(null)}>Close</Button>
            </div>
          </div>
          {selected ? (
            <RowOutcome jobId={params.id} row={selected} />
          ) : (
            <div className="p-3 text-sm text-slate-500">Row not in the loaded page.</div>
          )}
        </Card>
      ) : null}
    </div>
  );
}
