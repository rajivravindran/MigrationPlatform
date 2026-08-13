"use client";

import { clsx } from "clsx";
import {
  Activity,
  Boxes,
  Cable,
  CalendarClock,
  ChevronRight,
  FileStack,
  LayoutDashboard,
  LogOut,
  Settings
} from "lucide-react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import * as React from "react";

import { logout, useMe } from "@/lib/auth";

export function Button({
  variant = "default",
  size = "default",
  className,
  ...props
}: React.ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "default" | "secondary" | "ghost" | "danger";
  size?: "default" | "sm";
}) {
  const base =
    "inline-flex items-center justify-center gap-1.5 rounded-md font-medium transition focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand-500 focus-visible:ring-offset-2 disabled:cursor-not-allowed disabled:opacity-50";
  const styles = {
    default: "bg-brand-600 text-white shadow-sm hover:bg-brand-700",
    secondary: "border border-slate-300 bg-white text-slate-700 shadow-sm hover:bg-slate-50",
    ghost: "bg-transparent text-slate-600 hover:bg-slate-100 hover:text-slate-900",
    danger: "bg-rose-600 text-white shadow-sm hover:bg-rose-700"
  } as const;
  const sizes = { default: "min-h-9 px-3 py-1.5 text-sm", sm: "min-h-8 px-2.5 py-1 text-xs" };
  return <button className={clsx(base, styles[variant], sizes[size], className)} {...props} />;
}

export function Card({
  children,
  className
}: React.PropsWithChildren<{ className?: string }>) {
  return (
    <div className={clsx("rounded-lg border border-slate-200 bg-white p-5 shadow-sm", className)}>
      {children}
    </div>
  );
}

export function Badge({
  children,
  tone = "neutral"
}: React.PropsWithChildren<{ tone?: StatusTone }>) {
  const tones = {
    ok: "bg-emerald-100 text-emerald-800",
    fail: "bg-rose-100 text-rose-800",
    warn: "bg-amber-100 text-amber-800",
    neutral: "bg-slate-100 text-slate-700"
  } as const;
  return (
    <span className={clsx("inline-flex items-center rounded-full px-2 py-0.5 text-xs font-medium capitalize", tones[tone])}>
      {children}
    </span>
  );
}

export const Input = React.forwardRef<HTMLInputElement, React.InputHTMLAttributes<HTMLInputElement>>(
  function Input(props, ref) {
    return (
      <input
        ref={ref}
        {...props}
        className={clsx(
          "min-h-10 w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm shadow-sm placeholder:text-slate-400 focus:border-brand-500 focus:outline-none focus:ring-2 focus:ring-brand-500/20 disabled:bg-slate-100",
          props.className
        )}
      />
    );
  }
);

export const Textarea = React.forwardRef<
  HTMLTextAreaElement,
  React.TextareaHTMLAttributes<HTMLTextAreaElement>
>(function Textarea(props, ref) {
  return (
    <textarea
      ref={ref}
      {...props}
      className={clsx(
        "w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm shadow-sm placeholder:text-slate-400 focus:border-brand-500 focus:outline-none focus:ring-2 focus:ring-brand-500/20",
        props.className
      )}
    />
  );
});

export function EmptyState({ title, hint, action }: { title: string; hint?: string; action?: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-dashed border-slate-300 bg-white p-10 text-center">
      <div className="text-base font-medium text-slate-800">{title}</div>
      {hint ? <div className="mx-auto mt-1 max-w-lg text-sm text-slate-500">{hint}</div> : null}
      {action ? <div className="mt-4">{action}</div> : null}
    </div>
  );
}

export type StatusTone = "ok" | "fail" | "warn" | "neutral";

export function statusTone(status?: string): StatusTone {
  switch (status?.toLowerCase()) {
    case "succeeded":
    case "published":
    case "enabled":
    case "healthy":
    case "active":
      return "ok";
    case "failed":
    case "quarantined":
    case "expired":
      return "fail";
    case "paused":
    case "partial":
    case "cancelled":
    case "draft":
    case "trial":
      return "warn";
    default:
      return "neutral";
  }
}

export function StatusBadge({ status }: { status: string }) {
  return <Badge tone={statusTone(status)}>{status.replaceAll("_", " ")}</Badge>;
}

export const Select = React.forwardRef<HTMLSelectElement, React.SelectHTMLAttributes<HTMLSelectElement>>(
  function Select(props, ref) {
    return (
      <select
        ref={ref}
        {...props}
        className={clsx(
          "min-h-10 w-full rounded-md border border-slate-300 bg-white px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-2 focus:ring-brand-500/20 disabled:bg-slate-100",
          props.className
        )}
      />
    );
  }
);

export function FormField({
  label,
  hint,
  error,
  required,
  children
}: React.PropsWithChildren<{ label: string; hint?: string; error?: string; required?: boolean }>) {
  const id = React.useId();
  return (
    <label className="block text-sm font-medium text-slate-700">
      <span>{label}{required ? <span className="text-rose-600"> *</span> : null}</span>
      {React.isValidElement(children)
        ? React.cloneElement(children as React.ReactElement<any>, {
            id,
            "aria-invalid": Boolean(error),
            "aria-describedby": hint || error ? `${id}-help` : undefined,
            className: clsx("mt-1.5", (children as React.ReactElement<any>).props.className)
          })
        : children}
      {hint || error ? (
        <span id={`${id}-help`} className={clsx("mt-1.5 block text-xs", error ? "text-rose-700" : "text-slate-500")}>
          {error ?? hint}
        </span>
      ) : null}
    </label>
  );
}

export function PageHeader({
  title,
  description,
  eyebrow,
  actions
}: {
  title: string;
  description?: string;
  eyebrow?: React.ReactNode;
  actions?: React.ReactNode;
}) {
  return (
    <div className="flex flex-col justify-between gap-4 sm:flex-row sm:items-start">
      <div className="min-w-0">
        {eyebrow ? <div className="mb-1 text-sm text-slate-500">{eyebrow}</div> : null}
        <h1 className="text-2xl font-semibold tracking-tight text-slate-950">{title}</h1>
        {description ? <p className="mt-1 max-w-3xl text-sm leading-6 text-slate-600">{description}</p> : null}
      </div>
      {actions ? <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div> : null}
    </div>
  );
}

export function Breadcrumbs({ items }: { items: { label: string; href?: string }[] }) {
  return (
    <nav aria-label="Breadcrumb" className="flex items-center gap-1 text-sm text-slate-500">
      {items.map((item, index) => (
        <React.Fragment key={`${item.label}-${index}`}>
          {index ? <ChevronRight aria-hidden className="h-3.5 w-3.5" /> : null}
          {item.href ? <Link className="hover:text-brand-700 hover:underline" href={item.href}>{item.label}</Link> : <span aria-current="page">{item.label}</span>}
        </React.Fragment>
      ))}
    </nav>
  );
}

export function ErrorState({ title = "Couldn’t load this page", error, retry }: { title?: string; error?: unknown; retry?: () => void }) {
  return (
    <div role="alert" className="rounded-lg border border-rose-200 bg-rose-50 p-4">
      <div className="font-medium text-rose-900">{title}</div>
      <p className="mt-1 text-sm text-rose-700">{error instanceof Error ? error.message : String(error ?? "Try again.")}</p>
      {retry ? <Button variant="secondary" size="sm" className="mt-3" onClick={retry}>Try again</Button> : null}
    </div>
  );
}

export function LoadingState({ label = "Loading" }: { label?: string }) {
  return <div role="status" className="animate-pulse rounded-lg border border-slate-200 bg-white p-5 text-sm text-slate-500">{label}…</div>;
}

export function formatDateTime(value?: string | null) {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

const NAV = [
  { href: "/", label: "Overview", icon: LayoutDashboard },
  { href: "/templates", label: "Templates", icon: FileStack },
  { href: "/connectors", label: "Connectors", icon: Cable },
  { href: "/schedules", label: "Schedules", icon: CalendarClock },
  { href: "/jobs", label: "Jobs", icon: Activity },
  { href: "/batches", label: "Batches", icon: Boxes },
  { href: "/settings", label: "Settings", icon: Settings }
] as const;

export function Nav() {
  const pathname = usePathname();
  const me = useMe();
  return (
    <div className="flex min-w-0 flex-1 items-center justify-end gap-3">
      <nav aria-label="Primary navigation" className="flex min-w-0 items-center gap-1 overflow-x-auto">
        {NAV.map((i) => (
          <Link
            key={i.href}
            href={i.href}
            className={clsx(
              "inline-flex min-h-9 shrink-0 items-center gap-1.5 rounded-md px-2.5 py-1.5 text-sm font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand-500",
              (i.href === "/" ? pathname === "/" : pathname?.startsWith(i.href))
                ? "bg-brand-50 text-brand-700"
                : "text-slate-600 hover:bg-slate-100 hover:text-slate-900"
            )}
          >
            <i.icon aria-hidden className="h-4 w-4" />
            {i.label}
          </Link>
        ))}
      </nav>
      <div className="hidden shrink-0 items-center gap-2 text-xs text-slate-500 xl:flex">
        {me.data ? (
          <>
            <Badge tone="neutral">{me.data.role}</Badge>
            <span>{me.data.email}</span>
            <button aria-label="Sign out" className="rounded p-1 text-slate-500 hover:bg-slate-100 hover:text-brand-700" onClick={logout}>
              <LogOut className="h-4 w-4" />
            </button>
          </>
        ) : me.isLoading ? null : (
          <Link
            href="/login"
            className="rounded-md bg-brand-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-brand-700"
          >
            Sign in
          </Link>
        )}
      </div>
    </div>
  );
}
