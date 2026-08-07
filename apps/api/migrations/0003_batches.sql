-- P2: archive batches with ordered stages (manifest.json inside tar.gz/zip).

CREATE TYPE batch_status AS ENUM (
  'pending',
  'running',
  'succeeded',
  'failed',
  'partial',
  'quarantined'
);

CREATE TABLE batches (
  id                     BIGSERIAL PRIMARY KEY,
  org_id                 BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  schedule_id            BIGINT REFERENCES schedules(id) ON DELETE SET NULL,
  connector_id           BIGINT REFERENCES connectors(id) ON DELETE SET NULL,
  batch_key              TEXT,
  source_ref             JSONB NOT NULL,
  status                 batch_status NOT NULL DEFAULT 'pending',
  on_stage_failure       TEXT NOT NULL DEFAULT 'stop'
                         CHECK (on_stage_failure IN ('stop', 'continue')),
  manifest_json          JSONB,
  temporal_workflow_id   TEXT UNIQUE,
  temporal_run_id        TEXT,
  error_message          TEXT,
  quarantine_ref         JSONB,
  started_at             TIMESTAMPTZ,
  finished_at            TIMESTAMPTZ,
  created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX batches_org_status_idx ON batches(org_id, status);
CREATE INDEX batches_schedule_idx ON batches(schedule_id);

CREATE TABLE batch_stages (
  id                       BIGSERIAL PRIMARY KEY,
  batch_id                 BIGINT NOT NULL REFERENCES batches(id) ON DELETE CASCADE,
  stage_index              INT NOT NULL,
  stage_key                TEXT NOT NULL,
  file_path                TEXT NOT NULL,
  template_key             TEXT NOT NULL,
  rule_template_id         BIGINT REFERENCES rule_templates(id),
  rule_template_version    INT,
  job_id                   BIGINT REFERENCES jobs(id) ON DELETE SET NULL,
  status                   batch_status NOT NULL DEFAULT 'pending',
  on_stage_failure         TEXT CHECK (on_stage_failure IS NULL OR on_stage_failure IN ('stop', 'continue')),
  error_message            TEXT,
  created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (batch_id, stage_index),
  UNIQUE (batch_id, stage_key)
);
CREATE INDEX batch_stages_batch_idx ON batch_stages(batch_id);
CREATE INDEX batch_stages_job_idx ON batch_stages(job_id);

ALTER TABLE jobs
  ADD COLUMN batch_id BIGINT REFERENCES batches(id) ON DELETE SET NULL,
  ADD COLUMN batch_stage_id BIGINT REFERENCES batch_stages(id) ON DELETE SET NULL;
CREATE INDEX jobs_batch_idx ON jobs(batch_id);
