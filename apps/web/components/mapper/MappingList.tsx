"use client";

import { Unlink } from "lucide-react";

import { Button } from "@/components/ui";
import { bareName, kindOfNode, NODE_KINDS, parseRespNode, type TargetKind } from "./types";
import type { Edge, Node } from "reactflow";
import type { FieldNodeData } from "./types";

type MappingRow = {
  id: string;
  sourceLabel: string;
  targetLabel: string;
  targetKind: TargetKind;
  edgeId: string;
};

function sourceLabel(id: string, nodes: Node<FieldNodeData>[]): string {
  if (id.startsWith("src:")) return bareName(id);
  const parsed = parseRespNode(id);
  if (parsed) return `${parsed.step} → ${parsed.path}`;
  const n = nodes.find((x) => x.id === id);
  return n?.data.fieldName ?? id;
}

function targetLabel(id: string, nodes: Node<FieldNodeData>[]): string {
  const kind = kindOfNode(id);
  const name = bareName(id);
  const n = nodes.find((x) => x.id === id);
  const prefix = kind && kind !== "src" && kind !== "resp" ? NODE_KINDS[kind].label : "";
  return n?.data.fieldName ?? (prefix ? `${prefix}: ${name}` : name);
}

export function buildMappingRows(
  edges: Edge[],
  srcNodes: Node<FieldNodeData>[],
  stepNodes: Node<FieldNodeData>[]
): MappingRow[] {
  const all = [...srcNodes, ...stepNodes];
  return edges
    .map((e) => {
      const kind = kindOfNode(e.target);
      if (!kind || kind === "src" || kind === "resp") return null;
      return {
        id: e.id,
        edgeId: e.id,
        sourceLabel: sourceLabel(e.source, all),
        targetLabel: targetLabel(e.target, all),
        targetKind: kind
      };
    })
    .filter((r): r is MappingRow => r !== null);
}

export function MappingList({
  rows,
  onRemove
}: {
  rows: MappingRow[];
  onRemove: (edgeId: string) => void;
}) {
  if (rows.length === 0) {
    return (
      <div className="rounded-lg border border-dashed border-slate-200 bg-slate-50/80 px-4 py-6 text-center">
        <p className="text-sm font-medium text-slate-700">No mappings yet</p>
        <p className="mt-1 text-xs text-slate-500">
          Click a source field, then click a destination field to connect them.
        </p>
      </div>
    );
  }

  return (
    <ul className="divide-y divide-slate-100 rounded-lg border border-slate-200 bg-white">
      {rows.map((row) => (
        <li key={row.id} className="flex items-center gap-3 px-3 py-2.5 text-sm">
          <span className="min-w-0 flex-1 truncate font-medium text-slate-800" title={row.sourceLabel}>
            {row.sourceLabel}
          </span>
          <span className="shrink-0 text-slate-300">→</span>
          <span className="min-w-0 flex-1 truncate text-slate-600" title={row.targetLabel}>
            <span className="mr-1.5 rounded bg-slate-100 px-1.5 py-0.5 text-[10px] font-semibold uppercase text-slate-500">
              {NODE_KINDS[row.targetKind].label}
            </span>
            {row.targetLabel}
          </span>
          <Button
            variant="ghost"
            className="!px-1.5 !py-1 text-slate-400 hover:text-rose-600"
            title="Remove mapping"
            onClick={() => onRemove(row.edgeId)}
          >
            <Unlink className="h-3.5 w-3.5" />
          </Button>
        </li>
      ))}
    </ul>
  );
}
