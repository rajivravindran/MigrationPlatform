"use client";

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import Link from "next/link";
import { useParams, useRouter } from "next/navigation";
import { useState } from "react";
import { toast } from "sonner";

import { StageGraph, StageLegend, type GraphNode } from "@/components/dependency/StageGraph";
import { Chip, type ChipTone } from "@/components/ops";
import { Breadcrumbs, Button, Card, ErrorState, FormField, Input, LoadingState, PageHeader, Select, formatDateTime } from "@/components/ui";
import { apiFetch, getToken } from "@/lib/api";
import { layoutStages, type StageState } from "@/lib/package-graph";
import {
  fileKind,
  matchStep,
  readArchiveManifest,
  ruleText,
  stepState,
  type ManifestStage,
  type PlanDetail,
  type PlanRun
} from "@/lib/plans";

type Template = { id: number; template_key: string; name: string; version: number; schema_json?: { source?: { type?: string } } };

const LIVE_STATES: StageState[] = ["pending", "waiting", "blocked", "running", "completed", "failed", "cancelled"];

export default function PlanPage() {
  const { id } = useParams<{ id: string }>();
  const router = useRouter();
  const qc = useQueryClient();
  const { data, isLoading, error } = useQuery({
    queryKey: ["plans", id],
    queryFn: () => apiFetch<PlanDetail>(`/plans/${id}`),
    refetchInterval: 4000
  });
  const templates = useQuery({
    queryKey: ["templates", "published"],
    queryFn: () => apiFetch<{ items: Template[] }>("/rule-templates?published=true")
  });

  const revise = useMutation({
    mutationFn: () => apiFetch<PlanDetail>(`/plans/${id}/revise`, { method: "POST" }),
    onSuccess: (d) => router.push(`/plans/${d.plan.id}/edit`),
    onError: (e) => toast.error(e instanceof Error ? e.message : String(e))
  });
  const cancel = useMutation({
    mutationFn: (runId: number) => apiFetch(`/plan-runs/${runId}/cancel`, { method: "POST" }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["plans", id] }),
    onError: (e) => toast.error(e instanceof Error ? e.message : String(e))
  });

  if (isLoading) return <LoadingState label="Loading plan" />;
  if (error || !data) return <ErrorState title="Plan not found" error={error} />;
  const { plan, steps } = data;
  const published = templates.data?.items ?? [];
  const templateName = (key?: string | null) => (key ? published.find((t) => t.template_key === key)?.name ?? key : null);

  const statusByStep = new Map(data.step_status.map((s) => [s.step_key, s]));
  const latestRun = (step: string) => data.runs.find((r) => r.step_key === step && r.job_totals);
  const nodes: GraphNode[] = steps.map((s) => {
    const st = statusByStep.get(s.step_key);
    const run = latestRun(s.step_key);
    const t = run?.job_totals;
    return {
      key: s.step_key,
      dependsOn: s.depends_on,
      state: stepState(st),
      detail: [templateName(s.default_template_key) ?? "Template chosen per file", ruleText(s)].join(" · "),
      progress: t?.total ? { processed: t.processed ?? 0, total: t.total, failed: (t.failed ?? 0) + (t.unresolved ?? 0) } : null,
      note: st?.latest_reason ?? (st ? null : s.file_pattern ? `No file yet · routes ${s.file_pattern}` : "No file yet"),
      href: st?.latest_job_id ? `/jobs/${st.latest_job_id}` : null
    };
  });
  const counts: Partial<Record<StageState, number>> = {};
  nodes.forEach((n) => (counts[n.state] = (counts[n.state] ?? 0) + 1));
  const done = counts.completed ?? 0;
  const pct = steps.length ? Math.round((done / steps.length) * 100) : 0;
  const positions = Object.fromEntries(steps.filter((s) => s.position).map((s) => [s.step_key, s.position!]));
  const maxY = Math.max(0, ...Array.from(layoutStages(nodes).values(), (p) => p.y), ...Object.values(positions).map((p) => p.y));

  return (
    <div className="space-y-6">
      <PageHeader
        title={plan.name}
        eyebrow={<Breadcrumbs items={[{ label: "Plans", href: "/plans" }, { label: plan.name }]} />}
        description={plan.description ?? undefined}
        actions={
          <>
            {data.versions.length > 1 ? (
              <Select aria-label="Version" value={plan.id} onChange={(e) => router.push(`/plans/${e.target.value}`)} className="min-h-9 w-auto py-1">
                {data.versions.map((v) => (
                  <option key={v.id} value={v.id}>Version {v.version}{v.status === "draft" ? " (draft)" : ""}</option>
                ))}
              </Select>
            ) : (
              <Chip tone="neutral" icon={false}>Version {plan.version}</Chip>
            )}
            <Chip tone={plan.status === "published" ? "ok" : "warn"}>{plan.status}</Chip>
            {plan.status === "draft" ? (
              <Link href={`/plans/${plan.id}/edit`}><Button>Edit draft</Button></Link>
            ) : (
              <Button variant="secondary" onClick={() => revise.mutate()} disabled={revise.isPending}>Revise plan</Button>
            )}
          </>
        }
      />

      <section aria-labelledby="progress-heading" className="space-y-3">
        <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1">
          <h2 id="progress-heading" className="text-lg font-medium">Progress</h2>
          <span className="text-sm text-slate-500">{done} of {steps.length} steps completed ({pct}%) · across every version of this plan</span>
        </div>
        <div className="h-1.5 overflow-hidden rounded bg-slate-200" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100} aria-label="Steps completed">
          <div className="h-full bg-emerald-500 transition-all" style={{ width: `${pct}%` }} />
        </div>
        <StageLegend counts={counts} states={LIVE_STATES} />
        <StageGraph nodes={nodes} layoutKey={`plan-${plan.plan_key}`} initialPositions={positions} height={Math.min(620, Math.max(300, maxY + 260))} />
        <p className="text-xs text-slate-500">
          Waiting: a file arrived and is held for the steps it depends on. Blocked: one of those steps failed its completion rule; it starts automatically once that step is re-run successfully. Select a step to open its latest migration.
        </p>
      </section>

      {plan.status === "published" ? (
        <RunPanel detail={data} templates={published} onStarted={() => qc.invalidateQueries({ queryKey: ["plans", id] })} />
      ) : (
        <Card className="text-sm text-slate-600">Publish this plan to run files against it.</Card>
      )}

      <RunsTable runs={data.runs} onCancel={(runId) => cancel.mutate(runId)} cancelling={cancel.isPending} />

      {data.intakes.length ? (
        <section aria-labelledby="intakes-heading" className="space-y-2">
          <h2 id="intakes-heading" className="text-lg font-medium">Archives received</h2>
          <ul className="divide-y divide-slate-200 rounded-lg border border-slate-200 bg-white">
            {data.intakes.map((i) => (
              <li key={i.id} className="flex flex-wrap items-center gap-3 px-4 py-3 text-sm">
                <span className="font-mono text-[13px] text-slate-950">{i.filename ?? `intake #${i.id}`}</span>
                <Chip tone={i.status === "failed" ? "fail" : i.status === "dispatched" ? "ok" : "info"}>{i.status === "dispatched" ? `${i.runs} files routed` : i.status}</Chip>
                {i.error_message ? <span className="text-rose-700">{i.error_message}</span> : null}
                <span className="ml-auto text-xs text-slate-500">{formatDateTime(i.created_at)}</span>
              </li>
            ))}
          </ul>
        </section>
      ) : null}
    </div>
  );
}

/* ------------------------------------------------------------- run a file */

type Mapping = { stage: ManifestStage; stepKey: string; templateKey: string };

function RunPanel({ detail, templates, onStarted }: { detail: PlanDetail; templates: Template[]; onStarted: () => void }) {
  const { plan, steps } = detail;
  const [file, setFile] = useState<File | null>(null);
  const [stepKey, setStepKey] = useState("");
  const [templateId, setTemplateId] = useState("");
  const [mapping, setMapping] = useState<Mapping[] | null>(null);
  const [archiveError, setArchiveError] = useState<string | null>(null);
  const [inputKey, setInputKey] = useState(0);

  const kind = file ? fileKind(file.name) : null;
  const typeOf = (t?: Template) => t?.schema_json?.source?.type ?? "";
  const defaultTemplateFor = (key: string) => {
    const k = steps.find((s) => s.step_key === key)?.default_template_key;
    return k ? templates.find((t) => t.template_key === k) : undefined;
  };

  const choose = async (f: File | null) => {
    setFile(f);
    setMapping(null);
    setArchiveError(null);
    if (!f) return;
    if (fileKind(f.name) === "archive") {
      try {
        const stages = await readArchiveManifest(f);
        setMapping(
          stages.map((stage) => {
            const step = steps.find((s) => s.step_key === stage.id) ?? matchStep(steps, stage.file);
            return { stage, stepKey: step?.step_key ?? "", templateKey: stage.templateKey ?? step?.default_template_key ?? "" };
          })
        );
      } catch (e) {
        setArchiveError(e instanceof Error ? e.message : String(e));
      }
      return;
    }
    const step = matchStep(steps, f.name);
    const key = step?.step_key ?? "";
    setStepKey(key);
    setTemplateId(String(defaultTemplateFor(key)?.id ?? ""));
  };

  const selectedTemplate = templates.find((t) => String(t.id) === templateId);
  const mismatch =
    kind && kind !== "archive" && selectedTemplate && ["csv", "json", "xml"].includes(typeOf(selectedTemplate)) && typeOf(selectedTemplate) !== kind
      ? `${file!.name} is ${kind.toUpperCase()} but this template reads ${typeOf(selectedTemplate).toUpperCase()}.`
      : null;

  const start = useMutation({
    mutationFn: async () => {
      if (!file) throw new Error("Choose a file.");
      if (kind === "other") throw new Error("Use a CSV, JSON, XML, .tar.gz or .zip file.");
      if (kind !== "archive" && !stepKey) throw new Error("Choose the step this file feeds.");
      if (kind !== "archive" && !templateId) throw new Error("Choose a template.");
      if (mismatch) throw new Error(mismatch);
      if (kind === "archive" && !mapping) throw new Error(archiveError ?? "Reading the archive…");
      const form = new FormData();
      form.append("file", file);
      const res = await fetch("/api/files", { method: "POST", headers: { authorization: `Bearer ${getToken() ?? ""}` }, body: form });
      if (!res.ok) throw new Error(await res.text());
      const { source_ref } = await res.json();
      const body =
        kind === "archive"
          ? { source_ref, overrides: Object.fromEntries(mapping!.map((m) => [m.stage.id, { step_key: m.stepKey || undefined, template_key: m.templateKey || undefined }])) }
          : { source_ref, step_key: stepKey, rule_template_id: Number(templateId) };
      return apiFetch<{ run?: PlanRun; intake_id?: number }>(`/plans/${plan.id}/runs`, { method: "POST", body: JSON.stringify(body) });
    },
    onSuccess: (r) => {
      toast.success(r.intake_id ? "Archive received; its files are being routed to steps" : `Queued for ${r.run?.step_key}`);
      setFile(null);
      setMapping(null);
      setInputKey((k) => k + 1);
      onStarted();
    },
    onError: (e) => toast.error(e instanceof Error ? e.message : String(e))
  });

  return (
    <Card className="space-y-4">
      <div>
        <h2 className="text-lg font-medium">Run a file</h2>
        <p className="text-sm text-slate-500">
          A single CSV, JSON or XML file feeds one step. A .tar.gz or .zip feeds several: its manifest.json names the template for each file, and you can adjust the step and template below. Each file waits until the steps it depends on have completed.
        </p>
      </div>
      <FormField label="File" hint={file ? `${file.name} · ${(file.size / 1024).toFixed(1)} KB` : "CSV, JSON / JSON Lines, XML, .tar.gz or .zip"}>
        <Input key={inputKey} type="file" accept=".csv,.tsv,.json,.jsonl,.ndjson,.xml,.zip,.gz,.tgz" onChange={(e) => choose(e.target.files?.[0] ?? null)} />
      </FormField>

      {kind && kind !== "archive" ? (
        <div className="grid gap-4 sm:grid-cols-2">
          <FormField label="Step" required>
            <Select
              value={stepKey}
              onChange={(e) => {
                setStepKey(e.target.value);
                setTemplateId(String(defaultTemplateFor(e.target.value)?.id ?? templateId));
              }}
            >
              <option value="">Choose a step</option>
              {steps.map((s) => (
                <option key={s.step_key} value={s.step_key}>{s.step_key}{s.depends_on.length ? ` (waits for ${s.depends_on.join(", ")})` : ""}</option>
              ))}
            </Select>
          </FormField>
          <FormField label="Template" required error={mismatch ?? undefined} hint="Pre-selected from the step’s default.">
            <Select value={templateId} onChange={(e) => setTemplateId(e.target.value)}>
              <option value="">Choose a template</option>
              {templates.map((t) => (
                <option key={t.id} value={t.id}>{t.name}{typeOf(t) ? ` · ${typeOf(t).toUpperCase()}` : ""}</option>
              ))}
            </Select>
          </FormField>
        </div>
      ) : null}

      {archiveError ? <p role="alert" className="text-sm text-rose-700">{archiveError}</p> : null}
      {mapping ? (
        <div className="overflow-x-auto rounded-lg border border-slate-200">
          <table className="w-full min-w-[640px] text-sm">
            <caption className="sr-only">Files in the archive and where they will run</caption>
            <thead className="bg-slate-50 text-slate-600">
              <tr>
                <th className="px-3 py-2 text-left">File in archive</th>
                <th className="px-3 py-2 text-left">Step</th>
                <th className="px-3 py-2 text-left">Template</th>
              </tr>
            </thead>
            <tbody>
              {mapping.map((m, i) => {
                const set = (patch: Partial<Mapping>) => setMapping((all) => all!.map((x, j) => (j === i ? { ...x, ...patch } : x)));
                return (
                  <tr key={m.stage.id} className="border-t border-slate-100">
                    <td className="px-3 py-2 font-mono text-xs">{m.stage.file}</td>
                    <td className="px-3 py-2">
                      <Select aria-label={`Step for ${m.stage.file}`} value={m.stepKey} onChange={(e) => set({ stepKey: e.target.value })} className={m.stepKey ? undefined : "border-amber-400"}>
                        <option value="">Not matched — will be skipped</option>
                        {steps.map((s) => <option key={s.step_key} value={s.step_key}>{s.step_key}</option>)}
                      </Select>
                    </td>
                    <td className="px-3 py-2">
                      <Select aria-label={`Template for ${m.stage.file}`} value={m.templateKey} onChange={(e) => set({ templateKey: e.target.value })}>
                        <option value="">Step default</option>
                        {templates.map((t) => <option key={t.template_key} value={t.template_key}>{t.name}{typeOf(t) ? ` · ${typeOf(t).toUpperCase()}` : ""}</option>)}
                      </Select>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      ) : null}

      <div>
        <Button onClick={() => start.mutate()} disabled={!file || start.isPending}>{start.isPending ? "Starting…" : "Run"}</Button>
      </div>
    </Card>
  );
}

/* ------------------------------------------------------------------- runs */

const RUN_TONE: Record<string, ChipTone> = { waiting: "neutral", running: "info", completed: "ok", failed: "fail", cancelled: "warn" };

function RunsTable({ runs, onCancel, cancelling }: { runs: PlanRun[]; onCancel: (id: number) => void; cancelling: boolean }) {
  return (
    <section aria-labelledby="runs-heading" className="space-y-2">
      <h2 id="runs-heading" className="text-lg font-medium">Files</h2>
      {runs.length === 0 ? (
        <Card className="text-sm text-slate-500">No files have run against this plan yet.</Card>
      ) : (
        <div className="overflow-x-auto rounded-lg border border-slate-200 bg-white">
          <table className="w-full min-w-[900px] text-sm">
            <caption className="sr-only">Files run against this plan, newest first</caption>
            <thead className="bg-slate-50 text-slate-600">
              <tr>
                <th className="px-3 py-2 text-left">File</th>
                <th className="px-3 py-2 text-left">Step</th>
                <th className="px-3 py-2 text-left">Template</th>
                <th className="px-3 py-2 text-left">Status</th>
                <th className="px-3 py-2 text-left">Records</th>
                <th className="px-3 py-2 text-left">Why</th>
                <th className="px-3 py-2 text-left">Received</th>
                <th className="px-3 py-2" />
              </tr>
            </thead>
            <tbody>
              {runs.map((r) => {
                const t = r.job_totals;
                const label = r.status === "waiting" && r.blocked ? "blocked" : r.status;
                return (
                  <tr key={r.id} className="border-t border-slate-100 align-top">
                    <td className="px-3 py-2 font-mono text-xs">{r.filename ?? "—"}{r.intake_id ? <span className="block text-[11px] text-slate-500">from archive</span> : null}</td>
                    <td className="px-3 py-2">{r.step_key ?? "—"}{r.plan_version ? <span className="block text-[11px] text-slate-500">v{r.plan_version}</span> : null}</td>
                    <td className="px-3 py-2">{r.template_name ?? "—"}</td>
                    <td className="px-3 py-2"><Chip tone={label === "blocked" ? "warn" : RUN_TONE[r.status] ?? "neutral"}>{label}</Chip></td>
                    <td className="px-3 py-2 text-slate-600">
                      {t?.total ? `${t.processed ?? 0}/${t.total}` : "—"}
                      {r.failed_pct != null && r.failed_pct > 0 ? <span className="block text-[11px] text-rose-700">{Number(r.failed_pct.toFixed(2))}% failed</span> : null}
                    </td>
                    <td className="max-w-sm px-3 py-2 text-xs text-slate-600">{r.reason ?? "—"}</td>
                    <td className="px-3 py-2 text-xs text-slate-500">{formatDateTime(r.created_at)}</td>
                    <td className="px-3 py-2 text-right">
                      {r.job_id ? <Link className="text-brand-700 hover:underline" href={`/jobs/${r.job_id}`}>Migration #{r.job_id}</Link> : null}
                      {r.status === "waiting" || r.status === "running" ? (
                        <Button size="sm" variant="ghost" className="ml-2" onClick={() => onCancel(r.id)} disabled={cancelling}>Cancel</Button>
                      ) : null}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
