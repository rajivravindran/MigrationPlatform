"use client";

import { clsx } from "clsx";
import { Ban, CheckCircle2, Clock, Hourglass, Loader2, Lock, XCircle } from "lucide-react";
import { useRouter } from "next/navigation";
import { useEffect, useMemo, useRef, useState } from "react";
import ReactFlow, {
  Background,
  Controls,
  Handle,
  MarkerType,
  Panel,
  Position,
  type Connection,
  type Edge,
  type EdgeChange,
  type Node,
  type NodeChange,
  type NodeProps,
  type NodeTypes,
  type ReactFlowInstance
} from "reactflow";
import "reactflow/dist/style.css";

import { STAGE_STATES, graphEdges, layoutStages, type StageState } from "@/lib/package-graph";

export type GraphNode = {
  key: string;
  /** Display name when `key` is an internal id (builder). */
  label?: string;
  dependsOn: string[];
  state: StageState;
  /** Second line, e.g. the template key. */
  detail?: string;
  progress?: { processed: number; total: number; failed: number } | null;
  /** Shown on hover and under the title when the stage failed or was skipped. */
  note?: string | null;
  href?: string | null;
  /** Builder: mark a stage with a validation problem. */
  invalid?: boolean;
};

const STATE_ICON: Record<StageState, typeof Clock> = {
  pending: Clock,
  waiting: Hourglass,
  blocked: Lock,
  running: Loader2,
  completed: CheckCircle2,
  failed: XCircle,
  cancelled: Ban
};

const STATE_LABEL = Object.fromEntries(STAGE_STATES.map((s) => [s.state, s.label])) as Record<StageState, string>;

/** Edge colour follows the upstream stage: green once it completed, red if it failed. */
const EDGE_COLOUR: Record<StageState, string> = {
  pending: "#7f8ba3",
  waiting: "#7f8ba3",
  blocked: "#7f8ba3",
  running: "#5b8def",
  completed: "#3fcf8e",
  failed: "#f26d6d",
  cancelled: "#5c667a"
};

type StageNodeData = GraphNode & { editable: boolean };

function StageNode({ data }: NodeProps<StageNodeData>) {
  const Icon = STATE_ICON[data.state];
  const p = data.progress;
  const pct = p && p.total > 0 ? Math.min(100, Math.round(((p.processed + p.failed) / p.total) * 100)) : null;
  return (
    <div
      className={clsx("stage-node", `stage-glow-${data.state}`, data.invalid && "stage-invalid")}
      title={data.note ?? undefined}
    >
      <Handle type="target" position={Position.Left} isConnectable={data.editable} className={clsx(!data.editable && "!opacity-0")} />
      <div className="flex items-center gap-2">
        <Icon aria-hidden className={clsx("h-4 w-4 shrink-0 stage-icon", data.state === "running" && "animate-spin")} />
        <span className="min-w-0 flex-1 truncate font-mono text-[13px] text-slate-950">{(data.label ?? data.key) || "unnamed stage"}</span>
      </div>
      {data.detail ? <div className="mt-0.5 truncate text-[11.5px] text-slate-500">{data.detail}</div> : null}
      <div className="mt-2 flex items-center gap-2 text-[11.5px]">
        <span className="stage-state-label font-semibold">{STATE_LABEL[data.state]}</span>
        {p ? <span className="ml-auto text-slate-500">{p.processed.toLocaleString()}/{p.total.toLocaleString()} rows{p.failed ? ` · ${p.failed} failed` : ""}</span> : null}
      </div>
      {pct !== null ? (
        <div className="mt-1.5 h-1 overflow-hidden rounded bg-slate-200" aria-hidden>
          <div className="stage-bar h-full" style={{ width: `${pct}%` }} />
        </div>
      ) : null}
      {data.note && (data.state === "failed" || data.state === "cancelled") ? (
        <div className="mt-1 line-clamp-2 text-[11px] leading-snug text-slate-500">{data.note}</div>
      ) : null}
      <Handle type="source" position={Position.Right} isConnectable={data.editable} className={clsx(!data.editable && "!opacity-0")} />
    </div>
  );
}

const nodeTypes: NodeTypes = { stage: StageNode };

export type XY = { x: number; y: number };

const FIT = { padding: 0.25, maxZoom: 1 };

function loadPositions(key?: string): Record<string, XY> {
  if (!key) return {};
  try {
    return JSON.parse(window.localStorage.getItem(`migration.graph.${key}`) ?? "{}");
  } catch {
    return {};
  }
}

function savePositions(key: string | undefined, positions: Record<string, XY>) {
  if (!key) return;
  try {
    if (Object.keys(positions).length) window.localStorage.setItem(`migration.graph.${key}`, JSON.stringify(positions));
    else window.localStorage.removeItem(`migration.graph.${key}`);
  } catch {
    // Layout memory is a convenience; the graph works without it.
  }
}

export function StageGraph({
  nodes: graphNodes,
  editable = false,
  onConnect,
  onDisconnect,
  layoutKey,
  initialPositions,
  onMove,
  onResetLayout,
  showImplicit = true,
  height = 360
}: {
  nodes: GraphNode[];
  editable?: boolean;
  /** Builder: `to` now depends on `from`. */
  onConnect?: (from: string, to: string) => void;
  onDisconnect?: (from: string, to: string) => void;
  /** Remember dragged positions in this browser under this key. */
  layoutKey?: string;
  /** Saved arrangement (e.g. the plan's), used where nothing was dragged locally. */
  initialPositions?: Record<string, XY>;
  /** Called when a node is dropped somewhere new. */
  onMove?: (key: string, position: XY) => void;
  /** Called by Reset layout, after local positions are cleared. */
  onResetLayout?: () => void;
  /** Draw the list-order edges a manifest without dependencies runs by. */
  showImplicit?: boolean;
  height?: number;
}) {
  const router = useRouter();
  const flow = useRef<ReactFlowInstance | null>(null);
  const byKey = useMemo(() => new Map(graphNodes.map((n) => [n.key, n])), [graphNodes]);
  const [positions, setPositions] = useState<Record<string, XY>>({});
  const [selected, setSelected] = useState<Set<string>>(new Set());

  // localStorage is only readable after mount.
  useEffect(() => setPositions(loadPositions(layoutKey)), [layoutKey]);

  // Keep every stage in view as stages are added or removed.
  const keys = graphNodes.map((n) => n.key).join("|");
  useEffect(() => {
    const t = window.setTimeout(() => flow.current?.fitView(FIT), 50);
    return () => window.clearTimeout(t);
  }, [keys]);

  // Auto layout is recomputed only when stages or dependencies change, not on every status refresh.
  const structure = graphNodes.map((n) => `${n.key}<${n.dependsOn.join(",")}`).join("|");
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const layout = useMemo(() => layoutStages(graphNodes), [structure]);

  const nodes: Node<StageNodeData>[] = graphNodes.map((n) => ({
    id: n.key,
    type: "stage",
    position: positions[n.key] ?? initialPositions?.[n.key] ?? layout.get(n.key) ?? { x: 0, y: 0 },
    data: { ...n, editable },
    selected: selected.has(n.key),
    connectable: editable
  }));

  const edges: Edge[] = graphEdges(graphNodes)
    .filter((e) => showImplicit || !e.implicit)
    .map((e) => {
      const from = byKey.get(e.from)?.state ?? "pending";
      const to = byKey.get(e.to)?.state ?? "pending";
      const colour = EDGE_COLOUR[from];
      const id = `${e.from}->${e.to}`;
      return {
        id,
        source: e.from,
        target: e.to,
        selected: selected.has(id),
        animated: to === "running" || (from === "completed" && to === "pending"),
        deletable: editable && !e.implicit,
        interactionWidth: 24,
        style: { stroke: colour, strokeWidth: selected.has(id) ? 3.5 : 2, strokeDasharray: e.implicit ? "6 5" : undefined },
        markerEnd: { type: MarkerType.ArrowClosed, color: colour }
      };
    });

  const selectedEdge = edges.find((e) => e.selected && e.deletable);

  const select = (id: string, on: boolean) =>
    setSelected((prev) => {
      const next = new Set(prev);
      if (on) next.add(id);
      else next.delete(id);
      return next;
    });

  const handleNodesChange = (changes: NodeChange[]) => {
    for (const ch of changes) {
      if (ch.type === "position" && ch.position) {
        const at = ch.position;
        setPositions((prev) => ({ ...prev, [ch.id]: at }));
      }
      if (ch.type === "position" && ch.dragging === false) {
        setPositions((prev) => {
          savePositions(layoutKey, prev);
          if (prev[ch.id]) onMove?.(ch.id, prev[ch.id]);
          return prev;
        });
      }
      if (ch.type === "select") select(ch.id, ch.selected);
    }
  };

  const handleEdgesChange = (changes: EdgeChange[]) => {
    for (const ch of changes) {
      if (ch.type === "select") select(ch.id, ch.selected);
      if (ch.type === "remove") {
        const [from, to] = ch.id.split("->");
        select(ch.id, false);
        onDisconnect?.(from, to);
      }
    }
  };

  const resetLayout = () => {
    setPositions({});
    savePositions(layoutKey, {});
    onResetLayout?.();
    window.setTimeout(() => flow.current?.fitView({ ...FIT, duration: 300 }), 0);
  };

  return (
    <div style={{ height }} className={clsx("stage-graph rounded-lg border border-slate-200 bg-slate-50", editable && "stage-graph-editable")}>
      <ReactFlow
        nodes={nodes}
        edges={edges}
        nodeTypes={nodeTypes}
        onInit={(instance) => (flow.current = instance)}
        onNodesChange={handleNodesChange}
        onEdgesChange={handleEdgesChange}
        onConnect={editable ? (c: Connection) => c.source && c.target && onConnect?.(c.source, c.target) : undefined}
        isValidConnection={(c) => c.source !== c.target}
        onNodeClick={(_, n) => {
          const href = byKey.get(n.id)?.href;
          if (!editable && href) router.push(href);
        }}
        nodesDraggable
        nodesConnectable={editable}
        elementsSelectable
        connectionRadius={36}
        deleteKeyCode={editable ? ["Backspace", "Delete"] : null}
        nodeDragThreshold={4}
        fitView
        fitViewOptions={FIT}
        minZoom={0.3}
        maxZoom={1.75}
        proOptions={{ hideAttribution: true }}
      >
        <Background gap={20} color="#283449" />
        <Controls showInteractive={false} />
        {editable && selectedEdge ? (
          <Panel position="top-left">
            <button
              type="button"
              onClick={() => {
                select(selectedEdge.id, false);
                onDisconnect?.(selectedEdge.source, selectedEdge.target);
              }}
              className="rounded-md bg-rose-600 px-2.5 py-1 text-xs font-medium text-white shadow-sm hover:bg-rose-700 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand-500"
            >
              Remove link {byKey.get(selectedEdge.source)?.label ?? selectedEdge.source} → {byKey.get(selectedEdge.target)?.label ?? selectedEdge.target}
            </button>
          </Panel>
        ) : null}
        <Panel position="top-right">
          <button
            type="button"
            onClick={resetLayout}
            className="rounded-md border border-slate-300 bg-white px-2.5 py-1 text-xs text-slate-700 shadow-sm hover:bg-slate-100 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand-500"
          >
            Reset layout
          </button>
        </Panel>
      </ReactFlow>
    </div>
  );
}

/** Colour key with counts; every state is named, not only coloured. */
export function StageLegend({ counts, states }: { counts?: Partial<Record<StageState, number>>; states?: StageState[] }) {
  return (
    <ul className="flex flex-wrap items-center gap-x-4 gap-y-2 text-[12.5px] text-slate-600">
      {STAGE_STATES.filter(({ state }) => !states || states.includes(state)).map(({ state, label }) => {
        const Icon = STATE_ICON[state];
        return (
          <li key={state} className={clsx("inline-flex items-center gap-1.5", `stage-legend-${state}`)}>
            <span className="stage-dot" aria-hidden />
            <Icon aria-hidden className="h-3.5 w-3.5 stage-icon" />
            {label}
            {counts ? <span className="text-slate-500">{counts[state] ?? 0}</span> : null}
          </li>
        );
      })}
    </ul>
  );
}
