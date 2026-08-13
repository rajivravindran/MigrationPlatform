export type JobSource = {
  kind?: string;
  bucket?: string | null;
  key?: string | null;
  filename?: string | null;
  size?: number | null;
  etag?: string | null;
  sha256?: string | null;
  batch_id?: number | null;
  batch_stage_key?: string | null;
  batch_stage_file?: string | null;
  package_key?: string | null;
  package_filename?: string | null;
};

export type JobSourceFields = {
  batch_id?: number | null;
  source?: JobSource | null;
  source_ref?: JobSource | null;
};

export function basename(path?: string | null): string | undefined {
  if (!path) return undefined;
  const part = path.split(/[/\\]/).filter(Boolean).pop();
  return part || undefined;
}

export function formatBytes(n?: number | null): string | undefined {
  if (n == null || !Number.isFinite(n)) return undefined;
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  if (n < 1024 * 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`;
  return `${(n / (1024 * 1024 * 1024)).toFixed(1)} GB`;
}

export function sourceSummary(job: JobSourceFields): JobSource {
  if (job.source) return job.source;
  const ref = job.source_ref;
  return {
    kind: ref?.type ?? ref?.kind,
    bucket: ref?.bucket,
    key: ref?.key,
    filename: ref?.filename ?? basename(ref?.key),
    size: ref?.size,
    etag: ref?.etag,
    batch_id: job.batch_id
  };
}

export function sourceListLabel(job: JobSourceFields): string {
  const s = sourceSummary(job);
  if (s.batch_id) {
    const pkg = s.package_filename || basename(s.package_key);
    const stage = s.batch_stage_file || s.filename || basename(s.key);
    return [pkg ? `Package: ${pkg}` : null, stage ? `Stage: ${stage}` : null]
      .filter(Boolean)
      .join(" · ") || `Batch #${s.batch_id}`;
  }
  return s.filename || basename(s.key) || s.key || "—";
}
