"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { Download, Plus, Trash2 } from "lucide-react";
import { useRouter } from "next/navigation";
import { useMemo, useState } from "react";
import { toast } from "sonner";

import { StageGraph, type GraphNode } from "@/components/dependency/StageGraph";
import { Breadcrumbs, Button, Card, FormField, Input, PageHeader, Select } from "@/components/ui";
import { apiFetch, getToken } from "@/lib/api";
import { buildManifest, makeZip, stageFilePath, validateDraft, wouldCycle, type DraftStage } from "@/lib/package-graph";

type Template = { id: number; template_key: string; name: string; version: number };

let nextUid = 0;
const newStage = (): DraftStage => ({ uid: `s${++nextUid}`, id: "", templateKey: "", file: null, dependsOn: [] });

export default function BuildPackagePage() {
  const router = useRouter();
  const [batchId, setBatchId] = useState("");
  const [onStageFailure, setOnStageFailure] = useState<"stop" | "continue">("stop");
  const [stages, setStages] = useState<DraftStage[]>(() => [newStage()]);
  const [submitted, setSubmitted] = useState(false);

  const templates = useQuery({
    queryKey: ["templates", "published"],
    queryFn: () => apiFetch<{ items: Template[] }>("/rule-templates?published=true")
  });
  const published = templates.data?.items ?? [];
  const templateName = (key: string) => published.find((t) => t.template_key === key)?.name;

  const problems = useMemo(() => validateDraft(batchId, stages), [batchId, stages]);

  const update = (uid: string, patch: Partial<DraftStage>) =>
    setStages((all) => all.map((s) => (s.uid === uid ? { ...s, ...patch } : s)));

  const remove = (uid: string) =>
    setStages((all) => all.filter((s) => s.uid !== uid).map((s) => ({ ...s, dependsOn: s.dependsOn.filter((d) => d !== uid) })));

  /** `to` waits for `from`. */
  const connect = (from: string, to: string) => {
    const graph = stages.map((s) => ({ key: s.uid, dependsOn: s.dependsOn }));
    if (wouldCycle(graph, from, to)) {
      toast.error("That dependency would make a loop.");
      return;
    }
    setStages((all) => all.map((s) => (s.uid === to && !s.dependsOn.includes(from) ? { ...s, dependsOn: [...s.dependsOn, from] } : s)));
  };
  const disconnect = (from: string, to: string) =>
    setStages((all) => all.map((s) => (s.uid === to ? { ...s, dependsOn: s.dependsOn.filter((d) => d !== from) } : s)));

  const graphNodes: GraphNode[] = stages.map((s) => ({
    key: s.uid,
    label: s.id,
    dependsOn: s.dependsOn,
    state: "pending",
    detail: s.templateKey ? templateName(s.templateKey) ?? s.templateKey : "No template yet",
    invalid: submitted && problems.has(s.uid)
  }));

  const manifest = () => buildManifest(batchId, onStageFailure, stages);

  const downloadManifest = () => {
    const blob = new Blob([JSON.stringify(manifest(), null, 2) + "\n"], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "manifest.json";
    a.click();
    URL.revokeObjectURL(url);
  };

  const start = useMutation({
    mutationFn: async () => {
      setSubmitted(true);
      if (problems.size) throw new Error(problems.values().next().value ?? "Fix the highlighted stages.");
      const enc = new TextEncoder();
      const entries = [{ name: "manifest.json", data: enc.encode(JSON.stringify(manifest(), null, 2) + "\n") }];
      for (const s of stages) entries.push({ name: stageFilePath(s), data: new Uint8Array(await s.file!.arrayBuffer()) });
      const form = new FormData();
      form.append("file", makeZip(entries), `${batchId || "package"}.zip`);
      const res = await fetch("/api/files", {
        method: "POST",
        headers: { authorization: `Bearer ${getToken() ?? ""}` },
        body: form
      });
      if (!res.ok) throw new Error(await res.text());
      const { source_ref } = await res.json();
      return apiFetch<{ id: number }>("/batches", { method: "POST", body: JSON.stringify({ source_ref }) });
    },
    onSuccess: (batch) => {
      toast.success(`Package #${batch.id} started`);
      router.push(`/batches/${batch.id}`);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const packageProblem = submitted ? problems.get("") : undefined;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Build a package"
        eyebrow={<Breadcrumbs items={[{ label: "Packages", href: "/batches" }, { label: "Build" }]} />}
        description="Add a stage for each file, then draw which stages must finish first. A stage starts only when everything it waits for has completed; if one fails, the stages after it are skipped."
      />

      <Card className="grid gap-4 sm:grid-cols-2">
        <FormField label="Package name" hint="Optional. Shown as the package key.">
          <Input value={batchId} onChange={(e) => setBatchId(e.target.value.trim())} placeholder="property-cutover" />
        </FormField>
        <FormField label="When a stage fails">
          <Select value={onStageFailure} onChange={(e) => setOnStageFailure(e.target.value as "stop" | "continue")}>
            <option value="stop">Stop the package</option>
            <option value="continue">Keep running stages that don’t depend on it</option>
          </Select>
        </FormField>
      </Card>

      <section className="space-y-2" aria-labelledby="tree-heading">
        <div className="flex flex-wrap items-baseline gap-x-3">
          <h2 id="tree-heading" className="text-lg font-medium">Dependency tree</h2>
          <span className="text-sm text-slate-500">
            Drag a stage to move it. Drag from a stage’s right dot to another stage’s left dot: the second stage waits for the first. To remove a link, select its arrow and choose Remove link (or press Delete).
          </span>
        </div>
        <StageGraph nodes={graphNodes} editable onConnect={connect} onDisconnect={disconnect} showImplicit={false} height={420} />
      </section>

      <section className="space-y-3" aria-labelledby="stages-heading">
        <h2 id="stages-heading" className="text-lg font-medium">Stages</h2>
        {stages.map((s, i) => {
          const others = stages.filter((o) => o.uid !== s.uid);
          const problem = submitted ? problems.get(s.uid) : undefined;
          return (
            <Card key={s.uid} className={problem ? "border-rose-300" : undefined}>
              <div className="grid gap-4 lg:grid-cols-[1fr_1.2fr_1.2fr_auto]">
                <FormField label={`Stage ${i + 1} name`} required error={problem}>
                  <Input value={s.id} onChange={(e) => update(s.uid, { id: e.target.value.trim() })} placeholder="properties" />
                </FormField>
                <FormField label="Template" required>
                  <Select value={s.templateKey} onChange={(e) => update(s.uid, { templateKey: e.target.value })} disabled={templates.isLoading}>
                    <option value="">{templates.isLoading ? "Loading…" : "Choose a published template"}</option>
                    {published.map((t) => (
                      <option key={t.template_key} value={t.template_key}>{t.name} (v{t.version})</option>
                    ))}
                  </Select>
                </FormField>
                <FormField label="CSV file" required hint={s.file ? `${s.file.name} · ${(s.file.size / 1024).toFixed(1)} KB` : undefined}>
                  <Input type="file" accept=".csv,text/csv" onChange={(e) => update(s.uid, { file: e.target.files?.[0] ?? null })} />
                </FormField>
                <div className="flex items-end">
                  <Button variant="ghost" aria-label={`Remove stage ${s.id || i + 1}`} onClick={() => remove(s.uid)} disabled={stages.length === 1}>
                    <Trash2 className="h-4 w-4" aria-hidden />
                  </Button>
                </div>
              </div>
              {others.length ? (
                <fieldset className="mt-4">
                  <legend className="text-sm font-medium text-slate-700">Waits for</legend>
                  <div className="mt-1.5 flex flex-wrap gap-2">
                    {others.map((o) => {
                      const checked = s.dependsOn.includes(o.uid);
                      return (
                        <label key={o.uid} className="inline-flex cursor-pointer items-center gap-1.5 rounded border border-slate-300 px-2 py-1 text-sm has-[:checked]:border-brand-500 has-[:checked]:bg-brand-50">
                          <input
                            type="checkbox"
                            checked={checked}
                            onChange={() => (checked ? disconnect(o.uid, s.uid) : connect(o.uid, s.uid))}
                          />
                          <span className="font-mono text-[13px]">{o.id || "unnamed stage"}</span>
                        </label>
                      );
                    })}
                  </div>
                </fieldset>
              ) : null}
            </Card>
          );
        })}
        <Button variant="secondary" onClick={() => setStages((all) => [...all, newStage()])}>
          <Plus className="h-4 w-4" aria-hidden /> Add stage
        </Button>
      </section>

      {stages.length > 1 && stages.every((s) => s.dependsOn.length === 0) ? (
        <p className="text-sm text-slate-500">No dependencies drawn: stages will run one after another in the order listed.</p>
      ) : null}
      {packageProblem ? <p role="alert" className="text-sm text-rose-700">{packageProblem}</p> : null}

      <div className="flex flex-wrap gap-2 border-t border-slate-200 pt-4">
        <Button onClick={() => start.mutate()} disabled={start.isPending}>
          {start.isPending ? "Starting…" : "Start package"}
        </Button>
        <Button variant="secondary" onClick={downloadManifest}>
          <Download className="h-4 w-4" aria-hidden /> Download manifest.json
        </Button>
      </div>
    </div>
  );
}
