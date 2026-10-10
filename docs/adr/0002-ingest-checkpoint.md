# ADR 0002: Typed ingest checkpoint, history-aware continue-as-new, resumable Salesforce reads

- Status: accepted (R03, MP-02, MP-03). Implements the checkpoint contract in ADR 0001.
- Date: 2026-10-10

## Context

`MigrationWorkflow` reads its source in 500-row `Ingest` batches. Three
defects meant a large job could lose or repeat rows without any error:

1. **Continue-as-new restarted at row zero (MP-02).** Every 100 batches the
   workflow wrote the offset into `SourceRef["_offset"]` and continued as new,
   but the new run always started from an empty offset and zero totals. Row
   indexes restarted too, so `job_rows` entries were overwritten while the
   destination received each record again.
2. **The 100-batch interval ignored history size.** The pre-R03 harness
   histories show about 13 events per row for a single-step template and 19
   with a `$py` payload. A 500-row batch is therefore 6,500–9,500 events.
   Temporal terminates a run at 51,200 events, so a run was killed after
   roughly 6–8 batches, long before batch 100.
3. **Salesforce was not resumable (MP-03).** Each `Ingest` call created a
   new Bulk API query job, then applied the previous job's locator to it. The
   offset after a 500-row batch already pointed at the *next* 2,000-record
   page, so 1,500 rows of every page were skipped. Row indexes restarted at 0
   in every call. The activity also polled job completion in a loop without
   heartbeating, so a query slower than the 30 s heartbeat timeout was retried
   and created yet another query job.

## Decision

1. **Typed, versioned checkpoint.** A continued run receives
   `MigrationWorkflowInput.checkpoint`:

   ```json
   {"v": 1, "sourceVersion": "<etag>", "offset": {...},
    "nextRowIndex": 1000, "processed": 998, "failed": 2}
   ```

   - `v` must be 1. Any other value, negative values, `offset.items !=
     nextRowIndex`, or more outcomes than rows fails the run with the
     non-retryable error type `InvalidCheckpoint`. The workflow never falls
     back to row zero.
   - Source identity (`source_id`, `connector_id`) stays in the frozen
     `SourceRef` (ADR 0001); the checkpoint adds the version and position.
   - A legacy `_offset` string from a pre-R03 run is converted to a checkpoint
     at `offset.items`. A malformed one is `InvalidCheckpoint`. The totals were
     never carried, so they start at zero; `FinalizeJob` recomputes the job
     totals from `job_rows` either way.
   - The loaded template version is pinned into the continued input so a job
     started on "latest" cannot switch rules part way through.

2. **Source version is checked per batch.** `Ingest` returns the version it
   read (the object ETag). Later batches send it back as
   `expectedSourceVersion`. A different version is the non-retryable
   `SourceChanged` error. This catches a re-upload to the same key, which
   updates the registry row and would otherwise pass authorization.

3. **Row continuity is checked per batch.** Each batch must start at
   `nextRowIndex`, have no gaps, end at `offset.items`, and make progress
   unless the source reports done. A connector regression like MP-03 fails
   the run instead of losing rows or looping.

4. **History-aware continue-as-new.** After each batch, the pure helper
   `shouldContinueAsNew` continues as new when the server suggests it
   (`GetContinueAsNewSuggested`), or when the current history plus another
   batch of the same size would exceed 10,000 events or 8 MB. Those budgets
   sit below Temporal's warning thresholds (about a fifth of the 51,200-event
   / 50 MB limits), so one more batch always fits. In practice a 500-row batch
   continues as new after about every batch.

5. **Resumable Salesforce reads.** `Offset` records the Bulk query job
   (`sf_job`), the locator of the page being consumed (`sf_locator`, empty for
   the first page) and the rows of that page already delivered (`sf_skip`):
   - The query job is created once, by the first `Ingest`, and reused.
   - While the job is still running, `Ingest` returns `pending: true` with the
     offset. The workflow waits with durable timers (2 s doubling to 30 s)
     instead of the activity blocking.
   - A resumed read fetches the recorded page and skips `sf_skip` rows. An
     offset with progress but no `sf_job` (pre-R03), or a skip past the end of
     the page, is `InvalidCheckpoint`.
   - Results for a completed Bulk job are immutable, so re-fetching a page is
     deterministic. Each 2,000-record page is fetched up to four times (once
     per 500-row batch); see Consequences.

6. **Replay safety.** The behaviour that changes command order for in-flight
   runs (history-aware continue-as-new, continuity checks) is gated by
   `workflow.GetVersion("checkpointed-ingest", DefaultVersion, 1)`. Runs that
   started before the change keep the 100-batch interval but now pass a
   checkpoint when they continue. The pending wait only adds commands after a
   new activity result, so it needs no gate. `TestPreCheckpointHistoriesReplay`
   replays histories recorded by the pre-R03 worker.

## Consequences

- Large jobs now produce several workflow runs under one workflow ID. The
  Temporal UI shows them as `ContinuedAsNew` followed by one `Completed` run.
- Malformed or foreign checkpoints stop the job with a visible, non-retryable
  error. Until the R05 reconciler lands, such a job stays `running` in the API.
- Salesforce reads make up to 4× the results-page requests of a
  single-pass reader (four 500-row batches per 2,000-record page). For a
  1M-record extract this is about 2,000 extra GETs against the daily API
  limit. Matching `maxRecords` to the batch size would remove this. It is not
  done here because page size and batch size are independent knobs, and the
  Bulk API pages are cheap compared with the per-row destination calls.
- Throughput measured by the harness is about 5 rows/s for a single-step
  template, dominated by sequential per-row activities. This ADR does not
  change throughput.

## Alternatives considered

- **Persist the checkpoint in Postgres instead of workflow input.** It adds a
  write per batch, and on a mismatch it creates a second source of truth to
  reconcile with Temporal history. Continue-as-new input is already durable
  and atomic with the run handoff.
- **A separate "start Salesforce query" activity.** It would make query
  creation an explicit step, but it duplicates the authorization and token
  logic in `Ingest`. Encoding the job in the offset gives the same
  exactly-once creation, because the first `Ingest` result is recorded in
  history.
- **A fixed smaller batch interval (for example every 5 batches).** Rejected:
  multi-step templates grow history at different rates, and the server's
  suggestion already accounts for its own configured limits.
