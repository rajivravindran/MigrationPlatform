"use client";

import { clsx } from "clsx";
import { AlertCircle, Check, Copy, Loader, Maximize2, Minus, X } from "lucide-react";
import * as React from "react";
import { toast } from "sonner";

import type { StatusTone } from "./ui";

export type ChipTone = StatusTone | "info";

/** Status chip with text and an icon, never colour alone. */
export function Chip({
  tone = "neutral",
  children,
  className,
  icon = true
}: React.PropsWithChildren<{ tone?: ChipTone; className?: string; icon?: boolean }>) {
  const tones: Record<ChipTone, string> = {
    ok: "bg-emerald-100 text-emerald-800 ring-emerald-300/50",
    fail: "bg-rose-100 text-rose-800 ring-rose-300/50",
    warn: "bg-amber-100 text-amber-800 ring-amber-300/50",
    info: "bg-blue-100 text-blue-800 ring-blue-300/50",
    neutral: "bg-slate-100 text-slate-700 ring-slate-300/60"
  };
  const Icon = tone === "ok" ? Check : tone === "fail" || tone === "warn" ? AlertCircle : tone === "info" ? Loader : Minus;
  return (
    <span className={clsx("inline-flex items-center gap-1 rounded px-2 py-0.5 text-[12.5px] font-semibold ring-1", tones[tone], className)}>
      {icon ? <Icon className="h-3 w-3" aria-hidden strokeWidth={3} /> : null}
      {children}
    </span>
  );
}

/** Environment / mode tag (e.g. "UAT · TEST"). */
export function Tag({ children, tone = "brand" }: React.PropsWithChildren<{ tone?: "brand" | "neutral" }>) {
  return (
    <span
      className={clsx(
        "inline-flex items-center rounded px-2 py-0.5 text-[11.5px] font-semibold uppercase tracking-wide ring-1",
        tone === "brand" ? "bg-brand-50 text-brand-700 ring-brand-200" : "bg-slate-100 text-slate-600 ring-slate-300/60"
      )}
    >
      {children}
    </span>
  );
}

/** Stat tile: big serif numeral, label above, qualifier below. */
export function Stat({
  label,
  value,
  unit,
  note,
  tone = "neutral"
}: {
  label: string;
  value: React.ReactNode;
  unit?: string;
  note?: React.ReactNode;
  tone?: StatusTone;
}) {
  const noteTone: Record<StatusTone, string> = {
    ok: "text-emerald-700",
    fail: "text-rose-700",
    warn: "text-amber-700",
    neutral: "text-slate-500"
  };
  return (
    <div className="min-w-0 px-5 py-4">
      <div className="text-[13px] text-slate-500">{label}</div>
      <div className="tnum mt-1 flex items-baseline gap-1.5 text-[34px] leading-none text-slate-950">
        {value}
        {unit ? <span className="text-sm text-slate-500">{unit}</span> : null}
      </div>
      {note ? <div className={clsx("mt-2 text-[13px]", noteTone[tone])}>{note}</div> : null}
    </div>
  );
}

export function StatRow({ children }: React.PropsWithChildren) {
  return (
    <div className="grid grid-cols-2 divide-y divide-slate-200 rounded-lg border border-slate-200 bg-white sm:grid-cols-4 sm:divide-x sm:divide-y-0">
      {children}
    </div>
  );
}

export function Panel({ children, className }: React.PropsWithChildren<{ className?: string }>) {
  return <section className={clsx("rounded-lg border border-slate-200 bg-white", className)}>{children}</section>;
}

export function PanelHeader({ children, className }: React.PropsWithChildren<{ className?: string }>) {
  return <div className={clsx("flex flex-wrap items-center gap-3 border-b border-slate-200 px-5 py-3", className)}>{children}</div>;
}

/* ----- JSON ----- */

function tokenize(json: string): React.ReactNode[] {
  const re = /("(?:\\.|[^"\\])*")(\s*:)?|\b(true|false)\b|\bnull\b|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/g;
  const out: React.ReactNode[] = [];
  let last = 0;
  let m: RegExpExecArray | null;
  let i = 0;
  while ((m = re.exec(json))) {
    if (m.index > last) out.push(json.slice(last, m.index));
    const [whole, str, colon, bool] = m;
    if (str) {
      out.push(
        <span key={i++} className={colon ? "k" : "s"}>
          {str}
        </span>
      );
      if (colon) out.push(colon);
    } else if (bool) out.push(<span key={i++} className="b">{whole}</span>);
    else if (whole === "null") out.push(<span key={i++} className="z">null</span>);
    else out.push(<span key={i++} className="n">{whole}</span>);
    last = m.index + whole.length;
  }
  if (last < json.length) out.push(json.slice(last));
  return out;
}

/**
 * Formatted, highlighted JSON with wrapping, copy and expand. Explains the
 * three empty cases distinctly instead of printing a bare null (UX-03).
 */
export function JsonView({
  value,
  empty,
  label,
  maxHeight = 320
}: {
  value: unknown;
  /** What an absent value means here, e.g. "No request was sent". */
  empty: string;
  label: string;
  maxHeight?: number;
}) {
  const [expanded, setExpanded] = React.useState(false);
  const text = React.useMemo(() => {
    if (value === undefined || value === null) return null;
    if (typeof value === "string") {
      try {
        return JSON.stringify(JSON.parse(value), null, 2);
      } catch {
        return value;
      }
    }
    return JSON.stringify(value, null, 2);
  }, [value]);

  if (text === null) {
    return <div className="rounded-md border border-dashed border-slate-300 px-4 py-6 text-center text-sm text-slate-500">{empty}</div>;
  }
  if (text === "{}" || text === "") {
    return <div className="rounded-md border border-dashed border-slate-300 px-4 py-6 text-center text-sm text-slate-500">The body was intentionally empty.</div>;
  }
  const body = (
    <pre className="json-view" aria-label={label}>
      {tokenize(text)}
    </pre>
  );
  const copy = () => {
    navigator.clipboard?.writeText(text).then(() => toast.success("Copied"));
  };
  return (
    <>
      <div className="group relative rounded-md border border-slate-300 bg-slate-50">
        <div className="absolute right-2 top-2 flex gap-1 opacity-0 transition-opacity focus-within:opacity-100 group-hover:opacity-100">
          <button type="button" onClick={copy} className="rounded bg-slate-100 p-1.5 text-slate-500 ring-1 ring-slate-300 hover:text-slate-900" aria-label={`Copy ${label}`}>
            <Copy className="h-3.5 w-3.5" />
          </button>
          <button type="button" onClick={() => setExpanded(true)} className="rounded bg-slate-100 p-1.5 text-slate-500 ring-1 ring-slate-300 hover:text-slate-900" aria-label={`Expand ${label}`}>
            <Maximize2 className="h-3.5 w-3.5" />
          </button>
        </div>
        <div className="overflow-auto p-4" style={{ maxHeight }}>
          {body}
        </div>
      </div>
      {expanded ? (
        <div className="fixed inset-0 z-50 grid place-items-center bg-black/70 p-6" role="dialog" aria-modal aria-label={label} onClick={() => setExpanded(false)}>
          <div className="flex max-h-full w-full max-w-4xl flex-col rounded-lg border border-slate-300 bg-slate-50 shadow-lg" onClick={(e) => e.stopPropagation()}>
            <div className="flex items-center justify-between border-b border-slate-200 px-4 py-2">
              <span className="text-sm text-slate-700">{label}</span>
              <div className="flex gap-1">
                <button type="button" onClick={copy} className="rounded p-1.5 text-slate-500 hover:text-slate-900" aria-label="Copy">
                  <Copy className="h-4 w-4" />
                </button>
                <button type="button" onClick={() => setExpanded(false)} className="rounded p-1.5 text-slate-500 hover:text-slate-900" aria-label="Close" autoFocus>
                  <X className="h-4 w-4" />
                </button>
              </div>
            </div>
            <div className="overflow-auto p-4">{body}</div>
          </div>
        </div>
      ) : null}
    </>
  );
}

/** Row of tabs (ARIA tablist) used by the inline inspector. */
export function Tabs<T extends string>({
  tabs,
  value,
  onChange,
  idBase
}: {
  tabs: { id: T; label: string; count?: number }[];
  value: T;
  onChange: (t: T) => void;
  idBase: string;
}) {
  const refs = React.useRef<(HTMLButtonElement | null)[]>([]);
  return (
    <div role="tablist" className="flex gap-5 border-b border-slate-200" onKeyDown={(e) => {
      const i = tabs.findIndex((t) => t.id === value);
      if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
        e.preventDefault();
        const n = (i + (e.key === "ArrowRight" ? 1 : tabs.length - 1)) % tabs.length;
        onChange(tabs[n].id);
        refs.current[n]?.focus();
      }
    }}>
      {tabs.map((t, i) => {
        const active = t.id === value;
        return (
          <button
            key={t.id}
            ref={(el) => { refs.current[i] = el; }}
            role="tab"
            id={`${idBase}-tab-${t.id}`}
            aria-selected={active}
            aria-controls={`${idBase}-panel-${t.id}`}
            tabIndex={active ? 0 : -1}
            type="button"
            onClick={() => onChange(t.id)}
            className={clsx(
              "-mb-px border-b-2 px-0.5 pb-2 pt-1 text-sm",
              active ? "border-brand-500 text-slate-950" : "border-transparent text-slate-500 hover:text-slate-800"
            )}
          >
            {t.label}
            {t.count !== undefined ? <span className="ml-1.5 text-xs text-slate-500">{t.count}</span> : null}
          </button>
        );
      })}
    </div>
  );
}

export function formatMs(ms?: number | null) {
  if (ms == null || !Number.isFinite(ms)) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  return `${(ms / 1000).toFixed(1)} s`;
}

export function relativeTime(value?: string | null) {
  if (!value) return "—";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return value;
  const diff = Date.now() - d.getTime();
  const min = Math.round(diff / 60000);
  if (min < 1) return "just now";
  if (min < 60) return `${min} min ago`;
  const h = Math.round(min / 60);
  if (h < 24) return `${h} h ago`;
  const today = new Date();
  const sameDay = d.toDateString() === today.toDateString();
  const time = d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  if (sameDay) return `Today, ${time}`;
  const sameYear = d.getFullYear() === today.getFullYear();
  return `${d.toLocaleDateString(undefined, { month: "short", day: "numeric", ...(sameYear ? {} : { year: "numeric" }) })}, ${time}`;
}
