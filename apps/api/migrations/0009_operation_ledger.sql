-- R04 (ADR 0003): operation/attempt ledger for destination calls,
-- redelivery-safe attempt counters and destination-scoped secrets. The
-- `unresolved` row status is added by 0008 (enum values need their own
-- non-transactional migration).

ALTER TABLE job_rows ADD COLUMN last_attempt_id TEXT;
ALTER TABLE job_row_steps ADD COLUMN last_attempt_id TEXT;

ALTER TABLE secrets
  ADD COLUMN kind TEXT NOT NULL DEFAULT 'internal' CHECK (kind IN ('internal', 'destination')),
  ADD COLUMN revoked_at TIMESTAMPTZ;

-- One row per (job, row, step): the intent to apply that step.
CREATE TABLE operations (
  job_id          BIGINT NOT NULL,
  row_index       BIGINT NOT NULL,
  step_index      INT    NOT NULL,
  step_name       TEXT   NOT NULL,
  operation_key   TEXT   NOT NULL,
  payload_sha256  TEXT   NOT NULL,
  state           TEXT   NOT NULL CHECK (state IN ('pending', 'sending', 'succeeded', 'failed', 'unknown', 'unresolved')),
  attempts        INT    NOT NULL DEFAULT 0,
  lease_attempt   TEXT,
  lease_until     TIMESTAMPTZ,
  first_sent_at   TIMESTAMPTZ,
  receipt_status  INT,
  receipt_json    JSONB,
  last_error      TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (job_id, row_index, step_index)
) PARTITION BY HASH (job_id);

-- One row per try at sending an operation.
CREATE TABLE operation_attempts (
  job_id       BIGINT NOT NULL,
  attempt_id   TEXT   NOT NULL,
  row_index    BIGINT NOT NULL,
  step_index   INT    NOT NULL,
  attempt_no   INT    NOT NULL,
  started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at  TIMESTAMPTZ,
  outcome      TEXT CHECK (outcome IN ('responded', 'not_sent', 'unknown', 'replayed', 'found_by_lookup', 'refused')),
  http_status  INT,
  error        TEXT,
  PRIMARY KEY (job_id, attempt_id)
) PARTITION BY HASH (job_id);

DO $$
DECLARE i INT;
BEGIN
  FOR i IN 0..31 LOOP
    EXECUTE format(
      'CREATE TABLE operations_p%s PARTITION OF operations FOR VALUES WITH (MODULUS 32, REMAINDER %s);',
      i, i
    );
    EXECUTE format(
      'CREATE TABLE operation_attempts_p%s PARTITION OF operation_attempts FOR VALUES WITH (MODULUS 32, REMAINDER %s);',
      i, i
    );
  END LOOP;
END$$;

CREATE INDEX operation_attempts_op_idx ON operation_attempts (job_id, row_index, step_index);
CREATE INDEX operations_state_idx ON operations (job_id, state) WHERE state IN ('unknown', 'unresolved', 'sending');
