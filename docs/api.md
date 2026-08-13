# API reference

All endpoints are served by `apps/api` under the same base URL. The machine-
readable spec lives at `GET /openapi.json`; Swagger UI at `GET /swagger-ui`.
This page is a human-readable summary of the most important routes.

## Conventions

- All mutating endpoints require `Authorization: Bearer <jwt>`.
- JSON bodies; content type negotiation supports `application/json`.
- Every response includes `X-Request-Id`; pass it back when filing issues.
- Errors use RFC 7807 Problem Details with a platform extension `code`:
  ```json
  { "type":"/errors/invalid-request", "title":"Invalid request",
    "status":400, "code":"template.invalid",
    "detail":"$.mapping.payload.email: $from references unknown column" }
  ```
- SSE streams emit `event: progress`, `event: row`, `event: done` frames.

## Auth

### `POST /auth/login`

```json
{ "email":"admin@example.com", "password":"..." }
```

Returns `{ "token":"<jwt>", "expires_at":"..." }`. JWT is RS256, 1h TTL.

### `GET /auth/me`

Returns the caller's user record + role. Used by the UI for RBAC gating.

## Rule templates

| Method | Path                              | Role         | Purpose                                  |
| ------ | --------------------------------- | ------------ | ---------------------------------------- |
| GET    | `/rule-templates`                 | viewer+      | List templates, one row per `template_key` (latest version). Filters: `?published=true` (latest *published* version per key), `?template_key=...` (all versions of one key). |
| POST   | `/rule-templates`                 | editor       | Create a new template (draft).           |
| GET    | `/rule-templates/{id}`            | viewer+      | Fetch one.                               |
| PUT    | `/rule-templates/{id}`            | editor       | Save a new version (bumps `version`).    |
| POST   | `/rule-templates/{id}/publish`    | editor       | Publish version `N` — only published templates can be used in jobs/schedules. |
| DELETE | `/rule-templates/{id}`            | admin        | Soft-delete (publishes are archived).    |

Request body validates against `packages/rule-schema/schema/rule-template.schema.json`.
The `destination.url`, `destination.pathParams`, `destination.queryParams`,
and `destination.headers` fields support per-row templating — see
[`docs/url-templating.md`](./url-templating.md).

## Jobs

| Method | Path                                       | Role     | Purpose |
| ------ | ------------------------------------------ | -------- | ------- |
| GET    | `/jobs`                                    | viewer+  | List with filters: `status`, `template_id`, `schedule_id`. Each item includes `batch_id` and a `source` summary (filename, package vs stage file, size, etag). |
| POST   | `/jobs`                                    | operator | Start a job. Body: `{ rule_template_id, source_ref }`. |
| GET    | `/jobs/{id}`                               | viewer+  | Fetch + counters + `source` summary and `batch_id`. |
| POST   | `/jobs/{id}/pause`                         | operator | Signal Temporal to pause. |
| POST   | `/jobs/{id}/resume`                        | operator | Resume a paused job. |
| POST   | `/jobs/{id}/cancel`                        | operator | Cancel; rows in-flight finish. |
| GET    | `/jobs/{id}/rows`                          | viewer+  | Paginated; `?status=failed&cursor=...`. Includes `payload_json`, `response_json`, `last_error` (single-call request/response). |
| GET    | `/jobs/{id}/rows/{row_index}/steps`        | viewer+  | Per-step trail for multi-step templates: request/response JSON, HTTP status, error. |
| POST   | `/jobs/{id}/rows/{row_index}/retry`        | operator | Retry a single row. Body `{ "from_start": true }` re-runs succeeded steps too. |
| POST   | `/jobs/{id}/retry-failed`                  | operator | Enqueue retries for all failed rows. |
| GET    | `/jobs/{id}/results`                       | viewer+  | Download results CSV (`?failed=true` for failed rows only). Prefers the MinIO object written at finalize; falls back to a live DB export of ops columns. |
| GET    | `/jobs/{id}/stream`                        | viewer+  | SSE: live counters + row events. |

### `source_ref`

Either an upload reference (string returned by `POST /files`) or a JSON blob:

```json
{ "kind":"connector", "connector_id":42, "overrides":{"since":"2026-01-01"} }
```

## Files

### `POST /files`

Multipart upload. Stores in MinIO under `uploads/<sha256>/<filename>`,
returns `{ source_ref, bytes, sha256 }`. Client-side proxy at
`/api/files` fronts this for browser uploads.

### `POST /files/sample`

Reads the first 8 MiB of a previously uploaded object via an S3 Range request
and returns up to 25 typed columns + 25 preview rows. Powers the Designer's
"Sample input file" button. Supports CSV, JSON, NDJSON, and XML. See
[`docs/sampling.md`](./sampling.md) for the request/response shape, type
inference rules, and limits.

## Connectors

| Method | Path                     | Role    | Notes |
| ------ | ------------------------ | ------- | ----- |
| GET    | `/connectors`            | viewer+ | Returns config minus secrets. |
| POST   | `/connectors`            | editor  | Secrets encrypted AES-GCM. |
| PUT    | `/connectors/{id}`       | editor  | Patch semantics. |
| DELETE | `/connectors/{id}`       | admin   | Soft-delete. |
| POST   | `/connectors/{id}/oauth/salesforce/start` | editor | Returns Salesforce auth URL. |
| GET    | `/connectors/{id}/oauth/salesforce/callback` | — | OAuth redirect target. |

## Schedules

| Method | Path                              | Role     | Notes |
| ------ | --------------------------------- | -------- | ----- |
| GET    | `/schedules`                      | viewer+  | |
| POST   | `/schedules`                      | editor   | Creates Temporal Schedule + DB row. |
| GET    | `/schedules/{id}`                 | viewer+  | |
| PUT    | `/schedules/{id}`                 | editor   | Updates cron/overlap/paused state. |
| DELETE | `/schedules/{id}`                 | admin    | Removes Temporal Schedule. |
| POST   | `/schedules/{id}/pause`           | operator | |
| POST   | `/schedules/{id}/resume`          | operator | |
| POST   | `/schedules/{id}/trigger`         | operator | Fires "now". |
| GET    | `/schedules/{id}/runs`            | viewer+  | Last N runs from `schedule_runs`. |

## OpenAPI specs (Designer operation picker)

| Method | Path                                | Role    | Notes |
| ------ | ----------------------------------- | ------- | ----- |
| GET    | `/openapi-specs`                    | viewer+ | List imported specs. |
| POST   | `/openapi-specs`                    | editor  | Import by inline `spec` or `url` (OpenAPI 3.x JSON, ≤4 MiB; URL fetch is SSRF-guarded). Upserts by name. |
| GET    | `/openapi-specs/{id}`               | viewer+ | Full stored document. |
| GET    | `/openapi-specs/{id}/operations`    | viewer+ | Flattened operations: method, path, url, parameters, ref-resolved request/response schemas. |
| DELETE | `/openapi-specs/{id}`               | editor  | |
| POST   | `/openapi-specs/parse-preview`      | viewer+ | Parse an inline spec without persisting. |

## LLM mapping suggestions

| Method | Path                    | Role    | Notes |
| ------ | ----------------------- | ------- | ----- |
| GET    | `/llm-config`           | viewer+ | Org provider config (key never returned). |
| PUT    | `/llm-config`           | admin   | `{ base_url, model, api_key?, redact_pii?, enabled? }`. OpenAI-compatible endpoints; key AES-GCM encrypted. |
| POST   | `/mapping-suggestions`  | editor  | `{ columns, sample_rows, spec_id?, operation_id?, instructions? }` → schema-validated rule-template draft. Sample rows are PII-redacted (emails/phones/long digit runs) before leaving the platform unless `redact_pii=false`. Invalid LLM output is retried once with the validation error, then rejected. |

## License

`GET /license` — status of the deployment license. The API validates
`LICENSE_FILE` (RSA-signed JSON, see `migration-admin license-sign`) at
startup; without one it runs in development mode unless `LICENSE_ENFORCE=true`.

## Dry-run

### `POST /dry-run`

Runs preprocess + mapping against a body-supplied sample (max 100 rows),
returns the rendered payloads without calling the destination. Backs the
UI designer's "Preview" pane.

## Observability

- `GET /metrics` — Prometheus text format, includes custom metrics:
  - `migration_jobs_total{status}`
  - `migration_rows_processed_total{job_template}`
  - `migration_row_latency_seconds_bucket{phase}`
  - `migration_sse_subscribers`
- `GET /healthz` — liveness.
- `GET /readyz` — readiness (checks DB, Redis, Temporal, MinIO).
