"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import ReactFlow, {
  Background,
  Controls,
  MarkerType,
  MiniMap,
  addEdge,
  applyEdgeChanges,
  applyNodeChanges,
  type Connection,
  type Edge,
  type EdgeChange,
  type Node,
  type NodeChange,
  type NodeTypes
} from "reactflow";
import "reactflow/dist/style.css";
import { toast } from "sonner";

import { FieldNode } from "./FieldNode";
import {
  bareName,
  isSourceKind,
  isTargetKind,
  isValidFieldConnection,
  kindOfNode,
  type FieldNodeData
} from "./types";

const nodeTypes: NodeTypes = { field: FieldNode };

type MappingCanvasProps = {
  /** Change this when switching steps so pending selection resets */
  resetKey?: string | number;
  srcNodes: Node<FieldNodeData>[];
  stepNodes: Node<FieldNodeData>[];
  edges: Edge[];
  onSrcNodesChange: (nodes: Node<FieldNodeData>[]) => void;
  onStepNodesChange: (nodes: Node<FieldNodeData>[]) => void;
  onEdgesChange: (edges: Edge[]) => void;
};

function decorateNodes(
  nodes: Node<FieldNodeData>[],
  edges: Edge[],
  pendingSource: string | null,
  onFieldClick: (id: string) => void
): Node<FieldNodeData>[] {
  const connected = new Map<string, string[]>();
  edges.forEach((e) => {
    const srcName = bareName(e.source);
    const tgtName = bareName(e.target);
    const fromSrc = connected.get(e.source) ?? [];
    fromSrc.push(tgtName);
    connected.set(e.source, fromSrc);
    const fromTgt = connected.get(e.target) ?? [];
    fromTgt.push(srcName);
    connected.set(e.target, fromTgt);
  });

  return nodes.map((n) => {
    const kind = kindOfNode(n.id);
    const mapped = (connected.get(n.id)?.length ?? 0) > 0;
    const selected = pendingSource === n.id;
    const eligible =
      pendingSource !== null &&
      pendingSource !== n.id &&
      isValidFieldConnection(pendingSource, n.id);
    return {
      ...n,
      type: "field",
      draggable: true,
      data: {
        ...n.data,
        kind: kind ?? n.data.kind,
        connectedTo: connected.get(n.id) ?? [],
        mapped,
        selected,
        eligible,
        onFieldClick
      }
    };
  });
}

export function MappingCanvas({
  resetKey,
  srcNodes,
  stepNodes,
  edges,
  onSrcNodesChange,
  onStepNodesChange,
  onEdgesChange
}: MappingCanvasProps) {
  const [pendingSource, setPendingSource] = useState<string | null>(null);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setPendingSource(null);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  useEffect(() => {
    setPendingSource(null);
  }, [resetKey]);

  const connectFields = useCallback(
    (source: string, target: string) => {
      if (!isValidFieldConnection(source, target)) {
        toast.error("Connect a source (left) to a destination (right)");
        return;
      }
      const targetKind = kindOfNode(target);
      const next = edges.filter((e) => {
        // Non-query targets keep a single mapping — replace existing.
        if (targetKind !== "query" && e.target === target) return false;
        // Avoid exact duplicate
        if (e.source === source && e.target === target) return false;
        return true;
      });
      onEdgesChange(
        addEdge(
          {
            id: `e-${source}-${target}-${Date.now()}`,
            source,
            target,
            animated: true,
            style: { strokeWidth: 2, stroke: "#6d9bf5" },
            markerEnd: { type: MarkerType.ArrowClosed, color: "#6d9bf5", width: 18, height: 18 }
          } as Edge,
          next
        )
      );
      toast.success(`Mapped ${bareName(source)} → ${bareName(target)}`, {
        description: "The source stays selected: click another destination to map it again."
      });
      // Keep the source selected so one field can feed several destinations
      // without re-selecting it each time (one-to-many). Esc, clicking the
      // source again or clicking the pane clears it.
      setPendingSource((prev) => (prev === source ? prev : null));
    },
    [edges, onEdgesChange]
  );

  const onFieldClick = useCallback(
    (nodeId: string) => {
      const kind = kindOfNode(nodeId);
      if (isSourceKind(kind)) {
        setPendingSource((prev) => (prev === nodeId ? null : nodeId));
        return;
      }
      if (isTargetKind(kind)) {
        if (!pendingSource) {
          toast.message("Select a source field first", {
            description: "Click a field on the left, then click a destination on the right."
          });
          return;
        }
        connectFields(pendingSource, nodeId);
      }
    },
    [pendingSource, connectFields]
  );

  const canvasNodes = useMemo(() => {
    const base =
      srcNodes.length > 0
        ? srcNodes
        : [
            {
              id: "src:__hint",
              type: "field" as const,
              position: { x: 40, y: 24 },
              data: {
                label: "Add a source field",
                kind: "src" as const,
                fieldName: "No source fields yet",
                fieldType: "Sample a file or add a field"
              }
            }
          ];
    return decorateNodes([...base, ...stepNodes], edges, pendingSource, onFieldClick);
  }, [srcNodes, stepNodes, edges, pendingSource, onFieldClick]);

  const styledEdges = useMemo(
    () =>
      edges.map((e) => ({
        ...e,
        animated: true,
        style: { strokeWidth: 2, stroke: "#6d9bf5", ...(e.style ?? {}) },
        markerEnd: e.markerEnd ?? {
          type: MarkerType.ArrowClosed,
          color: "#6d9bf5",
          width: 18,
          height: 18
        }
      })),
    [edges]
  );

  const onNodesChange = useCallback(
    (changes: NodeChange[]) => {
      const isSrc = (id: string) => id.startsWith("src:");
      onSrcNodesChange(
        applyNodeChanges(
          changes.filter((c) => ("id" in c ? isSrc(String((c as { id?: string }).id ?? "")) : true)),
          srcNodes
        ) as Node<FieldNodeData>[]
      );
      onStepNodesChange(
        applyNodeChanges(
          changes.filter((c) => ("id" in c ? !isSrc(String((c as { id?: string }).id ?? "")) : true)),
          stepNodes
        ) as Node<FieldNodeData>[]
      );
    },
    [srcNodes, stepNodes, onSrcNodesChange, onStepNodesChange]
  );

  const onEdgesChangeRf = useCallback(
    (changes: EdgeChange[]) => {
      onEdgesChange(applyEdgeChanges(changes, edges));
    },
    [edges, onEdgesChange]
  );

  const onConnect = useCallback(
    (conn: Connection) => {
      if (!conn.source || !conn.target) return;
      if (!isValidFieldConnection(conn.source, conn.target)) {
        toast.error("Connect a source (left) to a destination (right)");
        return;
      }
      connectFields(conn.source, conn.target);
    },
    [connectFields]
  );

  const isValidConnection = useCallback(
    (conn: Connection | Edge) =>
      !!conn.source && !!conn.target && isValidFieldConnection(conn.source, conn.target),
    []
  );

  const pendingCount = pendingSource ? edges.filter((e) => e.source === pendingSource).length : 0;
  const pendingLabel = pendingSource
    ? pendingSource.startsWith("resp:")
      ? pendingSource.slice(5)
      : bareName(pendingSource)
    : null;

  return (
    <div className="relative flex h-full flex-col">
      <div
        className={
          "flex items-center justify-between gap-3 border-b px-3 py-2 text-xs " +
          (pendingSource
            ? "border-brand-200 bg-brand-50 text-brand-800"
            : "border-slate-100 bg-slate-50 text-slate-600")
        }
        data-testid="mapping-hint"
      >
        {pendingSource ? (
          <p>
            Connecting from <strong className="font-semibold">{pendingLabel}</strong>
            {pendingCount > 0 ? ` (feeds ${pendingCount} field${pendingCount === 1 ? "" : "s"})` : ""} — click
            each destination it should feed, then press Esc or click it again to finish.
          </p>
        ) : (
          <p>
            <strong className="font-semibold">Click</strong> a source field, then{" "}
            <strong className="font-semibold">click</strong> one or more destinations. A source can feed
            many fields; a destination takes one source (query parameters take several). Dragging from
            the handle dots works too.
          </p>
        )}
        {pendingSource ? (
          <button
            type="button"
            className="shrink-0 rounded-md bg-white px-2 py-1 text-xs font-medium text-brand-700 shadow-sm ring-1 ring-brand-200 hover:bg-brand-50"
            onClick={() => setPendingSource(null)}
          >
            Cancel
          </button>
        ) : null}
      </div>

      <div className="flex justify-between border-b border-slate-100 px-10 py-1.5 text-[12px] text-slate-500">
        <span>Source fields and earlier responses</span>
        <span>Destination fields</span>
      </div>
      <div className="relative min-h-0 flex-1" data-testid="mapping-canvas">

        <ReactFlow
          nodes={canvasNodes}
          edges={styledEdges}
          onNodesChange={onNodesChange}
          onEdgesChange={onEdgesChangeRf}
          onConnect={onConnect}
          isValidConnection={isValidConnection}
          nodeTypes={nodeTypes}
          fitView
          fitViewOptions={{ padding: 0.2 }}
          proOptions={{ hideAttribution: true }}
          deleteKeyCode={["Backspace", "Delete"]}
          connectionLineStyle={{ stroke: "#6d9bf5", strokeWidth: 2 }}
          defaultEdgeOptions={{
            animated: true,
            style: { strokeWidth: 2, stroke: "#6d9bf5" },
            markerEnd: { type: MarkerType.ArrowClosed, color: "#6d9bf5", width: 18, height: 18 }
          }}
          onPaneClick={() => setPendingSource(null)}
          className="bg-[radial-gradient(#283449_1px,transparent_1px)] [background-size:16px_16px]"
        >
          <Background gap={16} size={1} color="#283449" />
          <Controls showInteractive={false} />
          {canvasNodes.length > 12 ? (
            <MiniMap
              pannable
              zoomable
              nodeColor={(n) => {
                const k = kindOfNode(n.id);
                if (k === "src") return "#4fb3f0";
                if (k === "resp") return "#3fcf8e";
                if (k === "path") return "#e8b04c";
                if (k === "query") return "#6d9bf5";
                if (k === "hdr") return "#b08cf4";
                return "#8b93f4";
              }}
            />
          ) : null}
        </ReactFlow>
      </div>
    </div>
  );
}
