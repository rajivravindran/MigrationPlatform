"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { clsx } from "clsx";
import { ChevronDown, ChevronLeft, ChevronRight, FileText } from "lucide-react";
import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { toast } from "sonner";

import { Chip, JsonView, Panel, Stat, StatRow, Tabs, Tag, relativeTime } from "@/components/ops";
import { Button, ErrorState, LoadingState, formatDateTime } from "@/components/ui";
import { apiDownload, apiFetch, eventStream } from "@/lib/api";
import { migrationIdentity, outcomeLabel, templateNameIndex } from "@/lib/identity";
import { formatBytes, sourceSummary, type JobSource } from "@/lib/job-source";

type Job = {
  id: number;
  status: "pending" | "running" | "paused" | "succeeded" | "failed" | "cancelled";
  totals_json: { processed?: number; failed?: number; unresolved?: number; total?: number };
  rule_template_id: number;
  rule_template_version?: number | null;
  started_at?: string;
  finished_at?: string;
  updated_at?: string;
  batch_id?: number | null;
  source?: JobSource;
  source_ref?: JobSource;
  results_ref?: { key?: string; failed_key?: string; rows?: number; failed_rows?: number } | null;
  last_error?: string | null;
  export_status?: "pending" | "succeeded" | "failed" | null;
  export_error?: string | null;
};

type JobRow = {
  row_index: number;
  status: "pending" | "running" | "succeeded" | "failed" | "unresolved" | "skipped";
  attempts: number;
  last_error?: string | null;
  row_json?: Record<string, unknown> | null;
  payload_json?: unknown;
  response_json?: unknown;
  updated_at?: string;
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

type Template = { id: number; name: string; version?: number };

const PAGE = 50;

const UNRESOLVED_CONFIRM =
  "The destination may already have applied this record: the last call's outcome is unknown and the destination " +
  "has no deduplication or lookup policy. Check the destination first. Send it again only if the record is NOT there.";

function confirmNotApplied(rowIndex: number) {
  return window.confirm(`Row ${rowIndex}: ${UNRESOLVED_CONFIRM}\n\nI checked and it was not applied. Retry now?`);
}

function rowTone(s: JobRow["status"]) {
  if (s === "succeeded") return "ok" as const;
  if (s === "failed") return "fail" as const;
  if (s === "unresolved") return "warn" as const;
  return "neutral" as const;
}

function rowStatusLabel(s: JobRow["status"]) {
  switch (s) {
    case "succeeded":
      return "Succeeded";
    case "failed":
      return "Failed";
    case "unresolved":
      return "Unresolved";
    case "running":
      return "In flight";
    case "pending":
      return "Queued";
    default:
      return s;
  }
}

/** HTTP status from a CallEndpoint error ("status 404: …" / "retryable 503: …"). */
function httpStatusFromError(error?: string | null): number | undefined {
  if (!error) return undefined;
  const match = error.match(/\b(?:status|retryable|HTTP)\s+(\d{3})\b/i);
  if (!match) return undefined;
  const code = Number(match[1]);
  return code >= 100 && code <= 599 ? code : undefined;
}

/** Plain-language failure summary and what to do next. */
function explainFailure(row: JobRow): { headline: string; action: string } | null {
  if (row.status === "unresolved") {
    return { headline: "The destination's answer is unknown for this record.", action: "Check the destination for the record before retrying. Retrying blindly could create a duplicate." };
  }
  if (row.status !== "failed") return null;
  const err = row.last_error ?? "";
  const http = httpStatusFromError(err);
  if (/render|template|missing field|\$from/i.test(err)) return { headline: "The payload could not be built from this record.", action: "Correct the source record or the mapping, then retry this row." };
  if (http === 400 || http === 422) return { headline: "The destination rejected the record as invalid.", action: "Correct the source value named in the response, then retry this row." };
  if (http === 401 || http === 403) return { headline: "The destination refused the credentials.", action: "Fix the destination secret, then retry failed rows." };
  if (http === 404) return { headline: "The destination path was not found.", action: "Check the template's URL and path parameters, then retry." };
  if (http === 409) return { headline: "The destination reports a conflict for this record.", action: "Look up the existing record before retrying; a retry may duplicate it." };
  if (http === 429) return { headline: "The destination throttled this call.", action: "Retry failed rows once the rate limit window has passed." };
  if (http && http >= 500) return { headline: "The destination returned a server error.", action: "Retry failed rows once the destination is healthy." };
  return { headline: "This record failed.", action: "Read the response, correct the source if needed, then retry this row." };
}

/** The first source column with a short value, used as the record's name in the list. */
function recordLabel(row: JobRow): { key: string; value: string } | null {
  const data = row.row_json;
  if (!data || typeof data !== "object") return null;
  const entries = Object.entries(data);
  const preferred = entries.find(([k]) => /^(name|full_?name|title|display_?name|email|id|external_?id|key)$/i.test(k));
  const pick = preferred ?? entries.find(([, v]) => typeof v === "string" && (v as string).length > 0 && (v as string).length <= 60);
  if (!pick) return null;
  const v = pick[1];
  return { key: pick[0], value: typeof v === "string" ? v : JSON.stringify(v) };
}

function durationLabel(start?: string, end?: string) {
  if (!start) return null;
  const a = new Date(start).getTime();
  const b = end ? new Date(end).getTime() : Date.now();
  if (!Number.isFinite(a) || !Number.isFinite(b) || b < a) return null;
  return (b - a) / 1000;
}

/* ----- Inline inspector ----- */

type InspectorTab = "request" | "response" | "source" | "attempts";

function Inspector({
  jobId,
  row,
  purpose,
  templateVersion,
  onRetry,
  onNextFailed,
  hasNextFailed
}: {
  jobId: string;
  row: JobRow;
  purpose: string;
  templateVersion?: number | null;
  onRetry: (opts: { fromStart?: boolean; confirmNotApplied?: boolean }) => void;
  onNextFailed: () => void;
  hasNextFailed: boolean;
}) {
  const [tab, setTab] = useState<InspectorTab>("request");
  const [stepIdx, setStepIdx] = useState<number | null>(null);
  const stepsQ = useQuery({
    queryKey: ["job-row-steps", jobId, row.row_index],
    queryFn: () => apiFetch<{ items: JobRowStep[] }>(`/jobs/${jobId}/rows/${row.row_index}/steps`)
  });
  const steps = stepsQ.data?.items ?? [];
  const multi = steps.length > 0;
  const current = multi ? steps[stepIdx ?? Math.max(0, steps.findIndex((s) => s.status !== "succeeded"))] ?? steps[steps.length - 1] : null;
  const explained = explainFailure(row);
  const idBase = `row-${row.row_index}`;
  const request = multi ? current?.request_json : row.payload_json;
  const response = multi ? current?.response_json : row.response_json;
  const url = multi ? current?.request_url : undefined;
  const http = multi ? current?.response_status : httpStatusFromError(row.last_error);
  const sent = row.status !== "pending";

  return (
    <div className="border-t border-slate-200 bg-slate-50/60 px-5 py-4" id={`${idBase}-inspector`}>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          {explained ? (
            <>
              <div className="text-[15px] text-slate-950">{explained.headline} <span className="text-slate-700">{explained.action}</span></div>
            </>
          ) : (
            <div className="text-[15px] text-slate-950">{row.status === "succeeded" ? "The destination accepted this record." : "This record has not been sent yet."}</div>
          )}
          <div className="mt-0.5 text-[13px] text-slate-500">
            Row {row.row_index} · {purpose}
            {templateVersion ? ` v${templateVersion}` : ""}
            {multi ? ` · Step ${(current?.step_index ?? 0) + 1} of ${steps.length}` : " · 1 step"}
          </div>
        </div>
        <div className="flex shrink-0 flex-wrap gap-2">
          {row.status === "failed" ? (
            <>
              <Button size="sm" onClick={() => onRetry({})}>Retry this row</Button>
              <Button
                size="sm"
                variant="secondary"
                title="Re-runs every step, including ones that already succeeded. Only safe if earlier calls are idempotent."
                onClick={() => window.confirm("Retry from the first step? Previously successful destination calls will run again and must be idempotent.") && onRetry({ fromStart: true })}
              >
                Retry from start
              </Button>
            </>
          ) : null}
          {row.status === "unresolved" ? (
            <Button size="sm" variant="secondary" title={UNRESOLVED_CONFIRM} onClick={() => confirmNotApplied(row.row_index) && onRetry({ confirmNotApplied: true })}>
              Not applied — retry
            </Button>
          ) : null}
          {hasNextFailed ? (
            <Button size="sm" variant="ghost" onClick={onNextFailed}>
              Next failed row <ChevronRight className="h-3.5 w-3.5" />
            </Button>
          ) : null}
        </div>
      </div>

      <div className="mt-4">
        <Tabs
          idBase={idBase}
          value={tab}
          onChange={setTab}
          tabs={[
            { id: "request", label: "Request" },
            { id: "response", label: "Response" },
            { id: "source", label: "Source" },
            { id: "attempts", label: "Attempts", count: row.attempts }
          ]}
        />
      </div>

      {multi && (tab === "request" || tab === "response") ? (
        <div className="mt-3 flex flex-wrap items-center gap-2 text-[13px] text-slate-500">
          <span>Step</span>
          <div className="flex gap-1" role="group" aria-label="Select API step">
            {steps.map((s, i) => (
              <button
                key={s.step_index}
                type="button"
                onClick={() => setStepIdx(i)}
                aria-pressed={current?.step_index === s.step_index}
                className={clsx(
                  "rounded px-2 py-0.5 ring-1",
                  current?.step_index === s.step_index ? "bg-slate-100 text-slate-950 ring-slate-300" : "text-slate-500 ring-transparent hover:text-slate-800"
                )}
              >
                {s.step_index + 1}. {s.step_name}
                <span className={clsx("ml-1.5", s.status === "succeeded" ? "text-emerald-600" : s.status === "failed" ? "text-rose-600" : s.status === "unresolved" ? "text-amber-600" : "text-slate-500")}>{s.status}</span>
              </button>
            ))}
          </div>
        </div>
      ) : null}

      <div className="mt-3" role="tabpanel" id={`${idBase}-panel-${tab}`} aria-labelledby={`${idBase}-tab-${tab}`}>
        {tab === "request" ? (
          <div>
            <div className="mb-2 flex flex-wrap items-center justify-between gap-2 text-[13px]">
              <div className="min-w-0 truncate text-slate-700">
                {url ? (
                  <>
                    <span className="font-mono text-xs font-semibold text-slate-900">{current?.step_name ? "" : ""}</span>
                    <span className="font-mono text-xs text-slate-900">{url}</span>
                  </>
                ) : (
                  <span className="text-slate-500">{sent ? "Destination URL is not recorded for single-step runs." : "Not dispatched."}</span>
                )}
              </div>
              <div className="text-slate-500">
                {multi && current ? `Step ${current.step_index + 1} · ${current.step_name} · Attempt ${current.attempts}` : `Attempt ${row.attempts}`}
              </div>
            </div>
            <JsonView value={sent ? request : null} empty={sent ? "Request was not captured for this record." : "No request was sent."} label="Request payload" />
            <p className="mt-2 text-xs text-slate-500">{sent ? "Actual sent payload · Authorization header redacted" : "Shown once the record is dispatched; previews are not evidence of dispatch."}</p>
          </div>
        ) : null}
        {tab === "response" ? (
          <div>
            <div className="mb-2 flex flex-wrap items-center gap-3 text-[13px]">
              {http ? (
                <Chip tone={http >= 200 && http < 300 ? "ok" : http >= 500 || http === 429 ? "warn" : "fail"} icon={false}>
                  HTTP {http}
                </Chip>
              ) : (
                <span className="text-slate-500">No HTTP status recorded</span>
              )}
              {(multi ? current?.last_error : row.last_error) ? (
                <span className="min-w-0 flex-1 break-words text-rose-700" data-testid="row-error">
                  {multi ? current?.last_error : row.last_error}
                </span>
              ) : null}
            </div>
            <JsonView value={sent ? response : null} empty={sent ? "No response body was recorded." : "No request was sent, so there is no response."} label="Response body" />
          </div>
        ) : null}
        {tab === "source" ? (
          <div className="grid gap-3 md:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
            <div>
              <div className="mb-2 text-[13px] text-slate-500">Original columns</div>
              {row.row_json && Object.keys(row.row_json).length ? (
                <dl className="divide-y divide-slate-200 rounded-md border border-slate-300 bg-slate-50">
                  {Object.entries(row.row_json).map(([k, v]) => (
                    <div key={k} className="grid grid-cols-[minmax(120px,0.4fr)_minmax(0,1fr)] gap-3 px-3 py-1.5 text-[13px]">
                      <dt className="truncate font-mono text-xs text-slate-500" title={k}>{k}</dt>
                      <dd className="min-w-0 break-words text-slate-900">{v === null || v === undefined ? <span className="text-slate-500">empty</span> : typeof v === "string" ? v : JSON.stringify(v)}</dd>
                    </div>
                  ))}
                </dl>
              ) : (
                <div className="rounded-md border border-dashed border-slate-300 px-4 py-6 text-center text-sm text-slate-500">The source record was not retained for this row.</div>
              )}
            </div>
            <div>
              <div className="mb-2 text-[13px] text-slate-500">Raw record as read</div>
              <JsonView value={row.row_json ?? null} empty="The source record was not retained for this row." label="Raw source record" />
            </div>
          </div>
        ) : null}
        {tab === "attempts" ? (
          <div>
            {stepsQ.isLoading ? <div className="text-sm text-slate-500">Loading attempts…</div> : null}
            <ol className="divide-y divide-slate-200 rounded-md border border-slate-300 bg-slate-50">
              {(multi ? steps : [null]).map((s, i) => (
                <li key={i} className="flex flex-wrap items-center gap-3 px-3 py-2 text-[13px]">
                  <span className="text-slate-900">{s ? `${s.step_index + 1}. ${s.step_name}` : "Destination call"}</span>
                  <Chip tone={rowTone((s ? (s.status as JobRow["status"]) : row.status))}>{s ? s.status : rowStatusLabel(row.status)}</Chip>
                  <span className="tnum text-slate-500">{s ? s.attempts : row.attempts} attempt{(s ? s.attempts : row.attempts) === 1 ? "" : "s"}</span>
                  {s?.response_status ? <span className="text-slate-500">HTTP {s.response_status}</span> : null}
                  <span className="ml-auto text-slate-500">{formatDateTime(s ? s.updated_at : row.updated_at)}</span>
                </li>
              ))}
            </ol>
            <p className="mt-2 text-xs text-slate-500">
              Only the latest attempt&apos;s request and response are retained per step; earlier attempts are counted, not stored. An input revision history will appear here once source corrections are supported.
            </p>
          </div>
        ) : null}
      </div>
    </div>
  );
}

/* ----- Page ----- */

export default function JobDetailPage({ params }: { params: { id: string } }) {
  const client = useQueryClient();
  const jobQ = useQuery({ queryKey: ["job", params.id], queryFn: () => apiFetch<Job>(`/jobs/${params.id}`), refetchInterval: 5000 });
  const rowsQ = useQuery({ queryKey: ["job-rows", params.id], queryFn: () => apiFetch<{ items: JobRow[] }>(`/jobs/${params.id}/rows?limit=2000`), refetchInterval: 10000 });
  const templatesQ = useQuery({ queryKey: ["templates"], queryFn: () => apiFetch<{ items: Template[] }>("/rule-templates"), staleTime: 60_000 });

  const [live, setLive] = useState<{ processed: number; failed: number } | null>(null);
  const [selectedRow, setSelectedRow] = useState<number | null>(null);
  const [filter, setFilter] = useState<"all" | "failed" | "unresolved" | "succeeded">("all");
  const [page, setPage] = useState(0);

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

  const invalidate = () => {
    client.invalidateQueries({ queryKey: ["job", params.id] });
    client.invalidateQueries({ queryKey: ["job-rows", params.id] });
  };
  const useControl = (path: string, done: string) =>
    useMutation({
      mutationFn: () => apiFetch(`/jobs/${params.id}/${path}`, { method: "POST" }),
      onSuccess: () => {
        toast.success(done);
        invalidate();
      },
      onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
    });
  const pause = useControl("pause", "Paused");
  const resume = useControl("resume", "Resumed");
  const cancel = useControl("cancel", "Cancelled");
  const retryAll = useControl("retry-failed", "Retrying failed rows");
  const retryExport = useControl("export", "Export restarted");
  const retryOne = useMutation({
    mutationFn: ({ rowIndex, fromStart, confirmNotApplied }: { rowIndex: number; fromStart?: boolean; confirmNotApplied?: boolean }) =>
      apiFetch(`/jobs/${params.id}/rows/${rowIndex}/retry`, {
        method: "POST",
        body: JSON.stringify({ from_start: fromStart ?? false, confirm_not_applied: confirmNotApplied ?? false })
      }),
    onSuccess: (_res, vars) => {
      toast.success(`Row ${vars.rowIndex} queued for retry${vars.fromStart ? " (from start)" : ""}`);
      client.invalidateQueries({ queryKey: ["job-rows", params.id] });
      client.invalidateQueries({ queryKey: ["job-row-steps", params.id, vars.rowIndex] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const items = useMemo(() => rowsQ.data?.items ?? [], [rowsQ.data]);
  const filtered = useMemo(() => (filter === "all" ? items : items.filter((r) => r.status === filter)), [items, filter]);
  const pages = Math.max(1, Math.ceil(filtered.length / PAGE));
  const pageRows = filtered.slice(page * PAGE, page * PAGE + PAGE);
  const names = useMemo(() => templateNameIndex(templatesQ.data?.items), [templatesQ.data]);

  if (jobQ.isLoading) return <LoadingState label="Loading migration" />;
  if (jobQ.error) return <ErrorState error={jobQ.error} retry={() => jobQ.refetch()} />;
  if (!jobQ.data) return null;
  const job = jobQ.data;
  const identity = migrationIdentity(job, names);
  const outcome = outcomeLabel(job.status, job.totals_json);
  const source = sourceSummary(job);

  const processed = live?.processed ?? job.totals_json.processed ?? 0;
  const unresolved = job.totals_json.unresolved ?? 0;
  const notSucceeded = live?.failed ?? (job.totals_json.failed ?? 0) + unresolved;
  const failed = Math.max(0, notSucceeded - unresolved);
  const total = job.totals_json.total ?? processed + notSucceeded;
  const secs = durationLabel(job.started_at, job.finished_at);
  const throughput = secs && secs > 0 && processed ? Math.round((processed / secs) * 10) / 10 : null;
  const active = ["pending", "running", "paused"].includes(job.status);
  const terminal = ["succeeded", "failed", "cancelled"].includes(job.status);
  const failedIdx = filtered.map((r, i) => ({ r, i })).filter(({ r }) => r.status === "failed" || r.status === "unresolved");
  const nextFailedAfter = (rowIndex: number) => failedIdx.find(({ r }) => r.row_index > rowIndex);
  const topFailure = items.find((r) => r.status === "failed" || r.status === "unresolved");
  const failureNote = topFailure ? explainFailure(topFailure)?.headline : undefined;

  const openRow = (r: JobRow, i: number) => {
    setPage(Math.floor(i / PAGE));
    setSelectedRow(r.row_index);
  };

  return (
    <div className="space-y-5">
      <nav aria-label="Breadcrumb" className="flex items-center gap-2 text-[13px] text-slate-500">
        {identity.batchId ? (
          <>
            <Link href={`/batches/${identity.batchId}`} className="hover:text-slate-800 hover:underline">{identity.packageName ?? `Package #${identity.batchId}`}</Link>
            <span>/</span>
          </>
        ) : null}
        <Tag tone="neutral">{identity.batchId ? "Package stage" : "Direct run"}</Tag>
      </nav>

      <Link href="/jobs" className="inline-flex items-center gap-1 text-sm text-brand-700 hover:underline">
        <ChevronLeft className="h-4 w-4" /> All migrations
      </Link>

      <header className="flex flex-col justify-between gap-4 sm:flex-row sm:items-start">
        <div className="min-w-0">
          <h1 className="text-[28px] leading-tight text-slate-950">
            {identity.purpose}
            {identity.purposeIsFallback ? <span className="ml-2 text-base text-slate-500">template name not recorded</span> : null}
          </h1>
          <div className="mt-1 text-[14px] text-slate-500">
            {identity.fileName ?? "Source file not recorded"}
            {identity.templateVersion ? <> · Template v{identity.templateVersion}</> : null}
          </div>
          <div className="mt-2 flex flex-wrap items-center gap-x-2 text-[13px] text-slate-500">
            {identity.packageName ? (
              <>
                <span className="text-slate-700">{identity.packageName}</span>
                <span>›</span>
              </>
            ) : null}
            <span className="text-slate-700">{identity.fileName ?? "—"}</span>
            <span>·</span>
            <span className="font-mono text-xs" data-testid="execution-id">Execution {identity.executionId}</span>
            <span>·</span>
            <span title={formatDateTime(job.started_at)}>{relativeTime(job.started_at)}</span>
          </div>
        </div>
        <div className="flex shrink-0 flex-col items-start gap-2 sm:items-end">
          <span data-testid="job-status" data-status={job.status} title={`Status: ${job.status}`}>
            <Chip tone={outcome.tone}>{outcome.label}</Chip>
          </span>
          <div className="flex flex-wrap gap-2">
            {active ? (
              <>
                <Button size="sm" variant="secondary" onClick={() => pause.mutate()} disabled={job.status !== "running"}>Pause</Button>
                <Button size="sm" variant="secondary" onClick={() => resume.mutate()} disabled={job.status !== "paused"}>Resume</Button>
                <Button size="sm" variant="danger" onClick={() => window.confirm("Cancel this migration? In-flight calls may finish; completed writes are not undone.") && cancel.mutate()}>
                  Cancel
                </Button>
              </>
            ) : null}
            {failed > 0 ? <Button size="sm" onClick={() => retryAll.mutate()}>Retry failed rows</Button> : null}
            {terminal ? (
              <>
                <Button size="sm" variant="secondary" onClick={() => apiDownload(`/jobs/${job.id}/results`, `migration-${job.id}-results.csv`).catch((err) => toast.error(String(err)))}>
                  Download results
                </Button>
                {notSucceeded > 0 ? (
                  <Button size="sm" variant="secondary" onClick={() => apiDownload(`/jobs/${job.id}/results?failed=true`, `migration-${job.id}-failed.csv`).catch((err) => toast.error(String(err)))}>
                    Download failures
                  </Button>
                ) : null}
                {job.export_status === "failed" ? (
                  <Button size="sm" variant="secondary" onClick={() => retryExport.mutate()}>Retry export</Button>
                ) : null}
              </>
            ) : null}
          </div>
        </div>
      </header>

      {job.last_error ? (
        <div className="rounded-lg border border-rose-300/60 bg-rose-50 px-5 py-4 text-sm" data-testid="job-error">
          <div className="text-rose-800">The migration stopped before every source record was read.</div>
          <div className="mt-1 whitespace-pre-wrap break-words font-mono text-xs text-rose-700">{job.last_error}</div>
          <div className="mt-1 text-rose-700">Retrying rows cannot cover records that were never read. Fix the cause, then start a new run.</div>
        </div>
      ) : null}
      {job.export_status === "failed" || job.export_status === "pending" ? (
        <div className="rounded-lg border border-amber-300/60 bg-amber-50 px-5 py-3 text-sm text-amber-800" data-testid="job-export-status">
          {job.export_status === "pending" ? "Exporting results…" : "The results export failed; the migration outcome is unaffected. Downloads still read from the database."}
          {job.export_error ? <div className="mt-1 font-mono text-xs">{job.export_error}</div> : null}
        </div>
      ) : null}

      <StatRow>
        <Stat label="Records" value={total.toLocaleString()} note={source.kind ? `${source.kind.toUpperCase()} source` : "Source rows read"} />
        <Stat label="Succeeded" value={processed.toLocaleString()} tone="ok" note="Destination accepted" />
        <Stat
          label={unresolved ? "Failed / unresolved" : "Failed"}
          value={unresolved ? `${failed.toLocaleString()} / ${unresolved.toLocaleString()}` : failed.toLocaleString()}
          tone={failed || unresolved ? "fail" : "neutral"}
          note={failureNote ?? (failed || unresolved ? "Open a row for the reason" : "No failures")}
        />
        <Stat label="Throughput" value={throughput ?? "—"} unit={throughput ? "/sec" : undefined} note={secs ? `${job.finished_at ? "This run" : "So far"}, ${Math.round(secs)} s` : "Not started"} />
      </StatRow>

      <Panel>
        <div className="flex flex-wrap items-center gap-3 border-b border-slate-200 px-5 py-3">
          <FileText className="h-4 w-4 text-slate-500" aria-hidden />
          <span className="text-[15px] text-slate-950">{identity.fileName ?? "Source"}</span>
          <span className="text-[13px] text-slate-500">
            {identity.purpose}
            {identity.templateVersion ? ` v${identity.templateVersion}` : ""}
          </span>
          {formatBytes(source.size) ? <span className="text-[13px] text-slate-500">· {formatBytes(source.size)}</span> : null}
          <div className="ml-auto flex items-center gap-1" role="group" aria-label="Filter rows">
            {(
              [
                ["all", "All"],
                ["failed", "Failed"],
                ["unresolved", "Unresolved"],
                ["succeeded", "Succeeded"]
              ] as const
            ).map(([f, label]) => (
              <button
                key={f}
                type="button"
                aria-pressed={filter === f}
                onClick={() => {
                  setFilter(f);
                  setPage(0);
                }}
                className={clsx("rounded-md px-2.5 py-1 text-[13px]", filter === f ? "bg-slate-100 text-slate-950 ring-1 ring-slate-300" : "text-slate-500 hover:text-slate-800")}
              >
                {label}
              </button>
            ))}
          </div>
        </div>

        <div className="flex items-center justify-between px-5 py-3">
          <h2 className="text-lg text-slate-950">Source records</h2>
          <span className="tnum text-[13px] text-slate-500">
            {filtered.length ? `Showing rows ${page * PAGE + 1}–${Math.min(filtered.length, (page + 1) * PAGE)} of ${filtered.length.toLocaleString()}` : "No rows"}
            {items.length >= 2000 ? " (first 2,000 loaded)" : ""}
          </span>
        </div>

        {rowsQ.isLoading ? <div className="px-5 pb-5 text-sm text-slate-500">Loading rows…</div> : null}
        {!rowsQ.isLoading && filtered.length === 0 ? (
          <div className="px-5 pb-6 text-sm text-slate-500">{items.length === 0 ? "No records have been read yet." : "No rows match this filter."}</div>
        ) : null}

        {filtered.length ? (
          <div className="border-t border-slate-200">
            <div className="grid grid-cols-[28px_minmax(0,1fr)_120px_90px_110px] items-center gap-3 px-5 py-2 text-[11.5px] font-semibold uppercase tracking-wide text-slate-500" aria-hidden>
              <span />
              <span>Record / source row</span>
              <span>Outcome</span>
              <span className="text-right">Attempts</span>
              <span className="text-right">Updated</span>
            </div>
            <ul>
              {pageRows.map((r) => {
                const open = selectedRow === r.row_index;
                const label = recordLabel(r);
                const next = nextFailedAfter(r.row_index);
                return (
                  <li key={r.row_index} className={clsx("border-t border-slate-200", open && "bg-slate-50")} data-testid={`row-${r.row_index}`}>
                    <button
                      type="button"
                      aria-expanded={open}
                      aria-controls={`row-${r.row_index}-inspector`}
                      onClick={() => setSelectedRow(open ? null : r.row_index)}
                      className={clsx("grid w-full grid-cols-[28px_minmax(0,1fr)_120px_90px_110px] items-center gap-3 px-5 py-3 text-left hover:bg-slate-50", open && "bg-slate-100/60")}
                    >
                      <ChevronDown className={clsx("h-4 w-4 text-slate-500 transition-transform", !open && "-rotate-90")} aria-hidden />
                      <span className="min-w-0">
                        <span className="block truncate text-[15px] text-slate-950">{label ? label.value : `Row ${r.row_index}`}</span>
                        <span className="block truncate text-[13px] text-slate-500">
                          {label ? <><span className="font-mono text-xs">{label.key}</span> · </> : null}Row {r.row_index}
                        </span>
                      </span>
                      <span><Chip tone={rowTone(r.status)}>{rowStatusLabel(r.status)}</Chip></span>
                      <span className="tnum text-right text-[13px] text-slate-600">{r.attempts}</span>
                      <span className="text-right text-[13px] text-slate-500">{relativeTime(r.updated_at)}</span>
                    </button>
                    {open ? (
                      <Inspector
                        jobId={params.id}
                        row={r}
                        purpose={identity.purpose}
                        templateVersion={identity.templateVersion}
                        onRetry={(opts) => retryOne.mutate({ rowIndex: r.row_index, ...opts })}
                        hasNextFailed={!!next}
                        onNextFailed={() => next && openRow(next.r, next.i)}
                      />
                    ) : null}
                  </li>
                );
              })}
            </ul>
            {pages > 1 ? (
              <div className="flex items-center justify-between border-t border-slate-200 px-5 py-3 text-[13px] text-slate-500">
                <Button size="sm" variant="ghost" disabled={page === 0} onClick={() => setPage((p) => p - 1)}>
                  <ChevronLeft className="h-4 w-4" /> Previous
                </Button>
                <span className="tnum">Page {page + 1} of {pages}</span>
                <Button size="sm" variant="ghost" disabled={page >= pages - 1} onClick={() => setPage((p) => p + 1)}>
                  Next <ChevronRight className="h-4 w-4" />
                </Button>
              </div>
            ) : null}
          </div>
        ) : null}
      </Panel>
    </div>
  );
}
