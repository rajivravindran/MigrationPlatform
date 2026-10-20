# Observability

The platform ships a full telemetry stack locally and in production so that
the same dashboards, alerts, and trace links work on a laptop and in k8s.

## Pipeline

```
 apps ──OTLP─▶ OTEL Collector ──▶ Prometheus    (metrics)
                              ├──▶ Loki          (logs)
                              └──▶ Tempo         (traces)
                                    ▲
                                Grafana datasources
```

Collector config: `infra/otel-collector.yaml`. Prometheus scrape rules:
`infra/prometheus.yml`. Grafana is provisioned from
`infra/grafana/provisioning`.

## Metrics

Custom metrics exposed by every service on `/metrics` (port `9464` for all three
services; the API keeps this off its public `8080` listener):

| Service          | Metric                                                  | Type       |
| ---------------- | ------------------------------------------------------- | ---------- |
| API              | `http_request_duration_seconds{route,method,status}`    | Histogram  |
| API              | `migration_jobs_total{status}`                          | Counter    |
| API              | `migration_sse_subscribers`                             | Gauge      |
| Orchestrator (Go)| `rows_processed_total` / `rows_failed_total`            | Counter    |
| Orchestrator     | `endpoint_call_seconds`                                 | Histogram  |
| Orchestrator     | `jobs_reconciled_total{workflow_status,job_status}`     | Counter    |
| Orchestrator     | `batches_reconciled_total{workflow_status,batch_status}` | Counter    |
| Orchestrator     | `job_reconcile_errors_total`                            | Counter    |
| Transform worker | `migration_transform_batch_latency_seconds`             | Histogram  |
| Transform worker | `migration_transform_sandbox_kills_total{reason}`       | Counter    |
| All              | `process_*`, `go_*`, `tokio_*`                          | Std        |

## Logs

- Structured JSON, one event per line, from `tracing` (Rust), `zap` (Go),
  and `logging` (Python).
- Common fields: `timestamp`, `level`, `service`, `trace_id`, `span_id`,
  `request_id`, `job_id`, `schedule_id`, `message`.
- Secrets scrubber is in the formatter, not downstream, so leaks can't
  reach disk.
- Loki labels: `{service, namespace, job, status}`.

## Traces

- OTLP gRPC to the collector, then to Tempo.
- Sampled at 10% by default; errors always sampled.
- Every API request span includes `job.id` and `template.id` attributes
  when available; propagated into orchestrator and worker via the standard
  W3C traceparent headers.
- Grafana's Explore view → click a `trace_id` from a log line → opens the
  trace in Tempo.

## Dashboards

Provisioned dashboards in Grafana:

1. **Migration / Overview** — jobs per status, rows processed/failed,
   p95 endpoint latency, jobs reconciled.
2. **Migration / Jobs** — drill-down by `job_id`, transform vs call latency.
3. **Migration / Schedules** — last run status, catch-up queue, next fires.
4. **Infra / API** — http_request_duration heatmap, error rate, SSE connects.
5. **Infra / Postgres** — connections, long queries, table sizes.

## Alerts

Defined in `infra/grafana/provisioning/alerts/migration.yaml`:

| Alert                           | For  | Condition                                                        |
| ------------------------------- | ---- | ---------------------------------------------------------------- |
| `MigrationHighFailureRate`      | 10m  | `failed/total_rows > 0.5%` on any running job                    |
| `MigrationStalled`              | 5m   | `rate(migration_rows_processed_total[5m]) == 0` while `status=running` |
| `MigrationReconcileErrors`      | 15m  | `rate(job_reconcile_errors_total[15m]) > 0`                      |
| `MigrationSandboxKills`         | 10m  | `rate(migration_transform_sandbox_kills_total[5m]) > 0.1`        |
| `APIHighErrorRate`              | 5m   | `rate(http_requests_total{status=~"5.."}[5m]) > 0.02`            |
| `TemporalWorkerDown`            | 2m   | `up{job="orchestrator-go"} == 0`                                 |
| `PostgresConnectionsSaturation` | 10m  | `> 90%` of configured `max_connections`                          |

Route critical alerts to the primary on-call channel; warnings to the
team channel. `infra/alertmanager.yml` has a ready-to-edit template.

## Local parity

`docker compose -f infra/docker-compose.yml up -d` brings up the full
collector + Prometheus + Loki + Tempo + Grafana stack so dashboards you
iterate on locally render the same in production.
