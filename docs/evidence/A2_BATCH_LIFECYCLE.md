# A2 — Batch lifecycle convergence, Go side (task evidence)

```text
Task ID / owner: Architecture review A2 (P0 contract repair), orchestrator portion / Rajiv Ravindran; implemented by agent session 2026-10-19
Base SHA / result SHA / PR: working tree on local checkout (git state not inspected from this session; no commit made) / uncommitted / none
Dependencies verified: A1 verdict plumbing (same session); ADR 0004 reconciler and lifecycle module present (R05)
Problem and changed behaviour:
  Before: BatchWorkflow computed its final status from in-workflow counters and wrote it only on the happy
  path. A FinalizeBatchStage persistence error (or cancellation) returned from the workflow before
  FinalizeBatch, leaving batches.status = running forever; the reconciler only looked at jobs;
  MarkBatchRunning overwrote any status unconditionally.
  After: lifecycle.FinalizeBatch derives the batch verdict from persisted batch_stages plus the execution
  outcome (stages still pending/running are first recorded failed with "batch ended before this stage
  reached a terminal state"); quarantined is never overwritten. BatchWorkflow runs the body in runBatch and
  finalizes on every exit (finishBatch, disconnected context, FinalizeJob-style retry options) behind
  GetVersion("batch-finalize-on-exit"); old histories replay legacyFinalizeBatch. FinalizeBatch with an
  empty Status derives from stages and returns FinalizeBatchOutput{Status,Reason,Stages}; a non-empty
  Status keeps the legacy write (now also refusing to overwrite quarantined). MarkBatchRunning is
  conditional on pending/running. The reconciler pages stale pending/running batches, describes the
  workflow, moves pending→running when it is alive, otherwise finalizes through the same rule.
Files and schema/API changes:
  apps/orchestrator-go/internal/lifecycle/batch.go (new: StageTotals, BatchVerdict, BatchTerminalStatus, ExplainBatch, CountStages, FinalizeBatch)
  apps/orchestrator-go/internal/lifecycle/reconciler.go (staleBatches, describe, reconcileBatch; Pass covers batches)
  apps/orchestrator-go/internal/activities/batch.go (MarkBatchRunning conditional; FinalizeBatch derives + returns verdict)
  apps/orchestrator-go/internal/workflows/batch.go (runBatch, finishBatch, legacyFinalizeBatch, version gate; Result.Reason)
  apps/orchestrator-go/internal/metrics/metrics.go (batches_reconciled_total)
  Tests: internal/lifecycle/lifecycle_test.go (BatchTerminalStatus/ExplainBatch table),
  internal/workflows/batch_verdict_test.go (stage-persistence error still finalizes; cancelled batch
  finalized on disconnected context; verdict derived from stages), internal/workflows/watch_test.go stubs
  Docs: docs/adr/0004-job-lifecycle.md, docs/ARCHITECTURE_REVIEW_2026-10-17.md, docs/observability.md
  No SQL schema change. FinalizeBatchInput gains optional "cancelled"; Status becomes optional.
  BatchWorkflowResult gains optional "reason".
Before-fix reproduction or feature baseline:
  New TestStagePersistenceErrorStillFinalizesTheBatch: with FinalizeBatchStage failing for the first stage,
  the old workflow returned the error with zero FinalizeBatch calls (batch left running). It now records
  exactly one FinalizeBatch with Error set and the stub persists "failed".
Commands actually run and results:
  go build ./... — ok; go vet ./... — ok; gofmt -l . — clean
  go test ./... — ok for activities, batch, connectors, export, lifecycle, security, workflows (replay
  tests included). internal/template TestGoldenRoundtrip failed only for the unstaged golden.json fixture.
  Same sandbox note as A1 (GitHub-mirror go.mod overlay, not committed).
Destination receipts / reconciliation evidence: not applicable (no destination calls changed).
Workflow replay and upgrade compatibility:
  New GetVersion marker "batch-finalize-on-exit" inserted before the first activity; old histories take
  DefaultVersion and replay the previous command sequence (FinalizeBatch{Status} on success only). No
  batch history fixture exists in testdata, so legacy replay is argued from the gate, not replayed —
  record a BatchWorkflow history before deploying to add one. FinalizeBatch result decoding tolerates
  the activity returning nothing (older worker): result.Status then stays the counter-derived default.
Rollout, rollback and recovery limits:
  Deploy orchestrator workers together; a new workflow with an old FinalizeBatch worker receives an
  activity error for the unexpected return shape only if the old worker returns error-only — Temporal
  tolerates an ignored extra result, so the risk is the batch status being written by the legacy branch
  of the old worker (Status empty → "failed"). Reconciler repairs of batches set abandoned stages to
  failed; they do not restart child jobs or touch destinations.
Blocked checks / residual risks:
  Rust routes/batches.rs still writes failed/running unconditionally after start (not in this change: Rust
  sources were not staged and cannot be built in this sandbox). Real-stack interleavings (fast completion,
  lost start acknowledgment, parent termination) not executed. Batch cancellation is persisted as failed
  with reason "execution was cancelled" because batch_status has no cancelled member.
Status and reviewer: implemented-unverified; reviewer unassigned
Remote publication status: not committed, not pushed
```
