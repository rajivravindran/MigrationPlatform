"use client";

import { clsx } from "clsx";
import Link from "next/link";
import { usePathname } from "next/navigation";
import * as React from "react";

import { logout, useMe } from "@/lib/auth";

export function Button({
  variant = "default",
  className,
  ...props
}: React.ButtonHTMLAttributes<HTMLButtonElement> & { variant?: "default" | "ghost" | "danger" }) {
  const base =
    "inline-flex items-center justify-center rounded-md px-3 py-1.5 text-sm font-medium transition disabled:opacity-50 disabled:cursor-not-allowed";
  const styles = {
    default: "bg-brand-600 text-white hover:bg-brand-700",
    ghost: "bg-transparent text-slate-700 hover:bg-slate-200",
    danger: "bg-rose-600 text-white hover:bg-rose-700"
  } as const;
  return <button className={clsx(base, styles[variant], className)} {...props} />;
}

export function Card({
  children,
  className
}: React.PropsWithChildren<{ className?: string }>) {
  return (
    <div className={clsx("rounded-lg border border-slate-200 bg-white p-4 shadow-sm", className)}>
      {children}
    </div>
  );
}

export function Badge({
  children,
  tone = "neutral"
}: React.PropsWithChildren<{ tone?: "ok" | "fail" | "warn" | "neutral" }>) {
  const tones = {
    ok: "bg-emerald-100 text-emerald-800",
    fail: "bg-rose-100 text-rose-800",
    warn: "bg-amber-100 text-amber-800",
    neutral: "bg-slate-200 text-slate-800"
  } as const;
  return (
    <span className={clsx("inline-block rounded px-2 py-0.5 text-xs font-medium", tones[tone])}>
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
          "w-full rounded-md border border-slate-300 px-2 py-1.5 text-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500",
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
        "w-full rounded-md border border-slate-300 px-2 py-1.5 font-mono text-xs focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500",
        props.className
      )}
    />
  );
});

export function EmptyState({ title, hint, action }: { title: string; hint?: string; action?: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-dashed border-slate-300 bg-slate-50 p-8 text-center">
      <div className="text-sm font-medium text-slate-700">{title}</div>
      {hint ? <div className="mt-1 text-xs text-slate-500">{hint}</div> : null}
      {action ? <div className="mt-3">{action}</div> : null}
    </div>
  );
}

const NAV = [
  { href: "/templates", label: "Templates" },
  { href: "/jobs", label: "Jobs" },
  { href: "/batches", label: "Batches" },
  { href: "/schedules", label: "Schedules" },
  { href: "/connectors", label: "Connectors" },
  { href: "/settings", label: "Settings" }
] as const;

export function Nav() {
  const pathname = usePathname();
  const me = useMe();
  return (
    <div className="flex items-center gap-4">
      <nav className="flex gap-1">
        {NAV.map((i) => (
          <Link
            key={i.href}
            href={i.href}
            className={clsx(
              "rounded-md px-3 py-1.5 text-sm font-medium",
              pathname?.startsWith(i.href)
                ? "bg-brand-50 text-brand-700"
                : "text-slate-600 hover:bg-slate-100"
            )}
          >
            {i.label}
          </Link>
        ))}
      </nav>
      <div className="flex items-center gap-2 text-xs text-slate-500">
        {me.data ? (
          <>
            <Badge tone="neutral">{me.data.role}</Badge>
            <span>{me.data.email}</span>
            <button className="text-slate-600 hover:text-brand-700" onClick={logout}>
              Sign out
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
