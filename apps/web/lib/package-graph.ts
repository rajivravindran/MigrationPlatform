/**
 * Package dependency graphs: the stages of an archive package and the
 * `dependsOn` edges between them (manifest v1, see
 * apps/orchestrator-go/internal/batch/manifest.go).
 *
 * Shared by the live view on a package page and the package builder. The
 * builder produces exactly what the orchestrator already accepts — a zip with
 * a root manifest.json — so no new API is involved.
 */

/**
 * Visual state of a stage or plan step. "cancelled" covers stages the engine
 * skipped and stages whose job was cancelled. Plan steps add "waiting" (a file
 * arrived and is held for its prerequisites) and "blocked" (held because a
 * prerequisite failed its completion rule).
 */
export type StageState = "pending" | "waiting" | "blocked" | "running" | "completed" | "failed" | "cancelled";

export const STAGE_STATES: { state: StageState; label: string }[] = [
  { state: "pending", label: "Pending" },
  { state: "waiting", label: "Waiting" },
  { state: "blocked", label: "Blocked" },
  { state: "running", label: "Running" },
  { state: "completed", label: "Completed" },
  { state: "failed", label: "Failed" },
  { state: "cancelled", label: "Cancelled / skipped" }
];

/**
 * Stage rows store skipped stages as `failed` with a "skipped …" message
 * (workflows/batch.go skipStage), so the message is what tells them apart.
 */
export function stageState(status: string, errorMessage?: string | null, jobStatus?: string | null): StageState {
  if (jobStatus === "cancelled") return "cancelled";
  switch (status) {
    case "succeeded":
      return "completed";
    case "running":
      return "running";
    case "pending":
      return "pending";
    case "failed":
    case "partial":
    case "quarantined":
      return errorMessage?.toLowerCase().startsWith("skipped") ? "cancelled" : "failed";
    default:
      return "pending";
  }
}

export type GraphStage = { key: string; dependsOn: string[] };

/**
 * Edges the engine will honour. A manifest without any `dependsOn` runs its
 * stages one after another in list order; those edges are returned as implicit.
 */
export function graphEdges(stages: GraphStage[]): { from: string; to: string; implicit: boolean }[] {
  const explicit = stages.some((s) => s.dependsOn.length > 0);
  if (!explicit) {
    return stages.slice(1).map((s, i) => ({ from: stages[i].key, to: s.key, implicit: true }));
  }
  const keys = new Set(stages.map((s) => s.key));
  return stages.flatMap((s) => s.dependsOn.filter((d) => keys.has(d)).map((d) => ({ from: d, to: s.key, implicit: false })));
}

/** Column (longest path from a root) and row for each stage, left-to-right. */
export function layoutStages(stages: GraphStage[], colWidth = 300, rowHeight = 160): Map<string, { x: number; y: number }> {
  const edges = graphEdges(stages);
  const parents = new Map<string, string[]>();
  edges.forEach((e) => parents.set(e.to, [...(parents.get(e.to) ?? []), e.from]));
  const level = new Map<string, number>();
  const visiting = new Set<string>();
  const depth = (key: string): number => {
    if (level.has(key)) return level.get(key)!;
    if (visiting.has(key)) return 0; // cycle; validation reports it
    visiting.add(key);
    const d = Math.max(-1, ...(parents.get(key) ?? []).map(depth)) + 1;
    visiting.delete(key);
    level.set(key, d);
    return d;
  };
  stages.forEach((s) => depth(s.key));
  const columns = new Map<number, string[]>();
  stages.forEach((s) => {
    const l = level.get(s.key) ?? 0;
    columns.set(l, [...(columns.get(l) ?? []), s.key]);
  });
  const tallest = Math.max(1, ...Array.from(columns.values(), (c) => c.length));
  const out = new Map<string, { x: number; y: number }>();
  columns.forEach((keys, l) => {
    const offset = ((tallest - keys.length) * rowHeight) / 2;
    keys.forEach((k, i) => out.set(k, { x: l * colWidth, y: offset + i * rowHeight }));
  });
  return out;
}

/** True when adding `from → to` would close a cycle. */
export function wouldCycle(stages: GraphStage[], from: string, to: string): boolean {
  if (from === to) return true;
  const children = new Map<string, string[]>();
  stages.forEach((s) => s.dependsOn.forEach((d) => children.set(d, [...(children.get(d) ?? []), s.key])));
  const stack = [to];
  const seen = new Set<string>();
  while (stack.length) {
    const k = stack.pop()!;
    if (k === from) return true;
    if (seen.has(k)) continue;
    seen.add(k);
    stack.push(...(children.get(k) ?? []));
  }
  return false;
}

/* --------------------------------------------------------------- manifest */

export const SAFE_STAGE_ID = /^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/;

export type DraftStage = {
  /** Stable identity while the stage is renamed; dependencies point at uids. */
  uid: string;
  id: string;
  templateKey: string;
  file: File | null;
  dependsOn: string[]; // uids
};

export function stageFilePath(stage: Pick<DraftStage, "id">): string {
  return `data/${stage.id}.csv`;
}

/** Problems that would make the orchestrator reject the manifest, per stage uid ("" = package-level). */
export function validateDraft(batchId: string, stages: DraftStage[]): Map<string, string> {
  const problems = new Map<string, string>();
  if (batchId && !SAFE_STAGE_ID.test(batchId)) problems.set("", "Package name: letters, digits, '.', '_' or '-', starting with a letter or digit.");
  if (stages.length === 0) problems.set("", "Add at least one stage.");
  const names = new Set<string>();
  for (const s of stages) {
    const issue = !s.id
      ? "Name the stage."
      : !SAFE_STAGE_ID.test(s.id)
        ? "Stage name: letters, digits, '.', '_' or '-', starting with a letter or digit."
        : names.has(s.id)
          ? `Another stage is already called "${s.id}".`
          : !s.templateKey
            ? "Choose a published template."
            : !s.file
              ? "Choose a CSV file."
              : null;
    if (issue) problems.set(s.uid, issue);
    names.add(s.id);
  }
  if (!problems.has("") && hasCycle(stages.map((s) => ({ key: s.uid, dependsOn: s.dependsOn })))) {
    problems.set("", "Dependencies form a cycle.");
  }
  return problems;
}

function hasCycle(stages: GraphStage[]): boolean {
  const indeg = new Map(stages.map((s) => [s.key, s.dependsOn.length]));
  const ready = stages.filter((s) => s.dependsOn.length === 0).map((s) => s.key);
  let visited = 0;
  while (ready.length) {
    const k = ready.pop()!;
    visited++;
    for (const s of stages) {
      if (!s.dependsOn.includes(k)) continue;
      const n = indeg.get(s.key)! - 1;
      indeg.set(s.key, n);
      if (n === 0) ready.push(s.key);
    }
  }
  return visited < stages.length;
}

export function buildManifest(batchId: string, onStageFailure: "stop" | "continue", stages: DraftStage[]) {
  return {
    version: 1,
    ...(batchId ? { batchId } : {}),
    onStageFailure,
    stages: stages.map((s) => {
      const dependsOn = s.dependsOn.map((uid) => stages.find((x) => x.uid === uid)?.id).filter((id): id is string => Boolean(id));
      return {
        id: s.id,
        file: stageFilePath(s),
        templateKey: s.templateKey,
        ...(dependsOn.length ? { dependsOn } : {})
      };
    })
  };
}

/* -------------------------------------------------------------------- zip */

const CRC_TABLE = (() => {
  const t = new Uint32Array(256);
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    t[n] = c >>> 0;
  }
  return t;
})();

function crc32(data: Uint8Array): number {
  let c = 0xffffffff;
  for (let i = 0; i < data.length; i++) c = CRC_TABLE[(c ^ data[i]) & 0xff] ^ (c >>> 8);
  return (c ^ 0xffffffff) >>> 0;
}

/** Minimal stored (uncompressed) zip; the orchestrator unpacks it with Go's archive/zip. */
export function makeZip(entries: { name: string; data: Uint8Array<ArrayBuffer> }[]): Blob {
  const enc = new TextEncoder();
  const now = new Date();
  const dosTime = (now.getHours() << 11) | (now.getMinutes() << 5) | (now.getSeconds() >> 1);
  const dosDate = ((now.getFullYear() - 1980) << 9) | ((now.getMonth() + 1) << 5) | now.getDate();
  const parts: BlobPart[] = [];
  const central: Uint8Array<ArrayBuffer>[] = [];
  let offset = 0;
  for (const e of entries) {
    const name = enc.encode(e.name);
    const crc = crc32(e.data);
    const local = new DataView(new ArrayBuffer(30));
    local.setUint32(0, 0x04034b50, true);
    local.setUint16(4, 20, true);
    local.setUint16(6, 0x0800, true); // UTF-8 names
    local.setUint16(8, 0, true); // stored
    local.setUint16(10, dosTime, true);
    local.setUint16(12, dosDate, true);
    local.setUint32(14, crc, true);
    local.setUint32(18, e.data.length, true);
    local.setUint32(22, e.data.length, true);
    local.setUint16(26, name.length, true);
    parts.push(local.buffer, name, e.data);

    const cd = new DataView(new ArrayBuffer(46));
    cd.setUint32(0, 0x02014b50, true);
    cd.setUint16(4, 20, true);
    cd.setUint16(6, 20, true);
    cd.setUint16(8, 0x0800, true);
    cd.setUint16(10, 0, true);
    cd.setUint16(12, dosTime, true);
    cd.setUint16(14, dosDate, true);
    cd.setUint32(16, crc, true);
    cd.setUint32(20, e.data.length, true);
    cd.setUint32(24, e.data.length, true);
    cd.setUint16(28, name.length, true);
    cd.setUint32(42, offset, true);
    const rec = new Uint8Array(46 + name.length);
    rec.set(new Uint8Array(cd.buffer), 0);
    rec.set(name, 46);
    central.push(rec);
    offset += 30 + name.length + e.data.length;
  }
  const cdSize = central.reduce((n, c) => n + c.length, 0);
  const end = new DataView(new ArrayBuffer(22));
  end.setUint32(0, 0x06054b50, true);
  end.setUint16(8, entries.length, true);
  end.setUint16(10, entries.length, true);
  end.setUint32(12, cdSize, true);
  end.setUint32(16, offset, true);
  return new Blob([...parts, ...central, end.buffer], { type: "application/zip" });
}
