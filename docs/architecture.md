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

1. **Start**: `POST /jobs` resolves the source the caller's organization
   owns, writes a `jobs` row (`pending`), and starts `MigrationWorkflow`.
   The job becomes `running` only while it is still `pending`; a fast
   finish is not overwritten.
2. **Ingest**: 500-row batches. The connector streams from the registered
   source; a typed checkpoint (`offset`, `nextRowIndex`, totals, source
   version) is carried through continue-as-new when history grows.
3. **There is no shard fan-out.** `ProcessShardWorkflow` is registered but
   only calls `MigrationWorkflow`. One job is one sequential workflow.
   Rows are processed one at a time in the same run.
4. **Per-row**:
   1. Template preprocess (if any) runs on the Python transform worker.
   2. Each step is rendered and sent by `CallEndpoint`. Delivery is
      `idempotent`, `lookup` or `manual` (ADR 0003). There is no circuit
      breaker: retries follow the template's `retry` policy (5xx / 429 /
      transport-before-headers).
   3. `PersistOutcome` / `PersistStepOutcome` write the ledger. A
      persistence error stops the chain.
   4. `PublishProgress` updates totals and Redis SSE.
5. **Control**: pause/resume/cancel are Temporal signals and conditional
   status updates. Row retry is one `RetryRowWorkflow` / `RetryFailedRowsWorkflow`
   per job at a time. Export retry is `ExportResultsWorkflow` and makes no
   destination calls.
6. **Completion**: `FinalizeJob` records the outcome on every exit
   (including cancel) on a disconnected context. Results are then exported.
   Jobs left non-terminal are repaired by the reconciler (ADR 0004).

## Storage model

- Postgres: `organizations`, `users`, `rule_templates`, `jobs`, `job_rows`
  (hash partitioned by `job_id`), `connectors`, `secrets`,
  `audit_log`, `schedules`, `schedule_runs`.
- MinIO/S3: raw input uploads under `uploads/<content_hash>/<original_name>`;
  connector "watched_prefix" scans user buckets with a cursor in
  `connectors.config_json.cursor`.
- Redis: pub/sub channel `job:{id}:progress` (processed/failed counters).
  There is no per-row events channel and no Redis lock for schedule
  triggers (Temporal schedule overlap policy owns that).

## Fault tolerance

- Temporal persists every workflow event; orchestrator pods are stateless.
- Destination delivery policy (ADR 0003) decides whether an unknown
  outcome is retried, looked up, or left `unresolved`. An idempotency
  header is not enough on its own if the destination ignores it.
- Ingest carries a typed checkpoint through continue-as-new so a large
  file does not restart at row zero.
- File uploads are content-hashed; the same file re-uploaded is deduped.
- HPA + PDB keep the orchestrator pool above min replicas; node drains
  wait for active activities to heartbeat-complete.

## Scaling knobs

| Knob                               | Where                          | Default |
| ---------------------------------- | ------------------------------ | ------- |
| Ingest batch size                  | Workflow constant              | 500     |
| Destination host rate limit        | `DESTINATION_RPS` / `BURST`    | 50 / 100 |
| Activity concurrency               | `ACTIVITY_CONCURRENCY`         | 64      |
| Orchestrator replicas              | Compose / Helm `replicaCount`  | 1 locally |
| Transform-worker replicas          | Compose / Helm `replicaCount`  | 1 locally |
| API replicas                       | Compose / Helm `replicaCount`  | 1 locally |
| Postgres `job_rows` partitions     | Migration 0001                 | 32      |
| Reconciler interval / staleness    | `RECONCILE_*`                  | 1m / 5m |

`mapping.sharding` and a circuit breaker are not implemented. Adding
replicas does not parallelize one job: the row loop is sequential. Bound
and measure throughput before claiming it (roadmap R15).
