/**
 * Migration plans (API: /plans). A plan is the coordinator's dependency tree
 * of steps, designed before any file exists; files arrive later as plan runs.
 */

import type { StageState } from "./package-graph";

export type CompletionRule = "strict" | "max_failed_pct";

export type PlanStep = {
  step_key: string;
  label?: string | null;
  depends_on: string[];
  default_template_key?: string | null;
  file_pattern?: string | null;
  completion_rule: CompletionRule;
  max_failed_pct?: number | null;
  position?: { x: number; y: number } | null;
};

export type Plan = {
  id: number;
  plan_key: string;
  version: number;
  name: string;
  description?: string | null;
  status: "draft" | "published";
  created_at: string;
  published_at?: string | null;
  updated_at: string;
};

export type PlanSummary = Plan & {
  steps: number;
  steps_completed: number;
  runs_waiting: number;
  runs_running: number;
  runs_failed: number;
};

export type StepStatus = {
  step_key: string;
  completed: boolean;
  running: boolean;
  waiting: boolean;
  blocked: boolean;
  latest_status?: string | null;
  latest_reason?: string | null;
  latest_job_id?: number | null;
  runs: number;
};

export type JobTotals = { processed?: number; failed?: number; unresolved?: number; total?: number };

export type PlanRun = {
  id: number;
  plan_id: number;
  plan_version: number;
  step_key?: string | null;
  intake_id?: number | null;
  filename?: string | null;
  rule_template_id?: number | null;
  template_name?: string | null;
  status: "waiting" | "running" | "completed" | "failed" | "cancelled";
  blocked: boolean;
  rule_met?: boolean | null;
  failed_pct?: number | null;
  reason?: string | null;
  job_id?: number | null;
  job_status?: string | null;
  job_totals?: JobTotals | null;
  schedule_id?: number | null;
  created_at: string;
  started_at?: string | null;
  finished_at?: string | null;
};

export type PlanIntake = {
  id: number;
  plan_id: number;
  filename?: string | null;
  status: "unpacking" | "dispatched" | "failed";
  error_message?: string | null;
  runs: number;
  created_at: string;
};

export type PlanDetail = {
  plan: Plan;
  steps: PlanStep[];
  step_status: StepStatus[];
  runs: PlanRun[];
  intakes: PlanIntake[];
  versions: { id: number; version: number; status: string }[];
};

/**
 * One state per step across all its runs: a step that ever met its rule is
 * completed (later re-runs don't undo that); otherwise the most active state.
 */
export function stepState(s?: StepStatus): StageState {
  if (!s) return "pending";
  if (s.completed) return "completed";
  if (s.running) return "running";
  if (s.blocked) return "blocked";
  if (s.waiting) return "waiting";
  if (s.latest_status === "failed") return "failed";
  if (s.latest_status === "cancelled") return "cancelled";
  return "pending";
}

export function ruleText(step: Pick<PlanStep, "completion_rule" | "max_failed_pct">): string {
  return step.completion_rule === "max_failed_pct"
    ? `≤ ${step.max_failed_pct ?? 0}% records may fail`
    : "every record must succeed";
}

/**
 * Glob match on a file's base name, case-insensitive: * any run, ? one
 * character, [..] a class — the same rules the orchestrator routes with.
 */
export function globMatch(pattern: string, filename: string): boolean {
  const base = filename.split(/[\\/]/).pop()!.toLowerCase();
  let re = "";
  const p = pattern.trim().toLowerCase();
  for (let i = 0; i < p.length; i++) {
    const c = p[i];
    if (c === "*") re += "[^/]*";
    else if (c === "?") re += "[^/]";
    else if (c === "[") {
      const end = p.indexOf("]", i + 1);
      if (end < 0) return false;
      // Go's path.Match and JS both read a leading ^ as negation.
      re += `[${p.slice(i + 1, end)}]`;
      i = end;
    } else re += c.replace(/[.+^${}()|\\]/g, "\\$&");
  }
  try {
    return new RegExp(`^${re}$`).test(base);
  } catch {
    return false;
  }
}

export function matchStep(steps: PlanStep[], filename: string): PlanStep | undefined {
  return steps.find((s) => s.file_pattern && globMatch(s.file_pattern, filename));
}

export function fileKind(name: string): "csv" | "json" | "xml" | "archive" | "other" {
  const n = name.toLowerCase();
  if (n.endsWith(".tar.gz") || n.endsWith(".tgz") || n.endsWith(".zip")) return "archive";
  if (n.endsWith(".csv") || n.endsWith(".tsv")) return "csv";
  if (n.endsWith(".json") || n.endsWith(".jsonl") || n.endsWith(".ndjson")) return "json";
  if (n.endsWith(".xml")) return "xml";
  return "other";
}

/* ------------------------------------------------- reading archive manifests */

export type ManifestStage = { id: string; file: string; templateKey?: string; dependsOn?: string[] };

async function inflate(data: Uint8Array, format: "gzip" | "deflate-raw"): Promise<Uint8Array> {
  const stream = new Blob([data as Uint8Array<ArrayBuffer>]).stream().pipeThrough(new DecompressionStream(format));
  return new Uint8Array(await new Response(stream).arrayBuffer());
}

function readTar(data: Uint8Array, wanted: (name: string) => boolean): Uint8Array | null {
  const dec = new TextDecoder();
  const str = (o: number, n: number) => dec.decode(data.subarray(o, o + n)).replace(/\0.*$/s, "");
  for (let off = 0; off + 512 <= data.length; ) {
    const name = str(off, 100);
    if (!name) return null;
    const size = parseInt(str(off + 124, 12).trim() || "0", 8);
    const prefix = str(off + 345, 155);
    const full = (prefix ? `${prefix}/${name}` : name).replace(/^\.\//, "");
    const type = str(off + 156, 1);
    if ((type === "0" || type === "") && wanted(full)) return data.subarray(off + 512, off + 512 + size);
    off += 512 + Math.ceil(size / 512) * 512;
  }
  return null;
}

async function readZip(data: Uint8Array, wanted: (name: string) => boolean): Promise<Uint8Array | null> {
  const view = new DataView(data.buffer, data.byteOffset, data.byteLength);
  let eocd = -1;
  for (let i = data.length - 22; i >= Math.max(0, data.length - 65557); i--) {
    if (view.getUint32(i, true) === 0x06054b50) {
      eocd = i;
      break;
    }
  }
  if (eocd < 0) return null;
  const count = view.getUint16(eocd + 10, true);
  let p = view.getUint32(eocd + 16, true);
  const dec = new TextDecoder();
  for (let n = 0; n < count && p + 46 <= data.length; n++) {
    const method = view.getUint16(p + 10, true);
    const csize = view.getUint32(p + 20, true);
    const nameLen = view.getUint16(p + 28, true);
    const extraLen = view.getUint16(p + 30, true);
    const commentLen = view.getUint16(p + 32, true);
    const localOff = view.getUint32(p + 42, true);
    const name = dec.decode(data.subarray(p + 46, p + 46 + nameLen)).replace(/^\.\//, "");
    if (wanted(name)) {
      const lNameLen = view.getUint16(localOff + 26, true);
      const lExtraLen = view.getUint16(localOff + 28, true);
      const body = data.subarray(localOff + 30 + lNameLen + lExtraLen, localOff + 30 + lNameLen + lExtraLen + csize);
      if (method === 0) return body;
      if (method === 8) return inflate(body, "deflate-raw");
      throw new Error("manifest.json uses an unsupported zip compression method");
    }
    p += 46 + nameLen + extraLen + commentLen;
  }
  return null;
}

/** Reads manifest.json from a .zip / .tar.gz / .tgz in the browser, without uploading it. */
export async function readArchiveManifest(file: File): Promise<ManifestStage[]> {
  const raw = new Uint8Array(await file.arrayBuffer());
  const isManifest = (n: string) => n === "manifest.json";
  const body = fileKind(file.name) === "archive" && file.name.toLowerCase().endsWith(".zip")
    ? await readZip(raw, isManifest)
    : readTar(await inflate(raw, "gzip"), isManifest);
  if (!body) throw new Error("This archive has no manifest.json at its root.");
  const manifest = JSON.parse(new TextDecoder().decode(body));
  if (!Array.isArray(manifest?.stages)) throw new Error("manifest.json has no stages list.");
  return manifest.stages as ManifestStage[];
}
