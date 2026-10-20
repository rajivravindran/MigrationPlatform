"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { Plus, Trash2 } from "lucide-react";
import { useRouter } from "next/navigation";
import { useMemo, useState } from "react";
import { toast } from "sonner";

import { StageGraph, type GraphNode, type XY } from "@/components/dependency/StageGraph";
import { Button, Card, FormField, Input, Select, Textarea } from "@/components/ui";
import { apiFetch } from "@/lib/api";
import { SAFE_STAGE_ID, wouldCycle } from "@/lib/package-graph";
import { ruleText, type CompletionRule, type Plan, type PlanDetail, type PlanStep } from "@/lib/plans";

type Template = { id: number; template_key: string; name: string; version: number; schema_json?: { source?: { type?: string } } };

type DraftStep = {
  uid: string;
  key: string;
  dependsOn: string[]; // uids
  defaultTemplateKey: string;
  filePattern: string;
  rule: CompletionRule;
  maxPct: string;
  position?: XY | null;
};

let nextUid = 0;
const blankStep = (): DraftStep => ({
  uid: `p${++nextUid}`,
  key: "",
  dependsOn: [],
  defaultTemplateKey: "",
  filePattern: "",
  rule: "strict",
  maxPct: "5"
});

function fromPlan(steps: PlanStep[]): DraftStep[] {
  const drafts = steps.map((s) => ({
    uid: `p${++nextUid}`,
    key: s.step_key,
    dependsOn: [] as string[],
    defaultTemplateKey: s.default_template_key ?? "",
    filePattern: s.file_pattern ?? "",
    rule: s.completion_rule,
    maxPct: String(s.max_failed_pct ?? 5),
    position: s.position ?? null
  }));
  const uidOf = new Map(drafts.map((d) => [d.key, d.uid]));
  steps.forEach((s, i) => (drafts[i].dependsOn = s.depends_on.map((k) => uidOf.get(k)!).filter(Boolean)));
  return drafts;
}

function problemsOf(name: string, steps: DraftStep[]): Map<string, string> {
  const out = new Map<string, string>();
  if (!name.trim()) out.set("", "Name the plan.");
  if (!steps.length) out.set("", "Add at least one step.");
  const seen = new Set<string>();
  for (const s of steps) {
    const pct = Number(s.maxPct);
    const issue = !s.key
      ? "Name the step."
      : !SAFE_STAGE_ID.test(s.key)
        ? "Step name: letters, digits, '.', '_' or '-', starting with a letter or digit."
        : seen.has(s.key)
          ? `Another step is already called "${s.key}".`
          : s.rule === "max_failed_pct" && (s.maxPct.trim() === "" || !(pct >= 0 && pct <= 100))
            ? "Allowed failures must be a percentage from 0 to 100."
            : s.filePattern.includes("/")
              ? "A file pattern matches a file name, not a folder (e.g. accounts_*.csv)."
              : null;
    if (issue) out.set(s.uid, issue);
    seen.add(s.key);
  }
  return out;
}

export function PlanEditor({ existing }: { existing?: PlanDetail }) {
  const router = useRouter();
  const [name, setName] = useState(existing?.plan.name ?? "");
  const [description, setDescription] = useState(existing?.plan.description ?? "");
  const [steps, setSteps] = useState<DraftStep[]>(() => (existing?.steps.length ? fromPlan(existing.steps) : [blankStep()]));
  const [submitted, setSubmitted] = useState(false);

  const templates = useQuery({
    queryKey: ["templates", "published"],
    queryFn: () => apiFetch<{ items: Template[] }>("/rule-templates?published=true")
  });
  const published = templates.data?.items ?? [];
  const templateLabel = (key: string) => {
    const t = published.find((x) => x.template_key === key);
    return t ? `${t.name}${t.schema_json?.source?.type ? ` · ${t.schema_json.source.type.toUpperCase()}` : ""}` : key;
  };

  const problems = useMemo(() => problemsOf(name, steps), [name, steps]);
  const update = (uid: string, patch: Partial<DraftStep>) => setSteps((all) => all.map((s) => (s.uid === uid ? { ...s, ...patch } : s)));
  const remove = (uid: string) =>
    setSteps((all) => all.filter((s) => s.uid !== uid).map((s) => ({ ...s, dependsOn: s.dependsOn.filter((d) => d !== uid) })));
  const connect = (from: string, to: string) => {
    if (wouldCycle(steps.map((s) => ({ key: s.uid, dependsOn: s.dependsOn })), from, to)) {
      toast.error("That dependency would make a loop.");
      return;
    }
    setSteps((all) => all.map((s) => (s.uid === to && !s.dependsOn.includes(from) ? { ...s, dependsOn: [...s.dependsOn, from] } : s)));
  };
  const disconnect = (from: string, to: string) =>
    setSteps((all) => all.map((s) => (s.uid === to ? { ...s, dependsOn: s.dependsOn.filter((d) => d !== from) } : s)));

  const graphNodes: GraphNode[] = steps.map((s) => ({
    key: s.uid,
    label: s.key,
    dependsOn: s.dependsOn,
    state: "pending",
    detail: [s.defaultTemplateKey ? templateLabel(s.defaultTemplateKey) : "Template chosen at run time", s.filePattern || null]
      .filter(Boolean)
      .join(" · "),
    note: ruleText({ completion_rule: s.rule, max_failed_pct: Number(s.maxPct) }),
    invalid: submitted && problems.has(s.uid)
  }));
  const positions = Object.fromEntries(steps.filter((s) => s.position).map((s) => [s.uid, s.position!]));

  const payload = () => {
    const keyOf = new Map(steps.map((s) => [s.uid, s.key]));
    return {
      name: name.trim(),
      description: description.trim() || null,
      steps: steps.map((s) => ({
        step_key: s.key,
        depends_on: s.dependsOn.map((u) => keyOf.get(u)!).filter(Boolean),
        default_template_key: s.defaultTemplateKey || null,
        file_pattern: s.filePattern.trim() || null,
        completion_rule: s.rule,
        max_failed_pct: s.rule === "max_failed_pct" ? Number(s.maxPct) : null,
        position: s.position ?? null
      }))
    };
  };

  const save = useMutation({
    mutationFn: async (publish: boolean) => {
      setSubmitted(true);
      if (problems.size) throw new Error(problems.values().next().value ?? "Fix the highlighted steps.");
      const body = JSON.stringify(payload());
      const saved = existing
        ? await apiFetch<PlanDetail>(`/plans/${existing.plan.id}`, { method: "PUT", body })
        : await apiFetch<PlanDetail>("/plans", { method: "POST", body });
      if (publish) return apiFetch<PlanDetail>(`/plans/${saved.plan.id}/publish`, { method: "POST" });
      return saved;
    },
    onSuccess: (d: PlanDetail, publish) => {
      toast.success(publish ? `Published ${d.plan.name} v${d.plan.version}` : "Draft saved");
      router.push(publish ? `/plans/${d.plan.id}` : `/plans/${d.plan.id}/edit`);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const planProblem = submitted ? problems.get("") : undefined;

  return (
    <div className="space-y-6">
      <Card className="grid gap-4 sm:grid-cols-2">
        <FormField label="Plan name" required>
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Property cut-over wave 1" />
        </FormField>
        <FormField label="Description" hint="Optional. What this plan migrates and why.">
          <Textarea rows={1} value={description} onChange={(e) => setDescription(e.target.value)} />
        </FormField>
      </Card>

      <section className="space-y-2" aria-labelledby="plan-tree-heading">
        <div className="flex flex-wrap items-baseline gap-x-3">
          <h2 id="plan-tree-heading" className="text-lg font-medium">Dependency tree</h2>
          <span className="text-sm text-slate-500">
            Drag a step to arrange it. Drag from a step’s right dot to another step’s left dot: the second waits until the first meets its completion rule. Select an arrow and choose Remove link (or press Delete) to remove it.
          </span>
        </div>
        <StageGraph
          nodes={graphNodes}
          editable
          onConnect={connect}
          onDisconnect={disconnect}
          initialPositions={positions}
          onMove={(uid, xy) => update(uid, { position: xy })}
          onResetLayout={() => setSteps((all) => all.map((s) => ({ ...s, position: null })))}
          showImplicit={false}
          height={440}
        />
      </section>

      <section className="space-y-3" aria-labelledby="plan-steps-heading">
        <div>
          <h2 id="plan-steps-heading" className="text-lg font-medium">Steps</h2>
          <p className="text-sm text-slate-500">
            A step is a part of the migration, such as accounts or orders. Files arrive for it later: as a single CSV, JSON or XML file, inside a .tar.gz / .zip, or in a watched folder.
          </p>
        </div>
        {steps.map((s, i) => {
          const others = steps.filter((o) => o.uid !== s.uid);
          const problem = submitted ? problems.get(s.uid) : undefined;
          return (
            <Card key={s.uid} className={problem ? "border-rose-300" : undefined}>
              <div className="grid gap-4 lg:grid-cols-[1fr_1.3fr_1fr_auto]">
                <FormField label={`Step ${i + 1} name`} required error={problem}>
                  <Input value={s.key} onChange={(e) => update(s.uid, { key: e.target.value.trim() })} placeholder="accounts" />
                </FormField>
                <FormField label="Default template" hint="Pre-selected when a file arrives; can be changed then.">
                  <Select value={s.defaultTemplateKey} onChange={(e) => update(s.uid, { defaultTemplateKey: e.target.value })} disabled={templates.isLoading}>
                    <option value="">Choose at run time</option>
                    {published.map((t) => (
                      <option key={t.template_key} value={t.template_key}>{templateLabel(t.template_key)}</option>
                    ))}
                  </Select>
                </FormField>
                <FormField label="File name pattern" hint="Routes files from a watched folder or archive, e.g. accounts_*.csv">
                  <Input value={s.filePattern} onChange={(e) => update(s.uid, { filePattern: e.target.value })} placeholder="accounts_*.csv" />
                </FormField>
                <div className="flex items-end">
                  <Button variant="ghost" aria-label={`Remove step ${s.key || i + 1}`} onClick={() => remove(s.uid)} disabled={steps.length === 1}>
                    <Trash2 className="h-4 w-4" aria-hidden />
                  </Button>
                </div>
              </div>
              <div className="mt-4 grid gap-4 lg:grid-cols-[1fr_2.3fr]">
                <FormField label="Dependents may start when">
                  <Select value={s.rule} onChange={(e) => update(s.uid, { rule: e.target.value as CompletionRule })}>
                    <option value="strict">Every record succeeded</option>
                    <option value="max_failed_pct">Failures are within a percentage</option>
                  </Select>
                </FormField>
                {s.rule === "max_failed_pct" ? (
                  <FormField label="Allowed failed records (%)" hint="Failed and unresolved records both count. A run that didn't finish never passes.">
                    <Input type="number" min={0} max={100} step="0.1" value={s.maxPct} onChange={(e) => update(s.uid, { maxPct: e.target.value })} className="max-w-[10rem]" />
                  </FormField>
                ) : (
                  <p className="self-end pb-2 text-sm text-slate-500">If any record fails, the steps that wait for this one stay blocked until it is re-run successfully.</p>
                )}
              </div>
              {others.length ? (
                <fieldset className="mt-4">
                  <legend className="text-sm font-medium text-slate-700">Waits for</legend>
                  <div className="mt-1.5 flex flex-wrap gap-2">
                    {others.map((o) => {
                      const checked = s.dependsOn.includes(o.uid);
                      return (
                        <label key={o.uid} className="inline-flex cursor-pointer items-center gap-1.5 rounded border border-slate-300 px-2 py-1 text-sm has-[:checked]:border-brand-500 has-[:checked]:bg-brand-50">
                          <input type="checkbox" checked={checked} onChange={() => (checked ? disconnect(o.uid, s.uid) : connect(o.uid, s.uid))} />
                          <span className="font-mono text-[13px]">{o.key || "unnamed step"}</span>
                        </label>
                      );
                    })}
                  </div>
                </fieldset>
              ) : null}
            </Card>
          );
        })}
        <Button variant="secondary" onClick={() => setSteps((all) => [...all, blankStep()])}>
          <Plus className="h-4 w-4" aria-hidden /> Add step
        </Button>
      </section>

      {planProblem ? <p role="alert" className="text-sm text-rose-700">{planProblem}</p> : null}
      <div className="flex flex-wrap gap-2 border-t border-slate-200 pt-4">
        <Button onClick={() => save.mutate(true)} disabled={save.isPending}>Publish plan</Button>
        <Button variant="secondary" onClick={() => save.mutate(false)} disabled={save.isPending}>Save draft</Button>
        <span className="self-center text-sm text-slate-500">Files can run against a plan once it is published. Changing a published plan creates its next version.</span>
      </div>
    </div>
  );
}

export type { Plan };
