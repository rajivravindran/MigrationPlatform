-- Migration plans: a coordinator's dependency tree, designed before any file
-- exists. Files arrive later (upload, archive or watched folder) and become
-- plan runs bound to a step; a run waits until every step it depends on has
-- a run that met that step's completion rule.

-- A plan is versioned like a rule template: a draft is edited in place,
-- publishing freezes it, and revising a published plan creates the next
-- version. Runs pin the exact version they ran under; step completion is
-- tracked across versions of the same plan_key.
CREATE TABLE plans (
  id            BIGSERIAL PRIMARY KEY,
  org_id        BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  plan_key      TEXT NOT NULL,
  version       INT NOT NULL,
  name          TEXT NOT NULL,
  description   TEXT,
  status        TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'published')),
  created_by    BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at  TIMESTAMPTZ,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, plan_key, version)
);
CREATE INDEX plans_org_key_idx ON plans(org_id, plan_key, version DESC);

CREATE TABLE plan_steps (
  id                    BIGSERIAL PRIMARY KEY,
  plan_id               BIGINT NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  step_key              TEXT NOT NULL,
  label                 TEXT,
  ordinal               INT NOT NULL DEFAULT 0,
  depends_on            TEXT[] NOT NULL DEFAULT '{}',
  -- Pre-fills the run-time template choice; an archive manifest or the
  -- person submitting the file may override it.
  default_template_key  TEXT,
  -- Glob on the file's base name (e.g. accounts_*.csv) used to route files
  -- arriving in a watched folder or an archive without an explicit mapping.
  file_pattern          TEXT,
  -- When a run of this step counts as complete for its dependents.
  completion_rule       TEXT NOT NULL DEFAULT 'strict' CHECK (completion_rule IN ('strict', 'max_failed_pct')),
  max_failed_pct        NUMERIC(5, 2) CHECK (max_failed_pct IS NULL OR (max_failed_pct >= 0 AND max_failed_pct <= 100)),
  -- Canvas position chosen by the coordinator, {"x": .., "y": ..}.
  position              JSONB,
  CHECK (completion_rule = 'strict' OR max_failed_pct IS NOT NULL),
  UNIQUE (plan_id, step_key)
);

-- An archive that arrived for a plan: unpacked, then fanned out into runs.
CREATE TABLE plan_intakes (
  id            BIGSERIAL PRIMARY KEY,
  org_id        BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  plan_id       BIGINT NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  source_id     BIGINT REFERENCES sources(id) ON DELETE SET NULL,
  filename      TEXT,
  status        TEXT NOT NULL DEFAULT 'unpacking' CHECK (status IN ('unpacking', 'dispatched', 'failed')),
  error_message TEXT,
  manifest_json JSONB,
  schedule_id   BIGINT REFERENCES schedules(id) ON DELETE SET NULL,
  temporal_workflow_id TEXT UNIQUE,
  created_by    BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE plan_runs (
  id                    BIGSERIAL PRIMARY KEY,
  org_id                BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  plan_id               BIGINT NOT NULL REFERENCES plans(id) ON DELETE CASCADE,
  -- NULL only for an archive file no step could be matched to (status failed).
  step_key              TEXT,
  intake_id             BIGINT REFERENCES plan_intakes(id) ON DELETE SET NULL,
  source_id             BIGINT REFERENCES sources(id) ON DELETE SET NULL,
  source_ref            JSONB,
  filename              TEXT,
  rule_template_id      BIGINT REFERENCES rule_templates(id) ON DELETE SET NULL,
  rule_template_version INT,
  status                TEXT NOT NULL DEFAULT 'waiting'
                        CHECK (status IN ('waiting', 'running', 'completed', 'failed', 'cancelled')),
  -- A waiting run whose prerequisite has a failed run and no completed one.
  -- It still starts automatically if the prerequisite later completes.
  blocked               BOOLEAN NOT NULL DEFAULT false,
  rule_met              BOOLEAN,
  failed_pct            NUMERIC(6, 2),
  reason                TEXT,
  job_id                BIGINT REFERENCES jobs(id) ON DELETE SET NULL,
  schedule_id           BIGINT REFERENCES schedules(id) ON DELETE SET NULL,
  temporal_workflow_id  TEXT UNIQUE,
  -- Identity of the arrival that created the run (watched object version),
  -- so a re-listed file never creates a second run.
  dedupe_key            TEXT UNIQUE,
  created_by            BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  started_at            TIMESTAMPTZ,
  finished_at           TIMESTAMPTZ,
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX plan_runs_plan_idx ON plan_runs(plan_id, created_at DESC);
CREATE INDEX plan_runs_step_idx ON plan_runs(org_id, step_key, status);
CREATE INDEX plan_runs_job_idx ON plan_runs(job_id);

-- Watched folders can route arriving files into a plan by step file patterns.
ALTER TABLE schedules ADD COLUMN plan_id BIGINT REFERENCES plans(id) ON DELETE SET NULL;

-- Files unpacked from a plan archive are registered like batch stages.
ALTER TABLE sources DROP CONSTRAINT sources_provenance_check;
ALTER TABLE sources ADD CONSTRAINT sources_provenance_check
  CHECK (provenance IN ('upload', 'batch_stage', 'watched', 'legacy', 'plan_intake'));
