"use client";

import { useQuery } from "@tanstack/react-query";

import { apiFetch, clearToken, getToken } from "./api";

export type Me = {
  id: number;
  email: string;
  role: "admin" | "editor" | "operator" | "viewer";
  org_id: number;
};

export function useMe() {
  return useQuery({
    queryKey: ["me"],
    enabled: typeof window !== "undefined" && !!getToken(),
    queryFn: () => apiFetch<Me>("/auth/me"),
    retry: false,
    staleTime: 30_000
  });
}

export function logout() {
  clearToken();
  if (typeof window !== "undefined") window.location.href = "/login";
}

export function canEdit(role: Me["role"] | undefined): boolean {
  return role === "admin" || role === "editor";
}

export function canOperate(role: Me["role"] | undefined): boolean {
  return role === "admin" || role === "operator";
}
