"use client";

import { PlanEditor } from "@/components/plans/PlanEditor";
import { Breadcrumbs, PageHeader } from "@/components/ui";

export default function NewPlanPage() {
  return (
    <div className="space-y-6">
      <PageHeader
        title="Design a plan"
        eyebrow={<Breadcrumbs items={[{ label: "Plans", href: "/plans" }, { label: "New" }]} />}
        description="Lay out the steps of the migration and what each must wait for. No files are needed now: they arrive later, and each one is held until the steps it depends on have completed."
      />
      <PlanEditor />
    </div>
  );
}
