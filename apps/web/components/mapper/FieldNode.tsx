"use client";

import { clsx } from "clsx";
import { Check, Link2, Radio } from "lucide-react";
import { memo } from "react";
import { Handle, Position, type NodeProps } from "reactflow";

import type { FieldNodeData, NodeKind } from "./types";

const KIND_STYLES: Record<
  NodeKind,
  { chip: string; border: string; handle: string; icon: string }
> = {
  src: {
    chip: "bg-sky-100 text-sky-800",
    border: "border-sky-200",
    handle: "!bg-sky-500 !border-sky-600",
    icon: "Source"
  },
  resp: {
    chip: "bg-emerald-100 text-emerald-800",
    border: "border-emerald-200",
    handle: "!bg-emerald-500 !border-emerald-600",
    icon: "Response"
  },
  dst: {
    chip: "bg-indigo-100 text-indigo-800",
    border: "border-indigo-200",
    handle: "!bg-indigo-500 !border-indigo-600",
    icon: "Body"
  },
  path: {
    chip: "bg-amber-100 text-amber-900",
    border: "border-amber-200",
    handle: "!bg-amber-500 !border-amber-600",
    icon: "Path"
  },
  query: {
    chip: "bg-blue-100 text-blue-800",
    border: "border-blue-200",
    handle: "!bg-blue-500 !border-blue-600",
    icon: "Query"
  },
  hdr: {
    chip: "bg-violet-100 text-violet-800",
    border: "border-violet-200",
    handle: "!bg-violet-500 !border-violet-600",
    icon: "Header"
  }
};

function FieldNodeInner({ id, data }: NodeProps<FieldNodeData>) {
  const styles = KIND_STYLES[data.kind];
  const isSource = data.kind === "src" || data.kind === "resp";

  return (
    <div
      role="button"
      tabIndex={0}
      data-testid={`field-node-${id}`}
      onClick={(e) => {
        e.stopPropagation();
        data.onFieldClick?.(id);
      }}
      onKeyDown={(e) => {
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          e.stopPropagation();
          data.onFieldClick?.(id);
        }
      }}
      className={clsx(
        "group relative w-[220px] cursor-pointer rounded-xl border-2 bg-white px-3 py-2.5 shadow-sm transition-all select-none",
        styles.border,
        data.selected && "ring-2 ring-brand-500 ring-offset-2 border-brand-500 shadow-md scale-[1.02]",
        data.eligible && "ring-2 ring-emerald-400 ring-offset-1 border-emerald-400 shadow-md animate-pulse-soft",
        data.mapped && !data.selected && !data.eligible && "bg-slate-50",
        !data.selected && !data.eligible && "hover:shadow-md hover:border-slate-300"
      )}
    >
      {isSource ? (
        <Handle
          type="source"
          position={Position.Right}
          className={clsx(
            "!h-3.5 !w-3.5 !rounded-full !border-2 !right-[-7px]",
            styles.handle
          )}
        />
      ) : (
        <Handle
          type="target"
          position={Position.Left}
          className={clsx(
            "!h-3.5 !w-3.5 !rounded-full !border-2 !left-[-7px]",
            styles.handle
          )}
        />
      )}

      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0 flex-1">
          <div className="mb-1 flex items-center gap-1.5">
            <span className={clsx("rounded px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide", styles.chip)}>
              {styles.icon}
            </span>
            {data.mapped ? (
              <span className="inline-flex items-center gap-0.5 text-[10px] font-medium text-emerald-600">
                <Check className="h-3 w-3" strokeWidth={3} />
                mapped
              </span>
            ) : null}
          </div>
          <div className="truncate text-sm font-semibold text-slate-900" title={data.label}>
            {data.fieldName}
          </div>
          {data.fieldType ? (
            <div className="mt-0.5 truncate font-mono text-[11px] text-slate-500">{data.fieldType}</div>
          ) : null}
          {data.connectedTo && data.connectedTo.length > 0 ? (
            <div className="mt-1.5 flex items-center gap-1 truncate text-[11px] text-slate-500">
              <Link2 className="h-3 w-3 shrink-0" />
              <span className="truncate">{data.connectedTo.join(", ")}</span>
            </div>
          ) : (
            <div className="mt-1.5 text-[11px] text-slate-400">
              {isSource ? "Click to connect →" : "← Click to map"}
            </div>
          )}
        </div>
        {data.selected ? (
          <Radio className="mt-0.5 h-4 w-4 shrink-0 text-brand-600" />
        ) : null}
      </div>
    </div>
  );
}

export const FieldNode = memo(FieldNodeInner);
