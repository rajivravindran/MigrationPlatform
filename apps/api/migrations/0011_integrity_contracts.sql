-- P0 contract repair (principal review F1/F5, architecture review A3).

-- F1: an operator decision (from-start retry, confirm-not-applied) is one
-- generation; the ledger records which generation last spent its permission
-- on an operation so redelivery of that generation cannot send again.
ALTER TABLE operations ADD COLUMN decision_generation TEXT;

-- F5: every row-outcome write bumps the job's outcome revision. Exports are
-- written under a revision-specific prefix and published only while that
-- revision is current, so a slow export can never replace newer evidence.
ALTER TABLE jobs ADD COLUMN outcome_revision BIGINT NOT NULL DEFAULT 0;

CREATE OR REPLACE FUNCTION bump_job_outcome_revision() RETURNS trigger AS $$
BEGIN
  UPDATE jobs SET outcome_revision = outcome_revision + 1 WHERE id = NEW.job_id;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER job_rows_bump_revision
  AFTER INSERT OR UPDATE OF status, attempts, payload_json, response_json, last_error ON job_rows
  FOR EACH ROW EXECUTE FUNCTION bump_job_outcome_revision();

-- A3: pause/resume are recorded as commands with a stable identity before the
-- signal is sent, and cleared once the workflow confirms them. The
-- reconciler converges jobs whose confirmation never happened.
ALTER TABLE jobs
  ADD COLUMN control_requested TEXT CHECK (control_requested IN ('pause', 'resume')),
  ADD COLUMN control_command_id TEXT,
  ADD COLUMN control_requested_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS jobs_control_pending_idx
  ON jobs (control_requested_at)
  WHERE control_requested IS NOT NULL;
