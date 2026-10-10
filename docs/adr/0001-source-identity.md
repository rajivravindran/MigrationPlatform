# ADR 0001: Organization-owned source registry and frozen source identity

- Status: accepted (R01, MP-01). The checkpoint contract in the last section is shared with R03.
- Date: 2026-10-10

## Context

`POST /jobs` stored the client's `source_ref` JSON and forwarded it verbatim to
`MigrationWorkflow`. The ingest connector then read whatever that object named:

- any `bucket`/`key` in object storage, including other organizations' uploads
  (`uploads/{org}/…` is only a naming convention)
- a local `path` inside the orchestrator container
- any `url`, fetched with `http.DefaultClient` (no egress policy)
- Salesforce credentials passed inline as `secret`, which then lived in Temporal history

`POST /files/sample` and `POST /batches` accepted arbitrary `bucket`/`key`
pairs the same way. Prefix matching alone cannot fix this: staged SFTP files,
batch stage files and exports use other layouts, and a prefix check cannot
detect that an object changed after it was approved.

## Decision

1. **Source registry.** A new `sources` table records every object the
   platform may ingest: `id`, `org_id`, `bucket`, `object_key`, `sha256`
   (content hash when known), `etag`, `size_bytes`, `filename`, `format`,
   `provenance` (`upload`, `batch_stage`, `watched`, `legacy`) and `created_at`.
   `(org_id, bucket, object_key)` is unique. Rows are created only by trusted
   code:
   - `POST /files` after a successful upload (content-addressed key)
   - orchestrator activities that create jobs from internally staged objects
     (watched SFTP/prefix files, batch stage files)

2. **Public job and batch requests name a source; they do not describe one.**
   `POST /jobs` accepts:
   - `{"source_id": N}` (canonical)
   - the legacy upload shape `{"type":"minio","bucket","key",...}`, accepted
     only when `(org, bucket, key)` is already registered for the caller's
     organization. This establishes ownership; it is never inferred from a
     user-supplied org field or a key prefix.
   - `{"type":"connector","connector_id":N,"extra":{...}}` for connector-backed
     sources (Salesforce). The connector must belong to the caller's org.
     `extra` may only set query options (`soql`, `cursorField`, `cursor`);
     endpoints and credentials always come from the stored connector.

   Everything else (`path`, `url`, inline `secret`, arbitrary `type`) gets
   `400` before any job row is written, so no orphan `pending`/`running` job
   is left behind. `POST /batches` and `POST /files/sample` resolve through
   the same resolver.

3. **Canonical workflow input.** The API builds the workflow's `sourceRef`
   from the registry row: `{"type":"minio","source_id","bucket","key",
   "filename","size","sha256","etag"}` or
   `{"type":"connector","connector_id","extra"}`. The client's JSON is never
   forwarded.

4. **Defence in depth in the orchestrator.** The `Ingest` activity re-verifies
   ownership on every call:
   - Object sources must match a registry row for the workflow's org, by
     `source_id` when present, otherwise by `(bucket, key)`.
   - The object's current size, and its ETag when one was recorded, must equal
     the registered version. Otherwise ingest fails with a non-retryable
     `source changed` error instead of silently reading new content.
   - Connector sources are loaded and decrypted inside the activity
     (`connectors` + `secrets`, both org-scoped). Credentials never enter
     workflow input or history.
   - `path` sources are refused unless `ALLOW_LOCAL_SOURCE_PATHS=true`, a
     test/dev-only switch.
   - `url` sources and all Salesforce HTTP calls use the egress-policy client
     (`security.NewEgressHTTPClient`, with private ranges blocked unless
     `ALLOW_PRIVATE_DESTINATIONS=true`). Redirects are re-checked at dial time.

5. **Organization object namespace.** Locations an organization configures
   (watched-prefix `bucket`/`prefix`, SFTP `staging_bucket`/`staging_prefix`)
   are read and written with the platform's own storage credentials, so they
   are confined to `orgs/{org_id}/` in the platform bucket or an operator
   allow-listed bucket (`WATCH_ALLOWED_BUCKETS`). The API refuses other
   connector configs; the orchestrator re-checks the configured prefix when
   listing and the *final* key when staging or registering, so a remote
   filename such as `../../uploads/2/…` cannot escape. The legacy SFTP default
   `sftp-landing/` maps to `orgs/{org_id}/sftp-landing/`. Platform-generated
   namespaces (`uploads/{org}/`, `batches/{id}/stages/`, exports) are never
   configurable. Batch archives must be registered to the org before they are
   unpacked, and stage jobs must name a key under their own batch.

6. **Legacy data.** Migration `0007` backfills the registry from existing job
   and batch rows. Only keys the platform itself generated for that org are
   imported: `uploads/{org_id}/…`, `batches/{batch_id}/stages/…` for a batch of
   the same org, and schedule-created objects under `orgs/{org_id}/`. Watched
   objects at other locations were chosen by connector configuration, not the
   platform, so they stay unregistered. Anything not imported cannot be
   re-run. This is a deliberate fail-closed choice.

## Checkpoint contract (shared with R03)

A migration checkpoint identifies its source as
`{"source_id": N, "source_version": "<sha256|etag|size>"}` for object
sources, or `{"connector_id": N}` plus the connector cursor for connector
sources. Continuation and restart must present the same identity. A mismatch
fails visibly; it does not restart at row zero or skip ahead.

## Consequences

- Clients that sent hand-written `source_ref` objects (other than an upload
  response or a connector reference) now receive `400`. The UI's advanced
  field uses the connector form.
- Salesforce sources require a stored connector. Inline credentials are no
  longer accepted.
- Every ingest makes one extra indexed lookup and one `StatObject` per
  `Ingest` activity call (per 500-row batch), which is negligible next to the
  object read itself.

## Alternatives considered

- **Prefix matching (`uploads/{org}/`)**: rejected. It does not cover staged
  or batch layouts and cannot freeze a version.
- **Signed source tokens (HMAC over bucket/key/org)**: viable and stateless,
  but it cannot record provenance or support listing, and revocation still
  needs state. A registry row is simpler and is also what R08 manifests and
  R11 evidence need.
