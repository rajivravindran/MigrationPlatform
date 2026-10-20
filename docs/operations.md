# Operations

This runbook covers deploy, upgrade, day-2 operations, and incident response.

## Kubernetes deployment

### Prerequisites

- Kubernetes 1.27+ with an Ingress controller (nginx/traefik).
- A managed Postgres 15+ or the bundled chart (`postgresql.enabled=true`).
- Temporal server reachable over gRPC (recommend Temporal Cloud for prod).
- MinIO or S3-compatible object storage.
- An OTLP endpoint (this chart ships a collector by default).

### Install

```bash
helm repo add migration-platform ./infra/helm
kubectl create namespace migration
kubectl -n migration create secret generic migration-platform \
  --from-literal=JWT_SIGNING_KEY="$(openssl rand -hex 64)" \
  --from-literal=AES_MASTER_KEY="$(openssl rand -hex 32)" \
  --from-literal=DATABASE_URL="postgres://migration:...@pg:5432/migration" \
  --from-literal=TEMPORAL_ADDRESS=temporal.svc:7233

helm install migration ./infra/helm/migration-platform -n migration \
  -f infra/helm/values.prod.yaml
```

Helm applies in order: CRDs → secrets/configmaps → **init Job** that runs
`migration-admin migrate` → Deployments → Services/Ingress/HPA/PDB →
CronJobs (backups).

### Post-install bootstrap

```bash
kubectl -n migration exec deploy/migration-api -- \
  migration-admin create-org --name "Production" --slug prod
kubectl -n migration exec deploy/migration-api -- \
  migration-admin create-user --org 1 --role admin \
  --email "ops@example.com" --password "$(openssl rand -hex 24)"
```

## Upgrades

1. `helm diff upgrade migration ./infra/helm/migration-platform -f values.prod.yaml`
2. Scale orchestrator to `minReplicas` just below the target to guarantee
   headroom during rollout.
3. `helm upgrade ...`. The `preUpgrade` Helm hook reruns migrations.
4. Watch Temporal UI for stuck Workflows (usually none; all activities idempotent).
5. Roll back with `helm rollback migration` if SLOs regress.

## Backups

A CronJob `migration-pg-backup` runs daily at 02:00 UTC, stores
`pg_dump --format=custom` into MinIO under `backups/<yyyy-mm-dd>/`, and
retains 30 days (configurable).

### Restore

```bash
kubectl -n migration scale deploy --all --replicas=0
kubectl -n migration exec -it deploy/postgres -- bash
# inside:
aws s3 cp s3://migration-backups/2026-04-18/migration.dump /tmp/
pg_restore --clean --if-exists -d migration /tmp/migration.dump
# then:
kubectl -n migration scale deploy migration-api --replicas=2
kubectl -n migration scale deploy migration-orchestrator --replicas=2
kubectl -n migration scale deploy migration-transform-worker --replicas=2
```

Full restore runbook (with Temporal resetworkflow guidance) lives in
`docs/runbooks/restore.md`.

## Day-2 operations

### Job stuck in `running`

1. Check Temporal Web: is the Workflow still heartbeating? If not, inspect
   history for a terminal failure.
2. Check orchestrator pod logs: `kubectl logs -l app=migration-orchestrator --tail=200`.
3. Look at Grafana → "Migration / Overview" → Shard backlog panel.
4. If a pod is wedged, `kubectl delete pod` it; Temporal resumes cleanly.
5. If Postgres is the culprit, look at `pg_stat_activity` for long queries
   on `job_rows`; all row writes use `INSERT ... ON CONFLICT DO NOTHING` so
   blocking is unusual.

### High failure rate

1. Alert `MigrationHighFailureRate` fires at `failed/total > 0.5%` for 10 min.
2. Identify the endpoint via the Grafana "Per-destination" panel.
3. Pause the job: `POST /jobs/{id}/pause`.
4. Inspect a few failed rows: `GET /jobs/{id}/rows?status=failed`.
5. When the downstream is healthy, resume or `POST /jobs/{id}/retry-failed`.

### Schedule never fires

1. `GET /schedules/{id}` — confirm `enabled=true` and `next_run_at` is set.
2. Open Temporal Web → Schedules → see last 5 actions; if "skipped: another
   run active" repeats, switch `overlap_policy` to `buffer_one`.
3. Check timezone; Temporal runs in `spec_json.timezone`, not cluster UTC.

### Rotating the AES master key

1. Generate a new key; update the Secret with both `AES_MASTER_KEY` and
   `AES_MASTER_KEY_PREV`.
2. Restart the API (`kubectl rollout restart deploy/migration-api`). New
   writes use the new key; reads try current then previous.
3. Run `migration-admin rotate-secrets` to rewrap all `secrets.ciphertext`.
4. Remove `AES_MASTER_KEY_PREV` from the Secret.

### Rotating JWT keys

1. `migration-admin rotate-jwt` generates a new RSA keypair in `jwt_keys`.
2. The API publishes both kids in `/.well-known/jwks.json`.
3. After `JWT_OLD_KEY_TTL` (default 24h), the old key is archived.

## Cost controls

- Replica counts are set in Helm / Compose. There is no shard-queue
  custom metric and no HPA on it: one job is one sequential workflow.
- `DESTINATION_RPS` / activity timeouts bound how long a stalled
  destination holds a worker slot.

### Destination rate limits are per process

The orchestrator's limiter (`internal/security/ratelimit.go`) is an in-memory
token bucket per destination host **in each worker process**. It is not a
destination-wide guarantee: N replicas allow about N times the configured
rate, and one host may serve several separately limited destination accounts.

`DESTINATION_RPS` and `DESTINATION_BURST` are therefore read as the
deployment-wide budget per host, and each worker enforces
`DESTINATION_RPS / DESTINATION_RATE_WORKERS` (burst divided likewise, minimum
1). Set `DESTINATION_RATE_WORKERS` to the **maximum** number of orchestrator
processes that can run (the HPA `maxReplicas`, 40 in the default chart). With
fewer replicas the budget is under-used; it is never exceeded unless more
processes run than declared. The worker logs its share at startup
(`destination rate budget per host`). Verify the combined outbound rate with
several workers against a counting fixture before relying on any figure; a
coordinated quota at the dispatch seam is the planned replacement (R15,
architecture review A6).
- `job_rows` hot columns are indexed; cold audit data is pruned after 180d.
- Set `storage.retention.daysInput=30` and `daysResults=90` in values.yaml.

## Deploying the 19 October 2026 P0 changes

Order matters:

1. Apply migration `0011_integrity_contracts.sql` first. Workers and the API
   read the new columns (`operations.decision_generation`,
   `jobs.outcome_revision`, `jobs.control_*`) unconditionally.
2. Roll the orchestrator workers and the API together. Pause/resume needs the
   bridge's `/v1/workflows/query` endpoint; on an older bridge the API
   records the command and returns 409 "recorded but not confirmed" until the
   reconciler converges it.
3. Mixed orchestrator versions are safe for batch finalisation (both the
   counter status and `DeriveFromStages` are sent). Running workflows pick up
   row-level pause and batch sizing only after their next continue-as-new.

Before rolling the orchestrator *back* past this change, run
`migration-admin merge-cursors` (ADR 0005).
