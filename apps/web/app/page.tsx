"use client";

import { useQuery } from "@tanstack/react-query";
import { clsx } from "clsx";
import { ArrowRight, Search, Upload } from "lucide-react";
import Link from "next/link";
import { useMemo, useState } from "react";

import { Chip, Panel, PanelHeader, Stat, StatRow, relativeTime } from "@/components/ops";
import { Button, ErrorState, LoadingState } from "@/components/ui";
import { apiFetch } from "@/lib/api";
import { migrationIdentity, outcomeLabel, searchText, templateNameIndex, type MigrationIdentity } from "@/lib/identity";
import type { JobSource } from "@/lib/job-source";
import { WORKSPACES, useWorkspace, type Workspace } from "@/lib/workspace";

type Template = { id: number; name: string; published: boolean; version?: number; updated_at?: string };
type Connector = { id: number; name: string; kind?: string };
type Schedule = { id: number; name: string; enabled: boolean; next_run_at?: string; last_run_at?: string };
type Job = {
  id: number;
  rule_template_id: number;
  rule_template_version?: number | null;
  status: "pending" | "running" | "paused" | "succeeded" | "failed" | "cancelled";
  totals_json: { processed?: number; failed?: number; unresolved?: number; total?: number };
  started_at?: string;
  finished_at?: string;
  updated_at?: string;
  batch_id?: number | null;
  source?: JobSource;
  source_ref?: JobSource;
  last_error?: string | null;
};
type Batch = {
  id: number;
  batch_key?: string | null;
  status: string;
  source_ref?: { key?: string; bucket?: string };
  started_at?: string;
  finished_at?: string;
  error_message?: string | null;
};

const ACTIVE = new Set(["pending", "running", "paused"]);

function useOverviewData() {
  const templates = useQuery({ queryKey: ["templates"], queryFn: () => apiFetch<{ items: Template[] }>("/rule-templates") });
  const jobs = useQuery({ queryKey: ["jobs"], queryFn: () => apiFetch<{ items: Job[] }>("/jobs?limit=200"), refetchInterval: 10_000 });
  const batches = useQuery({ queryKey: ["batches"], queryFn: () => apiFetch<{ items: Batch[] }>("/batches"), refetchInterval: 15_000 });
  const schedules = useQuery({ queryKey: ["schedules"], queryFn: () => apiFetch<{ items: Schedule[] }>("/schedules") });
  const connectors = useQuery({ queryKey: ["connectors"], queryFn: () => apiFetch<{ items: Connector[] }>("/connectors") });
  return { templates, jobs, batches, schedules, connectors };
}

type Row = { job: Job; id: MigrationIdentity; outcome: ReturnType<typeof outcomeLabel>; progress: number; done: number; total: number };

function rowsFor(jobs: Job[], names: Map<number, string>): Row[] {
  return jobs.map((job) => {
    const id = migrationIdentity(job, names);
    const t = job.totals_json ?? {};
    const done = (t.processed ?? 0) + (t.failed ?? 0) + (t.unresolved ?? 0);
    const total = t.total ?? done;
    return { job, id, outcome: outcomeLabel(job.status, t), progress: total ? Math.min(100, Math.round((done / total) * 100)) : 0, done, total };
  });
}

function nextAction(r: Row): string | null {
  const t = r.job.totals_json ?? {};
  if (r.job.last_error) return "Stopped before every record was read: fix the cause, then start a new run";
  if ((t.unresolved ?? 0) > 0) return `${t.unresolved} record${t.unresolved === 1 ? "" : "s"} with an unknown destination effect: check the destination before retrying`;
  if ((t.failed ?? 0) > 0 && r.job.status !== "running") return `${t.failed} failed record${t.failed === 1 ? "" : "s"}: inspect, correct, retry`;
  if (r.job.status === "paused") return "Paused: resume when the destination is ready";
  return null;
}

function ProgressBar({ r }: { r: Row }) {
  const color = r.outcome.tone === "fail" ? "bg-rose-500" : r.outcome.tone === "warn" ? "bg-amber-500" : r.outcome.tone === "info" ? "bg-blue-500" : "bg-emerald-500";
  return (
    <div className="h-1 w-full rounded bg-slate-200" aria-hidden>
      <div className={clsx("h-1 rounded", color)} style={{ width: `${r.progress}%` }} />
    </div>
  );
}

function MigrationRow({ r, compact }: { r: Row; compact?: boolean }) {
  const action = nextAction(r);
  const when = relativeTime(r.job.updated_at ?? r.job.finished_at ?? r.job.started_at);
  if (compact) {
    return (
      <li>
        <Link href={`/jobs/${r.job.id}`} className="block px-5 py-3 hover:bg-slate-50">
          <div className="flex items-center gap-3">
            <span className="min-w-0 flex-1 truncate text-[15px] text-slate-950">{r.id.purpose}</span>
            <Chip tone={r.outcome.tone}>{r.outcome.label}</Chip>
          </div>
          <div className="mt-0.5 flex items-center gap-2 text-[13px] text-slate-500">
            <span className="min-w-0 flex-1 truncate">
              {r.id.packageName ? <>{r.id.packageName} › </> : null}
              {r.id.fileName ?? "source not recorded"} · {r.id.executionId}
            </span>
            <span className="tnum shrink-0">
              {r.total ? `${r.done.toLocaleString()} / ${r.total.toLocaleString()}` : "no rows"} · {when}
            </span>
          </div>
          {action ? <div className="mt-1 text-[13px] text-amber-700">{action}</div> : null}
        </Link>
      </li>
    );
  }
  return (
    <li>
      <Link href={`/jobs/${r.job.id}`} className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-4 gap-y-1 px-5 py-3.5 hover:bg-slate-50 sm:grid-cols-[minmax(0,1fr)_140px_120px_110px]">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <span className="truncate text-[15px] text-slate-950">{r.id.purpose}</span>
            {r.id.purposeIsFallback ? <span className="text-xs text-slate-500">name not recorded</span> : null}
          </div>
          <div className="mt-0.5 truncate text-[13px] text-slate-500">
            {r.id.packageName ? <>{r.id.packageName} › </> : null}
            {r.id.fileName ?? "source not recorded"}
            <span className="text-slate-400"> · Execution {r.id.executionId}</span>
          </div>
          {action ? <div className="mt-1 text-[13px] text-amber-700">{action}</div> : null}
        </div>
        <div className="row-start-1 sm:row-auto">
          <Chip tone={r.outcome.tone}>{r.outcome.label}</Chip>
        </div>
        <div className="tnum self-center text-[13px] text-slate-600">
          {r.total ? (
            <>
              <span className="text-slate-950">{r.done.toLocaleString()}</span> / {r.total.toLocaleString()}
              <div className="mt-1"><ProgressBar r={r} /></div>
            </>
          ) : (
            <span className="text-slate-500">no rows yet</span>
          )}
        </div>
        <div className="self-center text-[13px] text-slate-500">{when}</div>
      </Link>
    </li>
  );
}

function MigrationList({ rows, empty, compact }: { rows: Row[]; empty: string; compact?: boolean }) {
  if (rows.length === 0) return <div className="px-5 py-8 text-center text-sm text-slate-500">{empty}</div>;
  return <ul className="divide-y divide-slate-200">{rows.map((r) => <MigrationRow key={r.job.id} r={r} compact={compact} />)}</ul>;
}

type Filter = "all" | "running" | "attention" | "reconciled";

function filterRows(rows: Row[], f: Filter, q: string): Row[] {
  const needle = q.trim().toLowerCase();
  return rows.filter((r) => {
    if (needle && !searchText(r.id).includes(needle)) return false;
    switch (f) {
      case "running":
        return ACTIVE.has(r.job.status);
      case "attention":
        return r.job.status === "failed" || r.job.status === "paused" || (r.job.totals_json?.unresolved ?? 0) > 0;
      case "reconciled":
        return r.job.status === "succeeded";
      default:
        return true;
    }
  });
}

function Lead({ ws, children }: { ws: Workspace; children?: React.ReactNode }) {
  return (
    <div className="flex flex-col justify-between gap-4 sm:flex-row sm:items-end">
      <div>
        <h1 className="text-[28px] leading-tight text-slate-950">Operations overview</h1>
        <p className="mt-1 text-[15px] text-slate-500">{WORKSPACES[ws].lead}</p>
      </div>
      {children ? <div className="flex shrink-0 gap-2">{children}</div> : null}
    </div>
  );
}

/* ----- Operator ----- */

function OperatorHome({ rows, batches }: { rows: Row[]; batches: Batch[] }) {
  const [filter, setFilter] = useState<Filter>("all");
  const [q, setQ] = useState("");
  const counts = {
    all: rows.length,
    running: filterRows(rows, "running", "").length,
    attention: filterRows(rows, "attention", "").length,
    reconciled: filterRows(rows, "reconciled", "").length
  };
  const shown = filterRows(rows, filter, q);
  const packagesPending = batches.filter((b) => ["pending", "running"].includes(b.status)).length;
  const quarantined = batches.filter((b) => b.status === "quarantined").length;
  const unresolved = rows.reduce((n, r) => n + (r.job.totals_json?.unresolved ?? 0), 0);
  const firstRun = rows.length === 0 && batches.length === 0;

  return (
    <div className="space-y-6">
      <Lead ws="operator">
        <Link href="/jobs/new">
          <Button>
            <Upload className="h-4 w-4" /> Upload package
          </Button>
        </Link>
      </Lead>

      <StatRow>
        <Stat label="Running" value={counts.running} note={packagesPending ? `${packagesPending} package${packagesPending === 1 ? "" : "s"} in progress` : "No packages in progress"} />
        <Stat label="Needs attention" value={counts.attention} tone={counts.attention ? "fail" : "neutral"} note={unresolved ? `${unresolved} unresolved record${unresolved === 1 ? "" : "s"}` : "No unresolved records"} />
        <Stat label="Reconciled" value={counts.reconciled} tone="ok" note="Every record accounted for" />
        <Stat label="Quarantined packages" value={quarantined} tone={quarantined ? "warn" : "neutral"} note={quarantined ? <Link href="/batches" className="underline">Review archives</Link> : "Archives unpacked cleanly"} />
      </StatRow>

      {firstRun ? (
        <Panel className="p-5">
          <h2 className="text-lg text-slate-950">Nothing has run yet</h2>
          <p className="mt-1 max-w-xl text-sm text-slate-500">Upload an archive with a manifest, or start a single-file migration from a published template. Schedules can watch a prefix for new arrivals.</p>
          <div className="mt-4 flex gap-2">
            <Link href="/jobs/new"><Button>Start a migration</Button></Link>
            <Link href="/schedules/new"><Button variant="secondary">Create a schedule</Button></Link>
          </div>
        </Panel>
      ) : null}

      <Panel>
        <PanelHeader>
          <div role="tablist" aria-label="Filter migrations" className="flex gap-1">
            {(
              [
                ["all", "All"],
                ["running", "Running"],
                ["attention", "Needs attention"],
                ["reconciled", "Reconciled"]
              ] as [Filter, string][]
            ).map(([f, label]) => (
              <button
                key={f}
                role="tab"
                aria-selected={filter === f}
                type="button"
                onClick={() => setFilter(f)}
                className={clsx("rounded-md px-2.5 py-1 text-sm", filter === f ? "bg-slate-100 text-slate-950 ring-1 ring-slate-300" : "text-slate-500 hover:text-slate-800")}
              >
                {label} <span className="tnum ml-1 text-xs text-slate-500">{counts[f]}</span>
              </button>
            ))}
          </div>
          <label className="ml-auto flex min-w-[220px] items-center gap-2 rounded-md bg-slate-50 px-2.5 py-1.5 text-sm ring-1 ring-slate-300 focus-within:ring-brand-400">
            <Search className="h-4 w-4 text-slate-500" aria-hidden />
            <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Name, archive, file or execution id" className="w-full bg-transparent text-slate-900 placeholder:text-slate-500 focus:outline-none" aria-label="Search migrations" />
          </label>
        </PanelHeader>
        <MigrationList rows={shown} empty={q ? "No migration matches that search." : "No migrations in this view."} />
      </Panel>
    </div>
  );
}

/* ----- Executive ----- */

function ExecutiveHome({ rows, batches }: { rows: Row[]; batches: Batch[] }) {
  const planned = rows.reduce((n, r) => n + r.total, 0);
  const processed = rows.reduce((n, r) => n + (r.job.totals_json?.processed ?? 0), 0);
  const reconciledRows = rows.filter((r) => r.job.status === "succeeded");
  const reconciled = reconciledRows.reduce((n, r) => n + (r.job.totals_json?.processed ?? 0), 0);
  const failed = rows.reduce((n, r) => n + (r.job.totals_json?.failed ?? 0), 0);
  const unresolved = rows.reduce((n, r) => n + (r.job.totals_json?.unresolved ?? 0), 0);
  const blocked = rows.filter((r) => r.job.status === "failed" || r.job.status === "paused" || (r.job.totals_json?.unresolved ?? 0) > 0);
  const pct = planned ? Math.round((reconciled / planned) * 100) : null;

  return (
    <div className="space-y-6">
      <Lead ws="executive" />
      <StatRow>
        <Stat label="Planned records" value={planned.toLocaleString()} note="Source rows read across all migrations" />
        <Stat label="Processed" value={processed.toLocaleString()} note="Accepted by the destination" />
        <Stat label="Reconciled" value={pct === null ? "—" : `${pct}%`} tone="ok" note={pct === null ? "No completed migration yet" : `${reconciled.toLocaleString()} records in migrations that completed clean`} />
        <Stat label="Unresolved" value={(failed + unresolved).toLocaleString()} tone={failed + unresolved ? "fail" : "neutral"} note={unresolved ? `${unresolved} with an unknown destination effect` : "Failed records awaiting correction"} />
      </StatRow>
      <p className="text-[13px] text-slate-500">
        Percentages use source records as the unit. Retries do not add to processed counts. Campaign and environment scoping is not yet recorded, so totals are for this installation.
      </p>
      <div className="grid gap-6 lg:grid-cols-[minmax(0,1.3fr)_minmax(320px,1fr)]">
        <Panel>
          <PanelHeader>
            <h2 className="text-base text-slate-950">Blocked work</h2>
            <span className="ml-auto text-sm text-slate-500">{blocked.length}</span>
          </PanelHeader>
          <MigrationList rows={blocked} empty="Nothing is blocked." compact />
        </Panel>
        <Panel>
          <PanelHeader>
            <h2 className="text-base text-slate-950">Packages</h2>
            <Link href="/batches" className="ml-auto inline-flex items-center gap-1 text-sm text-brand-700 hover:underline">All packages <ArrowRight className="h-3.5 w-3.5" /></Link>
          </PanelHeader>
          {batches.length === 0 ? (
            <div className="px-5 py-8 text-center text-sm text-slate-500">No packages yet.</div>
          ) : (
            <ul className="divide-y divide-slate-200">
              {batches.slice(0, 8).map((b) => (
                <li key={b.id}>
                  <Link href={`/batches/${b.id}`} className="flex items-center gap-3 px-5 py-3 hover:bg-slate-50">
                    <div className="min-w-0 flex-1">
                      <div className="truncate text-sm text-slate-950">{b.batch_key || b.source_ref?.key?.split("/").pop() || `Package #${b.id}`}</div>
                      <div className="text-xs text-slate-500">{relativeTime(b.finished_at ?? b.started_at)}</div>
                    </div>
                    <Chip tone={b.status === "succeeded" ? "ok" : b.status === "failed" || b.status === "quarantined" ? "fail" : b.status === "partial" ? "warn" : "neutral"}>{b.status}</Chip>
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </Panel>
      </div>
    </div>
  );
}

/* ----- Coordinator ----- */

function CoordinatorHome({ rows, batches, schedules }: { rows: Row[]; batches: Batch[]; schedules: Schedule[] }) {
  const ready = batches.filter((b) => b.status === "pending");
  const inFlight = batches.filter((b) => b.status === "running");
  const blockedPkgs = batches.filter((b) => ["failed", "partial", "quarantined"].includes(b.status));
  const upcoming = schedules.filter((s) => s.enabled).sort((a, b) => (a.next_run_at ?? "").localeCompare(b.next_run_at ?? ""));
  const blocked = rows.filter((r) => r.job.status === "failed" || (r.job.totals_json?.unresolved ?? 0) > 0);
  return (
    <div className="space-y-6">
      <Lead ws="coordinator">
        <Link href="/plans"><Button variant="secondary">Dependency view</Button></Link>
        <Link href="/plans/new"><Button>Design a plan</Button></Link>
      </Lead>
      <StatRow>
        <Stat label="Ready to run" value={ready.length} note="Packages admitted, not started" />
        <Stat label="In flight" value={inFlight.length} note="Packages running stages" />
        <Stat label="Blocked packages" value={blockedPkgs.length} tone={blockedPkgs.length ? "fail" : "neutral"} note="Failed, partial or quarantined" />
        <Stat label="Scheduled" value={upcoming.length} note={upcoming[0]?.next_run_at ? `Next: ${relativeTime(upcoming[0].next_run_at)}` : "No upcoming runs"} />
      </StatRow>
      <div className="grid gap-6 lg:grid-cols-2">
        <Panel>
          <PanelHeader>
            <h2 className="text-base text-slate-950">Packages and their blockers</h2>
          </PanelHeader>
          {batches.length === 0 ? (
            <div className="px-5 py-8 text-center text-sm text-slate-500">No packages yet.</div>
          ) : (
            <ul className="divide-y divide-slate-200">
              {batches.slice(0, 12).map((b) => (
                <li key={b.id}>
                  <Link href={`/batches/${b.id}`} className="block px-5 py-3 hover:bg-slate-50">
                    <div className="flex items-center gap-3">
                      <span className="min-w-0 flex-1 truncate text-sm text-slate-950">{b.batch_key || b.source_ref?.key?.split("/").pop() || `Package #${b.id}`}</span>
                      <Chip tone={b.status === "succeeded" ? "ok" : ["failed", "quarantined"].includes(b.status) ? "fail" : b.status === "partial" ? "warn" : "neutral"}>{b.status}</Chip>
                    </div>
                    {b.error_message ? <div className="mt-1 truncate text-[13px] text-amber-700">{b.error_message}</div> : null}
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </Panel>
        <Panel>
          <PanelHeader>
            <h2 className="text-base text-slate-950">Migrations blocking dependents</h2>
          </PanelHeader>
          <MigrationList rows={blocked} empty="No migration is blocking dependent work." compact />
        </Panel>
      </div>
    </div>
  );
}

/* ----- Designer ----- */

function DesignerHome({ templates, rows }: { templates: Template[]; rows: Row[] }) {
  const drafts = templates.filter((t) => !t.published);
  const published = templates.filter((t) => t.published);
  const recentTests = rows.slice(0, 6);
  return (
    <div className="space-y-6">
      <Lead ws="designer">
        <Link href="/templates/new"><Button>New template</Button></Link>
      </Lead>
      <StatRow>
        <Stat label="Published templates" value={published.length} note="Available to runs and schedules" />
        <Stat label="Drafts" value={drafts.length} tone={drafts.length ? "warn" : "neutral"} note="Unpublished changes" />
        <Stat label="Recent runs" value={rows.length} note="Across all templates" />
        <Stat label="Failing runs" value={rows.filter((r) => r.job.status === "failed").length} tone="fail" note="Check mappings against failures" />
      </StatRow>
      <div className="grid gap-6 lg:grid-cols-2">
        <Panel>
          <PanelHeader>
            <h2 className="text-base text-slate-950">Templates needing attention</h2>
            <Link href="/templates" className="ml-auto text-sm text-brand-700 hover:underline">All templates</Link>
          </PanelHeader>
          {templates.length === 0 ? (
            <div className="px-5 py-8 text-center text-sm text-slate-500">No templates yet. Sample a file to start mapping.</div>
          ) : (
            <ul className="divide-y divide-slate-200">
              {[...drafts, ...published].slice(0, 10).map((t) => (
                <li key={t.id}>
                  <Link href={`/templates/${t.id}`} className="flex items-center gap-3 px-5 py-3 hover:bg-slate-50">
                    <span className="min-w-0 flex-1 truncate text-sm text-slate-950">{t.name}</span>
                    <span className="text-xs text-slate-500">v{t.version ?? "?"}</span>
                    <Chip tone={t.published ? "ok" : "warn"}>{t.published ? "published" : "draft"}</Chip>
                  </Link>
                </li>
              ))}
            </ul>
          )}
        </Panel>
        <Panel>
          <PanelHeader>
            <h2 className="text-base text-slate-950">Recent runs</h2>
          </PanelHeader>
          <MigrationList rows={recentTests} empty="No runs yet." compact />
        </Panel>
      </div>
    </div>
  );
}

/* ----- Admin ----- */

function AdminHome({ connectors, templates, schedules, rows }: { connectors: Connector[]; templates: Template[]; schedules: Schedule[]; rows: Row[] }) {
  return (
    <div className="space-y-6">
      <Lead ws="admin">
        <Link href="/settings"><Button variant="secondary">Settings</Button></Link>
      </Lead>
      <StatRow>
        <Stat label="Connectors" value={connectors.length} note={<Link href="/connectors" className="underline">Manage sources</Link>} />
        <Stat label="Published templates" value={templates.filter((t) => t.published).length} />
        <Stat label="Enabled schedules" value={schedules.filter((s) => s.enabled).length} />
        <Stat label="Active migrations" value={rows.filter((r) => ACTIVE.has(r.job.status)).length} />
      </StatRow>
      <Panel>
        <PanelHeader>
          <h2 className="text-base text-slate-950">Recent migrations</h2>
        </PanelHeader>
        <MigrationList rows={rows.slice(0, 10)} empty="No migrations yet." />
      </Panel>
    </div>
  );
}

export default function HomePage() {
  const { workspace } = useWorkspace();
  const { templates, jobs, batches, schedules, connectors } = useOverviewData();
  const names = useMemo(() => templateNameIndex(templates.data?.items), [templates.data]);
  const rows = useMemo(() => rowsFor(jobs.data?.items ?? [], names), [jobs.data, names]);

  const queries = [templates, jobs, batches, schedules, connectors];
  if (queries.some((q) => q.isLoading)) return <LoadingState label="Loading overview" />;
  const firstError = queries.find((q) => q.error);
  if (firstError) return <ErrorState error={firstError.error} retry={() => queries.forEach((q) => q.refetch())} />;

  const b = batches.data?.items ?? [];
  switch (workspace) {
    case "executive":
      return <ExecutiveHome rows={rows} batches={b} />;
    case "coordinator":
      return <CoordinatorHome rows={rows} batches={b} schedules={schedules.data?.items ?? []} />;
    case "designer":
      return <DesignerHome templates={templates.data?.items ?? []} rows={rows} />;
    case "admin":
      return <AdminHome connectors={connectors.data?.items ?? []} templates={templates.data?.items ?? []} schedules={schedules.data?.items ?? []} rows={rows} />;
    default:
      return <OperatorHome rows={rows} batches={b} />;
  }
}
