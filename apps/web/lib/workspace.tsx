"use client";

import { createContext, useContext, useEffect, useMemo, useState } from "react";

import { useMe, type Me } from "./auth";

/**
 * Role-specific workspaces (product plan "User experiences").
 *
 * A workspace is a view of the product for one kind of user: what the home
 * page leads with and which areas the sidebar offers. It changes what is shown,
 * never what is permitted — every action is still authorized by the API with
 * the account's role. A person whose role grants several workspaces can
 * switch between them; the choice is remembered in this browser only.
 */
export type Workspace = "operator" | "designer" | "coordinator" | "executive" | "admin";

export const WORKSPACES: Record<
  Workspace,
  { label: string; who: string; lead: string }
> = {
  operator: {
    label: "Migration operator",
    who: "Operator",
    lead: "Run packages, investigate failures, recover and reconcile."
  },
  designer: {
    label: "Mapping designer",
    who: "Designer",
    lead: "Map source fields, preview and test, publish releases."
  },
  coordinator: {
    label: "Migration coordinator",
    who: "Coordinator",
    lead: "Order packages, watch dependencies, unblock the next wave."
  },
  executive: {
    label: "Executive",
    who: "Executive",
    lead: "Planned, processed and reconciled totals; what is blocked and why."
  },
  admin: {
    label: "Administrator",
    who: "Administrator",
    lead: "Connections, access, limits and installation health."
  }
};

/** Workspaces each account role may open. Server roles stay the authority. */
export function workspacesFor(role: Me["role"] | undefined): Workspace[] {
  switch (role) {
    case "admin":
      return ["operator", "designer", "coordinator", "executive", "admin"];
    case "editor":
      return ["designer", "executive"];
    case "operator":
      return ["operator", "coordinator", "executive"];
    case "viewer":
      return ["executive"];
    default:
      return ["executive"];
  }
}

export type NavItem = { href: string; label: string; icon: "overview" | "migrations" | "packages" | "campaigns" | "templates" | "connectors" | "schedules" | "settings" };

/** Sidebar areas per workspace, most-used first. */
export function navFor(ws: Workspace): NavItem[] {
  switch (ws) {
    case "operator":
      return [
        { href: "/", label: "Overview", icon: "overview" },
        { href: "/plans", label: "Plans", icon: "campaigns" },
        { href: "/jobs", label: "Migrations", icon: "migrations" },
        { href: "/batches", label: "Packages", icon: "packages" },
        { href: "/schedules", label: "Schedules", icon: "schedules" }
      ];
    case "designer":
      return [
        { href: "/", label: "Overview", icon: "overview" },
        { href: "/templates", label: "Templates", icon: "templates" },
        { href: "/jobs", label: "Test runs", icon: "migrations" },
        { href: "/connectors", label: "Sources", icon: "connectors" }
      ];
    case "coordinator":
      return [
        { href: "/", label: "Overview", icon: "overview" },
        { href: "/plans", label: "Plans", icon: "campaigns" },
        { href: "/batches", label: "Packages", icon: "packages" },
        { href: "/jobs", label: "Migrations", icon: "migrations" },
        { href: "/schedules", label: "Schedules", icon: "schedules" }
      ];
    case "executive":
      return [
        { href: "/", label: "Overview", icon: "overview" },
        { href: "/plans", label: "Plans", icon: "campaigns" },
        { href: "/batches", label: "Packages", icon: "packages" },
        { href: "/jobs", label: "Migrations", icon: "migrations" }
      ];
    case "admin":
      return [
        { href: "/", label: "Overview", icon: "overview" },
        { href: "/connectors", label: "Connectors", icon: "connectors" },
        { href: "/templates", label: "Templates", icon: "templates" },
        { href: "/schedules", label: "Schedules", icon: "schedules" },
        { href: "/settings", label: "Settings", icon: "settings" }
      ];
  }
}

const KEY = "migration.workspace";

type Ctx = {
  workspace: Workspace;
  available: Workspace[];
  setWorkspace: (ws: Workspace) => void;
  role?: Me["role"];
};

const WorkspaceContext = createContext<Ctx>({
  workspace: "operator",
  available: ["operator"],
  setWorkspace: () => {}
});

export function WorkspaceProvider({ children }: { children: React.ReactNode }) {
  const me = useMe();
  const role = me.data?.role;
  const available = useMemo(() => workspacesFor(role), [role]);
  const [chosen, setChosen] = useState<Workspace | null>(null);

  useEffect(() => {
    try {
      const saved = window.localStorage.getItem(KEY) as Workspace | null;
      if (saved && saved in WORKSPACES) setChosen(saved);
    } catch {}
  }, []);

  const workspace: Workspace = chosen && available.includes(chosen) ? chosen : available[0];

  const value = useMemo<Ctx>(
    () => ({
      workspace,
      available,
      role,
      setWorkspace: (ws) => {
        if (!available.includes(ws)) return;
        setChosen(ws);
        try {
          window.localStorage.setItem(KEY, ws);
        } catch {}
      }
    }),
    [workspace, available, role]
  );

  return <WorkspaceContext.Provider value={value}>{children}</WorkspaceContext.Provider>;
}

export function useWorkspace() {
  return useContext(WorkspaceContext);
}
