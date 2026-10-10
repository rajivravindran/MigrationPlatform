-- R01 / MP-01: organization-owned source registry (see docs/adr/0001-source-identity.md).
-- Only trusted code inserts rows: POST /files after an upload, and orchestrator
-- activities that stage objects internally (watched files, batch stages).

CREATE TABLE sources (
  id           BIGSERIAL PRIMARY KEY,
  org_id       BIGINT NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
  bucket       TEXT NOT NULL,
  object_key   TEXT NOT NULL,
  sha256       TEXT,
  etag         TEXT,
  size_bytes   BIGINT,
  filename     TEXT,
  format       TEXT CHECK (format IS NULL OR format IN ('csv', 'json', 'xml', 'archive')),
  provenance   TEXT NOT NULL CHECK (provenance IN ('upload', 'batch_stage', 'watched', 'legacy')),
  created_by   BIGINT REFERENCES users(id) ON DELETE SET NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (org_id, bucket, object_key)
);
CREATE INDEX sources_bucket_key_idx ON sources(bucket, object_key);

-- Backfill only references whose ownership is provable from platform-generated
-- keys; client- or connector-supplied locations are never trusted. Unprovable
-- references stay unregistered and can no longer be ingested.
INSERT INTO sources (org_id, bucket, object_key, sha256, etag, size_bytes, filename, format, provenance)
SELECT DISTINCT ON (j.org_id, j.source_ref->>'bucket', j.source_ref->>'key')
       j.org_id,
       j.source_ref->>'bucket',
       j.source_ref->>'key',
       NULLIF(j.source_ref->>'sha256', ''),
       NULLIF(j.source_ref->>'etag', ''),
       CASE WHEN jsonb_typeof(j.source_ref->'size') = 'number' THEN (j.source_ref->>'size')::bigint END,
       NULLIF(j.source_ref->>'filename', ''),
       NULL,
       'legacy'
FROM jobs j
LEFT JOIN batches b ON b.id = j.batch_id
WHERE j.source_ref->>'type' = 'minio'
  AND COALESCE(j.source_ref->>'bucket', '') <> ''
  AND COALESCE(j.source_ref->>'key', '') <> ''
  AND (
        -- uploads are written under uploads/{org}/ by the API itself
        j.source_ref->>'key' LIKE 'uploads/' || j.org_id::text || '/%'
        -- batch stage files are staged by the orchestrator under batches/{id}/stages/
     OR (j.batch_id IS NOT NULL AND b.org_id = j.org_id
         AND j.source_ref->>'key' LIKE 'batches/' || j.batch_id::text || '/stages/%')
        -- watched objects inside the org's own namespace (orgs/{org}/)
     OR (j.schedule_id IS NOT NULL AND j.source_ref->>'key' LIKE 'orgs/' || j.org_id::text || '/%')
  )
ORDER BY j.org_id, j.source_ref->>'bucket', j.source_ref->>'key', j.id
ON CONFLICT (org_id, bucket, object_key) DO NOTHING;

INSERT INTO sources (org_id, bucket, object_key, etag, size_bytes, filename, format, provenance)
SELECT DISTINCT ON (b.org_id, b.source_ref->>'bucket', b.source_ref->>'key')
       b.org_id,
       b.source_ref->>'bucket',
       b.source_ref->>'key',
       NULLIF(b.source_ref->>'etag', ''),
       CASE WHEN jsonb_typeof(b.source_ref->'size') = 'number' THEN (b.source_ref->>'size')::bigint END,
       NULLIF(b.source_ref->>'filename', ''),
       'archive',
       'legacy'
FROM batches b
WHERE b.source_ref->>'type' = 'minio'
  AND COALESCE(b.source_ref->>'bucket', '') <> ''
  AND COALESCE(b.source_ref->>'key', '') <> ''
  AND (b.source_ref->>'key' LIKE 'uploads/' || b.org_id::text || '/%'
       OR (b.schedule_id IS NOT NULL AND b.source_ref->>'key' LIKE 'orgs/' || b.org_id::text || '/%'))
ORDER BY b.org_id, b.source_ref->>'bucket', b.source_ref->>'key', b.id
ON CONFLICT (org_id, bucket, object_key) DO NOTHING;
