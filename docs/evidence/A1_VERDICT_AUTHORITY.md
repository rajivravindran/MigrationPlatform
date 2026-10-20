# A1 — One persisted terminal verdict (task evidence)

```text
Task ID / owner: Architecture review A1 (P0 contract repair) / Rajiv Ravindran; implemented by agent session 2026-10-19
Base SHA / result SHA / PR: working tree on local checkout (git state not inspected from this session; no commit made) / uncommitted / none
Dependencies verified: ADR 0004 lifecycle module present (R05); FinalizeJob routed through lifecycle.Finalize; reconciler shares finalize()
Problem and changed behaviour:
  Before: lifecycle.Finalize decided a job's terminal status (e.g. an empty job is failed) and persisted it, but
  returned nothing to the workflow; MigrationWorkflow returned zero counters with no error; BatchWorkflow
  reconstructed "succeeded" from failed/unresolved/cancelled counters and let dependent stages run.
  After: lifecycle.Finalize returns Verdict{Status, Reason, Totals}. FinalizeJob returns FinalizeJobOutput.
  finishJob returns the verdict; MigrationWorkflow/RetryRow/RetryFailedRows surface it as
  MigrationWorkflowResult.Status/Reason. BatchWorkflow (sequential and DAG) decides each stage with
  stageVerdict(): succeeded only for a persisted "succeeded"; "execution error: …" for a failed child
  workflow; "job <status>: <reason>" for a completed child with a failed/cancelled verdict; and
  "job completed without a persisted verdict … success not established" for a child that reported none.
  Reconciler log lines now include the verdict reason.
Files and schema/API changes:
  apps/orchestrator-go/internal/lifecycle/lifecycle.go (Verdict, Explain, status consts; Finalize returns Verdict)
  apps/orchestrator-go/internal/lifecycle/reconciler.go (consume Verdict)
  apps/orchestrator-go/internal/activities/activities.go (FinalizeJobOutput; FinalizeJob returns it)
  apps/orchestrator-go/internal/workflows/lifecycle.go (finishJob returns verdict; applyVerdict)
  apps/orchestrator-go/internal/workflows/workflows.go (Result.Status/Reason; all finishJob callers)
  apps/orchestrator-go/internal/workflows/batch.go (stageVerdict; both stage runners)
  Tests: internal/lifecycle/lifecycle_test.go, internal/workflows/{lifecycle,delivery,watch}_test.go,
  new internal/workflows/batch_verdict_test.go
  Docs: docs/adr/0004-job-lifecycle.md (amendment), docs/ARCHITECTURE_REVIEW_2026-10-17.md (A1 status)
  No SQL schema change. No HTTP API change. Workflow result JSON gains optional "status"/"reason" fields.
Before-fix reproduction or feature baseline:
  TestBatchWorkflowDAGParallelRootsThenDependent documented the defect: Ingest returned no rows and the
  FinalizeJob stub returned nothing, yet all three stages counted as succeeded. The new
  TestEmptyChildJobFailsItsStageAndBlocksDependents fails against the old batch.go (stage counted OK).
Commands actually run and results:
  go build ./... — ok
  go vet ./... — ok; gofmt -l . — clean
  go test ./... — ok for activities, batch, connectors, export, lifecycle, security, workflows (incl.
  replay tests on testdata/pre_r03_*.json). internal/template TestGoldenRoundtrip failed only because
  packages/rule-schema/fixtures/golden.json was not present in the build sandbox (fixture not staged; unrelated).
  Build environment note: the sandbox could not reach proxy.golang.org; a throwaway go.mod overlay with
  GitHub-mirror replace directives was used (not committed, no change to go.mod/go.sum).
Destination receipts / reconciliation evidence: not applicable (no destination calls changed).
Workflow replay and upgrade compatibility:
  Additive only: the FinalizeJob activity gained a return value (old histories decode an absent result as
  nil → treated as "no verdict"), and the workflow result gained two optional fields. No new commands, so
  no new GetVersion marker. Replay tests pass. Mixed-version rollout: a new parent with an old FinalizeJob
  worker records stages as "without a persisted verdict" (failed, not success) — deploy workers before or
  with the parent; an old parent with new children ignores the extra fields.
Rollout, rollback and recovery limits:
  Rollback to the previous build restores counter-based stage success (the A1 defect). Stages recorded as
  "without a persisted verdict" during a mixed rollout need operator review; the reconciler does not
  reinterpret batch stages.
Blocked checks / residual risks:
  Real-stack interleaving (empty child, cancelled child, errored child against Postgres+Temporal) not run
  from this session — no Docker/Temporal here. A2 (batch admission/repair locality) and A3 (pause two-write)
  remain open; batch-level repair of stages is still absent.
Status and reviewer: implemented-unverified; reviewer unassigned
Remote publication status: not committed, not pushed
```
