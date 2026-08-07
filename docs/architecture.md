# Architecture

## 30 second summary

Rule templates (authored in the UI) describe a source, preprocess steps,
payload mapping, and an HTTP destination. A **Migration Job** materialises a
template against a specific source (uploaded file or connector). A Go-based
Temporal **worker pool** drives the job: ingest rows, hand off batches to a
Python transform worker, POST to the destination, and persist row-level
outcomes. Redis powers progress pub/sub for the SSE stream; Postgres stores
jobs, rows, templates, connectors, audit log, and schedules. Recurring jobs
are implemented via Temporal **Schedules**.

## Components

```
 ┌──────────┐    HTTP/JSON     ┌─────────────┐   gRPC     ┌─────────────────┐
 │ Next.js  │ ───────────────▶ │  Rust API   │ ─────────▶ │ Temporal Server │
 │  (web)   │ ◀─── SSE ─────── │  (axum)     │            └────────┬────────┘
 └────┬─────┘                  │             │                     │ task q's
      │ uploads                │             │                     ▼
      ▼                        │             │            ┌─────────────────┐
 ┌──────────┐                  │             │            │ orchestrator-go │
 │  MinIO   │◀── S3 API ──────▶│             │            │ workers (Go)    │
 │  (S3)    │                  │             │            └──────┬──────────┘
 └────┬─────┘                  │             │                   │ gRPC
      │ reads                  │             │                   ▼
      ▼                        │             │            ┌─────────────────┐
 ┌──────────┐      sqlx        │             │            │ transform-worker │
 │ Postgres │◀────────────────▶│             │            │ Python sandbox   │
 └──────────┘                  │             │            └──────────────────┘
                               │             │   pub/sub
                               │             │◀──────┐   ┌──────────┐
                               └──────┬──────┘       └───│  Redis   │
                                      │                  └──────────┘
                                      │  OTLP
                                      ▼
                              ┌──────────────────┐
                              │ OTEL Collector   │──▶ Prometheus / Loki / Tempo ──▶ Grafana
                              └──────────────────┘
```

## Data flow for one job

1. **Start**: `POST /jobs` validates the template+source, writes a `jobs` row
   (status=`pending`), and kicks off `MigrationWorkflow` on Temporal.
2. **Ingest**: the workflow runs the `Ingest` activity; the connector streams
   rows in bounded batches (default 1_000) and writes them to `job_rows`
   with content-hash based idempotency keys.
3. **Shard fan-out**: `MigrationWorkflow` schedules `ProcessShardWorkflow`
   children keyed on `row_index % shard_count`.
4. **Per-row**:
   1. `LoadTemplate` returns the published template version (cached).
   2. `Transform` sends a batch of N rows to the Python sandbox, which
      applies preprocess + mapping + `$py` snippets inside a resource-capped
      subprocess.
   3. `CallEndpoint` POSTs the payload with retry/backoff + circuit breaker.
   4. `PersistOutcome` writes row status/latency/error to `job_rows`.
   5. `PublishProgress` increments counters and emits SSE events to Redis.
5. **Control signals**: pause/resume/cancel are Temporal signals; "Retry
   failed" enqueues a `RetryRowWorkflow` per row that skips `PersistOutcome`
   write-once enforcement.
6. **Completion**: workflow reconciles counters, marks the job terminal,
   fires audit log + Grafana alert if `failed > threshold`.

## Storage model

- Postgres: `organizations`, `users`, `rule_templates`, `jobs`, `job_rows`
  (hash partitioned by `job_id`), `connectors`, `secrets`,
  `audit_log`, `schedules`, `schedule_runs`.
- MinIO/S3: raw input uploads under `uploads/<content_hash>/<original_name>`;
  connector "watched_prefix" scans user buckets with a cursor in
  `connectors.config_json.cursor`.
- Redis: pub/sub channels `jobs:{id}:progress` (processed/failed counters),
  `jobs:{id}:events` (per-row completion), and distributed locks used to
  deduplicate schedule triggers.

## Fault tolerance

- Temporal persists every workflow event; orchestrator pods are stateless.
- `job_rows.idempotency_key` prevents duplicate POSTs on retry.
- Ingest checkpoints a byte or line offset per source so partial files
  resume from the last flushed batch.
- File uploads are content-hashed; the same file re-uploaded is deduped.
- HPA + PDB keep the orchestrator pool above min replicas; node drains
  wait for active activities to heartbeat-complete.

## Scaling knobs

| Knob                               | Where                          | Default |
| ---------------------------------- | ------------------------------ | ------- |
| Shard count per job                | Template, `mapping.sharding`   | 8       |
| Batch size (transform + call)      | Orchestrator env               | 200     |
| Orchestrator replicas              | Helm `replicaCount` + HPA      | 2–20    |
| Transform-worker replicas          | Helm `replicaCount` + HPA      | 2–10    |
| API replicas                       | Helm `replicaCount` + HPA      | 2–6     |
| Postgres `job_rows` partitions     | Migration 0001                 | 64      |
| Redis pubsub buffer                | Orchestrator env               | 4096    |
