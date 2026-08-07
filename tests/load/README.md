# Load & smoke tests

Two tiers:

| Tier    | File                      | Where         | Rows     | Purpose                                    |
|---------|---------------------------|---------------|----------|--------------------------------------------|
| Laptop  | `smoke_laptop.js`         | Docker Compose| 10k      | Prove the pipeline end-to-end on a laptop. |
| Full    | `load_10m.js`             | Kubernetes    | 10M      | Stress test: throughput, fault tolerance.  |

The full plan (stages, success criteria, chaos steps) lives in
[`load_test_plan.md`](./load_test_plan.md).

## Prereqs

- `k6` >= 0.49 (`brew install k6` / `apt-get install k6`)
- For laptop smoke: `docker compose up` via `make dev-up && make seed` — this
  creates a published template and returns a `RULE_TEMPLATE_ID`.
- For the 10M test: the Helm chart deployed with HPA enabled and a target
  endpoint that can absorb sustained POST traffic (use the in-cluster
  `echo-sink` Deployment from `infra/helm/charts/echo-sink` or point at a
  staging API).

## Generating synthetic inputs

```bash
python tests/load/generate_csv.py --rows 10000 --out /tmp/smoke.csv
python tests/load/generate_csv.py --rows 10000000 --out /tmp/huge.csv --shards 40
```

The 10M generator shards the output into `huge_part_000.csv .. huge_part_039.csv`
so it can be uploaded in parallel to MinIO (`watched_prefix` connector) without
buffering a 10M-row file in the browser.

## Running the laptop smoke test

```bash
export API=http://localhost:8080
export TOKEN=$(./scripts/login.sh)      # or paste from /auth/login
export RULE_TEMPLATE_ID=1
export SOURCE_CSV=/tmp/smoke.csv

k6 run tests/load/smoke_laptop.js
```

Budget on a MacBook Pro / 16 GB dev laptop:

- 10k rows complete in < 90 s with `ROW_CONCURRENCY=16`.
- p95 end-to-end latency per row < 500 ms (httpbin.org sink).
- Zero failed rows.

## Running the 10M-row test in Kubernetes

```bash
export API=https://migration.example.com
export TOKEN=$(cursor secrets get migration/admin-token)
export RULE_TEMPLATE_ID=42
export WATCHED_PREFIX_BUCKET=migration
export WATCHED_PREFIX_GLOB='huge_part_*.csv'

kubectl -n migration apply -f tests/load/k6-job.yaml
kubectl -n migration logs job/k6-load -f
```

The job mounts a `k6` container that runs `load_10m.js`, which:

1. Uploads the sharded CSV to MinIO via `mc` (pre-step).
2. Kicks off a single Migration job using the `watched_prefix` connector.
3. Polls `/jobs/{id}` until `status in (succeeded, failed, cancelled)`.
4. Streams `/jobs/{id}/stream` (SSE) and records processed/sec.
5. Asserts:
   - `processed + failed == total_rows`
   - `failed / total_rows < 0.001`
   - `rows_per_second >= 2_500` (sustained)
   - Resumes cleanly after a simulated pod kill (`kubectl delete pod` on
     `migration-orchestrator` half-way through).
