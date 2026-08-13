"use client";

import { useMutation, useQuery } from "@tanstack/react-query";
import { Check, Clock3, Info } from "lucide-react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import { useMemo, useState } from "react";
import { toast } from "sonner";

import { Breadcrumbs, Button, Card, FormField, Input, PageHeader, Select } from "@/components/ui";
import { apiFetch } from "@/lib/api";
import { previewCron } from "@/lib/cron";

type Template = { id: number; name: string; published: boolean; version: number };
type Connector = { id: number; name: string; connector_kind: string };

const OVERLAPS = ["skip", "buffer_one", "buffer_all", "cancel_other", "allow_all"] as const;
const OVERLAP_HELP: Record<(typeof OVERLAPS)[number], string> = {
  skip: "Skip a new run when the previous run is still active. Safest default.",
  buffer_one: "Queue at most one missed run, then discard additional overlaps.",
  buffer_all: "Queue every overlapping run. Can create a large backlog.",
  cancel_other: "Cancel the active run and start the newest occurrence.",
  allow_all: "Run occurrences concurrently. Use only when destinations are concurrency-safe."
};
const PRESETS = [
  { label: "Every hour", cron: "0 * * * *" },
  { label: "Daily at 2:00", cron: "0 2 * * *" },
  { label: "Weekdays at 6:00", cron: "0 6 * * 1-5" },
  { label: "Weekly on Sunday", cron: "0 2 * * 0" }
];

export default function NewSchedulePage() {
  const router = useRouter();
  const [name, setName] = useState("");
  const [templateId, setTemplateId] = useState<number | null>(null);
  const [connectorId, setConnectorId] = useState<number | null>(null);
  const [cron, setCron] = useState("0 2 * * *");
  const [timezone, setTimezone] = useState("UTC");
  const [overlap, setOverlap] = useState<(typeof OVERLAPS)[number]>("skip");
  const [catchup, setCatchup] = useState(3600);
  const [submitted, setSubmitted] = useState(false);

  const templates = useQuery({
    queryKey: ["templates"],
    queryFn: () => apiFetch<{ items: Template[] }>("/rule-templates")
  });
  const connectors = useQuery({
    queryKey: ["connectors"],
    queryFn: () => apiFetch<{ items: Connector[] }>("/connectors")
  });

  const nextFive = useMemo(() => previewCron(cron, timezone), [cron, timezone]);

  const create = useMutation({
    mutationFn: async () => {
      setSubmitted(true);
      if (!name.trim() || !templateId || !connectorId || nextFive.length === 0) throw new Error("Review the required fields.");
      const tpl = (templates.data?.items ?? []).find((t) => t.id === templateId);
      if (!tpl?.published) throw new Error("select a published template");
      return apiFetch<{ id: number }>("/schedules", {
        method: "POST",
        body: JSON.stringify({
          name: name.trim(),
          rule_template_id: templateId,
          rule_template_version: tpl.version,
          connector_id: connectorId,
          spec: { cron },
          timezone,
          overlap_policy: overlap,
          catchup_window_seconds: catchup,
          enabled: true
        })
      });
    },
    onSuccess: (s) => {
      toast.success(`Schedule #${s.id} created`);
      router.push(`/schedules/${s.id}`);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const published = (templates.data?.items ?? []).filter((t) => t.published);
  const selectedTemplate = published.find((item) => item.id === templateId);
  const selectedConnector = connectors.data?.items.find((item) => item.id === connectorId);
  const invalidCron = submitted && nextFive.length === 0;

  return (
    <div className="mx-auto max-w-4xl space-y-6">
      <PageHeader
        title="Create schedule"
        description="Choose a published migration definition and source, then decide when and how recurring runs should start."
        eyebrow={<Breadcrumbs items={[{ label: "Schedules", href: "/schedules" }, { label: "Create" }]} />}
      />

      <Card>
        <div className="mb-5 flex items-start gap-3">
          <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-brand-50 text-sm font-semibold text-brand-700">1</span>
          <div><h2 className="font-semibold">Migration definition</h2><p className="text-sm text-slate-500">Schedules pin the selected published version so future drafts cannot change runs unexpectedly.</p></div>
        </div>
        <div className="space-y-4">
          <FormField label="Schedule name" required error={submitted && !name.trim() ? "Enter a schedule name." : undefined}>
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="Nightly customer import" />
          </FormField>
          <div className="grid gap-4 sm:grid-cols-2">
          <FormField label="Published template" required error={submitted && !templateId ? "Select a template." : undefined} hint={published.length ? "Only published versions can be scheduled." : "No published templates are available."}>
            <Select
              value={templateId ?? ""}
              onChange={(e) => setTemplateId(e.target.value ? Number(e.target.value) : null)}
            >
              <option value="">Select a template…</option>
              {published.map((t) => (
                <option key={t.id} value={t.id}>{t.name} (v{t.version})</option>
              ))}
            </Select>
          </FormField>
          <FormField label="Source connector" required error={submitted && !connectorId ? "Select a connector." : undefined} hint="Compatibility metadata is not available from the API; verify the connector matches the template source.">
            <Select
              value={connectorId ?? ""}
              onChange={(e) => setConnectorId(e.target.value ? Number(e.target.value) : null)}
            >
              <option value="">Select a connector…</option>
              {connectors.data?.items?.map((c) => (
                <option key={c.id} value={c.id}>{c.name} ({c.connector_kind})</option>
              ))}
            </Select>
          </FormField>
          </div>
          {published.length === 0 ? <p className="text-sm text-amber-800">Create and publish a <Link className="font-medium underline" href="/templates/new">template</Link> before scheduling it.</p> : null}
        </div>
      </Card>

      <Card>
        <div className="mb-5 flex items-start gap-3">
          <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-brand-50 text-sm font-semibold text-brand-700">2</span>
          <div><h2 className="font-semibold">Run timing</h2><p className="text-sm text-slate-500">Pick a preset or enter a five-field POSIX cron expression.</p></div>
        </div>
        <div className="mb-4 flex flex-wrap gap-2" aria-label="Schedule presets">
          {PRESETS.map((preset) => <Button key={preset.cron} size="sm" variant={cron === preset.cron ? "default" : "secondary"} onClick={() => setCron(preset.cron)}>{cron === preset.cron ? <Check className="h-3.5 w-3.5" /> : null}{preset.label}</Button>)}
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <FormField label="Cron expression" required error={invalidCron ? "Enter a valid supported five-field expression." : undefined} hint="Format: minute hour day-of-month month day-of-week">
            <Input
              className="font-mono"
              value={cron}
              onChange={(e) => setCron(e.target.value)}
              placeholder="m h dom mon dow"
            />
          </FormField>
          <FormField label="Timezone" required hint="Use an IANA timezone such as UTC or America/New_York.">
            <Input className="mt-1" value={timezone} onChange={(e) => setTimezone(e.target.value)} />
          </FormField>
        </div>
        <div className="mt-4 rounded-lg border border-slate-200 bg-slate-50 p-4">
          <div className="mb-2 flex items-center gap-2 text-sm font-medium text-slate-700"><Clock3 className="h-4 w-4" />Upcoming runs</div>
          {nextFive.length === 0 ? <div className="text-sm text-rose-700">Preview unavailable for this expression or timezone.</div> : (
            <ol className="grid gap-x-6 gap-y-1 text-sm text-slate-600 sm:grid-cols-2">{nextFive.map((date, index) => <li key={date}>{index + 1}. {date}</li>)}</ol>
          )}
        </div>
      </Card>

      <Card>
        <div className="mb-5 flex items-start gap-3">
          <span className="flex h-7 w-7 shrink-0 items-center justify-center rounded-full bg-brand-50 text-sm font-semibold text-brand-700">3</span>
          <div><h2 className="font-semibold">Missed and overlapping runs</h2><p className="text-sm text-slate-500">Control what happens when runs take longer than expected or the scheduler is unavailable.</p></div>
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <FormField label="Overlap policy" hint={OVERLAP_HELP[overlap]}>
            <Select value={overlap} onChange={(e) => setOverlap(e.target.value as (typeof OVERLAPS)[number])}>
              <option value="skip">Skip new run</option><option value="buffer_one">Queue one run</option><option value="buffer_all">Queue every run</option><option value="cancel_other">Cancel active run</option><option value="allow_all">Allow concurrent runs</option>
            </Select>
          </FormField>
          <FormField label="Catch-up window" hint="After downtime, runs older than this window are skipped.">
            <Select value={catchup} onChange={(e) => setCatchup(Number(e.target.value))}>
              <option value={0}>Do not catch up</option><option value={900}>15 minutes</option><option value={3600}>1 hour</option><option value={21600}>6 hours</option><option value={86400}>24 hours</option>
            </Select>
          </FormField>
        </div>
        <div className="mt-4 flex gap-2 rounded border border-blue-200 bg-blue-50 p-3 text-xs leading-5 text-blue-900"><Info className="mt-0.5 h-4 w-4 shrink-0" />The schedule starts enabled. You can pause it later without deleting its configuration.</div>
      </Card>

      <div className="flex flex-col-reverse justify-between gap-3 border-t border-slate-200 pt-5 sm:flex-row sm:items-center">
        <Button variant="ghost" onClick={() => router.push("/schedules")}>Cancel</Button>
        <div className="flex flex-col items-end gap-2">
          {selectedTemplate && selectedConnector ? <span className="text-xs text-slate-500">Will use {selectedTemplate.name} v{selectedTemplate.version} with {selectedConnector.name}</span> : null}
          <Button onClick={() => create.mutate()} disabled={create.isPending || templates.isLoading || connectors.isLoading}>
            {create.isPending ? "Creating…" : "Create enabled schedule"}
          </Button>
        </div>
      </div>
    </div>
  );
}
