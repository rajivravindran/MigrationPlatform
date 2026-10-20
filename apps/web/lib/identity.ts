import { basename, sourceSummary, type JobSourceFields } from "./job-source";

/**
 * Meaningful migration identity (product plan UX-01).
 *
 * Lead with purpose, then package › file, with the execution id searchable
 * but secondary. Every value comes from stored execution metadata; where a
 * field is missing the fallback says so instead of inventing a label.
 */
export type IdentityJob = JobSourceFields & {
  id: number;
  rule_template_id: number;
  rule_template_version?: number | null;
  started_at?: string | null;
  created_at?: string | null;
};

export type TemplateName = { id: number; name: string; version?: number };

export type MigrationIdentity = {
  /** Migration purpose: the template's display name, or a truthful fallback. */
  purpose: string;
  /** True when purpose is a fallback because the template name is unknown. */
  purposeIsFallback: boolean;
  /** Archive filename when the execution came from a package. */
  packageName?: string;
  /** Source file name. */
  fileName?: string;
  /** Execution id, formatted for display. */
  executionId: string;
  batchId?: number | null;
  templateVersion?: number | null;
};

export function templateNameIndex(templates: TemplateName[] | undefined): Map<number, string> {
  const m = new Map<number, string>();
  (templates ?? []).forEach((t) => m.set(t.id, t.name));
  return m;
}

export function migrationIdentity(job: IdentityJob, names: Map<number, string>): MigrationIdentity {
  const s = sourceSummary(job);
  const name = names.get(job.rule_template_id);
  return {
    purpose: name ?? `Template #${job.rule_template_id}`,
    purposeIsFallback: !name,
    packageName: s.batch_id ? s.package_filename || basename(s.package_key) || undefined : undefined,
    fileName: s.batch_stage_file || s.filename || basename(s.key) || undefined,
    executionId: `#${job.id}`,
    batchId: s.batch_id,
    templateVersion: job.rule_template_version ?? null
  };
}

/** Human status text for a migration: distinguishes failure from cancellation and unresolved work. */
export function outcomeLabel(status: string, totals?: { failed?: number; unresolved?: number }): { label: string; tone: "ok" | "fail" | "warn" | "neutral" | "info" } {
  const unresolved = totals?.unresolved ?? 0;
  switch (status) {
    case "succeeded":
      return { label: "Reconciled", tone: "ok" };
    case "failed":
      return unresolved > 0 ? { label: "Needs attention", tone: "warn" } : { label: "Needs attention", tone: "fail" };
    case "cancelled":
      return { label: "Cancelled", tone: "neutral" };
    case "paused":
      return { label: "Paused", tone: "warn" };
    case "running":
      return { label: "Running", tone: "info" };
    case "pending":
      return { label: "Queued", tone: "neutral" };
    default:
      return { label: status.replace(/_/g, " "), tone: "neutral" };
  }
}

/** Searchable text for a migration: name, archive, file or execution id. */
export function searchText(identity: MigrationIdentity): string {
  return [identity.purpose, identity.packageName, identity.fileName, identity.executionId, String(identity.batchId ?? "")]
    .filter(Boolean)
    .join(" ")
    .toLowerCase();
}
