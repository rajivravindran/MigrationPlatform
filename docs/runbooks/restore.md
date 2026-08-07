# Runbook: Restore from `pg_dump` backup

Use this when a database restore is required after data loss, corruption,
or for a DR exercise.

## Prereqs

- Access to the MinIO/S3 bucket configured in `storage.backupsBucket`.
- `kubectl` pointed at the target cluster/namespace.
- Appropriate approval captured in the incident channel.

## Steps

### 1. Announce + freeze

Post in the on-call channel: "Restoring Postgres from `<dump>` — paused all
schedules and scaled app pods to 0. ETA <NN> min."

### 2. Pause schedules

```bash
kubectl -n migration exec deploy/migration-api -- \
  migration-admin schedule pause --all
```

### 3. Scale down application pods

```bash
kubectl -n migration scale deploy migration-api --replicas=0
kubectl -n migration scale deploy migration-orchestrator --replicas=0
kubectl -n migration scale deploy migration-transform-worker --replicas=0
```

Leave Temporal, Redis, Postgres, and OTEL running.

### 4. Fetch the dump

```bash
kubectl -n migration run pg-restore --rm -it --image=postgres:16 --restart=Never -- bash
# inside the ephemeral pod:
apt-get update && apt-get install -y awscli
aws --endpoint-url https://s3.internal \
  s3 cp s3://migration-backups/2026-04-17/migration.dump /tmp/migration.dump
```

### 5. Restore

Restore into a fresh database name first, then swap:

```bash
export PGHOST=postgres.migration.svc PGUSER=migration PGDATABASE=postgres
createdb migration_restore
pg_restore --clean --if-exists --no-owner --jobs=4 \
  --dbname=migration_restore /tmp/migration.dump

# verify
psql -d migration_restore -c "SELECT count(*) FROM jobs;"
psql -d migration_restore -c "SELECT count(*) FROM job_rows;"
psql -d migration_restore -c "SELECT max(created_at) FROM jobs;"

# swap (require downtime window)
psql -d postgres -c "ALTER DATABASE migration RENAME TO migration_old;"
psql -d postgres -c "ALTER DATABASE migration_restore RENAME TO migration;"
```

### 6. Re-run migrations

```bash
kubectl -n migration run migrate --rm -it \
  --image=ghcr.io/acme/migration-api:<tag> --restart=Never \
  --env=DATABASE_URL=postgres://... \
  -- migration-admin migrate
```

### 7. Reset Temporal if necessary

If the restored DB is older than the Temporal history, workflows may
reference rows that no longer exist. Reset affected Workflows:

```bash
temporal workflow reset --workflow-id <wf> --reason "post-restore cleanup" \
  --type LastWorkflowTask
```

For a full reset (rare, DR-only) recreate the Temporal namespace.

### 8. Scale up + resume

```bash
kubectl -n migration scale deploy migration-api --replicas=2
kubectl -n migration scale deploy migration-orchestrator --replicas=2
kubectl -n migration scale deploy migration-transform-worker --replicas=2
kubectl -n migration exec deploy/migration-api -- \
  migration-admin schedule resume --all
```

### 9. Validate

- Grafana dashboards green.
- Trigger one schedule manually, confirm a job succeeds end-to-end.
- Spot-check audit log entries match the expected timeframe.

### 10. Post-incident

- Archive the runbook timings.
- File a retro ticket with cause, remediation, prevention.
- Drop `migration_old` after 72h if everything is healthy.
