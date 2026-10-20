# Architecture review — 17 October 2026

Revision: `1cb02cff0cae720a3d348472015f5a73f52d331a` (unchanged since the previous production review). Review-only; comparison baseline `3b1ee66`. The local template-designer edit, untracked prior reviews and `tests/posts.csv` were preserved. The designer diff was sampled but is not the basis of the findings below.

## Assessment

There are consequential architectural issues. The principal weakness is duplicated ownership of execution decisions across routes, workflow return values, persistence activities and recovery processes. It causes actual state contradictions, rather than merely making the code less tidy.

Retain Postgres, Temporal, object storage, the Rust request layer, Go orchestration and isolated Python execution. The evidence does not justify a language rewrite, splitting into more services, or adding infrastructure. First create deeper modules whose interfaces enforce the guarantees currently left to callers.

Published templates already use separate immutable version rows, direct jobs pin the actual version, and continuation preserves that binding. Those are useful foundations. The operation ledger, lifecycle package and source registry are also useful, but their guarantees are not yet consistently enforced across all entry paths.

## Standards

### A1 — P1: Two modules independently decide whether a job succeeded

Evidence: `apps/orchestrator-go/internal/lifecycle/lifecycle.go:31–40`; `internal/workflows/workflows.go:82–90`; `internal/workflows/batch.go:437–469` (also the DAG path).

The lifecycle module declares an empty job failed. Finalization persists that verdict but does not return it to the parent workflow. The workflow returns zero outcome counters with no execution error. The batch module reconstructs success from failed/unresolved/cancelled counters, marks that same child successful and permits dependent stages to run.

This violates ADR0004's authority model and R05's requirement that job/batch/workflow outcomes agree. Make the lifecycle interface return one persisted terminal verdict, including why it was reached. Parent workflows should consume that verdict instead of reconstructing it. Keep execution errors distinct from a completed execution with a failed business outcome.

Acceptance: empty child, cancelled child and child with a preserved execution error must yield consistent job/stage/batch results, and failed parents cannot unblock dependent work. Preserve replay compatibility when changing workflow output.

**Status (2026-10-19): implemented-unverified.** `lifecycle.Finalize` now returns one persisted `Verdict` (status, reason, totals); `FinalizeJob` returns it; `MigrationWorkflowResult` carries `Status`/`Reason`; `workflows/batch.go` decides stage outcomes solely from that verdict (`stageVerdict`), keeping execution errors distinct and refusing to count a verdict-less completion as success. Unit/workflow tests and replay tests pass locally in a Temporal test environment; the real-stack interleaving run on the integrated commit remains to be executed. See [evidence/A1_VERDICT_AUTHORITY.md](evidence/A1_VERDICT_AUTHORITY.md).

### A2 — P2: Lifecycle repair has insufficient locality

Evidence: `apps/api/src/routes/batches.rs:199–216`; `apps/orchestrator-go/internal/workflows/batch.go:102–108`; `internal/lifecycle/reconciler.go:144–152`.

Jobs received conditional transitions and reconciliation, but batch admission still marks every start error as failed and unconditionally writes running after a successful start. A fast batch can finish before that write; an ambiguous start can be reported failed while executing. Parent stage-persistence errors exit before batch finalization, and reconciliation queries jobs only.

These are partly inherited defects. Their persistence shows that fixing one caller does not fix the domain rule. Define a shared lifecycle contract for start intent, ambiguous acceptance, conditional completion and repair, while retaining distinct job/batch/stage outcomes. It need not become a generic workflow framework or another service.

Acceptance: inject fast completion, lost start acknowledgment and parent termination. Each persisted execution eventually converges to its actual outcome; no blind restart of uncertain external work.

**Status (2026-10-19, later): both halves implemented; verified at the API/DB boundary** (`routes/batches.rs` start transitions are now conditional and tested in `batch_start_transitions_are_conditional`; see [evidence/P0_CLOSURE_2026-10-19.md](evidence/P0_CLOSURE_2026-10-19.md)). Earlier note:** `lifecycle.FinalizeBatch` derives the batch verdict from persisted `batch_stages` (abandoned stages are first recorded failed); `BatchWorkflow` finalizes on every exit on a disconnected context behind `GetVersion("batch-finalize-on-exit")`, so a stage-persistence error or cancellation no longer leaves the batch `running`; `MarkBatchRunning` is conditional on a non-terminal status; the reconciler now pages stale `pending`/`running` batches and repairs them through the same rule (`batches_reconciled_total`). Still open on the Rust side: `routes/batches.rs` marks every start error `failed` and writes `running` unconditionally after a start — it should mirror the job route's conditional transitions and leave an ambiguous start `pending` for the reconciler. Real-stack injection (fast completion, lost start acknowledgment, parent termination) not yet run. See [evidence/A2_BATCH_LIFECYCLE.md](evidence/A2_BATCH_LIFECYCLE.md).

### A3 — P2: Pause/resume exposes an unsafe two-write interface

Evidence: `apps/api/src/routes/jobs.rs:455–480`; `apps/orchestrator-go/internal/workflows/workflows.go:119–126`; `internal/lifecycle/reconciler.go:177–185`.

The route sends the pause signal, then records paused in Postgres. If the signal succeeds but the SQL write fails, workflow-local state is paused while the database says running. Resume eligibility uses the database state, and reconciliation only sees a running Temporal execution, so the usual resume path can reject the stuck job.

An HTTP success/failure response cannot represent both effects atomically. Put control commands behind an orchestration module interface with stable command identity, requested state and acknowledged state. Reconcile unacknowledged commands rather than treating a SQL status as proof of workflow state.

Acceptance: fail persistence after signal acceptance; retrying the command is safe and the operator can recover. Specify when pause acknowledgment actually prevents new dispatch.

**Status (2026-10-19): implemented; verified at API/DB/workflow-environment boundaries.** Commands have a stable id recorded before the signal; status is written only from the workflow's `pauseState` query; the reconciler converges unconfirmed commands; acknowledgment is defined as no further row dispatched (checked between rows and batches). ADR 0004 amendment; evidence in [P0_CLOSURE_2026-10-19.md](evidence/P0_CLOSURE_2026-10-19.md).

Standards: **3 findings**; worst A1, conflicting success authority.

## Spec

### A4 — P1: Watched-source identity is mistaken for execution intent

Evidence: `apps/orchestrator-go/internal/activities/watch.go:364–376,405–414`; `internal/workflows/watch.go:269–280`.

The workflow ID hashes only organization, bucket, key and ETag. Two schedules processing the same file with different templates/destinations therefore resolve to the same job. The second can be skipped as already started. Discovery cursor state is also shared at the connector level. Schedule names are unique, but there is no restriction making a connector exclusive to one schedule.

The source is the same; the requested external effects are different. Separate immutable SourceVersion identity from ExecutionIntent identity. A watched-execution module should own discovery progress and deduplication scoped to the schedule/plan intent. Explicitly define whether later plan versions reprocess old source versions.

Acceptance: two schedules over the same source with distinct destination templates both execute once; redelivery of either schedule does not create an extra execution. Plan identity changes require an explicit replay policy. This is an existing structural defect, not a newly introduced regression.

**Status (2026-10-19): implemented-unverified.** `WatchExecutionWorkflowID` / `BatchExecutionWorkflowID` hash the schedule with the source version; `StartWatchObjectJob` / `StartBatch` reuse a job or batch the same schedule already created under the legacy source-only id; discovery cursors are per schedule (`cursor.bySchedule.<id>.seen`) with the legacy connector-wide `cursor.seen` honoured as seen for all. Replay policy recorded in [ADR 0005](adr/0005-watched-execution-intent.md). Workflow and unit tests pass; real-stack run with two schedules over one connector not yet executed. See [evidence/A4_EXECUTION_INTENT.md](evidence/A4_EXECUTION_INTENT.md).

### A5 — P2: Admission validation differs by entry point

Evidence: `apps/api/src/routes/schedules.rs:86–110`; `apps/api/src/temporal.rs:178`; `apps/orchestrator-go/internal/activities/activities.go:109–118`.

Direct job creation requires a published template and derives its actual version. Schedule creation checks ownership only, accepts client-supplied version and connector kind, and always dispatches WatchPrefixWorkflow. That executor supports watched-prefix/SFTP inputs, so an owned Salesforce connector can be accepted into a schedule that cannot execute it; an unpublished template or wrong version can also cross admission.

Create one execution-admission module whose interface returns a validated execution kind and immutable template/source binding. Job, schedule and batch entry points should be adapters to it, with explicit differences rather than independent partial validation.

Acceptance: unsupported schedule connector kinds, unpublished templates and mismatched versions fail before creating a schedule; supported paths preserve their intended semantics.

**Status (2026-10-19): implemented-unverified.** `apps/api/src/admission.rs` is the single admission rule (`admit_template`: published and, when supplied, the requested version must be the published one; `admit_schedule_connector`: watched kinds only). `POST /jobs` and `POST /schedules` route through it. A schedule-route negative test is still to be written.

Spec: **2 findings**; worst A4, distinct migrations suppressing each other.

## Additional scaling concern

**A6 — P2: Destination rate limits are per process, while deployment scales processes.** `internal/security/ratelimit.go:14–43` holds token buckets in memory; every worker creates its own limiter (`cmd/worker/main.go:86`). Helm configures four orchestrator replicas and an HPA up to forty (`infra/k8s/helm/values.yaml:52–63`). Under sufficient work, N replicas can permit approximately N times the configured per-host rate and burst. The same host can also serve multiple separately limited destination accounts.

Define the quota scope—host, destination account or tenant—and distinguish a per-worker tuning knob from a destination-wide guarantee. Until coordinated admission is implemented, document/enforce a deployment-wide budget. If destination-wide limits are required, use a durable owner or coordinated quota at the dispatch seam using existing infrastructure. Verify the combined outbound rate with multiple workers; do not infer it from single-worker tests.

**Status (2026-10-19): interim budget implemented-unverified; coordinated quota open.** `DESTINATION_RPS`/`DESTINATION_BURST` are now read as the deployment-wide per-host budget and each worker enforces its share (`/ DESTINATION_RATE_WORKERS`, default 1; the Helm chart sets 40 = `hpa.maxReplicas`). The limiter is documented as a per-process knob (`ratelimit.go`, `docs/operations.md`), and the worker logs its share at startup. Scope remains per host; per-destination-account quotas and a coordinated limiter at the dispatch seam are R15 work. Combined outbound rate has not been measured.

CSV resume also reparses every preceding record for each batch (`internal/connectors/csv.go:53–64`). This is acknowledged R15 work, not a new surprise: smaller batches to control history increase repeated source work. Design immutable chunks or format-aware resume together with history budgets, then measure both parsing work and reconciled throughput.

## How the earlier integrity findings fit this architecture

The previous review's findings remain applicable because committed HEAD has not changed:

- **Source module:** authorization and version verification must be coupled to the actual read; exposing a raw bucket/key after a metadata check lets callers bypass immutability, especially archive staging.
- **Operation module:** replace reusable resend/confirmation booleans with durable logical decision generations. The module must distinguish an operator's new intent from redelivery of that same intent.
- **Evidence module:** export publication needs an outcome revision and generation fence. Separate workflow IDs alone do not prevent old exports overwriting newer evidence.
- **Checkpoint module:** resource budgets must apply before the next unit of work can exceed them, not only after 500 complete rows.

These deepen existing modules; they do not require a new microservice for each responsibility. See `PRINCIPAL_REVIEW_1cb02cf_2026-10-10.md` for the earlier failure traces and reproduction results.

## Future plan binding, not a claimed new regression

R08 remains relevant. `MaterializeBatchStages` resolves the latest published template on every attempt and overwrites existing stage bindings (`internal/activities/batch.go:367–398`). A lost activity acknowledgment followed by a newly published version can change the chosen plan on redelivery. Resolve once and return the existing binding on retries. Preserve secret references while allowing their credential values to rotate under an explicit policy.

**Status (2026-10-19): verified at the DB boundary** (`TestPGMaterializeBatchStagesBindsOnce`). `MaterializeBatchStages` now keeps an existing stage row's `rule_template_id`/`rule_template_version` (`COALESCE(existing, latest)` in the upsert) and only resolves the latest published version for stages with no binding; a redelivered attempt returns the binding first persisted. A row whose stage key, file or template key disagrees with the manifest fails with a non-retryable `StageBindingConflict` instead of being overwritten. Needs a DB-backed test (redeliver after publishing a newer version; assert the first version is used) — none exists in this sandbox.

Connector configuration is reloaded during ingestion, but current public connector routes do not provide a settings update operation. Therefore this review does not claim a demonstrated public connector-edit exploit. A complete immutable plan manifest is a future contract requirement.

## Implementation order and verification

1. Unify terminal verdict and lifecycle/control contracts (A1–A3). Keep adapters small; preserve domain-specific outcomes and old workflow histories. Test actual cross-module interleavings, not only helpers.
2. Define execution intent and validated admission (A4/A5), then persist-once batch plan binding. Preserve historical IDs; explicitly handle legacy watch jobs and cursor ownership before changing deduplication.
3. Complete the source-read, operation-generation and export-generation guarantees from the previous review. Do not roll back to workers that bypass new generation checks while work is active.
4. Establish quota scope and ingestion/history budgets (A6/R15), then measure multi-worker behavior under a declared workload.

No new tests, migrations, deployments or load experiments were run in this architectural pass. Findings are source-grounded failure traces; earlier executed tests remain historical evidence, not reruns. Coverage was deep for the referenced orchestration/admission/state paths, sampled for schema/deployment and the local UI diff, and incomplete for full frontend behavior, every connector, OS isolation, backup/restore and capacity. The recommendation is focused redesign of ownership and interfaces, not replacement of the stack.
