"use client";

import { clsx } from "clsx";
import {
  Boxes,
  Cable,
  CalendarClock,
  ChevronDown,
  FileStack,
  GitFork,
  LayoutGrid,
  LogOut,
  Settings,
  Workflow
} from "lucide-react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useEffect, useRef, useState } from "react";

import { logout, useMe } from "@/lib/auth";
import { WORKSPACES, navFor, useWorkspace, type NavItem } from "@/lib/workspace";

const ICONS: Record<NavItem["icon"], React.ComponentType<{ className?: string }>> = {
  overview: LayoutGrid,
  migrations: Workflow,
  packages: Boxes,
  campaigns: GitFork,
  templates: FileStack,
  connectors: Cable,
  schedules: CalendarClock,
  settings: Settings
};

function Mark() {
  return (
    <span aria-hidden className="grid h-8 w-8 place-items-center rounded-md bg-brand-500/20 text-brand-700 ring-1 ring-brand-300/40">
      <Workflow className="h-4 w-4" />
    </span>
  );
}

/** True after hydration; the account block depends on browser storage. */
function useMounted() {
  const [m, setM] = useState(false);
  useEffect(() => setM(true), []);
  return m;
}

function initials(email?: string) {
  if (!email) return "?";
  const name = email.split("@")[0];
  const parts = name.split(/[._-]/).filter(Boolean);
  return (parts.length > 1 ? parts[0][0] + parts[1][0] : name.slice(0, 2)).toUpperCase();
}

export function Topbar() {
  const pathname = usePathname();
  const inDetail = /^\/(jobs|batches)\/\d+/.test(pathname ?? "");
  return (
    <header className="sticky top-0 z-30 border-b border-slate-200 bg-white/95 backdrop-blur">
      <div className="flex h-14 items-center gap-4 px-4 sm:px-6">
        <Link href="/" className="flex shrink-0 items-center gap-2.5 text-[15px] font-semibold tracking-tight text-slate-950">
          <Mark />
          <span className="hidden sm:inline">Migration Platform</span>
        </Link>
        <div className="ml-auto flex items-center gap-1 rounded-lg bg-slate-50 p-1 text-sm ring-1 ring-slate-200" role="tablist" aria-label="View">
          <Link
            href="/"
            role="tab"
            aria-selected={!inDetail}
            className={clsx("rounded-md px-3 py-1.5", !inDetail ? "bg-slate-100 text-slate-950 ring-1 ring-slate-300" : "text-slate-500 hover:text-slate-800")}
          >
            Operations overview
          </Link>
          <Link
            href="/jobs"
            role="tab"
            aria-selected={inDetail}
            className={clsx("rounded-md px-3 py-1.5", inDetail ? "bg-slate-100 text-slate-950 ring-1 ring-slate-300" : "text-slate-500 hover:text-slate-800")}
          >
            Migration details
          </Link>
        </div>
      </div>
    </header>
  );
}

function WorkspaceSwitcher() {
  const { workspace, available, setWorkspace } = useWorkspace();
  const [open, setOpen] = useState(false);
  const ref = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!open) return;
    const onDoc = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", onDoc);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDoc);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  if (available.length <= 1) {
    return <div className="text-xs text-slate-500">{WORKSPACES[workspace].who}</div>;
  }
  return (
    <div ref={ref} className="relative">
      <button
        type="button"
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-center justify-between gap-2 rounded-md px-1 py-0.5 text-left text-xs text-slate-500 hover:text-slate-800"
        data-testid="workspace-switcher"
      >
        <span>{WORKSPACES[workspace].who}</span>
        <ChevronDown className="h-3.5 w-3.5" />
      </button>
      {open ? (
        <ul
          role="listbox"
          aria-label="Switch workspace"
          className="absolute bottom-full left-0 z-40 mb-2 w-64 overflow-hidden rounded-lg border border-slate-300 bg-slate-50 py-1 shadow-lg"
        >
          {available.map((ws) => (
            <li key={ws}>
              <button
                type="button"
                role="option"
                aria-selected={ws === workspace}
                onClick={() => {
                  setWorkspace(ws);
                  setOpen(false);
                }}
                className={clsx(
                  "block w-full px-3 py-2 text-left hover:bg-slate-100",
                  ws === workspace ? "text-slate-950" : "text-slate-700"
                )}
              >
                <div className="text-sm">{WORKSPACES[ws].label}</div>
                <div className="text-xs text-slate-500">{WORKSPACES[ws].lead}</div>
              </button>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

export function Sidebar() {
  const pathname = usePathname();
  const me = useMe();
  const mounted = useMounted();
  const { workspace } = useWorkspace();
  const items = navFor(workspace);
  return (
    <aside className="hidden w-[164px] shrink-0 flex-col border-r border-slate-200 bg-white md:flex" aria-label="Workspace">
      <nav aria-label="Primary navigation" className="flex flex-col gap-0.5 p-3 pt-5">
        {items.map((item) => {
          const Icon = ICONS[item.icon];
          const active = item.href === "/" ? pathname === "/" : pathname?.startsWith(item.href);
          return (
            <Link
              key={item.href}
              href={item.href}
              aria-current={active ? "page" : undefined}
              className={clsx(
                "flex items-center gap-2.5 rounded-md px-2.5 py-2 text-sm",
                active ? "bg-brand-50 text-brand-800 ring-1 ring-brand-200" : "text-slate-600 hover:bg-slate-50 hover:text-slate-900"
              )}
            >
              <Icon className="h-4 w-4 shrink-0" aria-hidden />
              {item.label}
            </Link>
          );
        })}
      </nav>
      <div className="mt-auto border-t border-slate-200 p-3">
        {!mounted ? null : me.data ? (
          <div className="flex items-start gap-2.5">
            <span className="grid h-8 w-8 shrink-0 place-items-center rounded-full bg-brand-50 text-xs font-semibold text-brand-700 ring-1 ring-brand-200">
              {initials(me.data.email)}
            </span>
            <div className="min-w-0 flex-1">
              <div className="truncate text-sm text-slate-900" title={me.data.email}>
                {me.data.email.split("@")[0]}
              </div>
              <WorkspaceSwitcher />
            </div>
            <button aria-label="Sign out" className="rounded p-1 text-slate-500 hover:text-slate-900" onClick={logout}>
              <LogOut className="h-4 w-4" />
            </button>
          </div>
        ) : me.isLoading ? null : (
          <Link href="/login" className="block rounded-md bg-brand-500 px-3 py-1.5 text-center text-sm text-white hover:bg-brand-600">
            Sign in
          </Link>
        )}
      </div>
    </aside>
  );
}

/** Small-screen nav: the sidebar areas as a scrolling strip. */
export function MobileNav() {
  const pathname = usePathname();
  const { workspace } = useWorkspace();
  return (
    <nav aria-label="Primary navigation" className="flex gap-1 overflow-x-auto border-b border-slate-200 bg-white px-3 py-2 md:hidden">
      {navFor(workspace).map((item) => {
        const active = item.href === "/" ? pathname === "/" : pathname?.startsWith(item.href);
        return (
          <Link
            key={item.href}
            href={item.href}
            className={clsx("shrink-0 rounded-md px-2.5 py-1.5 text-sm", active ? "bg-brand-50 text-brand-800" : "text-slate-600")}
          >
            {item.label}
          </Link>
        );
      })}
    </nav>
  );
}
