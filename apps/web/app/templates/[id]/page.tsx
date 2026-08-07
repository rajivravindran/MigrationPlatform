"use client";

import MonacoEditor from "@monaco-editor/react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  FileUp,
  Pencil,
  Plus,
  Sparkles,
  Trash2,
  Wand2
} from "lucide-react";
import { useRouter } from "next/navigation";
import { useCallback, useMemo, useRef, useState } from "react";
import type { Edge, Node } from "reactflow";
import { toast } from "sonner";

import { AddFieldDialog, ConfirmDialog, Dialog } from "@/components/mapper/Dialog";
import { MappingCanvas } from "@/components/mapper/MappingCanvas";
import { MappingList, buildMappingRows } from "@/components/mapper/MappingList";
import {
  NODE_KINDS,
  bareName,
  kindOfNode,
  layoutY,
  nodeIdFor,
  parseRespNode,
  respNodeId,
  type FieldNodeData,
  type NodeKind,
  type StepUI,
  type TargetKind
} from "@/components/mapper/types";
import { Badge, Button, Card, Input, Textarea } from "@/components/ui";
import { apiFetch, getToken } from "@/lib/api";

type ValueExpr =
  | string
  | { $from: string }
  | { $literal: string }
  | { $fromResponse: string; path: string };

type Destination = {
  url: string;
  method: string;
  type?: string;
  pathParams?: Record<string, ValueExpr>;
  queryParams?: Record<string, ValueExpr | ValueExpr[]>;
  headers?: Record<string, ValueExpr>;
};

type StepDef = {
  name: string;
  description?: string;
  mapping?: { payload: Record<string, unknown> };
  destination: Destination;
  onFailure?: "stop" | "continue";
};

type TemplateSchema = {
  id?: string;
  version?: number;
  name?: string;
  source: { type: string; schema?: { name: string; type: string }[]; options?: Record<string, unknown> };
  preprocess?: unknown[];
  mapping?: { payload: Record<string, unknown> };
  destination?: Destination;
  steps?: StepDef[];
};

type Template = {
  id: number;
  name: string;
  version: number;
  published: boolean;
  schema_json: TemplateSchema;
};

const STEP_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

function makeFieldNode(
  kind: NodeKind,
  name: string,
  index: number,
  opts?: { label?: string; fieldType?: string }
): Node<FieldNodeData> {
  return {
    id: nodeIdFor(kind, name),
    type: "field",
    position: { x: NODE_KINDS[kind].x, y: layoutY(index) },
    data: {
      label: opts?.label ?? name,
      kind,
      fieldName: opts?.label ?? name,
      fieldType: opts?.fieldType
    }
  };
}

function makeRespNode(step: string, path: string, index: number): Node<FieldNodeData> {
  return {
    id: respNodeId(step, path),
    type: "field",
    position: { x: NODE_KINDS.resp.x, y: layoutY(index) },
    data: {
      label: `${step} → ${path}`,
      kind: "resp",
      fieldName: path,
      fieldType: `from ${step}`
    }
  };
}

function schemaSteps(schema: TemplateSchema): StepDef[] {
  if (schema.steps && schema.steps.length > 0) return schema.steps;
  return [
    {
      name: "main",
      mapping: schema.mapping ?? { payload: {} },
      destination: schema.destination ?? { url: "", method: "POST", type: "http" }
    }
  ];
}

function initSrcNodes(schema: TemplateSchema): Node<FieldNodeData>[] {
  const source = schema.source.schema ?? [];
  return source.map((field, i) =>
    makeFieldNode("src", field.name, i, { fieldType: field.type, label: field.name })
  );
}

function initStepUI(def: StepDef, srcCount: number): StepUI {
  const nodes: Node<FieldNodeData>[] = [];
  const edges: Edge[] = [];
  const respSeen = new Map<string, number>();

  const bind = (
    kind: TargetKind,
    bindings: Record<string, ValueExpr | ValueExpr[]>,
    decorate?: (n: string) => string
  ) => {
    Object.keys(bindings).forEach((key) => {
      const idx = nodes.filter((n) => kindOfNode(n.id) === kind).length;
      nodes.push(
        makeFieldNode(kind, key, idx, {
          label: decorate ? decorate(key) : key
        })
      );
      const exprs = Array.isArray(bindings[key])
        ? (bindings[key] as ValueExpr[])
        : [bindings[key] as ValueExpr];
      exprs.forEach((expr, j) => {
        if (typeof expr !== "object" || expr === null) return;
        if ("$from" in expr) {
          edges.push({
            id: `e-${kind}-${expr.$from}-${key}-${j}`,
            source: nodeIdFor("src", expr.$from),
            target: nodeIdFor(kind, key),
            animated: true
          });
        } else if ("$fromResponse" in expr) {
          const rid = respNodeId(expr.$fromResponse, expr.path);
          if (!respSeen.has(rid)) {
            respSeen.set(rid, respSeen.size);
            nodes.push(makeRespNode(expr.$fromResponse, expr.path, srcCount + respSeen.get(rid)!));
          }
          edges.push({
            id: `e-${kind}-resp-${key}-${j}`,
            source: rid,
            target: nodeIdFor(kind, key),
            animated: true
          });
        }
      });
    });
  };

  bind("dst", (def.mapping?.payload ?? {}) as Record<string, ValueExpr | ValueExpr[]>);
  bind("path", def.destination.pathParams ?? {}, (n) => `{${n}}`);
  bind("query", def.destination.queryParams ?? {}, (n) => `?${n}`);
  bind("hdr", def.destination.headers ?? {});

  return {
    name: def.name,
    onFailure: def.onFailure,
    nodes,
    edges,
    url: def.destination.url ?? "",
    method: def.destination.method ?? "POST",
    responseSchema: null
  };
}

function deriveStepJson(step: StepUI): StepDef {
  const collect = (kind: TargetKind) => {
    const buckets: Record<string, ValueExpr[]> = {};
    step.edges.forEach((edge) => {
      if (kindOfNode(edge.target) !== kind) return;
      const key = bareName(edge.target);
      if (edge.source.startsWith("src:")) {
        (buckets[key] ??= []).push({ $from: bareName(edge.source) });
      } else if (edge.source.startsWith("resp:")) {
        const parsed = parseRespNode(edge.source);
        if (parsed) (buckets[key] ??= []).push({ $fromResponse: parsed.step, path: parsed.path });
      }
    });
    const out: Record<string, ValueExpr | ValueExpr[]> = {};
    step.nodes
      .filter((n) => kindOfNode(n.id) === kind)
      .forEach((n) => {
        const key = bareName(n.id);
        const exprs = buckets[key] ?? [];
        if (kind === "query" && exprs.length > 1) out[key] = exprs;
        else if (exprs.length >= 1) out[key] = exprs[0];
      });
    return out;
  };

  const payload = collect("dst");
  const pathParams = collect("path") as Record<string, ValueExpr>;
  const queryParams = collect("query");
  const headers = collect("hdr") as Record<string, ValueExpr>;

  const destination: Destination = { type: "http", method: step.method, url: step.url };
  if (Object.keys(pathParams).length) destination.pathParams = pathParams;
  if (Object.keys(queryParams).length) destination.queryParams = queryParams;
  if (Object.keys(headers).length) destination.headers = headers;

  return {
    name: step.name,
    mapping: { payload },
    destination,
    ...(step.onFailure && step.onFailure !== "stop" ? { onFailure: step.onFailure } : {})
  };
}

function deriveTemplateJson(template: Template, srcNodes: Node<FieldNodeData>[], steps: StepUI[]): TemplateSchema {
  const sourceFields = srcNodes
    .filter((n) => n.id !== "src:__hint")
    .map((n) => ({
      name: bareName(n.id),
      type: n.data.fieldType ?? "string"
    }));

  const base = {
    id: template.schema_json.id ?? `rt_${template.id}`,
    version: template.schema_json.version ?? template.version,
    name: template.name,
    source: {
      type: template.schema_json.source.type ?? "csv",
      schema: sourceFields,
      options: template.schema_json.source.options ?? { header: true, delimiter: "," }
    },
    preprocess: template.schema_json.preprocess ?? []
  };

  // Always persist steps[] so step names (including the first) survive save.
  // Legacy flat mapping+destination is still accepted on load via schemaSteps().
  return { ...base, steps: steps.map(deriveStepJson) };
}

type SpecSummary = { id: number; name: string; source_url?: string };
type Operation = {
  operation_id: string;
  method: string;
  path: string;
  url: string;
  summary: string;
  parameters: { name: string; in: string; required: boolean }[];
  request_schema: { properties?: Record<string, unknown>; required?: string[] } | null;
  response_schema: { properties?: Record<string, unknown> } | null;
};

type SampleColumn = { name: string; type: string; nullable: boolean };
type SampleResponse = {
  format: string;
  columns: SampleColumn[];
  rows: Record<string, unknown>[];
  row_count: number;
  truncated: boolean;
  bytes_read: number;
  source: { bucket: string; key: string };
};
type SampleSummary = { filename: string; format: string; rowCount: number; truncated: boolean };

function sniffFormat(filename: string): string | null {
  const lower = filename.toLowerCase();
  if (lower.endsWith(".csv") || lower.endsWith(".tsv")) return "csv";
  if (lower.endsWith(".ndjson") || lower.endsWith(".jsonl")) return "ndjson";
  if (lower.endsWith(".json")) return "json";
  if (lower.endsWith(".xml")) return "xml";
  return null;
}

type DialogState =
  | { kind: "none" }
  | { kind: "add-source" }
  | { kind: "add-target"; targetKind: TargetKind }
  | { kind: "add-resp" }
  | { kind: "add-step" }
  | { kind: "rename-step"; index: number }
  | { kind: "import-spec"; stage: "url" | "name"; url?: string }
  | { kind: "confirm-sample"; sample: SampleResponse; filename: string; existing: number }
  | { kind: "confirm-remove-step"; index: number };

function Designer({ template }: { template: Template }) {
  const client = useQueryClient();
  const router = useRouter();

  const [srcNodes, setSrcNodes] = useState<Node<FieldNodeData>[]>(() => initSrcNodes(template.schema_json));
  const [steps, setSteps] = useState<StepUI[]>(() => {
    const src = initSrcNodes(template.schema_json).length;
    return schemaSteps(template.schema_json).map((s) => initStepUI(s, src));
  });
  const [active, setActive] = useState(0);
  const [dryRunRows, setDryRunRows] = useState('[{"email":"a@b.com","country":"usa"}]');
  const [sampleSummary, setSampleSummary] = useState<SampleSummary | null>(null);
  const [llmInstructions, setLlmInstructions] = useState("");
  const [specId, setSpecId] = useState<number | null>(null);
  const [operationId, setOperationId] = useState<string>("");
  const [dialog, setDialog] = useState<DialogState>({ kind: "none" });
  const [respStep, setRespStep] = useState("");
  const [respPath, setRespPath] = useState("$.id");
  const [importUrl, setImportUrl] = useState("");
  const [importName, setImportName] = useState("");
  const fileInputRef = useRef<HTMLInputElement>(null);

  const step = steps[active];

  const updateStep = useCallback(
    (idx: number, patch: Partial<StepUI> | ((s: StepUI) => StepUI)) => {
      setSteps((ss) =>
        ss.map((s, i) => (i === idx ? (typeof patch === "function" ? patch(s) : { ...s, ...patch }) : s))
      );
    },
    []
  );

  const mappingRows = useMemo(
    () => buildMappingRows(step.edges, srcNodes, step.nodes),
    [step.edges, srcNodes, step.nodes]
  );

  const applySample = useCallback((sample: SampleResponse, filename: string) => {
    setSrcNodes(
      sample.columns.map((col, i) =>
        makeFieldNode("src", col.name, i, { fieldType: col.type, label: col.name })
      )
    );
    const valid = new Set(sample.columns.map((c) => nodeIdFor("src", c.name)));
    setSteps((ss) =>
      ss.map((s) => ({ ...s, edges: s.edges.filter((e) => !e.source.startsWith("src:") || valid.has(e.source)) }))
    );
    setDryRunRows(JSON.stringify(sample.rows.slice(0, 5), null, 2));
    setSampleSummary({ filename, format: sample.format, rowCount: sample.row_count, truncated: sample.truncated });
  }, []);

  const sample = useMutation({
    mutationFn: async (file: File) => {
      const format = sniffFormat(file.name);
      if (!format) throw new Error("Unsupported file type. Use .csv, .json, .ndjson, or .xml");
      const form = new FormData();
      form.append("file", file);
      const uploadRes = await fetch("/api/files", {
        method: "POST",
        headers: { authorization: `Bearer ${getToken() ?? ""}` },
        body: form
      });
      if (!uploadRes.ok) throw new Error(`upload failed: ${await uploadRes.text()}`);
      const { source_ref } = (await uploadRes.json()) as { source_ref: { bucket: string; key: string } };
      return apiFetch<SampleResponse>("/files/sample", {
        method: "POST",
        body: JSON.stringify({ bucket: source_ref.bucket, key: source_ref.key, format, rows: 25 })
      }).then((res) => ({ res, filename: file.name }));
    },
    onSuccess: ({ res, filename }) => {
      const existing = srcNodes.filter((n) => n.id !== "src:__hint").length;
      if (existing > 0) {
        setDialog({ kind: "confirm-sample", sample: res, filename, existing });
        return;
      }
      applySample(res, filename);
      toast.success(
        `Loaded ${res.columns.length} columns from ${filename}` + (res.truncated ? " (sample truncated)" : "")
      );
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const onSampleFileChosen = (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    if (file) sample.mutate(file);
    if (fileInputRef.current) fileInputRef.current.value = "";
  };

  const addSourceField = (name: string) => {
    const id = nodeIdFor("src", name);
    if (srcNodes.some((n) => n.id === id)) return void toast.error("Field already exists");
    setSrcNodes((ns) => [...ns, makeFieldNode("src", name, ns.length, { label: name, fieldType: "string" })]);
  };

  const addTargetKey = (kind: TargetKind, name: string) => {
    const decorate =
      kind === "path" ? (n: string) => `{${n}}` : kind === "query" ? (n: string) => `?${n}` : undefined;
    const id = nodeIdFor(kind, name);
    if (step.nodes.some((n) => n.id === id)) return void toast.error("Key already exists");
    const idx = step.nodes.filter((n) => kindOfNode(n.id) === kind).length;
    updateStep(active, (s) => ({
      ...s,
      nodes: [...s.nodes, makeFieldNode(kind, name, idx, { label: decorate ? decorate(name) : name })]
    }));
  };

  const addResponseRef = (stepName: string, path: string) => {
    if (active === 0) return void toast.error("Response refs are only available from step 2 onward");
    const earlier = steps.slice(0, active).map((s) => s.name);
    if (!earlier.includes(stepName)) return void toast.error(`"${stepName}" is not an earlier step`);
    if (!path.startsWith("$")) return void toast.error("JSONPath must start with $");
    const id = respNodeId(stepName, path);
    if (step.nodes.some((n) => n.id === id)) return void toast.error("Reference already exists");
    const respCount = step.nodes.filter((n) => n.id.startsWith("resp:")).length;
    updateStep(active, (s) => ({
      ...s,
      nodes: [...s.nodes, makeRespNode(stepName, path, srcNodes.length + respCount)]
    }));
  };

  const addStep = (name: string) => {
    if (!STEP_NAME_RE.test(name)) return void toast.error("Step names must match [A-Za-z_][A-Za-z0-9_]*");
    if (steps.some((s) => s.name === name)) return void toast.error("Step name already used");
    setSteps((ss) => [...ss, { name, nodes: [], edges: [], url: "", method: "POST", responseSchema: null }]);
    setActive(steps.length);
  };

  const removeStep = (idx: number) => {
    if (steps.length === 1) return void toast.error("A template needs at least one step");
    const name = steps[idx].name;
    const referenced = steps.some(
      (s, i) => i !== idx && s.nodes.some((n) => parseRespNode(n.id)?.step === name)
    );
    if (referenced) return void toast.error(`Later steps reference "${name}"'s response; remove those refs first`);
    setSteps((ss) => ss.filter((_, i) => i !== idx));
    setActive((a) => Math.max(0, a > idx ? a - 1 : Math.min(a, steps.length - 2)));
  };

  const renameStep = (idx: number, newName: string) => {
    if (!STEP_NAME_RE.test(newName)) return void toast.error("Step names must match [A-Za-z_][A-Za-z0-9_]*");
    const oldName = steps[idx].name;
    if (newName === oldName) return;
    if (steps.some((s, i) => i !== idx && s.name === newName)) {
      return void toast.error("Step name already used");
    }
    const referenced = steps.some(
      (s, i) => i !== idx && s.nodes.some((n) => parseRespNode(n.id)?.step === oldName)
    );
    if (referenced) {
      return void toast.error(
        `Later steps reference "${oldName}" via Response refs; update or remove those refs before renaming`
      );
    }
    setSteps((ss) => ss.map((s, i) => (i === idx ? { ...s, name: newName } : s)));
    toast.success(`Renamed step to ${newName}`);
  };

  const specsQ = useQuery({
    queryKey: ["openapi-specs"],
    queryFn: () => apiFetch<{ items: SpecSummary[] }>("/openapi-specs")
  });
  const opsQ = useQuery({
    queryKey: ["openapi-operations", specId],
    queryFn: () => apiFetch<{ items: Operation[] }>(`/openapi-specs/${specId}/operations`),
    enabled: specId !== null
  });

  const importSpec = useMutation({
    mutationFn: async ({ url, name }: { url: string; name: string }) =>
      apiFetch<{ id: number; operation_count: number }>("/openapi-specs", {
        method: "POST",
        body: JSON.stringify({ name, url })
      }),
    onSuccess: (res) => {
      toast.success(`Imported spec with ${res.operation_count} operations`);
      client.invalidateQueries({ queryKey: ["openapi-specs"] });
      setSpecId(res.id);
      setDialog({ kind: "none" });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const applyOperation = () => {
    const op = opsQ.data?.items.find((o) => o.operation_id === operationId);
    if (!op) return void toast.error("Pick an operation first");
    if (!op.url.startsWith("http://") && !op.url.startsWith("https://")) {
      return void toast.error(
        `Operation URL is not absolute: ${op.url}. Re-import the spec or set the URL manually.`
      );
    }
    updateStep(active, (s) => {
      const nodes = [...s.nodes];
      const have = new Set(nodes.map((n) => n.id));
      const push = (kind: TargetKind, name: string, decorate?: (n: string) => string) => {
        const id = nodeIdFor(kind, name);
        if (have.has(id)) return;
        have.add(id);
        const idx = nodes.filter((n) => kindOfNode(n.id) === kind).length;
        nodes.push(makeFieldNode(kind, name, idx, { label: decorate ? decorate(name) : name }));
      };
      op.parameters.forEach((p) => {
        if (p.in === "path") push("path", p.name, (n) => `{${n}}`);
        else if (p.in === "query" && p.required) push("query", p.name, (n) => `?${n}`);
      });
      const props = Object.keys(op.request_schema?.properties ?? {});
      const required = new Set(op.request_schema?.required ?? []);
      [...props]
        .sort((a, b) => Number(required.has(b)) - Number(required.has(a)))
        .slice(0, 24)
        .forEach((name) => push("dst", name));
      return { ...s, url: op.url, method: op.method, nodes, responseSchema: op.response_schema ?? null };
    });
    toast.success(`Applied ${op.method} ${op.path} to step "${step.name}"`);
  };

  const suggest = useMutation({
    mutationFn: async () => {
      const columns = srcNodes
        .filter((n) => n.id !== "src:__hint")
        .map((n) => ({ name: bareName(n.id), type: n.data.fieldType ?? "string" }));
      if (columns.length === 0) throw new Error("Add source fields (or sample a file) first");
      let rows: unknown[] = [];
      try {
        rows = JSON.parse(dryRunRows);
      } catch {
        rows = [];
      }
      return apiFetch<{ template: TemplateSchema; model: string; redacted: boolean }>("/mapping-suggestions", {
        method: "POST",
        body: JSON.stringify({
          columns,
          sample_rows: rows,
          spec_id: specId,
          operation_id: operationId || null,
          instructions: llmInstructions || null,
          source_type: template.schema_json.source.type ?? "csv"
        })
      });
    },
    onSuccess: (res) => {
      const src = initSrcNodes(res.template).length;
      setSrcNodes(initSrcNodes(res.template));
      setSteps(schemaSteps(res.template).map((s) => initStepUI(s, src)));
      setActive(0);
      toast.success(`Applied suggestion from ${res.model}${res.redacted ? " (samples were PII-redacted)" : ""}`);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const templateJson = useMemo(
    () => deriveTemplateJson(template, srcNodes, steps),
    [template, srcNodes, steps]
  );

  const save = useMutation({
    mutationFn: async () =>
      apiFetch<{ id: number; version: number }>(`/rule-templates/${template.id}/versions`, {
        method: "POST",
        body: JSON.stringify({ name: template.name, schema_json: templateJson })
      }),
    onSuccess: (row) => {
      toast.success(`Saved as v${row.version} (draft). Publish it to use it in jobs.`);
      client.invalidateQueries({ queryKey: ["templates"] });
      router.replace(`/templates/${row.id}`);
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const publish = useMutation({
    mutationFn: async () => apiFetch(`/rule-templates/${template.id}/publish`, { method: "POST" }),
    onSuccess: () => {
      toast.success("Template published");
      client.invalidateQueries({ queryKey: ["template", String(template.id)] });
      client.invalidateQueries({ queryKey: ["templates"] });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const dryRun = useMutation({
    mutationFn: async () => {
      const parsed = JSON.parse(dryRunRows);
      return apiFetch<{ previews: unknown[] }>("/dry-run", {
        method: "POST",
        body: JSON.stringify({ template: templateJson, rows: parsed })
      });
    },
    onError: (err) => toast.error(err instanceof Error ? err.message : String(err))
  });

  const targetPrompts: Record<TargetKind, { title: string; placeholder: string; hint: string }> = {
    dst: {
      title: "Add payload key",
      placeholder: "contactEmail",
      hint: "Appears in the JSON request body."
    },
    path: {
      title: "Add path parameter",
      placeholder: "userId",
      hint: "Must match a {name} segment in the URL."
    },
    query: {
      title: "Add query parameter",
      placeholder: "region",
      hint: "Appended to the URL as ?name=…"
    },
    hdr: {
      title: "Add header",
      placeholder: "X-Tenant-Id",
      hint: "Sent as an HTTP request header."
    }
  };

  const earlierSteps = steps.slice(0, active).map((s) => s.name);

  return (
    <div className="space-y-4">
      {/* Top bar */}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <h1 className="text-xl font-semibold tracking-tight text-slate-900">{template.name}</h1>
          <Badge tone={template.published ? "ok" : "warn"}>{template.published ? "published" : "draft"}</Badge>
          <span className="text-sm text-slate-500">v{template.version}</span>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <input
            ref={fileInputRef}
            type="file"
            accept=".csv,.tsv,.json,.ndjson,.jsonl,.xml"
            className="hidden"
            onChange={onSampleFileChosen}
          />
          <Button variant="ghost" onClick={() => fileInputRef.current?.click()} disabled={sample.isPending}>
            <FileUp className="mr-1.5 h-3.5 w-3.5" />
            {sample.isPending ? "Sampling…" : "Sample file"}
          </Button>
          <Button variant="ghost" onClick={() => dryRun.mutate()} disabled={dryRun.isPending}>
            Dry run
          </Button>
          <Button onClick={() => save.mutate()} disabled={save.isPending}>
            Save version
          </Button>
          <Button onClick={() => publish.mutate()} disabled={publish.isPending || template.published}>
            Publish
          </Button>
        </div>
      </div>

      {sampleSummary ? (
        <div className="flex items-center gap-2 text-xs text-slate-600">
          <Badge tone={sampleSummary.truncated ? "warn" : "ok"}>{sampleSummary.format.toUpperCase()}</Badge>
          <span>
            Sampled {sampleSummary.rowCount} row{sampleSummary.rowCount === 1 ? "" : "s"} from{" "}
            <code className="rounded bg-slate-100 px-1.5 py-0.5">{sampleSummary.filename}</code>
            {sampleSummary.truncated ? " · file was larger than the 8 MiB sample window" : ""}
          </span>
        </div>
      ) : null}

      <div className="grid grid-cols-1 gap-4 xl:grid-cols-[minmax(0,1fr)_360px]">
        {/* Main mapper */}
        <Card className="!p-0 overflow-hidden">
          {/* Step tabs */}
          <div
            className="flex items-center gap-1 border-b border-slate-200 bg-white px-3 pt-3"
            data-testid="step-tabs"
          >
            {steps.map((s, i) => (
              <div key={s.name} className="flex items-center">
                <button
                  type="button"
                  className={
                    "rounded-t-lg px-3 py-2 text-sm font-medium transition " +
                    (i === active
                      ? "bg-brand-50 text-brand-700 ring-1 ring-inset ring-brand-200"
                      : "text-slate-500 hover:bg-slate-50")
                  }
                  onClick={() => setActive(i)}
                >
                  <span className="mr-1.5 inline-flex h-5 w-5 items-center justify-center rounded-full bg-slate-200 text-[11px] font-semibold text-slate-600">
                    {i + 1}
                  </span>
                  {s.name}
                </button>
                <button
                  type="button"
                  className="mb-1 ml-0.5 rounded p-1 text-slate-400 hover:bg-slate-100 hover:text-brand-700"
                  title={`Rename step ${s.name}`}
                  onClick={() => setDialog({ kind: "rename-step", index: i })}
                >
                  <Pencil className="h-3.5 w-3.5" />
                </button>
                {steps.length > 1 ? (
                  <button
                    type="button"
                    className="mb-1 ml-0.5 rounded p-1 text-slate-400 hover:bg-rose-50 hover:text-rose-600"
                    title={`Remove step ${s.name}`}
                    onClick={() => setDialog({ kind: "confirm-remove-step", index: i })}
                  >
                    <Trash2 className="h-3.5 w-3.5" />
                  </button>
                ) : null}
              </div>
            ))}
            <button
              type="button"
              className="mb-1 ml-1 inline-flex items-center gap-1 rounded-lg px-2.5 py-1.5 text-sm text-slate-500 hover:bg-slate-100"
              onClick={() => setDialog({ kind: "add-step" })}
              title="Add a chained call that can reference earlier responses"
            >
              <Plus className="h-3.5 w-3.5" />
              Step
            </button>
          </div>

          {/* Palette */}
          <div className="flex flex-wrap gap-1.5 border-b border-slate-100 bg-slate-50/80 px-3 py-2">
            <Button variant="ghost" className="!bg-white !shadow-sm ring-1 ring-slate-200" onClick={() => setDialog({ kind: "add-source" })}>
              <Plus className="mr-1 h-3.5 w-3.5" /> Source
            </Button>
            <Button variant="ghost" className="!bg-white !shadow-sm ring-1 ring-slate-200" onClick={() => setDialog({ kind: "add-target", targetKind: "dst" })}>
              <Plus className="mr-1 h-3.5 w-3.5" /> Body field
            </Button>
            <Button variant="ghost" className="!bg-white !shadow-sm ring-1 ring-slate-200" onClick={() => setDialog({ kind: "add-target", targetKind: "path" })}>
              <Plus className="mr-1 h-3.5 w-3.5" /> Path
            </Button>
            <Button variant="ghost" className="!bg-white !shadow-sm ring-1 ring-slate-200" onClick={() => setDialog({ kind: "add-target", targetKind: "query" })}>
              <Plus className="mr-1 h-3.5 w-3.5" /> Query
            </Button>
            <Button variant="ghost" className="!bg-white !shadow-sm ring-1 ring-slate-200" onClick={() => setDialog({ kind: "add-target", targetKind: "hdr" })}>
              <Plus className="mr-1 h-3.5 w-3.5" /> Header
            </Button>
            {active > 0 ? (
              <Button
                variant="ghost"
                className="!bg-emerald-50 !text-emerald-800 !shadow-sm ring-1 ring-emerald-200"
                onClick={() => {
                  setRespStep(earlierSteps[earlierSteps.length - 1] ?? "");
                  setRespPath("$.id");
                  setDialog({ kind: "add-resp" });
                }}
              >
                <Plus className="mr-1 h-3.5 w-3.5" /> Response ref
              </Button>
            ) : null}
          </div>

          <div className="h-[560px]">
            <MappingCanvas
              resetKey={`${active}:${step.name}`}
              srcNodes={srcNodes}
              stepNodes={step.nodes}
              edges={step.edges}
              onSrcNodesChange={setSrcNodes}
              onStepNodesChange={(nodes) => updateStep(active, { nodes })}
              onEdgesChange={(edges) => updateStep(active, { edges })}
            />
          </div>

          <div className="border-t border-slate-200 bg-white p-3">
            <div className="mb-2 flex items-center justify-between">
              <h3 className="text-sm font-semibold text-slate-800">
                Mappings
                <span className="ml-2 text-xs font-normal text-slate-500">
                  {mappingRows.length} connection{mappingRows.length === 1 ? "" : "s"}
                </span>
              </h3>
            </div>
            <MappingList
              rows={mappingRows}
              onRemove={(edgeId) =>
                updateStep(active, (s) => ({ ...s, edges: s.edges.filter((e) => e.id !== edgeId) }))
              }
            />
          </div>
        </Card>

        {/* Side panel */}
        <div className="space-y-4">
          <Card>
            <h3 className="mb-3 text-sm font-semibold text-slate-900">
              Destination
              <span className="ml-1.5 font-normal text-slate-500">· {step.name}</span>
            </h3>
            <label className="mb-1 block text-xs font-medium text-slate-600">Method</label>
            <select
              className="mb-3 w-full rounded-md border border-slate-300 px-2.5 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-2 focus:ring-brand-500/20"
              value={step.method}
              onChange={(e) => updateStep(active, { method: e.target.value })}
            >
              {["GET", "POST", "PUT", "PATCH", "DELETE"].map((m) => (
                <option key={m}>{m}</option>
              ))}
            </select>
            <label className="mb-1 block text-xs font-medium text-slate-600">URL</label>
            <Input
              value={step.url}
              onChange={(e) => updateStep(active, { url: e.target.value })}
              placeholder="https://api.example.com/v1/resource"
            />
            <label className="mb-1 mt-3 block text-xs font-medium text-slate-600">
              On failure
            </label>
            <select
              className="w-full rounded-md border border-slate-300 px-2.5 py-2 text-sm focus:border-brand-500 focus:outline-none focus:ring-2 focus:ring-brand-500/20"
              value={step.onFailure ?? "stop"}
              onChange={(e) =>
                updateStep(active, {
                  onFailure: e.target.value === "continue" ? "continue" : "stop"
                })
              }
            >
              <option value="stop">Stop chain (default)</option>
              <option value="continue">Continue to next steps</option>
            </select>
            <p className="mt-1.5 text-xs leading-relaxed text-slate-500">
              Applies after this step definitively fails (4xx or retries exhausted). The row is still
              marked failed if any step failed.
            </p>
          </Card>

          <Card>
            <h3 className="mb-3 flex items-center gap-1.5 text-sm font-semibold text-slate-900">
              <Wand2 className="h-4 w-4 text-brand-600" />
              OpenAPI operation
            </h3>
            <div className="space-y-2">
              <div className="flex gap-2">
                <select
                  className="w-full rounded-md border border-slate-300 px-2.5 py-2 text-sm"
                  value={specId ?? ""}
                  onChange={(e) => {
                    setSpecId(e.target.value ? Number(e.target.value) : null);
                    setOperationId("");
                  }}
                >
                  <option value="">— pick a spec —</option>
                  {(specsQ.data?.items ?? []).map((s) => (
                    <option key={s.id} value={s.id}>
                      {s.name}
                    </option>
                  ))}
                </select>
                <Button
                  variant="ghost"
                  onClick={() => {
                    setImportUrl("");
                    setImportName("");
                    setDialog({ kind: "import-spec", stage: "url" });
                  }}
                  disabled={importSpec.isPending}
                >
                  Import…
                </Button>
              </div>
              {specId !== null ? (
                <select
                  className="w-full rounded-md border border-slate-300 px-2.5 py-2 text-sm"
                  value={operationId}
                  onChange={(e) => setOperationId(e.target.value)}
                >
                  <option value="">— pick an operation —</option>
                  {(opsQ.data?.items ?? []).map((o) => (
                    <option key={o.operation_id} value={o.operation_id}>
                      {o.method} {o.path}
                      {o.summary ? ` — ${o.summary}` : ""}
                    </option>
                  ))}
                </select>
              ) : null}
              <Button className="w-full" onClick={applyOperation} disabled={!operationId}>
                Apply to “{step.name}”
              </Button>
              <p className="text-xs leading-relaxed text-slate-500">
                Fills URL, method, body keys, and path/query params from the operation schema.
              </p>
            </div>
          </Card>

          <Card>
            <h3 className="mb-3 flex items-center gap-1.5 text-sm font-semibold text-slate-900">
              <Sparkles className="h-4 w-4 text-amber-500" />
              AI mapping
            </h3>
            <Textarea
              rows={3}
              placeholder='Optional guidance, e.g. "create the contact first, then attach the address"'
              value={llmInstructions}
              onChange={(e) => setLlmInstructions(e.target.value)}
            />
            <Button className="mt-2 w-full" onClick={() => suggest.mutate()} disabled={suggest.isPending}>
              {suggest.isPending ? "Thinking…" : "Suggest mapping"}
            </Button>
            <p className="mt-2 text-xs leading-relaxed text-slate-500">
              Uses your org LLM + sample rows (PII-redacted). Review and dry-run before publishing.
            </p>
          </Card>

          <Card>
            <h3 className="mb-2 text-sm font-semibold text-slate-900">Template JSON</h3>
            <MonacoEditor
              height="180px"
              language="json"
              value={JSON.stringify(templateJson, null, 2)}
              options={{ readOnly: true, minimap: { enabled: false }, fontSize: 11, scrollBeyondLastLine: false }}
            />
          </Card>

          <Card>
            <h3 className="mb-2 text-sm font-semibold text-slate-900">Dry-run rows</h3>
            <MonacoEditor
              height="120px"
              language="json"
              value={dryRunRows}
              onChange={(v) => setDryRunRows(v ?? "")}
              options={{ minimap: { enabled: false }, fontSize: 11, scrollBeyondLastLine: false }}
            />
            {dryRun.data ? (
              <pre className="mt-3 max-h-48 overflow-auto rounded-lg bg-slate-900 p-3 text-xs text-slate-100">
                {JSON.stringify(dryRun.data.previews, null, 2)}
              </pre>
            ) : null}
          </Card>
        </div>
      </div>

      {/* Dialogs */}
      <AddFieldDialog
        open={dialog.kind === "add-source"}
        title="Add source field"
        description="A column from your CSV/JSON/XML input."
        placeholder="email"
        onClose={() => setDialog({ kind: "none" })}
        onSubmit={addSourceField}
      />

      {dialog.kind === "add-target" ? (
        <AddFieldDialog
          open
          title={targetPrompts[dialog.targetKind].title}
          description={targetPrompts[dialog.targetKind].hint}
          placeholder={targetPrompts[dialog.targetKind].placeholder}
          onClose={() => setDialog({ kind: "none" })}
          onSubmit={(name) => addTargetKey(dialog.targetKind, name)}
        />
      ) : null}

      <AddFieldDialog
        open={dialog.kind === "add-step"}
        title="Add step"
        description="Chained API call that can reference earlier step responses."
        label="Step name"
        placeholder={`step${steps.length + 1}`}
        defaultValue={`step${steps.length + 1}`}
        hint="Letters, numbers, underscore. Must start with a letter or _."
        validate={(v) => (STEP_NAME_RE.test(v) ? null : "Invalid step name")}
        onClose={() => setDialog({ kind: "none" })}
        onSubmit={addStep}
      />

      {dialog.kind === "rename-step" ? (
        <AddFieldDialog
          open
          title="Rename step"
          description="Must stay unique. Remove Response refs that point at the old name first."
          label="Step name"
          defaultValue={steps[dialog.index]?.name ?? ""}
          hint="Letters, numbers, underscore. Must start with a letter or _."
          validate={(v) => (STEP_NAME_RE.test(v) ? null : "Invalid step name")}
          onClose={() => setDialog({ kind: "none" })}
          onSubmit={(name) => renameStep(dialog.index, name)}
          submitLabel="Rename"
        />
      ) : null}

      <Dialog
        open={dialog.kind === "add-resp"}
        title="Add response reference"
        description="Pull a value from an earlier step's JSON response."
        onClose={() => setDialog({ kind: "none" })}
        footer={
          <>
            <Button variant="ghost" onClick={() => setDialog({ kind: "none" })}>
              Cancel
            </Button>
            <Button
              onClick={() => {
                addResponseRef(respStep, respPath);
                setDialog({ kind: "none" });
              }}
            >
              Add
            </Button>
          </>
        }
      >
        <label className="block text-xs font-medium text-slate-600">Earlier step</label>
        <select
          className="w-full rounded-md border border-slate-300 px-2.5 py-2 text-sm"
          value={respStep}
          onChange={(e) => setRespStep(e.target.value)}
        >
          {earlierSteps.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
        <label className="block text-xs font-medium text-slate-600">JSONPath</label>
        <Input value={respPath} onChange={(e) => setRespPath(e.target.value)} placeholder="$.id" />
      </Dialog>

      <Dialog
        open={dialog.kind === "import-spec" && dialog.stage === "url"}
        title="Import OpenAPI spec"
        description="Paste a public OpenAPI 3.x JSON URL."
        onClose={() => setDialog({ kind: "none" })}
        footer={
          <>
            <Button variant="ghost" onClick={() => setDialog({ kind: "none" })}>
              Cancel
            </Button>
            <Button
              onClick={() => {
                if (!importUrl.trim()) return;
                try {
                  const host = new URL(importUrl).hostname;
                  setImportName(host);
                } catch {
                  setImportName("imported-spec");
                }
                setDialog({ kind: "import-spec", stage: "name", url: importUrl.trim() });
              }}
            >
              Next
            </Button>
          </>
        }
      >
        <label className="block text-xs font-medium text-slate-600">Spec URL</label>
        <Input
          value={importUrl}
          onChange={(e) => setImportUrl(e.target.value)}
          placeholder="https://petstore3.swagger.io/api/v3/openapi.json"
        />
      </Dialog>

      <Dialog
        open={dialog.kind === "import-spec" && dialog.stage === "name"}
        title="Name this spec"
        onClose={() => setDialog({ kind: "none" })}
        footer={
          <>
            <Button variant="ghost" onClick={() => setDialog({ kind: "import-spec", stage: "url" })}>
              Back
            </Button>
            <Button
              disabled={importSpec.isPending || !importName.trim()}
              onClick={() =>
                importSpec.mutate({
                  url: (dialog.kind === "import-spec" && dialog.url) || importUrl,
                  name: importName.trim()
                })
              }
            >
              {importSpec.isPending ? "Importing…" : "Import"}
            </Button>
          </>
        }
      >
        <label className="block text-xs font-medium text-slate-600">Display name</label>
        <Input value={importName} onChange={(e) => setImportName(e.target.value)} />
      </Dialog>

      {dialog.kind === "confirm-sample" ? (
        <ConfirmDialog
          open
          title="Replace source fields?"
          description={`Replace ${dialog.existing} existing source field${dialog.existing === 1 ? "" : "s"} with ${dialog.sample.columns.length} columns from ${dialog.filename}? Mapping edges to removed fields will be dropped.`}
          confirmLabel="Replace"
          danger
          onClose={() => setDialog({ kind: "none" })}
          onConfirm={() => {
            applySample(dialog.sample, dialog.filename);
            toast.success(`Loaded ${dialog.sample.columns.length} columns from ${dialog.filename}`);
          }}
        />
      ) : null}

      {dialog.kind === "confirm-remove-step" ? (
        <ConfirmDialog
          open
          title={`Remove step “${steps[dialog.index]?.name}”?`}
          description="Its mappings will be deleted. This cannot be undone."
          confirmLabel="Remove"
          danger
          onClose={() => setDialog({ kind: "none" })}
          onConfirm={() => removeStep(dialog.index)}
        />
      ) : null}
    </div>
  );
}

export default function TemplatePage({ params }: { params: { id: string } }) {
  const { data, isLoading, error } = useQuery({
    queryKey: ["template", params.id],
    queryFn: () => apiFetch<Template>(`/rule-templates/${params.id}`)
  });
  if (isLoading) return <Card>Loading…</Card>;
  if (error) return <Card className="text-sm text-rose-700">{String(error)}</Card>;
  if (!data) return null;
  return <Designer template={data} />;
}
