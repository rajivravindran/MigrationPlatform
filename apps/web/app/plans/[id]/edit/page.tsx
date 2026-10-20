"use client";

import { useQuery } from "@tanstack/react-query";
import Link from "next/link";
import { useParams } from "next/navigation";

import { PlanEditor } from "@/components/plans/PlanEditor";
import { Breadcrumbs, Button, ErrorState, LoadingState, PageHeader } from "@/components/ui";
import { apiFetch } from "@/lib/api";
import type { PlanDetail } from "@/lib/plans";

export default function EditPlanPage() {
  const { id } = useParams<{ id: string }>();
  const { data, isLoading, error } = useQuery({
    queryKey: ["plans", id, "edit"],
    queryFn: () => apiFetch<PlanDetail>(`/plans/${id}`),
    refetchOnWindowFocus: false
  });
  if (isLoading) return <LoadingState label="Loading plan" />;
  if (error || !data) return <ErrorState title="Plan not found" error={error} />;
  const crumbs = [{ label: "Plans", href: "/plans" }, { label: data.plan.name, href: `/plans/${data.plan.id}` }, { label: "Edit" }];
  if (data.plan.status !== "draft") {
    return (
      <div className="space-y-4">
        <PageHeader title={data.plan.name} eyebrow={<Breadcrumbs items={crumbs} />} description={`Version ${data.plan.version} is published and can no longer change. Revise it from the plan page to create version ${data.plan.version + 1}.`} />
        <Link href={`/plans/${data.plan.id}`}><Button variant="secondary">Back to plan</Button></Link>
      </div>
    );
  }
  return (
    <div className="space-y-6">
      <PageHeader title={`Edit ${data.plan.name}`} eyebrow={<Breadcrumbs items={crumbs} />} description={`Draft version ${data.plan.version}.`} />
      <PlanEditor key={data.plan.id} existing={data} />
    </div>
  );
}
