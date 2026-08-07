CREATE EXTENSION IF NOT EXISTS citext;

-- Enum types for status columns.
CREATE TYPE user_role       AS ENUM ('admin', 'editor', 'operator', 'viewer');
CREATE TYPE job_status      AS ENUM ('pending', 'running', 'paused', 'succeeded', 'failed', 'cancelled');
CREATE TYPE row_status      AS ENUM ('pending', 'running', 'succeeded', 'failed', 'skipped');
CREATE TYPE connector_kind  AS ENUM ('salesforce', 'watched_prefix', 'custom');
CREATE TYPE schedule_overlap AS ENUM ('skip', 'buffer_one', 'buffer_all', 'cancel_other', 'allow_all');

CREATE TABLE organizations (
  id          BIGSERIAL PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE users (
  id             BIGSERIAL PRIMARY KEY,
  org_id         BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  email          CITEXT UNIQUE,
  role           user_role NOT NULL DEFAULT 'viewer',
  password_hash  TEXT NOT NULL,
  disabled       BOOLEAN NOT NULL DEFAULT false,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX users_org_id_idx ON users(org_id);

CREATE TABLE secrets (
  id            BIGSERIAL PRIMARY KEY,
  org_id        BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  name          TEXT NOT NULL,
  ciphertext    BYTEA NOT NULL,
  nonce         BYTEA NOT NULL,
  kms_key_id    TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, name)
);

CREATE TABLE connectors (
  id             BIGSERIAL PRIMARY KEY,
  org_id         BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  name           TEXT NOT NULL,
  connector_kind connector_kind NOT NULL,
  config_json    JSONB NOT NULL,
  secret_id      BIGINT REFERENCES secrets(id) ON DELETE SET NULL,
  created_by     BIGINT REFERENCES users(id),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, name)
);

CREATE TABLE rule_templates (
  id             BIGSERIAL PRIMARY KEY,
  org_id         BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  template_key   TEXT NOT NULL,
  version        INT  NOT NULL,
  name           TEXT NOT NULL,
  schema_json    JSONB NOT NULL,
  published      BOOLEAN NOT NULL DEFAULT false,
  created_by     BIGINT REFERENCES users(id),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, template_key, version)
);
CREATE INDEX rule_templates_key_idx ON rule_templates(org_id, template_key);

CREATE TABLE schedules (
  id                      BIGSERIAL PRIMARY KEY,
  org_id                  BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  name                    TEXT NOT NULL,
  rule_template_id        BIGINT NOT NULL REFERENCES rule_templates(id),
  rule_template_version   INT NOT NULL,
  connector_id            BIGINT NOT NULL REFERENCES connectors(id),
  spec_json               JSONB NOT NULL,
  timezone                TEXT NOT NULL DEFAULT 'UTC',
  overlap_policy          schedule_overlap NOT NULL DEFAULT 'skip',
  catchup_window_seconds  INT NOT NULL DEFAULT 3600,
  enabled                 BOOLEAN NOT NULL DEFAULT true,
  next_run_at             TIMESTAMPTZ,
  last_run_at             TIMESTAMPTZ,
  temporal_schedule_id    TEXT UNIQUE,
  created_by              BIGINT REFERENCES users(id),
  created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, name)
);
CREATE INDEX schedules_enabled_idx ON schedules(org_id, enabled);

CREATE TABLE jobs (
  id                     BIGSERIAL PRIMARY KEY,
  org_id                 BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  rule_template_id       BIGINT NOT NULL REFERENCES rule_templates(id),
  rule_template_version  INT NOT NULL,
  schedule_id            BIGINT REFERENCES schedules(id) ON DELETE SET NULL,
  source_ref             JSONB NOT NULL,
  status                 job_status NOT NULL DEFAULT 'pending',
  totals_json            JSONB NOT NULL DEFAULT '{}'::jsonb,
  temporal_workflow_id   TEXT UNIQUE,
  temporal_run_id        TEXT,
  started_at             TIMESTAMPTZ,
  paused_at              TIMESTAMPTZ,
  finished_at            TIMESTAMPTZ,
  created_by             BIGINT REFERENCES users(id),
  created_at             TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at             TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX jobs_org_status_idx ON jobs(org_id, status);
CREATE INDEX jobs_schedule_idx   ON jobs(schedule_id);

CREATE TABLE schedule_runs (
  id                 BIGSERIAL PRIMARY KEY,
  schedule_id        BIGINT NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
  job_id             BIGINT REFERENCES jobs(id) ON DELETE SET NULL,
  scheduled_time     TIMESTAMPTZ NOT NULL,
  actual_start_time  TIMESTAMPTZ,
  status             job_status NOT NULL DEFAULT 'pending',
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX schedule_runs_schedule_idx ON schedule_runs(schedule_id, scheduled_time DESC);

-- Hash-partitioned job_rows for 10M-scale jobs.
CREATE TABLE job_rows (
  job_id            BIGINT NOT NULL,
  row_index         BIGINT NOT NULL,
  status            row_status NOT NULL DEFAULT 'pending',
  attempts          INT NOT NULL DEFAULT 0,
  last_error        TEXT,
  idempotency_key   TEXT,
  payload_json      JSONB,
  response_json     JSONB,
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (job_id, row_index)
) PARTITION BY HASH (job_id);

DO $$
DECLARE i INT;
BEGIN
  FOR i IN 0..31 LOOP
    EXECUTE format(
      'CREATE TABLE job_rows_p%s PARTITION OF job_rows FOR VALUES WITH (MODULUS 32, REMAINDER %s);',
      i, i
    );
  END LOOP;
END$$;

CREATE INDEX job_rows_status_idx ON job_rows(job_id, status);
CREATE UNIQUE INDEX job_rows_idem_idx ON job_rows(job_id, idempotency_key) WHERE idempotency_key IS NOT NULL;

CREATE TABLE audit_log (
  id           BIGSERIAL PRIMARY KEY,
  org_id       BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  actor        TEXT NOT NULL,
  entity       TEXT NOT NULL,
  entity_id    TEXT,
  action       TEXT NOT NULL,
  before_json  JSONB,
  after_json   JSONB,
  ts           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX audit_log_entity_idx ON audit_log(org_id, entity, entity_id);
