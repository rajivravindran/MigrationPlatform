# Load & smoke test plan

Two tiers — a laptop smoke suite for iteration and a full 10M-row burn test
for production readiness sign-off.

## Tier 1 — Laptop smoke (Docker Compose)

Goal: prove the happy-path works end-to-end on a developer laptop in < 2 min.

- Input: `tests/load/generate_csv.py --rows 10000`
- Flow: `smoke_laptop.js` uploads via `/files`, starts a job, polls `/jobs/{id}`
- Success criteria:
  - HTTP `2xx` on every API call (`checks{} == 1.0`)
  - Job reaches `succeeded` within 120s
  - `processed + failed == total_rows`
  - `failed / total_rows < 1%`
- Exercised in CI via the `compose-smoke` GitHub Actions job.

## Tier 2 — 10M-row burn (Kubernetes, Helm)

Run in a namespace deployed via `infra/helm/migration-platform`. The k6 Job
definition is `tests/load/k6-job.yaml`; the script is `load_10m.js`.

### Data

- Synthetic CSV sharded into 40 files × 250k rows via
  `generate_csv.py --rows 10000000 --shards 40 --out /tmp/huge.csv`.
- Uploaded to MinIO/S3 bucket `migration-load-test/huge_part_*.csv`.
- Connector: `watched_prefix` pointing at the bucket with `glob: huge_part_*.csv`.

### Template

- Published template with:
  - preprocess: `lowercase(country)`, `trim(email)`
  - mapping: 6 fields → 6 payload keys, one `$py` expression
  - destination: in-cluster `echo-sink` Service (absorbs POSTs, sleeps 5ms)

### Stages

1. **Ramp**: 0→64 `ROW_CONCURRENCY` over first 5 min (orchestrator worker pool
   autoscales via HPA on CPU + Redis queue depth).
2. **Sustain**: 64 workers, 60 min — target **≥ 2500 rows/sec sustained**.
3. **Chaos**: at t=30 min, kill one orchestrator pod (`kubectl delete pod -l app=migration-orchestrator --field-selector status.phase=Running -o name | head -1 | xargs kubectl delete`).
   - Temporal must resume shards without duplicates.
4. **Cool-down**: watch drain until processed+failed == total.

### Success criteria

| Metric                             | Threshold                          |
| ---------------------------------- | ---------------------------------- |
| Job terminal status                | `succeeded`                        |
| Data loss                          | 0 rows missing                     |
| Failure rate                       | `failed / total < 0.1%`            |
| Sustained throughput               | `rows/sec >= 2_500`                |
| Endpoint p95 latency               | `< 500ms`                          |
| Transform p95 latency              | `< 50ms`                           |
| Recovery after pod kill            | Processed counter continues ≤ 10s  |
| Idempotency (retry all failed x3)  | No duplicate POSTs to sink         |

### Observability checks

- Grafana dashboard `Migration / Overview`:
  - "Rows/sec" stays above threshold line after ramp.
  - "Shard backlog" returns to 0 after chaos step.
- Prometheus alert `MigrationStalled` must NOT fire.
- Tempo trace sampling: select any `MigrationWorkflow` run and confirm no
  spans > 10s (transform batch).

### Artifacts to attach to sign-off

- k6 summary JSON (`--summary-export=load-10m.json`)
- Grafana dashboard snapshot
- Links to 3 Temporal Workflow histories (normal, chaos, retry-failed)
- `pg_dump --schema-only` + row counts before/after
