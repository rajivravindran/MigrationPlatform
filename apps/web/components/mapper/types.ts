import type { Edge, Node } from "reactflow";

export const NODE_KINDS = {
  src: { prefix: "src:", x: 40, label: "Source", tone: "source" as const },
  resp: { prefix: "resp:", x: 40, label: "Response", tone: "response" as const },
  dst: { prefix: "dst:", x: 520, label: "Body", tone: "body" as const },
  path: { prefix: "path:", x: 520, label: "Path", tone: "path" as const },
  query: { prefix: "query:", x: 520, label: "Query", tone: "query" as const },
  hdr: { prefix: "hdr:", x: 520, label: "Header", tone: "header" as const }
} as const;

export type NodeKind = keyof typeof NODE_KINDS;
export type TargetKind = Exclude<NodeKind, "src" | "resp">;
export type SourceKind = "src" | "resp";

export type FieldNodeData = {
  label: string;
  kind: NodeKind;
  fieldName: string;
  fieldType?: string;
  /** Partner labels connected to this node (for display) */
  connectedTo?: string[];
  /** True while this node is the pending click-to-connect source */
  selected?: boolean;
  /** True when a source is selected and this target is eligible */
  eligible?: boolean;
  /** True when already mapped (has at least one edge) */
  mapped?: boolean;
  onFieldClick?: (nodeId: string) => void;
};

export type StepUI = {
  name: string;
  /** After this step definitively fails: stop (default) or continue. */
  onFailure?: "stop" | "continue";
  nodes: Node<FieldNodeData>[];
  edges: Edge[];
  url: string;
  method: string;
  responseSchema?: Record<string, unknown> | null;
};

export function nodeIdFor(kind: NodeKind, name: string) {
  return `${NODE_KINDS[kind].prefix}${name}`;
}

export function kindOfNode(id: string): NodeKind | null {
  for (const k of Object.keys(NODE_KINDS) as NodeKind[]) {
    if (id.startsWith(NODE_KINDS[k].prefix)) return k;
  }
  return null;
}

export function bareName(id: string) {
  const kind = kindOfNode(id);
  if (!kind) return id;
  return id.slice(NODE_KINDS[kind].prefix.length);
}

/** resp node ids look like `resp:stepName|$.json.path` */
export function respNodeId(step: string, path: string) {
  return `resp:${step}|${path}`;
}

export function parseRespNode(id: string): { step: string; path: string } | null {
  if (!id.startsWith("resp:")) return null;
  const rest = id.slice("resp:".length);
  const sep = rest.indexOf("|");
  if (sep < 0) return null;
  return { step: rest.slice(0, sep), path: rest.slice(sep + 1) };
}

export function isSourceKind(kind: NodeKind | null): kind is SourceKind {
  return kind === "src" || kind === "resp";
}

export function isTargetKind(kind: NodeKind | null): kind is TargetKind {
  return kind === "dst" || kind === "path" || kind === "query" || kind === "hdr";
}

export function isValidFieldConnection(sourceId: string, targetId: string): boolean {
  return isSourceKind(kindOfNode(sourceId)) && isTargetKind(kindOfNode(targetId));
}

export const ROW_H = 64;
export const COL_PAD = 24;

export function layoutY(index: number) {
  return COL_PAD + index * ROW_H;
}
