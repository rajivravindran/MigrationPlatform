-- Multi-step chains, raw-row persistence, OpenAPI specs, and LLM config.

-- Raw source row (post-ingest, pre-transform) so retries never re-read the
-- source file.
ALTER TABLE job_rows ADD COLUMN IF NOT EXISTS row_json JSONB;

-- Per-step outcomes for multi-step (chained) templates. Partitioned like
-- job_rows so a 10M-row job with 4 steps stays manageable.
CREATE TABLE job_row_steps (
  job_id           BIGINT NOT NULL,
  row_index        BIGINT NOT NULL,
  step_index       INT    NOT NULL,
  step_name        TEXT   NOT NULL,
  status           row_status NOT NULL DEFAULT 'pending',
  attempts         INT NOT NULL DEFAULT 0,
  request_url      TEXT,
  request_json     JSONB,
  response_status  INT,
  response_json    JSONB,
  last_error       TEXT,
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (job_id, row_index, step_index)
) PARTITION BY HASH (job_id);

DO $$
DECLARE i INT;
BEGIN
  FOR i IN 0..31 LOOP
    EXECUTE format(
      'CREATE TABLE job_row_steps_p%s PARTITION OF job_row_steps FOR VALUES WITH (MODULUS 32, REMAINDER %s);',
      i, i
    );
  END LOOP;
END$$;

CREATE INDEX job_row_steps_status_idx ON job_row_steps(job_id, status);

-- Imported OpenAPI specifications (per org) used by the designer and the LLM
-- mapping-suggestion endpoint.
CREATE TABLE openapi_specs (
  id          BIGSERIAL PRIMARY KEY,
  org_id      BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  name        TEXT NOT NULL,
  source_url  TEXT,
  spec_json   JSONB NOT NULL,
  created_by  BIGINT REFERENCES users(id),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, name)
);

-- Org-scoped LLM provider configuration. The API key lives in `secrets`
-- (AES-GCM under MASTER_KEY) and is referenced by id.
CREATE TABLE org_llm_configs (
  org_id        BIGINT PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
  provider      TEXT NOT NULL DEFAULT 'openai-compatible',
  base_url      TEXT NOT NULL,
  model         TEXT NOT NULL,
  secret_id     BIGINT REFERENCES secrets(id) ON DELETE SET NULL,
  redact_pii    BOOLEAN NOT NULL DEFAULT true,
  enabled       BOOLEAN NOT NULL DEFAULT true,
  updated_by    BIGINT REFERENCES users(id),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
