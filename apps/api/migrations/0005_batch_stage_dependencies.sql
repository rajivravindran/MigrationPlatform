-- Persist manifest DAG dependencies for diagnostics and API consumers.
ALTER TABLE batch_stages
  ADD COLUMN depends_on TEXT[] NOT NULL DEFAULT ARRAY[]::TEXT[];
