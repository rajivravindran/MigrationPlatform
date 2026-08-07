# Sample batch package (P2)

Contents of this directory are packed as:

- [`../demo_batch.tar.gz`](../demo_batch.tar.gz)
- [`../demo_batch.zip`](../demo_batch.zip)

## `manifest.json`

```json
{
  "version": 1,
  "batchId": "demo-batch-1",
  "onStageFailure": "stop",
  "stages": [
    { "id": "contacts", "file": "contacts.csv", "templateKey": "contacts-upsert" },
    { "id": "pets", "file": "pets.csv", "templateKey": "pets-upsert" }
  ]
}
```

Publish templates with keys `contacts-upsert` and `pets-upsert` in your org before running.

## Smoke test

1. Rebuild/redeploy **api** + **orchestrator** (runs migration `0003_batches`).
2. Watched prefix glob that matches archives, e.g. `*` or `*.tar.gz`.
3. Upload `demo_batch.tar.gz` to MinIO under the connector prefix.
4. Trigger the schedule → **Batches** UI shows a batch with two stages → each stage links to a job.
