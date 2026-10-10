-- Execution failures that happen outside a row (ingest, preprocess, license,
-- persistence, termination) are recorded on the job. A job with last_error is
-- failed: retrying rows cannot cover source records that were never read.
ALTER TABLE jobs
  ADD COLUMN last_error TEXT,
  -- Export availability is separate from the migration outcome: a job can
  -- succeed while its results file failed to upload, and the export can be
  -- retried without touching the destination.
  ADD COLUMN export_status TEXT CHECK (export_status IN ('pending', 'succeeded', 'failed')),
  ADD COLUMN export_error TEXT;

-- The reconciler scans non-terminal jobs that have not changed recently.
CREATE INDEX IF NOT EXISTS jobs_active_updated_idx
  ON jobs (updated_at)
  WHERE status IN ('pending', 'running', 'paused');
